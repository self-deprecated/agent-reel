package reel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/links"
	"github.com/self-deprecated/agent-reel/internal/videoexport"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Link sources of a composition (EDL.md, "Layouts and links"). A source
// {"link":"parent:linkid"} is resolved through the parent source's links
// manifest joined with its areel:link records (ResolveLinks): a recording
// link freezes the child recording like any source, a screenshot link reads
// its copied asset (digest-checked), and a video link reads its path in place
// (CLI only; the library never serves it). The link's anchor alignment maps
// {"parent":…} timerefs and layout slots between the clocks.

const (
	// composeScreenshotEnd is a screenshot's nominal clock length: it has no
	// length of its own, so clips name one with duration or to.
	composeScreenshotEnd = visualtime.Time(24 * time.Hour)
	// composeVideoFramesMax bounds the decoded frames of one video slot.
	composeVideoFramesMax = editmap.MaxFrames
	// composePNGOverheadBytes is a PNG's worst-case framing per decoded frame
	// beyond its raw rgb24 pixels (signature, chunks, zlib stored blocks).
	composePNGOverheadBytes = 64 << 10
	// composeVideoSamples is the number of frames sampled to estimate a
	// video slot's decoded size.
	composeVideoSamples = 3
)

// composeImageBytesMax bounds all staged image slot files of one render (a
// variable so tests can lower it).
var composeImageBytesMax = videoexport.MaxJobBytes / 2

// composeVideoArgs is the ffmpeg decode of frames frames of a video slot from
// media instant from (the source's own timestamps, mediaStart included) at
// the chunk's sample rate, scaled to fit the slot, as numbered PNGs relative
// to the working directory. The image2 pattern is relative because ffmpeg
// %-expands the whole output name, so a directory containing % must not be
// in it.
func (j *composeJob) composeVideoArgs(src *composeSource, ch *editmap.Chunk, slot *editmap.ChunkSlot, from visualtime.Time, frames int) []string {
	seek := max(0, from-visualtime.Time(time.Second))
	return []string{"-nostdin", "-v", "error", "-copyts", "-ss", editmap.FormatSeconds(seek), "-i", src.path,
		"-map", "0:v:0", "-an", "-sn", "-dn",
		"-vf", "fps=fps=" + composeVideoFPS(j.comp.Rate, ch.Speed) + ":start_time=" + editmap.FormatSeconds(from) + ":round=up," +
			fmt.Sprintf("scale=w=%d:h=%d:force_original_aspect_ratio=decrease:flags=bicubic,format=rgb24", slot.Rect.W, slot.Rect.H),
		"-fps_mode", "passthrough", "-frames:v", strconv.Itoa(frames), "-f", "image2", "%06d.png"}
}

// composeVideoRawBytes is the raw worst case of a video slot's decode: every
// frame as rgb24 at the slot size plus PNG overhead.
func composeVideoRawBytes(ch *editmap.Chunk, slot *editmap.ChunkSlot) int64 {
	return int64(ch.Frames) * (int64(slot.Rect.W)*int64(slot.Rect.H)*3 + composePNGOverheadBytes)
}

// sampleVideoBytes estimates what slot s of chunk i stages: it decodes
// composeVideoSamples frames at evenly spaced samples of the span with the
// full decode's filter chain (into a private temporary directory, removed
// afterwards) and returns frames × mean PNG size × 1.5 + overhead.
func (j *composeJob) sampleVideoBytes(src *composeSource, i, s int) (int64, error) {
	ch := &j.comp.Chunks[i]
	slot := &ch.Slots[s]
	ffmpeg, err := exportExecutable("AREEL_EXPORT_FFMPEG", "ffmpeg")
	if err != nil {
		return 0, err
	}
	dir, err := os.MkdirTemp("", ".areel-compose-sample-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	var total int64
	for n := 0; n < composeVideoSamples; n++ {
		k := min(ch.Frames-1, (2*n+1)*ch.Frames/(2*composeVideoSamples))
		out := filepath.Join(dir, strconv.Itoa(n))
		if err := os.Mkdir(out, 0700); err != nil {
			return 0, err
		}
		cmd := exec.CommandContext(j.ctx, ffmpeg, j.composeVideoArgs(src, ch, slot, src.mediaStart+ch.SlotSample(s, k), 1)...)
		cmd.Dir = out
		stderr := &boundedStderr{}
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			return 0, fmt.Errorf("sample decode: %w (%s)", err, stderr.String())
		}
		info, err := os.Stat(filepath.Join(out, "000001.png"))
		if err != nil {
			return 0, fmt.Errorf("sample decode produced no frame")
		}
		total += info.Size()
	}
	mean := total / composeVideoSamples
	return int64(ch.Frames)*mean*3/2 + composePNGOverheadBytes, nil
}

