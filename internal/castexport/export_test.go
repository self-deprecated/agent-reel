package castexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

func fixture(events string, duration float64) (Source, []byte) {
	raw := []byte("{\"version\":3,\"term\":{\"cols\":80,\"rows\":24}}\n" + events)
	return Source{Recording: "recording", Segment: 1, Reader: bytes.NewReader(raw), Bytes: int64(len(raw)), Duration: duration, Cutoff: json.RawMessage(`{"frames":3,"complete":false}`)}, raw
}
func events(t *testing.T, raw []byte) (map[string]any, []struct {
	At         float64
	Kind, Text string
}) {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	var header map[string]any
	if err := json.Unmarshal(lines[0], &header); err != nil {
		t.Fatal(err)
	}
	var result []struct {
		At         float64
		Kind, Text string
	}
	at := float64(0)
	for _, line := range lines[1:] {
		var v []json.RawMessage
		if err := json.Unmarshal(line, &v); err != nil {
			t.Fatal(err)
		}
		var delta float64
		var kind, text string
		if json.Unmarshal(v[0], &delta) != nil || json.Unmarshal(v[1], &kind) != nil || json.Unmarshal(v[2], &text) != nil {
			t.Fatal("bad event")
		}
		at += delta
		result = append(result, struct {
			At         float64
			Kind, Text string
		}{at, kind, text})
	}
	return header, result
}
func TestFullFixedCutoffExcludesAppendAndInput(t *testing.T) {
	s, raw := fixture("[0,\"o\",\"first\"]\n[1,\"i\",\"INPUT-SECRET\"]\n[1,\"o\",\"second\"]\n[1,\"x\",\"0\"]\n", 3)
	s.Reader = bytes.NewReader(append(append([]byte{}, raw...), []byte("[0,\"o\",\"AFTER-CUTOFF\"]\n")...))
	var out bytes.Buffer
	meta, err := Export(context.Background(), &out, []Source{s}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "SECRET") || strings.Contains(out.String(), "AFTER-CUTOFF") {
		t.Fatal("excluded source content leaked")
	}
	_, ev := events(t, out.Bytes())
	if len(ev) != 3 || ev[0].At != 0 || ev[1].At != 2 || ev[2].At != 3 || meta.Duration != 3 {
		t.Fatalf("timing: %+v %+v", ev, meta)
	}
	wantHash := sha256.Sum256(raw)
	if meta.Sources[0].SourceSHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatal("source digest included post-cutoff bytes")
	}
	if meta.CastBytes != int64(out.Len()) || len(meta.CastSHA256) != 64 || len(meta.Sources[0].SourceSHA256) != 64 || !meta.UnflushedTailExcluded {
		t.Fatalf("missing cutoff provenance: %+v", meta)
	}
}
func TestCombineOmitsPauseButPreservesWallTiming(t *testing.T) {
	a, _ := fixture("[0,\"o\",\"first\"]\n[1,\"x\",\"0\"]\n", 1)
	b, _ := fixture("[0,\"o\",\"second\"]\n[2,\"x\",\"0\"]\n", 2)
	b.Segment = 2
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	endA := at.Add(time.Second)
	startB := at.Add(100 * time.Second)
	endB := startB.Add(2 * time.Second)
	a.RecordedAt = &at
	a.EndedAt = &endA
	b.RecordedAt = &startB
	b.EndedAt = &endB
	var out bytes.Buffer
	meta, err := Export(context.Background(), &out, []Source{a, b}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, ev := events(t, out.Bytes())
	foundSecond := false
	exits := 0
	for _, e := range ev {
		if e.Kind == "x" {
			exits++
			if e.At != 3 {
				t.Fatal("intermediate exit survived concatenation")
			}
		}
		if e.Text == "second" {
			foundSecond = true
			if e.At != 1 {
				t.Fatal("pause included in footage")
			}
		}
	}
	if !foundSecond || exits != 1 || meta.Duration != 3 || !meta.PauseGapsOmitted || !meta.Sources[1].RecordedAt.Equal(startB) || meta.Sources[1].TimelineStart != 1 {
		t.Fatalf("provenance: %+v", meta)
	}
	if !strings.Contains(out.String(), `\u0018\u001bc`) {
		t.Fatal("segment state not reset")
	}
}

type checkpointFake struct {
	feed       string
	cols, rows int
	snapshots  int
	closed     bool
}

func (s *checkpointFake) Feed(b []byte) error   { s.feed += string(b); return nil }
func (s *checkpointFake) Resize(c, r int) error { s.cols, s.rows = c, r; return nil }
func (s *checkpointFake) Snapshot() (Checkpoint, error) {
	s.snapshots++
	return Checkpoint{s.cols, s.rows, "SAFE-STATE"}, nil
}
func (s *checkpointFake) Close() error { s.closed = true; return nil }
func TestRangeUsesCheckpointNotRawHistory(t *testing.T) {
	s, _ := fixture("[0,\"o\",\"ERASED-SECRET\"]\n[1,\"o\",\"clear\"]\n[0,\"r\",\"100x30\"]\n[1,\"o\",\"SELECTED\"]\n[1,\"o\",\"END-EXCLUDED\"]\n", 3)
	terminal := &checkpointFake{}
	factory := func(_ context.Context, c, r int) (Terminal, error) {
		terminal.cols, terminal.rows = c, r
		return terminal, nil
	}
	var out bytes.Buffer
	meta, err := Export(context.Background(), &out, []Source{s}, &Range{Start: 2, End: 3}, factory)
	if err != nil {
		t.Fatal(err)
	}
	if !terminal.closed || terminal.snapshots != 1 || terminal.feed != "ERASED-SECRETclear" {
		t.Fatalf("checkpoint calls %+v", terminal)
	}
	if strings.Contains(out.String(), "ERASED-SECRET") || strings.Contains(out.String(), "END-EXCLUDED") || !strings.Contains(out.String(), "SAFE-STATE") {
		t.Fatal("range output leaked prior/raw/excluded content")
	}
	h, ev := events(t, out.Bytes())
	term := h["term"].(map[string]any)
	if term["cols"] != float64(100) || term["rows"] != float64(30) || ev[1].At != 0 || ev[len(ev)-1].At != 1 || meta.Sources[0].SelectedFrom != 2 {
		t.Fatalf("range time/geometry wrong: %s %+v", out.String(), meta)
	}
}
func TestRangeAcrossSegmentsUsesFootageClock(t *testing.T) {
	a, _ := fixture("[0,\"o\",\"PRIOR\"]\n[1,\"x\",\"0\"]\n", 1)
	b, _ := fixture("[0,\"o\",\"NEXT\"]\n[2,\"x\",\"0\"]\n", 2)
	b.Segment = 2
	terminal := &checkpointFake{cols: 80, rows: 24}
	var out bytes.Buffer
	metadata, err := Export(context.Background(), &out, []Source{a, b}, &Range{.5, 1.5}, func(context.Context, int, int) (Terminal, error) { return terminal, nil })
	if err != nil {
		t.Fatal(err)
	}
	_, ev := events(t, out.Bytes())
	found := false
	for _, e := range ev {
		if e.Text == "NEXT" {
			found = true
			if e.At != .5 {
				t.Fatal("incorrect cross-segment timeline")
			}
		}
	}
	if !found || metadata.Duration != 1 || len(metadata.Sources) != 2 || ev[len(ev)-1].At != 1 || strings.Contains(out.String(), "PRIOR") {
		t.Fatalf("bad cross-segment selection: %s", out.String())
	}
}

func TestRangeDuringIdleStillGetsStateAndDuration(t *testing.T) {
	s, _ := fixture("[0,\"o\",\"before\"]\n[10,\"o\",\"after\"]\n", 10)
	terminal := &checkpointFake{cols: 80, rows: 24}
	var out bytes.Buffer
	_, err := Export(context.Background(), &out, []Source{s}, &Range{2, 4}, func(context.Context, int, int) (Terminal, error) { return terminal, nil })
	if err != nil {
		t.Fatal(err)
	}
	_, ev := events(t, out.Bytes())
	if len(ev) != 2 || ev[0].Text != "SAFE-STATE" || ev[1].At != 2 {
		t.Fatalf("idle range wrong: %+v", ev)
	}
}
func TestExportRefusals(t *testing.T) {
	for _, kind := range []string{"no-helper", "truncated", "duration", "nan", "no-provenance", "bad-resize", "partial-line", "cancelled", "bad-range"} {
		t.Run(kind, func(t *testing.T) {
			s, raw := fixture("[0,\"o\",\"a\"]\n[1,\"o\",\"b\"]\n", 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var selection *Range
			switch kind {
			case "no-helper":
				selection = &Range{.5, 1}
			case "truncated":
				s.Bytes++
			case "duration":
				s.Duration = 2
			case "nan":
				s.Duration = math.NaN()
			case "no-provenance":
				s.Cutoff = nil
			case "bad-resize":
				s, _ = fixture("[0,\"r\",\"999x999\"]\n", 0)
			case "partial-line":
				s.Reader = bytes.NewReader(raw[:len(raw)-1])
				s.Bytes--
			case "cancelled":
				cancel()
			case "bad-range":
				selection = &Range{0, 2}
			}
			meta, err := Export(ctx, io.Discard, []Source{s}, selection, nil)
			if err == nil || meta.Version != 0 {
				t.Fatal("invalid export apparently succeeded")
			}
		})
	}
}
func TestThemeChangeRefused(t *testing.T) {
	a, _ := fixture("[1,\"o\",\"a\"]\n", 1)
	b, raw := fixture("[1,\"o\",\"b\"]\n", 1)
	raw = bytes.Replace(raw, []byte(`"rows":24`), []byte(`"rows":24,"theme":{"fg":"#ffffff"}`), 1)
	b.Reader = bytes.NewReader(raw)
	b.Bytes = int64(len(raw))
	b.Segment = 2
	if _, err := Export(context.Background(), io.Discard, []Source{a, b}, nil, nil); !errors.Is(err, ErrThemeChange) {
		t.Fatal("incompatible palettes silently combined")
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }
func TestExportShortWrite(t *testing.T) {
	s, _ := fixture("[0,\"o\",\"x\"]\n", 0)
	if _, err := Export(context.Background(), shortWriter{}, []Source{s}, nil, nil); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
}
