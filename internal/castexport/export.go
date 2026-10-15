package castexport

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrThemeChange means one cast header cannot represent all source palettes.
// Callers can offer separate segment artifacts rather than silently recolor.
var ErrThemeChange = errors.New("combining different terminal themes is unsupported")

const (
	maxSources       = 1024
	maxEvents        = 10_000_000
	maxBytes   int64 = 8 << 30
	maxLine          = 4 << 20
)

// Source must already be verified and immutable through its declared cutoff.
// Reader is consumed once but not closed. Duration excludes inter-segment pause
// gaps. Cutoff is caller-supplied provenance (e.g. archive frame/hash boundaries),
// not proof fabricated by this package. RecordedAt/EndedAt preserve wall timing.
type Source struct {
	Recording           string
	Segment             int
	Reader              io.Reader
	Bytes               int64 // fixed immutable cast extent; bytes appended afterward are never read
	Duration            float64
	Cutoff              json.RawMessage
	RecordedAt, EndedAt *time.Time
}

// Range uses the concatenated recorded-footage clock, excluding pause gaps.
// Events at Start are included; events at End are excluded. Nil selects all.
type Range struct{ Start, End float64 }

type SourceMetadata struct {
	Recording     string          `json:"recording"`
	Segment       int             `json:"segment"`
	TimelineStart float64         `json:"timelineStart"`
	Duration      float64         `json:"sourceDuration"`
	SelectedFrom  float64         `json:"selectedFrom"`
	SelectedTo    float64         `json:"selectedTo"`
	RecordedAt    *time.Time      `json:"recordedAt,omitempty"`
	EndedAt       *time.Time      `json:"endedAt,omitempty"`
	Cutoff        json.RawMessage `json:"cutoff"`
	SourceSHA256  string          `json:"sourceCastSHA256"`
	SourceBytes   int64           `json:"sourceCastBytes"`
}

type Metadata struct {
	Version               int              `json:"version"`
	Start                 float64          `json:"start"`
	End                   float64          `json:"end"`
	Duration              float64          `json:"duration"`
	PauseGapsOmitted      bool             `json:"pauseGapsOmitted"`
	RangeEndExclusive     bool             `json:"rangeEndExclusive"`
	UnflushedTailExcluded bool             `json:"unflushedTailExcluded"`
	ReconstructedState    bool             `json:"reconstructedState"`
	Sources               []SourceMetadata `json:"sources"`
	CastSHA256            string           `json:"castSHA256"`
	CastBytes             int64            `json:"castBytes"`
}

type castHeader struct {
	fields     map[string]json.RawMessage
	term       map[string]json.RawMessage
	cols, rows int
	theme      []byte
}

func header(line []byte) (castHeader, error) {
	var h castHeader
	if err := json.Unmarshal(line, &h.fields); err != nil {
		return h, err
	}
	var version int
	if json.Unmarshal(h.fields["version"], &version) != nil || version != 3 || json.Unmarshal(h.fields["term"], &h.term) != nil {
		return h, fmt.Errorf("export requires asciicast v3")
	}
	if json.Unmarshal(h.term["cols"], &h.cols) != nil || json.Unmarshal(h.term["rows"], &h.rows) != nil || !validGeometry(h.cols, h.rows) {
		return h, fmt.Errorf("invalid export geometry")
	}
	var theme any
	if raw := h.term["theme"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &theme); err != nil {
			return h, err
		}
	}
	h.theme, _ = json.Marshal(theme)
	return h, nil
}
func (h castHeader) encode(cols, rows int) ([]byte, error) {
	h.term["cols"], _ = json.Marshal(cols)
	h.term["rows"], _ = json.Marshal(rows)
	h.fields["term"], _ = json.Marshal(h.term)
	delete(h.fields, "duration") // Source duration is not this selection's duration.
	// The copied source header's timestamp remains source metadata, not a claim
	// that a mid-segment selection started at that wall time; see the sidecar.
	return json.Marshal(h.fields)
}