// reserveVideoBytes adds the estimate of slot s of chunk i's decode (sampled,
// or the raw worst case when sampling fails) to the render's staged image
// budget, refusing it before any full decode when the total would exceed
// composeImageBytesMax. The plan runs it too.
func (j *composeJob) reserveVideoBytes(src *composeSource, i, s int) error {
	ch := &j.comp.Chunks[i]
	slot := &ch.Slots[s]
	need, err := j.sampleVideoBytes(src, i, s)
	if err != nil {
		need = composeVideoRawBytes(ch, slot)
	}
	if j.videoReserved+need > composeImageBytesMax {
		return fmt.Errorf("segment %q: the video slot %s is estimated to stage %d bytes of decoded frames (%d frames at %dx%d), beyond the %d-byte limit for one composition; shorten the span, lower the output rate or use a smaller slot", ch.ID, slot.Source, j.videoReserved+need, ch.Frames, slot.Rect.W, slot.Rect.H, composeImageBytesMax)
	}
	j.videoReserved += need
	return nil
}

// composeLinkManifest describes a link source in the plan and metadata.
type composeLinkManifest struct {
	Parent string `json:"parent"`
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	// Start is the link's start anchor on the parent clock with its
	// precision (estimated, declared or unknown).
	Start *composeAnchorManifest `json:"start,omitempty"`
	// Anchors are the alignment's anchor pairs (start and resume re-anchors).
	Anchors []composeAnchorManifest `json:"anchors"`
	Path    string                  `json:"path,omitempty"`
	Asset   string                  `json:"asset,omitempty"`
	// MediaSeconds is a video's probed duration.
	MediaSeconds string `json:"mediaSeconds,omitempty"`
}

type composeAnchorManifest struct {
	Phase        string `json:"phase,omitempty"`
	Segment      int    `json:"segment"`
	At           string `json:"at"`
	ChildSeconds string `json:"childSeconds,omitempty"`
	Precision    string `json:"precision"`
}

// linkSource freezes the link source name: its parent first, then the
// link's target.
func (c *composeSources) linkSource(name string, decl editmap.EDLSource) (*composeSource, error) {
	parentName, linkID, ok := editmap.ParseLink(decl.Link)
	if !ok {
		return nil, fmt.Errorf("source %s: invalid link %q", name, decl.Link)
	}
	parent, err := c.source(parentName)
	if err != nil {
		return nil, err
	}
	res, err := resolveLinks(c.ctx, c.root, parent.id, true, contextSealName(c.ctx))
	if err != nil {
		return nil, fmt.Errorf("source %s: links of %s: %w", name, parentName, err)
	}
	l := res.Find(linkID)
	if l == nil {
		var ids []string
		for _, r := range res.Links {
			ids = append(ids, r.Link.ID)
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("source %s: recording %s (source %s) has no links (areel link ls %s)", name, parent.id, parentName, parent.id)
		}
		return nil, fmt.Errorf("source %s: recording %s (source %s) has no link %q; its links: %q", name, parent.id, parentName, linkID, ids)
	}
	for _, issue := range l.Issues {
		switch issue {
		case "child-missing", "asset-missing", "asset-digest-mismatch":
			return nil, fmt.Errorf("source %s: link %s: %s (areel link ls %s)", name, linkID, issue, parent.id)
		}
	}
	var s *composeSource
	switch l.Link.Kind {
	case links.KindRecording:
		if s, err = c.freeze(name, l.Link.Ref.Recording); err != nil {
			return nil, err
		}
		s.kind = editmap.SourceRecording
	case links.KindScreenshot:
		s = &composeSource{name: name, kind: editmap.SourceScreenshot, digest: *l.Link.Digest}
	case links.KindVideo:
		s = &composeSource{name: name, kind: editmap.SourceVideo, path: l.Link.Ref.Path}
		if s.media, s.mediaStart, err = composeProbeVideo(c.ctx, s.path); err != nil {
			return nil, fmt.Errorf("source %s: video link %s: %w", name, linkID, err)
		}
	default:
		return nil, fmt.Errorf("source %s: link %s has unknown kind %q", name, linkID, l.Link.Kind)
	}
	s.parent, s.parentID, s.link = parentName, parent.id, l
	c.open[name] = s
	return s, nil
}

