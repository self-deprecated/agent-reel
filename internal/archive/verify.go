package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// Header is archive.json: the legacy raw archive's metadata shape (identity,
// initial geometry, limits). The asciicast header inside the frames is the
// external recorder format and is validated separately in format.go.
type Header struct {
	Identity Identity `json:"identity"`
	Geometry Geometry `json:"geometry"`
	Limits   Limits   `json:"limits"`
}
type Entry struct {
	Sequence         int    `json:"sequence"`
	Offset           int64  `json:"offset"`
	Bytes            int    `json:"bytes"`
	LineOffset       int64  `json:"lineOffset"`
	Lines            int64  `json:"lines"`
	CompressedBytes  int    `json:"compressedBytes"`
	SHA256           string `json:"sha256"`
	CompressedSHA256 string `json:"compressedSHA256"`
}
type Completion struct {
	Frames       int    `json:"frames"`
	Bytes        int64  `json:"bytes"`
	Lines        int64  `json:"lines"`
	SHA256       string `json:"sha256"`
	SinkComplete bool   `json:"sinkComplete"`
}

func frameName(n int) string { return fmt.Sprintf("%06d.zst", n) }
func entryName(n int) string { return fmt.Sprintf("%06d.json", n) }
func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func largestWindow(n int) int {
	w := 1024
	for w < n {
		w *= 2
	}
	return w
}

func readBounded(path string, limit int) ([]byte, error) {
	// Reject FIFOs/devices before open, which could otherwise block before the
	// size/type check. Same-UID replacement races are not an auth boundary.
	st, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Size() > int64(limit) {
		return nil, fmt.Errorf("nonregular/oversized archive file %s", path)
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e = f.Stat()
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Size() > int64(limit) {
		return nil, fmt.Errorf("nonregular/oversized archive file %s", path)
	}
	data, e := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if e == nil && len(data) > limit {
		e = fmt.Errorf("archive file exceeds bound")
	}
	return data, e
}
func readMetadata(path string, v any) error {
	b, e := readBounded(path, MetadataBytes)
	if e != nil {
		return e
	}
	return decodeMetadata(b, v)
}
func decodeMetadata(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("trailing metadata")
	}
	return nil
}

// Verify reads a completed legacy raw archive (the removed recording format)
// and streams only fully verified chunks to out (nil discards). Earlier chunks
// may already be emitted when a later chunk fails. No repair is performed. It
// is read-only and kept solely for the internal/loopoc PoC measurement of
// existing archives; no agent-reel command reads raw archives.
func Verify(dir string, out io.Writer) (Completion, error) {
	var c Completion
	if e := readMetadata(filepath.Join(dir, "complete.json"), &c); e != nil {
		return Completion{}, fmt.Errorf("archive incomplete: %w", e)
	}
	if c.Frames < 1 || c.Frames > MaxFrames || !c.SinkComplete {
		return Completion{}, fmt.Errorf("invalid completion")
	}
	got, e := verify(dir, out, c.Frames, true)
	if e != nil {
		return got, e
	}
	if got != c {
		return got, fmt.Errorf("completion totals mismatch")
	}
	return got, nil
}

// frameReader reads one frame at a time for Verify. accept is called only
// after every check for the entire frame succeeds.
type verifiedFrame struct {
	entry                     Entry
	metadata, compressed, raw []byte
}
type frameReader struct {
	dir          string
	header       Header
	headerBytes  []byte
	decoder      *zstd.Decoder
	validator    Validator
	result       Completion
	sum          hash.Hash
	read         func(string, int) ([]byte, error)
	decodeBudget func(int) error
	candidate    *Entry
}