func line(r *bufio.Reader) ([]byte, error) {
	var result []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(result)+len(part) > maxLine {
			return nil, fmt.Errorf("export line limit")
		}
		result = append(result, part...)
		if err == nil {
			return result, nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF && len(result) > 0 {
			return nil, fmt.Errorf("partial cast line")
		}
		return nil, err
	}
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

type writer struct {
	output io.Writer
	time   float64
	bytes  int64
	theme  []byte
	begun  bool
}

func (w *writer) row(data []byte) error {
	if w.bytes+int64(len(data))+1 > maxBytes {
		return fmt.Errorf("export output byte limit")
	}
	data = append(data, '\n')
	n, err := w.output.Write(data)
	w.bytes += int64(n)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}
func (w *writer) event(at float64, kind, data string) error {
	delta := at - w.time
	if delta < 0 && delta > -1e-9 {
		delta = 0
	}
	if !finite(delta) {
		return fmt.Errorf("export event clock regressed")
	}
	row, err := json.Marshal([]any{delta, kind, data})
	if err != nil {
		return err
	}
	if err = w.row(row); err == nil {
		w.time = at
	}
	return err
}
func (w *writer) begin(h castHeader, at float64, checkpoint *Checkpoint) error {
	cols, rows := h.cols, h.rows
	if checkpoint != nil {
		cols, rows = checkpoint.Cols, checkpoint.Rows
	}
	if !w.begun {
		data, err := h.encode(cols, rows)
		if err != nil {
			return err
		}
		if err = w.row(data); err != nil {
			return err
		}
		w.begun = true
		w.theme = h.theme
	} else {
		if !bytes.Equal(w.theme, h.theme) {
			return ErrThemeChange
		}
		// Cancel any partial parser sequence before RIS, then set this segment's
		// original geometry. These are explicit segment-boundary reconstruction.
		if err := w.event(at, "o", "\x18\x1bc"); err != nil {
			return err
		}
		if err := w.event(at, "r", fmt.Sprintf("%dx%d", cols, rows)); err != nil {
			return err
		}
	}
	if checkpoint != nil {
		return w.event(at, "o", checkpoint.Init)
	}
	return nil
}

// Export writes a new cast artifact and returns its sidecar metadata. It never
// changes sources or asserts source completion. Output may be partial on error;
// write to an owned temporary destination and publish only after success.
// Full/whole-segment export needs no terminal helper. A nonzero range start
// requires an exact TerminalFactory; there is no raw-prefix bootstrap fallback.
func Export(ctx context.Context, output io.Writer, sources []Source, selection *Range, factory TerminalFactory) (Metadata, error) {
	var empty Metadata
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if len(sources) == 0 || len(sources) > maxSources || output == nil {
		return empty, fmt.Errorf("invalid export sources")
	}
	var total float64
	var inputBytes int64
	for _, s := range sources {
		if s.Reader == nil || s.Bytes <= 0 || s.Bytes > maxBytes || !finite(s.Duration) || s.Recording == "" || len(s.Recording) > 256 || s.Segment < 1 {
			return empty, fmt.Errorf("invalid source metadata")
		}
		var cutoff map[string]json.RawMessage
		if len(s.Cutoff) > 64<<10 || json.Unmarshal(s.Cutoff, &cutoff) != nil || cutoff == nil {
			return empty, fmt.Errorf("fixed cutoff provenance required")
		}
		inputBytes += s.Bytes
		if inputBytes > maxBytes {
			return empty, fmt.Errorf("export input byte limit")
		}
		total += s.Duration
		if !finite(total) || total > 315360000 {
			return empty, fmt.Errorf("export duration limit")
		}
	}
	start, end := float64(0), total
	if selection != nil {
		start, end = selection.Start, selection.End
		if !finite(start) || !finite(end) || end <= start || end > total {
			return empty, fmt.Errorf("invalid export range")
		}
	}
	metadata := Metadata{Version: 1, Start: start, End: end, Duration: end - start, PauseGapsOmitted: true, RangeEndExclusive: selection != nil, UnflushedTailExcluded: true}
	outHash := sha256.New()
	w := &writer{output: io.MultiWriter(output, outHash)}
	events := 0
	offset := float64(0)
	for i, s := range sources {
		selected := selection == nil || (offset < end && offset+s.Duration > start)
		from, to := math.Max(0, start-offset), math.Min(s.Duration, end-offset)
		if selection == nil {
			from, to = 0, s.Duration
		}
		sourceHash, err := exportSource(ctx, w, s, offset, from, to, start, selection == nil, i == len(sources)-1, selected, factory, &events)
		if err != nil {
			return empty, fmt.Errorf("export segment %d: %w", s.Segment, err)
		}
		if selected {
			metadata.Sources = append(metadata.Sources, SourceMetadata{Recording: s.Recording, Segment: s.Segment, TimelineStart: offset, Duration: s.Duration, SelectedFrom: from, SelectedTo: to, RecordedAt: s.RecordedAt, EndedAt: s.EndedAt, Cutoff: append(json.RawMessage(nil), s.Cutoff...), SourceSHA256: sourceHash, SourceBytes: s.Bytes})
			if from > 0 {
				metadata.ReconstructedState = true
			}
		}
		offset += s.Duration
	}
	if !w.begun {
		return empty, fmt.Errorf("selection has no terminal source")
	}
	if w.time < end-start {
		if err := w.event(end-start, "m", "Export boundary"); err != nil {
			return empty, err
		}
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	metadata.CastSHA256 = hex.EncodeToString(outHash.Sum(nil))
	metadata.CastBytes = w.bytes
	return metadata, nil
}

func exportSource(ctx context.Context, w *writer, s Source, offset, from, to, selectionStart float64, full, last, selected bool, factory TerminalFactory, eventCount *int) (string, error) {
	hash := sha256.New()
	fixed := &io.LimitedReader{R: s.Reader, N: s.Bytes}
	r := bufio.NewReaderSize(io.TeeReader(fixed, hash), 64<<10)
	first, err := line(r)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(first) {
		return "", fmt.Errorf("invalid cast header UTF-8")
	}
	h, err := header(first)
	if err != nil {
		return "", err
	}
	var terminal Terminal
	if selected && from > 0 {
		if factory == nil {
			return "", fmt.Errorf("range reconstruction requires exact checkpoint helper")
		}
		terminal, err = factory(ctx, h.cols, h.rows)
		if err != nil {
			return "", err
		}
		defer terminal.Close()
	}
	begun := false
	begin := func() error {
		if begun || !selected {
			return nil
		}
		begun = true
		var checkpoint *Checkpoint
		if terminal != nil {
			value, err := terminal.Snapshot()
			if err != nil {
				return err
			}
			if !validGeometry(value.Cols, value.Rows) || len(value.Init) > MaxCheckpointBytes {
				return fmt.Errorf("invalid terminal checkpoint")
			}
			checkpoint = &value
		}
		return w.begin(h, math.Max(0, offset-selectionStart), checkpoint)
	}
	at := float64(0)
	for {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		data, e := line(r)
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return "", e
		}
		*eventCount = *eventCount + 1
		if *eventCount > maxEvents {
			return "", fmt.Errorf("export event limit")
		}
		var event []json.RawMessage
		if !utf8.Valid(data) || json.Unmarshal(data, &event) != nil || len(event) != 3 {
			return "", fmt.Errorf("invalid cast event")
		}
		var delta float64
		var kind, text string
		if json.Unmarshal(event[0], &delta) != nil || !finite(delta) || json.Unmarshal(event[1], &kind) != nil || json.Unmarshal(event[2], &text) != nil {
			return "", fmt.Errorf("invalid cast event fields")
		}
		at += delta
		if !finite(at) || at > s.Duration+math.Max(1e-9, s.Duration*1e-12) {
			return "", fmt.Errorf("event exceeds source cutoff duration")
		}
		var cols, rows int
		switch kind {
		case "o", "i", "m", "x":
		case "r":
			parts := strings.Split(text, "x")
			if len(parts) != 2 {
				return "", fmt.Errorf("invalid resize")
			}
			cols, err = strconv.Atoi(parts[0])
			if err != nil {
				return "", err
			}
			rows, err = strconv.Atoi(parts[1])
			if err != nil || !validGeometry(cols, rows) {
				return "", fmt.Errorf("invalid resize geometry")
			}
		default:
			return "", fmt.Errorf("unsupported cast event")
		}
		if selected && at < from && terminal != nil {
			if kind == "o" {
				err = terminal.Feed([]byte(text))
			} else if kind == "r" {
				err = terminal.Resize(cols, rows)
			}
			if err != nil {
				return "", err
			}
		}
		if selected && at >= from && (full || at < to) {
			if err = begin(); err != nil {
				return "", err
			}
			if kind == "i" || (kind == "x" && !last) {
				continue
			}
			if err = w.event(offset+at-selectionStart, kind, text); err != nil {
				return "", err
			}
		}
	}
	if fixed.N != 0 {
		return "", fmt.Errorf("source ended before fixed byte cutoff")
	}
	if math.Abs(at-s.Duration) > math.Max(1e-9, s.Duration*1e-12) {
		return "", fmt.Errorf("source ended before declared cutoff duration")
	}
	if err = begin(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