// manifestLink is the plan's view of a link source.
func (s *composeSource) manifestLink() *composeLinkManifest {
	if s.link == nil {
		return nil
	}
	l := s.link
	m := &composeLinkManifest{Parent: s.parent, ID: l.Link.ID, Kind: l.Link.Kind, Anchors: []composeAnchorManifest{}}
	if p := l.Start; p != nil {
		m.Start = &composeAnchorManifest{Segment: p.Segment, At: editmap.FormatSeconds(visualtime.Time(p.AtNs)), Precision: p.Precision}
	}
	for _, p := range l.Alignment.Pairs {
		m.Anchors = append(m.Anchors, composeAnchorManifest{Phase: p.Phase, Segment: p.Parent.Segment, At: editmap.FormatSeconds(visualtime.Time(p.Parent.AtNs)), ChildSeconds: editmap.FormatSeconds(visualtime.Time(p.ChildNs)), Precision: p.Precision})
	}
	switch l.Link.Kind {
	case links.KindVideo:
		m.Path, m.MediaSeconds = s.path, editmap.FormatSeconds(s.media)
	case links.KindScreenshot:
		m.Asset = l.Link.Ref.Asset
	}
	return m
}

func (c *composeSources) SourceLink(name string) (string, links.Alignment, bool, error) {
	s, err := c.source(name)
	if err != nil {
		return "", links.Alignment{}, false, err
	}
	if s.link == nil {
		return "", links.Alignment{}, false, nil
	}
	return s.parent, s.link.Alignment, true, nil
}

// ParentOf answers {"parent":…}: the link source's parent source.
func (c *composeSources) ParentOf(name string) (string, error) {
	s, err := c.source(name)
	if err != nil {
		return "", err
	}
	if s.link == nil {
		return "", fmt.Errorf(`source %s is not a link; {"parent":…} names a moment of a link source's parent`, name)
	}
	return s.parent, nil
}

// MapParent maps a parent instant onto the link's child clock (segment 1 of
// a recording, media seconds of a video, any instant of a screenshot).
func (c *composeSources) MapParent(name string, at editmap.Instant) (editmap.Instant, error) {
	s, err := c.source(name)
	if err != nil {
		return editmap.Instant{}, err
	}
	if s.link == nil {
		return editmap.Instant{}, fmt.Errorf("source %s is not a link", name)
	}
	child, err := s.link.Alignment.ParentToChild(links.ParentTime{Segment: at.Segment, AtNs: int64(at.At)})
	if err != nil {
		return editmap.Instant{}, err
	}
	return editmap.Instant{Segment: 1, At: visualtime.Time(child)}, nil
}

