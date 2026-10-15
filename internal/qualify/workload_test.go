package qualify

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

var allWorkloads = []string{W1, W2, W3, W4, W5, W6, W7, W8, W9}

func mustGenerate(t *testing.T, spec Spec) (Stream, Expectation) {
	t.Helper()
	s, x, err := Generate(spec)
	if err != nil {
		t.Fatal(err)
	}
	return s, x
}

func render(t *testing.T, s Stream) []byte {
	t.Helper()
	var b bytes.Buffer
	if _, err := s.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// parsedEvent is one cast line read back through the product's own exact
// interval parser (visualtime.ParseSeconds).
type parsedEvent struct {
	at   int64
	kind string
	data string
}

func parseCast(t *testing.T, cast []byte) (cols, rows int, events []parsedEvent) {
	t.Helper()
	sc := bufio.NewScanner(bytes.NewReader(cast))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	if !sc.Scan() {
		t.Fatal("empty cast")
	}
	var header struct {
		Version int `json:"version"`
		Term    struct {
			Cols int `json:"cols"`
			Rows int `json:"rows"`
		} `json:"term"`
	}
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil || header.Version != 3 {
		t.Fatalf("header %q: %v", sc.Bytes(), err)
	}
	var at visualtime.Time
	for sc.Scan() {
		var raw []json.RawMessage
		if err := json.Unmarshal(sc.Bytes(), &raw); err != nil || len(raw) != 3 {
			t.Fatalf("event %q: %v", sc.Bytes(), err)
		}
		interval, err := visualtime.ParseSeconds(string(raw[0]))
		if err != nil {
			t.Fatalf("interval %s: %v", raw[0], err)
		}
		at += interval
		var kind, data string
		if json.Unmarshal(raw[1], &kind) != nil || json.Unmarshal(raw[2], &data) != nil {
			t.Fatalf("event %q", sc.Bytes())
		}
		events = append(events, parsedEvent{int64(at), kind, data})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return header.Term.Cols, header.Term.Rows, events
}

// goldenStreams pins the sha256 of every workload at a 60 s logical span
// (seed 1) and of the mixed day profile's first logical hour. A change here
// is a change of the workloads: every later measurement depends on them.
// W9's cast equals W4's by design: its extra load is activity, not output.
var goldenStreams = map[string]string{
	"W1":           "b2f98fa54c71e271e5030662f4d28a93194852b46da0c83e2a69902fea07aaab",
	"W2":           "38db58de9fe61247ccb60dd73ed036b95d672c0d17ed07de70968e074f0d61f8",
	"W3":           "c0da9ba7f2a67922ee6fbdc4e6194e31639edab7763ff6a979f45d31fe341929",
	"W4":           "9010bf2a2ac61d9c4aaadcd5aa43c39f22af175e7f975c4a38d2865443521fa7",
	"W5":           "514e68c9b84a11d53c7bb88a638ac754ee8ae0d1e9f9cd9c6bbf8f3f733f52af",
	"W6":           "3f96ce3c8cd59c82aab235908e7840038980afdb8dfac411978d0472192ab6ed",
	"W7":           "4156dd2b9194bad852076d66d8d37a3c052a42798b8636bfd675b74303be3d4c",
	"W8":           "5400dcc80540ff98a46d50f87b38e12d061743be7da501056f140bb089c069b6",
	"W9":           "9010bf2a2ac61d9c4aaadcd5aa43c39f22af175e7f975c4a38d2865443521fa7",
	"mixed-day-1h": "c9f6e8510800347b7940f828453d0c2848c8bc4bdd969004cd7324849727bd5b",
}

func goldenSpec(name string) Spec {
	if name == "mixed-day-1h" {
		return Spec{Workload: Mixed, SpanNs: Hour, Seed: 1, Start: StartDay}
	}
	return Spec{Workload: name, SpanNs: 60 * Second, Seed: 1}
}

func TestWorkloadDeterminismGolden(t *testing.T) {
	for _, name := range sortedKeys(goldenStreams) {
		t.Run(name, func(t *testing.T) {
			s, _ := mustGenerate(t, goldenSpec(name))
			a := render(t, s)
			s2, _ := mustGenerate(t, goldenSpec(name))
			if b := render(t, s2); !bytes.Equal(a, b) {
				t.Fatal("same spec produced different bytes")
			}
			sum := sha256.Sum256(a)
			got := hex.EncodeToString(sum[:])
			if want := goldenStreams[name]; got != want {
				t.Errorf("%s sha256 %s, want %s", name, got, want)
			}
			// A different seed changes seeded workloads only.
			spec := goldenSpec(name)
			spec.Seed = 2
			s3, _ := mustGenerate(t, spec)
			seeded := name == W4 || name == W7 || name == W9 || name == "mixed-day-1h"
			if c := render(t, s3); bytes.Equal(a, c) == seeded {
				t.Errorf("%s: seed sensitivity is %v, want %v", name, !bytes.Equal(a, c), seeded)
			}
		})
	}
}

// Every interval is read back exactly and the intervals sum to the span in
// nanoseconds; the stream ends with x "0" at the span.
func TestWorkloadDurationExact(t *testing.T) {
	specs := []Spec{}
	for _, w := range allWorkloads {
		for _, span := range []int64{60 * Second, 60*Second + 7*Millisecond + 3} {
			specs = append(specs, Spec{Workload: w, SpanNs: span, Seed: 9})
		}
	}
	specs = append(specs,
		Spec{Workload: W3, SpanNs: 30 * Second, Seed: 1, Cols: 200, Rows: 50},
		Spec{Workload: Mixed, SpanNs: Hour, Seed: 3, Target: 5, Start: StartDay},
		Spec{Workload: Mixed, SpanNs: Hour + 1, Seed: 3, Target: 5, Start: StartNight},
		Spec{Workload: MixB, SpanNs: 9 * Minute, Seed: 3})
	for _, spec := range specs {
		s, _ := mustGenerate(t, spec)
		cols, rows, events := parseCast(t, render(t, s))
		wantCols, wantRows := spec.geometry()
		if cols != wantCols || rows != wantRows {
			t.Fatalf("%+v: header %dx%d", spec, cols, rows)
		}
		last := events[len(events)-1]
		if last.kind != "x" || last.data != "0" || last.at != spec.SpanNs {
			t.Fatalf("%+v: last event %+v", spec, last)
		}
		for i, e := range events[:len(events)-1] {
			if e.at >= spec.SpanNs || e.kind == "x" {
				t.Fatalf("%+v: event %d %+v at/after the span", spec, i, e)
			}
			if !utf8.ValidString(e.data) {
				t.Fatalf("%+v: invalid UTF-8", spec)
			}
		}
	}
}

func TestFormatSecondsExact(t *testing.T) {
	cases := map[int64]string{0: "0", 1: "0.000000001", 50 * Millisecond: "0.05", Second: "1", 150 * Millisecond: "0.15",
		3*Day + 1: "259200.000000001", 123456789012: "123.456789012"}
	for ns, want := range cases {
		if got := FormatSeconds(ns); got != want {
			t.Errorf("FormatSeconds(%d) = %q, want %q", ns, got, want)
		}
	}
	for ns := int64(0); ns < 3*Second; ns += 999_983 {
		back, err := visualtime.ParseSeconds(FormatSeconds(ns))
		if err != nil || int64(back) != ns {
			t.Fatalf("%d -> %q -> %d %v", ns, FormatSeconds(ns), back, err)
		}
	}
	var b bytes.Buffer
	w, _ := NewCastWriter(&b, 80, 24)
	if err := w.Write(Event{AtNs: 5, Kind: "o", Data: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Event{AtNs: 4, Kind: "o", Data: "a"}); err == nil {
		t.Fatal("a backwards event was accepted")
	}
}

// The reviewer's grep, made executable: no float type or float formatting
// takes part in workload timing.
func TestNoFloatInTiming(t *testing.T) {
	for _, f := range []string{"cast.go", "workload.go", "expect.go"} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := regexp.MustCompile(`(?i)float|%[0-9.]*[fge]\b|math\.`).FindIndex(data); loc != nil {
			t.Errorf("%s contains %q", f, data[loc[0]:loc[1]])
		}
	}
}

// countStream tallies a generated stream independently of expect().
type streamCounts struct {
	out, resize, meta int64
}

func countStream(t *testing.T, s Stream) (streamCounts, []Event) {
	t.Helper()
	var c streamCounts
	var events []Event
	if err := s.Events(func(e Event) error {
		switch e.Kind {
		case "o":
			c.out++
		case "r":
			c.resize++
			events = append(events, e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Activity(func(ActivityEvent) error { c.meta++; return nil }); err != nil {
		t.Fatal(err)
	}
	return c, events
}

func TestExpectationCountsMatchStream(t *testing.T) {
	specs := []Spec{{Workload: Mixed, SpanNs: Hour, Seed: 1, Start: StartDay},
		{Workload: Mixed, SpanNs: 8 * Hour, Seed: 4, Target: 31, Start: StartDay},
		{Workload: Mixed, SpanNs: 9 * Hour, Seed: 4, Target: 2, Start: StartNight},
		{Workload: MixB, SpanNs: 17*Minute + 3*Second, Seed: 1},
		{Workload: W3, SpanNs: 30 * Second, Seed: 1, Cols: 200, Rows: 50}}
	for _, w := range allWorkloads {
		specs = append(specs, Spec{Workload: w, SpanNs: 60 * Second, Seed: 1}, Spec{Workload: w, SpanNs: 7*Minute + 17*Millisecond, Seed: 5})
	}
	for _, spec := range specs {
		s, x := mustGenerate(t, spec)
		c, events := countStream(t, s)
		if c.out != x.OutputEvts || c.resize != x.ResizeEvts || c.meta != x.MetadataEvents {
			t.Errorf("%s/%s %d ns: stream out/resize/meta %d/%d/%d, expectation %d/%d/%d", spec.Workload, spec.Start, spec.SpanNs,
				c.out, c.resize, c.meta, x.OutputEvts, x.ResizeEvts, x.MetadataEvents)
		}
		// Generic upper bounds hold by construction; check they are consistent.
		if x.Views != 1+x.Changes || x.Changes > x.OutputEvts+x.ResizeEvts || x.ScreensMax > 1+x.OutputEvts+x.ResizeEvts {
			t.Errorf("%s: inconsistent bounds %+v", spec.Workload, x)
		}
		// Resize events stay within the capture's accepted geometry.
		for _, e := range events {
			var cols, rows int
			if _, err := fmt.Sscanf(e.Data, "%dx%d", &cols, &rows); err != nil || cols < 1 || rows < 1 || cols > 512 || rows > 512 || cols*rows > 32768 {
				t.Fatalf("resize %q", e.Data)
			}
		}
	}
	// Rates as the plan states them.
	for w, perHour := range map[string]int64{W1: 3600, W3: 72000, W4: 24000, W5: 36000, W6: 14400, W8: 3600} {
		_, x := mustGenerate(t, Spec{Workload: w, SpanNs: Hour, Seed: 1})
		if x.OutputEvts != perHour {
			t.Errorf("%s: %d output events per logical hour, want %d", w, x.OutputEvts, perHour)
		}
	}
	if _, x := mustGenerate(t, Spec{Workload: W9, SpanNs: Hour, Seed: 1}); x.MetadataEvents != 8*3600 {
		t.Errorf("W9: %d activity events per hour", x.MetadataEvents)
	}
	if _, x := mustGenerate(t, Spec{Workload: W7, SpanNs: Hour, Seed: 1}); x.ResizeEvts != 119 {
		// Cycles at 30 s, 60 s, … 3570 s: 119 in the first hour (none at 0).
		t.Errorf("W7: %d resize events per hour", x.ResizeEvts)
	}
}

// W1-W5 analytic claims, proven with the independent model.
func TestExpectationProvenByModel(t *testing.T) {
	for _, w := range []string{W1, W2, W3, W4, W5} { // W9's cast is W4's (golden)
		t.Run(w, func(t *testing.T) {
			spec := Spec{Workload: w, SpanNs: 60 * Second, Seed: 7}
			switch w {
			case W4:
				spec.SpanNs = 4 * Minute // 26 prompt lines: the screen scrolls
			case W3:
				spec.SpanNs = 20 * Second // 400 pages of numbered lines
			}
			s, x := mustGenerate(t, spec)
			_, _, events := parseCast(t, render(t, s))
			m := newVTModel(s.Cols, s.Rows)
			rows := map[string]bool{m.rowKey(0): true}
			screens := map[string]bool{m.screenKey(): true}
			states := map[string]bool{}
			prev := m.viewKey()
			var changes int64
			lastNew := int64(-1)
			for _, e := range events {
				if e.kind != "o" {
					continue
				}
				if err := m.feed(e.data); err != nil {
					t.Fatal(err)
				}
				view := m.viewKey()
				if view != prev {
					changes++
					states[view] = true
				}
				prev = view
				for i := range m.cells {
					if k := m.rowKey(i); !rows[k] {
						rows[k] = true
						lastNew = e.at
					}
				}
				if k := m.screenKey(); !screens[k] {
					screens[k] = true
					lastNew = e.at
				}
			}
			if !x.ChangesExact || changes != x.Changes || x.Views != 1+changes {
				t.Errorf("changes %d, expectation %d (exact %v), views %d", changes, x.Changes, x.ChangesExact, x.Views)
			}
			if int64(len(rows)) > x.RowsMax || int64(len(screens)) > x.ScreensMax {
				t.Errorf("rows %d > %d or screens %d > %d", len(rows), x.RowsMax, len(screens), x.ScreensMax)
			}
			if x.DistinctStatesExact && int64(len(states)) != x.DistinctStates {
				t.Errorf("distinct states %d, expectation %d", len(states), x.DistinctStates)
			}
			if x.WarmupNs >= 0 && lastNew > x.WarmupNs {
				t.Errorf("a new row/screen appeared at %d ns, after the warm-up %d ns", lastNew, x.WarmupNs)
			}
			switch w {
			case W1, W2:
				if len(screens) != 2 || len(rows) != 25 || x.DistinctStates != 1 || changes != 1 {
					t.Errorf("W1/W2: screens %d rows %d states %d changes %d", len(screens), len(rows), x.DistinctStates, changes)
				}
			case W5:
				if len(states) != 10 || changes != 600 || len(screens) != 11 || int64(len(rows)) != x.RowsMax {
					t.Errorf("W5: states %d changes %d screens %d rows %d", len(states), changes, len(screens), len(rows))
				}
			}
		})
	}
}

func TestW2EmitsNothingBetweenFirstScreenAndExit(t *testing.T) {
	for _, span := range []int64{Minute, 3 * Day} {
		s, x := mustGenerate(t, Spec{Workload: W2, SpanNs: span})
		_, _, events := parseCast(t, render(t, s))
		if len(events) != 2 || events[0].at != 0 || events[0].kind != "o" || events[1].kind != "x" || events[1].at != span {
			t.Fatalf("W2 events %d: %+v", len(events), events)
		}
		if x.OutputEvts != 1 || x.Views != 2 || x.WarmupNs != 0 {
			t.Fatalf("W2 expectation %+v", x)
		}
	}
}

// fidelityCounts feeds every output event through the product scanner.
func fidelityCounts(t *testing.T, s Stream) map[string]int64 {
	t.Helper()
	sc := fidelity.New()
	got := map[string]int64{}
	if err := s.Events(func(e Event) error {
		if e.Kind == "o" {
			for _, f := range sc.Feed(e.Data) {
				got[string(f.Class)+"/"+string(f.Kind)]++
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestFidelityExpectationMatchesScanner(t *testing.T) {
	specs := []Spec{{Workload: W8, SpanNs: 60 * Second}, {Workload: W8, SpanNs: 2*Hour + 5*Second},
		{Workload: Mixed, SpanNs: 8 * Hour, Seed: 34, Start: StartDay}, {Workload: Mixed, SpanNs: 12 * Hour, Seed: 34, Target: 3, Start: StartNight},
		{Workload: MixB, SpanNs: 20 * Minute}}
	for _, w := range allWorkloads {
		specs = append(specs, Spec{Workload: w, SpanNs: 5 * Minute, Seed: 1})
	}
	for _, spec := range specs {
		s, x := mustGenerate(t, spec)
		if spec.Workload == Mixed && x.Blocks[BlockW8] == 0 {
			t.Fatalf("%+v has no W8 block: pick a seed that exercises fidelity across blocks", spec)
		}
		got := fidelityCounts(t, s)
		var total int64
		for _, v := range got {
			total += v
		}
		if fmt.Sprint(got) != fmt.Sprint(x.Fidelity) || total != x.FidelityTotal {
			t.Errorf("%s/%s %d ns: scanner %v, expectation %v (total %d)", spec.Workload, spec.Start, spec.SpanNs, got, x.Fidelity, x.FidelityTotal)
		}
		if spec.Workload == W8 && spec.SpanNs == 2*Hour+5*Second {
			// 7,205 ticks: intervals open once, unknown-sequence once per 1,024.
			if x.Fidelity["palette/open"] != 1 || x.Fidelity["unknown-sequence/point"] != 8 {
				t.Errorf("W8 sparse counts %v", x.Fidelity)
			}
		}
	}
}

func TestMixedProfileShape(t *testing.T) {
	// The 3-day profile is night first: 6 h W2, 2 h W1, then 16 h of 30-min
	// day blocks with a per-target phase.
	for target := 0; target < 3; target++ {
		blocks, err := Schedule(Spec{Workload: Mixed, SpanNs: 3 * Day, Seed: 11, Target: target, Start: StartNight})
		if err != nil {
			t.Fatal(err)
		}
		var at int64
		for _, b := range blocks {
			if b.Start != at || b.End <= b.Start {
				t.Fatalf("gap or overlap at %+v", b)
			}
			at = b.End
			inDay := b.Start % Day
			switch {
			case inDay < 6*Hour:
				if b.Kind != BlockW2 || b.End-b.Start != 6*Hour {
					t.Fatalf("night block %+v", b)
				}
			case inDay < 8*Hour:
				if b.Kind != BlockW1 || b.End-b.Start != 2*Hour {
					t.Fatalf("night block %+v", b)
				}
			default:
				if b.End-b.Start > DayBlock || b.Kind == BlockW2 {
					t.Fatalf("day block %+v", b)
				}
			}
		}
		if at != 3*Day {
			t.Fatalf("blocks end at %d", at)
		}
	}
	// Different targets get different phases/draws; the same target is stable.
	a, _ := Schedule(Spec{Workload: Mixed, SpanNs: 8 * Hour, Seed: 11, Target: 0, Start: StartDay})
	b, _ := Schedule(Spec{Workload: Mixed, SpanNs: 8 * Hour, Seed: 11, Target: 1, Start: StartDay})
	c, _ := Schedule(Spec{Workload: Mixed, SpanNs: 8 * Hour, Seed: 11, Target: 0, Start: StartDay})
	if fmt.Sprint(a) == fmt.Sprint(b) || fmt.Sprint(a) != fmt.Sprint(c) {
		t.Fatal("per-target schedules are not target-specific and stable")
	}
	// Over many draws the day shares approach the plan's weights.
	counts := map[string]int{}
	total := 0
	for target := 0; target < 64; target++ {
		blocks, _ := Schedule(Spec{Workload: Mixed, SpanNs: 3 * Day, Seed: 5, Target: target, Start: StartNight})
		for _, b := range blocks {
			if b.Start%Day >= 8*Hour {
				counts[b.Kind]++
				total++
			}
		}
	}
	for _, w := range dayWeights {
		share := counts[w.kind] * 100 / total
		if d := share - int(w.weight); d < -3 || d > 3 {
			t.Errorf("block %s share %d%%, weight %d%%", w.kind, share, w.weight)
		}
	}
}

func TestW6LinesNeverWrap(t *testing.T) {
	for i := 0; i < W6Lines; i++ {
		line := W6Line(i)
		// Strip SGR sequences and count runes; even if every rune were wide,
		// the line fits 80 columns.
		plain := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(line, "")
		plain = strings.TrimSuffix(plain, "\r\n")
		if n := utf8.RuneCountInString(plain); 2*n >= 80 {
			t.Fatalf("W6 line %d is %d runes", i, n)
		}
		for j := 0; j < i; j++ {
			if W6Line(j) == line {
				t.Fatalf("W6 lines %d and %d are equal", i, j)
			}
		}
	}
}

func TestActivityEnvelopeShape(t *testing.T) {
	env := ActivityEnvelope("q-1")
	var v map[string]any
	if err := json.Unmarshal(env, &v); err != nil {
		t.Fatal(err)
	}
	if v["id"] != "q-1" || v["source"] != "future.vendor:extension" || v["kind"] != "unfamiliar:checkpoint" || len(v) != 13 {
		t.Fatalf("envelope %v", v)
	}
	if n := len(env); n < 1900 || n > 2200 {
		t.Fatalf("envelope %d bytes, want about 2 KiB", n)
	}
	s, _ := mustGenerate(t, Spec{Workload: W9, SpanNs: Second, Seed: 3})
	var ids []string
	_ = s.Activity(func(e ActivityEvent) error { ids = append(ids, e.ID); return nil })
	if len(ids) != 8 || ids[0] == ids[1] {
		t.Fatalf("W9 activity ids %v", ids)
	}
}

func TestSentinelRoundTrip(t *testing.T) {
	text := "noise" + Sentinel(12, 1_790_000_000_123_456_789) + "more" + Sentinel(13, 1_790_000_001_000_000_000)
	got := ParseSentinels(text)
	if len(got) != 2 || got[0] != (SentinelStamp{12, 1_790_000_000_123_456_789}) || got[1].SourceSecond != 13 {
		t.Fatalf("sentinels %+v", got)
	}
	if f := fidelity.New().Feed(Sentinel(1, 1)); len(f) != 0 {
		t.Fatalf("the sentinel raises fidelity findings %v", f)
	}
}

func TestStreamWriteFile(t *testing.T) {
	s, _ := mustGenerate(t, Spec{Workload: W5, SpanNs: 3 * Second})
	path := filepath.Join(t.TempDir(), "w5.cast")
	n, err := s.WriteFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if int64(len(data)) != n || !bytes.Equal(data, render(t, s)) {
		t.Fatal("file differs from the stream")
	}
	if _, err := s.WriteFile(path); err == nil {
		t.Fatal("WriteFile overwrote an existing file")
	}
}

func TestGenerateRefusesBadSpecs(t *testing.T) {
	for _, spec := range []Spec{{Workload: "W0", SpanNs: Second}, {Workload: W1}, {Workload: W1, SpanNs: Second, Cols: 600, Rows: 80},
		{Workload: Mixed, SpanNs: Second}, {Workload: W1, SpanNs: Second, Start: StartDay}, {Workload: W7, SpanNs: Second, Cols: 100, Rows: 30}} {
		if _, _, err := Generate(spec); err == nil {
			t.Errorf("%+v accepted", spec)
		}
	}
}