func openFrames(dir string, read func(string, int) ([]byte, error)) (*frameReader, error) {
	r := &frameReader{dir: dir, read: read, sum: sha256.New()}
	b, e := read(filepath.Join(dir, "archive.json"), MetadataBytes)
	if e != nil {
		return nil, e
	}
	// Existing headers may carry an inert obsolete `version: 1` member; the
	// original bytes remain the header identity for hashing and salvage copies.
	if e = decodeMetadata(WithoutLegacyVersion(b), &r.header); e != nil {
		return nil, e
	}
	h := r.header
	if h.Geometry.Columns < 1 || h.Geometry.Rows < 1 || h.Geometry.Columns > 65535 || h.Geometry.Rows > 65535 || !h.Limits.valid() || h.Identity.Recording == "" || h.Identity.Segment < 1 || h.Identity.Backend == "" || h.Identity.Instance == "" {
		return nil, fmt.Errorf("invalid archive header")
	}
	r.headerBytes = b
	r.validator.Geometry = h.Geometry
	r.decoder, e = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(DecoderMemory), zstd.WithDecoderMaxWindow(MaxChunkBytes), zstd.WithDecodeAllCapLimit(true))
	return r, e
}
func (r *frameReader) next(accept func(verifiedFrame) error) error {
	r.candidate = nil
	seq := r.result.Frames
	var entry Entry
	metadata, e := r.read(filepath.Join(r.dir, entryName(seq)), MetadataBytes)
	if e != nil {
		return e
	}
	if e = decodeMetadata(metadata, &entry); e != nil {
		return e
	}
	r.candidate = &entry
	h := r.header
	if entry.Sequence != seq || entry.Offset != r.result.Bytes || entry.LineOffset != r.result.Lines || entry.Bytes < 1 || entry.Bytes > h.Limits.ChunkBytes || entry.Lines < 1 || entry.Lines > int64(entry.Bytes) || entry.CompressedBytes < 1 || entry.CompressedBytes > compressedLimit(h.Limits.ChunkBytes) {
		return fmt.Errorf("invalid manifest sequence/offset/limits at %d", seq)
	}
	if r.decodeBudget != nil {
		if e = r.decodeBudget(entry.Bytes); e != nil {
			return e
		}
	}
	compressed, e := r.read(filepath.Join(r.dir, frameName(seq)), compressedLimit(h.Limits.ChunkBytes))
	if e != nil {
		return e
	}
	if len(compressed) != entry.CompressedBytes || digest(compressed) != entry.CompressedSHA256 {
		return fmt.Errorf("compressed hash/size mismatch at %d", seq)
	}
	if e = oneFrame(compressed, largestWindow(h.Limits.ChunkBytes)); e != nil {
		return e
	}
	raw, e := r.decoder.DecodeAll(compressed, make([]byte, 0, entry.Bytes))
	if e != nil {
		return fmt.Errorf("untrusted frame %d: %w", seq, e)
	}
	if len(raw) != entry.Bytes || digest(raw) != entry.SHA256 {
		return fmt.Errorf("raw hash/size mismatch at %d", seq)
	}
	v := r.validator // Reject a frame atomically, including its validator state.
	before := v.Lines
	for line := raw; len(line) > 0; {
		n := bytes.IndexByte(line, '\n')
		if n < 0 || n+1 > h.Limits.LineBytes {
			return fmt.Errorf("incomplete/oversized line in frame %d", seq)
		}
		if e = v.Line(line[:n+1]); e != nil {
			return e
		}
		line = line[n+1:]
	}
	if v.Lines-before != entry.Lines {
		return fmt.Errorf("line count mismatch")
	}
	if accept != nil {
		if e = accept(verifiedFrame{entry, metadata, compressed, raw}); e != nil {
			return e
		}
	}
	_, _ = r.sum.Write(raw)
	r.validator = v
	r.result.Frames++
	r.result.Bytes += int64(len(raw))
	r.result.Lines = v.Lines
	r.result.SHA256 = hex.EncodeToString(r.sum.Sum(nil))
	return nil
}
func verify(dir string, out io.Writer, count int, finished bool) (Completion, error) {
	r, e := openFrames(dir, readBounded)
	if e != nil {
		return Completion{}, e
	}
	defer r.decoder.Close()
	for seq := 0; seq < count; seq++ {
		e = r.next(func(f verifiedFrame) error {
			if out == nil {
				return nil
			}
			n, e := out.Write(f.raw)
			if e == nil && n != len(f.raw) {
				e = io.ErrShortWrite
			}
			return e
		})
		if e != nil {
			return r.result, e
		}
	}
	if e = r.validator.Complete(); e != nil {
		return r.result, e
	}
	if finished {
		for _, name := range []string{entryName(count), frameName(count)} {
			if _, e = os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(e) {
				return r.result, fmt.Errorf("unexpected archive tail: %s", name)
			}
		}
	}
	r.result.SinkComplete = true
	return r.result, nil
}

// The public zstd decoder accepts concatenated/skippable frames and exposes no
// next-frame boundary API. Use its header parser, then walk ONLY standard block
// lengths (RFC 8878 section 3.1.1.2), not compressed contents. The library still
// performs all actual decoding and checksum validation.
func oneFrame(b []byte, max int) error {
	var h zstd.Header
	if e := h.Decode(b); e != nil {
		return e
	}
	if h.Skippable || h.DictionaryID != 0 || !h.HasCheckSum || h.WindowSize > uint64(max) || (h.HasFCS && h.FrameContentSize > uint64(max)) {
		return fmt.Errorf("unsupported zstd frame header")
	}
	pos := h.HeaderSize
	for {
		if len(b)-pos < 3 {
			return io.ErrUnexpectedEOF
		}
		word := binary.LittleEndian.Uint32([]byte{b[pos], b[pos+1], b[pos+2], 0})
		pos += 3
		last := word&1 != 0
		kind := (word >> 1) & 3
		n := int(word >> 3)
		if kind == 3 || n > 128<<10 {
			return fmt.Errorf("invalid zstd block")
		}
		if kind == 1 {
			n = 1
		}
		if n > len(b)-pos {
			return io.ErrUnexpectedEOF
		}
		pos += n
		if last {
			break
		}
	}
	if len(b)-pos != 4 {
		return fmt.Errorf("expected exactly one checksummed zstd frame")
	}
	return nil
}
func compressedLimit(n int) int { return n + n/128 + 1024 } // exceeds Encoder.MaxEncodedSize for supported chunk bounds