// composeProbeVideo reads a video's duration and first timestamp with
// ffprobe. The path must be a regular file (never a symlink or device).
func composeProbeVideo(ctx context.Context, path string) (visualtime.Time, visualtime.Time, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, 0, errors.New("the path is not a regular file")
	}
	probe, err := exportExecutable("AREEL_EXPORT_FFPROBE", "ffprobe")
	if err != nil {
		return 0, 0, fmt.Errorf("video links need ffprobe: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, probe, "-v", "error", "-select_streams", "v:0", "-show_entries", "format=duration,start_time:stream=codec_type", "-of", "json", "-i", path).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("ffprobe: %w", err)
	}
	var v struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
		Format struct {
			Duration  string `json:"duration"`
			StartTime string `json:"start_time"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &v); err != nil || len(v.Streams) == 0 || v.Streams[0].CodecType != "video" {
		return 0, 0, errors.New("ffprobe found no video stream")
	}
	seconds := func(s string) (visualtime.Time, error) {
		if whole, frac, ok := strings.Cut(s, "."); ok && len(frac) > 9 {
			s = whole + "." + frac[:9]
		}
		return visualtime.ParseSeconds(s)
	}
	duration, err := seconds(v.Format.Duration)
	if err != nil || duration <= 0 {
		return 0, 0, fmt.Errorf("ffprobe duration %q is not usable", v.Format.Duration)
	}
	start := visualtime.Time(0)
	if v.Format.StartTime != "" {
		if start, err = seconds(strings.TrimPrefix(v.Format.StartTime, "-")); err != nil {
			return 0, 0, fmt.Errorf("ffprobe start time %q is not usable", v.Format.StartTime)
		}
		if strings.HasPrefix(v.Format.StartTime, "-") {
			start = -start
		}
	}
	return duration, start, nil
}

// composeImageFiles are an image slot's staged files and their hash.
type composeImageFiles struct {
	Files  []string
	SHA256 string
}

// writeScreenshotSlot copies a screenshot link's verified asset into dir.
func (j *composeJob) writeScreenshotSlot(src *composeSource, dir string, i, s int) (composeImageFiles, error) {
	data, _, err := links.OpenAsset(j.root, src.parentID, src.link.Link)
	if err != nil {
		return composeImageFiles{}, fmt.Errorf("screenshot link %s: %w", src.link.Link.ID, err)
	}
	j.imageBytes += int64(len(data))
	path := filepath.Join(dir, fmt.Sprintf("chunk-%04d-slot%d%s", i, s, strings.ToLower(filepath.Ext(src.link.Link.Ref.Asset))))
	if err := os.WriteFile(path, data, 0600); err != nil {
		return composeImageFiles{}, err
	}
	return composeImageFiles{Files: []string{path}, SHA256: strings.TrimPrefix(src.digest, "sha256:")}, nil
}

// composeVideoFPS is the media rate a chunk's samples advance at:
// rate·speedDen/speedNum, as ffmpeg's num/den.
func composeVideoFPS(rate editmap.Rate, speed editmap.Speed) string {
	r := new(big.Rat).SetFrac64(rate.Num*speed.Den, rate.Den*speed.Num)
	return r.Num().String() + "/" + r.Denom().String()
}

// writeVideoSlot decodes the slot's span once, at the chunk's sample rate,
// into one PNG per local frame, scaled to fit the slot: frame k is the last
// video frame at or before the slot's sample k (ffmpeg's fps filter with
// round=up on the original timestamps). A video that ends before the span is
// refused, as are decoded frames beyond composeImageBytesMax in total.
func (j *composeJob) writeVideoSlot(src *composeSource, dir string, i, s int) (composeImageFiles, error) {
	ch := &j.comp.Chunks[i]
	slot := &ch.Slots[s]
	if ch.Frames > composeVideoFramesMax {
		return composeImageFiles{}, fmt.Errorf("segment %q: too many video frames", ch.ID)
	}
	ffmpeg, err := exportExecutable("AREEL_EXPORT_FFMPEG", "ffmpeg")
	if err != nil {
		return composeImageFiles{}, err
	}
	out := filepath.Join(dir, fmt.Sprintf("chunk-%04d-slot%d", i, s))
	if err := os.Mkdir(out, 0700); err != nil {
		return composeImageFiles{}, err
	}
	cmd := exec.CommandContext(j.ctx, ffmpeg, j.composeVideoArgs(src, ch, slot, src.mediaStart+slot.From, ch.Frames)...)
	cmd.Dir = out
	stderr := &boundedStderr{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return composeImageFiles{}, fmt.Errorf("segment %q: decoding video link %s: %w (%s)", ch.ID, src.link.Link.ID, err, stderr.String())
	}
	files := make([]string, ch.Frames)
	h := sha256.New()
	for k := range files {
		files[k] = filepath.Join(out, fmt.Sprintf("%06d.png", k+1))
		f, err := os.Open(files[k])
		if err != nil {
			return composeImageFiles{}, fmt.Errorf("segment %q: video link %s ends before the span (%d of %d frames decoded)", ch.ID, src.link.Link.ID, k, ch.Frames)
		}
		sum := sha256.New()
		n, err := io.Copy(sum, f)
		f.Close()
		if err != nil {
			return composeImageFiles{}, err
		}
		if j.imageBytes += n; j.imageBytes > composeImageBytesMax {
			return composeImageFiles{}, fmt.Errorf("segment %q: decoded video frames exceed %d bytes; shorten the video spans or lower the output size", ch.ID, composeImageBytesMax)
		}
		h.Write(sum.Sum(nil))
	}
	return composeImageFiles{Files: files, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// boundedStderr keeps the first 4 KiB of a child's stderr.
type boundedStderr struct{ b []byte }

func (w *boundedStderr) Write(p []byte) (int, error) {
	if room := 4096 - len(w.b); room > 0 {
		w.b = append(w.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
func (w *boundedStderr) String() string { return strings.TrimSpace(string(w.b)) }
