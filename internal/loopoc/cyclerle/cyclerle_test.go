package cyclerle

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/loopoc"
)

var synthetic = loopoc.SyntheticCorpora()

func syntheticNamed(t *testing.T, name string) (loopoc.Corpus, loopoc.Script) {
	t.Helper()
	s, err := loopoc.Workload(name, loopoc.SyntheticSeed, loopoc.SyntheticSpan(name))
	if err != nil {
		t.Fatal(err)
	}
	return loopoc.Synthesize(s), s
}

// captured returns the captured corpora when AREEL_LOOPOC_OUT has them.
func captured(t *testing.T) []loopoc.Corpus {
	if loopoc.OutDir() == "" {
		return nil
	}
	cs, err := loopoc.LoadCorpora(filepath.Join(loopoc.OutDir(), "corpus"))
	if err != nil {
		t.Logf("no captured corpora: %v", err)
		return nil
	}
	return cs
}

// encodeAll encodes c under commits, applying rewrites; it also returns the
// records as they stood after each commit.
func encodeAll(t *testing.T, codec loopoc.Codec, views []loopoc.View, commits []int64) ([][]byte, [][][]byte) {
	t.Helper()
	enc := codec.NewEncoder()
	defer enc.(interface{ Close() error }).Close()
	var recs [][]byte
	var after [][][]byte
	next := 0
	for _, b := range commits {
		for ; next < int(b); next++ {
			if err := enc.Append(views[next]); err != nil {
				t.Fatal(err)
			}
		}
		out, rw, err := enc.Commit()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range out {
			recs = append(recs, slices.Clone(r))
		}
		for i, r := range rw {
			recs[i] = slices.Clone(r)
		}
		after = append(after, slices.Clone(recs))
	}
	return recs, after
}

func open(t *testing.T, codec loopoc.Codec, recs [][]byte, views []loopoc.View) *decoder {
	t.Helper()
	d, err := codec.Open(recs, loopoc.Screens(views))
	if err != nil {
		t.Fatal(err)
	}
	return d.(*decoder)
}

// inRun reports, per seq (index seq-1), whether the view is inside a run piece.
func (d *decoder) inRun() []bool {
	out := make([]bool, d.Len())
	for _, u := range d.units {
		for j := range u.n {
			out[u.start-1+int64(j)] = u.k > 0
		}
	}
	return out
}

// referenceRuns is the detector rule written non-incrementally: a run of
// period k starts after view j when views j-2k+1..j are two equal cycles
// (smallest such k), and continues while each view equals the view k back.
func referenceRuns(views []loopoc.View, maxP int) []bool {
	out := make([]bool, len(views))
	k := 0
	for j := range views {
		if k > 0 && keyOf(views[j]) == keyOf(views[j-k]) {
			out[j] = true
			continue
		}
		k = 0
		for p := 1; p <= maxP && k == 0; p++ {
			if j+1 < 2*p {
				continue
			}
			ok := true
			for i := 0; i < p && ok; i++ {
				ok = keyOf(views[j-i]) == keyOf(views[j-i-p])
			}
			if ok {
				k = p
			}
		}
	}
	return out
}

func TestOpenNil(t *testing.T) {
	for _, codec := range Codecs() {
		d, err := codec.Open(nil, nil)
		if err != nil || d.Len() != 0 {
			t.Fatalf("%s: Open(nil) = %v, %v", codec.Name(), d, err)
		}
	}
}

// TestSyntheticExact: every variant, both policies, every Prefixes truncation.
func TestSyntheticExact(t *testing.T) {
	for _, codec := range append(Codecs(), Codec{MaxPeriod: 4}) {
		for _, c := range synthetic {
			for _, policy := range loopoc.Policies {
				r := loopoc.MeasurePolicy(c, codec, policy)
				if !r.Exact {
					t.Errorf("%s %s %s: seq %d %s: %s %s", codec.Name(), c.Name, policy, r.FirstBadSeq, r.BadCheck, r.BadDetail, r.Err)
				}
			}
		}
	}
}

// TestRunsMatchReference: the online detector classifies every view exactly
// as the non-incremental reference rule, independent of commit policy.
func TestRunsMatchReference(t *testing.T) {
	for _, c := range append(slices.Clone(synthetic), captured(t)...) {
		for _, codec := range []Codec{{}, {MaxPeriod: 4}} {
			want := referenceRuns(c.Views, codec.maxPeriod())
			for _, policy := range loopoc.Policies {
				recs, _ := encodeAll(t, codec, c.Views, loopoc.PolicyCommits(c, policy))
				got := open(t, codec, recs, c.Views).inRun()
				if !slices.Equal(got, want) {
					i := 0
					for i < len(got)-1 && got[i] == want[i] {
						i++
					}
					t.Errorf("%s %s %s: seq %d in run %v, reference %v", codec.Name(), c.Name, policy, i+1, got[i], want[i])
				}
			}
		}
	}
}

// TestNoPrediction: after every commit the records so far decode to exactly
// the views appended so far; a tail appended without Commit emits nothing;
// encoding a prefix gives records byte-identical to the leading records of
// the full encoding (Extend: all but the prefix's last, open record).
func TestNoPrediction(t *testing.T) {
	for _, c := range append(slices.Clone(synthetic), captured(t)...) {
		for _, codec := range Codecs() {
			cc := codec.(Codec)
			full, after := encodeAll(t, codec, c.Views, c.Commits)
			for i, b := range c.Commits {
				d := open(t, codec, after[i], c.Views[:b])
				if d.Len() != b {
					t.Fatalf("%s %s: after commit at %d, Len()=%d", codec.Name(), c.Name, b, d.Len())
				}
				if cc.Extend && i%8 != 0 && i != len(c.Commits)-1 {
					continue // the extend variant's check below is O(n) per boundary
				}
				pre, _ := encodeAll(t, codec, c.Views[:b], c.Commits[:i+1])
				cmp := pre
				if cc.Extend {
					cmp = pre[:len(pre)-1]
				}
				for j := range cmp {
					if !bytes.Equal(cmp[j], full[j]) {
						t.Fatalf("%s %s: prefix @%d record %d differs from the full encoding", codec.Name(), c.Name, b, j)
					}
				}
				if !cc.Extend && len(pre) != len(after[i]) {
					t.Fatalf("%s %s: prefix @%d has %d records, full had %d", codec.Name(), c.Name, b, len(pre), len(after[i]))
				}
			}
			// A tail appended after the last commit is never emitted.
			enc := codec.NewEncoder()
			half := len(c.Views) / 2
			for _, v := range c.Views[:half] {
				_ = enc.Append(v)
			}
			recs, _, _ := enc.Commit()
			for _, v := range c.Views[half:] {
				_ = enc.Append(v)
			}
			if d := open(t, codec, recs, c.Views[:half]); d.Len() != int64(half) {
				t.Fatalf("%s %s: Len()=%d with an uncommitted tail, want %d", codec.Name(), c.Name, d.Len(), half)
			}
			enc.(interface{ Close() error }).Close()
		}
	}
}

// TestDivergenceBoundaries: on syn-S5 every state break ends the run exactly
// at the last matching view (the view before the break step is in a run, the
// break view is literal), the literals last until two cycles of the new state
// are observed, then a run resumes. The 100 → 80 ms period change keeps every
// view state, so the run must carry on across it (only residuals change).
func TestDivergenceBoundaries(t *testing.T) {
	c, s := syntheticNamed(t, loopoc.S5Diverge)
	byLine := map[int64]int64{}
	for _, v := range c.Views {
		byLine[v.Line] = v.Seq
	}
	for _, policy := range loopoc.Policies {
		recs, _ := encodeAll(t, Codec{}, c.Views, loopoc.PolicyCommits(c, policy))
		d := open(t, Codec{}, recs, c.Views)
		run := d.inRun()
		in := func(seq int64) bool { return run[seq-1] }
		period := func(seq int64) int { return d.units[d.unitOf(seq)].k }
		seen := 0
		for i, st := range s.Steps {
			kind, ok := strings.CutPrefix(st.Note, "break:")
			if !ok {
				continue
			}
			seen++
			b, ok := byLine[s.Line(i)]
			if !ok {
				t.Fatalf("break %s at step %d produced no view", kind, i)
			}
			if !in(b-1) || period(b-1) != 10 {
				t.Errorf("%s %s: view %d before the break is not in a period-10 run", policy, kind, b-1)
			}
			if kind == "period" {
				for s := b; s < b+30; s++ {
					if !in(s) {
						t.Errorf("%s period: timing-only change ended the run at %d", policy, s)
						break
					}
				}
				continue
			}
			if in(b) {
				t.Errorf("%s %s: break view %d is inside a run", policy, kind, b)
				continue
			}
			next := b
			for next <= d.Len() && !in(next) {
				next++
			}
			// Literals until two whole cycles of the resumed state are
			// observed: 2k, plus the views that fit no cycle (the one-off
			// frame; the reversed pair's second view).
			want := map[string][2]int64{"skip": {10, 20}, "reverse": {10, 22}, "oneoff": {10, 21}, "text": {10, 20}, "spinner": {4, 8}}[kind]
			if k := period(next); int64(k) != want[0] || next-b != want[1] {
				t.Errorf("%s %s: literal views %d..%d (%d), then period %d; want %d literals then period %d", policy, kind, b, next-1, next-b, k, want[1], want[0])
			}
			k := period(next)
			t.Logf("%s %-7s run ends at %d, literals %d..%d, run resumes (k=%d) at %d", policy, kind, b-1, b, next-1, k, next)
		}
		if seen != 6 {
			t.Fatalf("saw %d breaks, want 6", seen)
		}
	}
}

// TestResizeEndsRun: no run piece spans views of different screen geometry.
func TestResizeEndsRun(t *testing.T) {
	c, s := syntheticNamed(t, loopoc.S4Resize)
	var resizeLines []int64
	for i, st := range s.Steps {
		if st.Cols != 0 {
			resizeLines = append(resizeLines, s.Line(i))
		}
	}
	geometry := func(v loopoc.View) int {
		g := 0
		for _, l := range resizeLines {
			if v.Line >= l {
				g++
			}
		}
		return g
	}
	for _, policy := range loopoc.Policies {
		recs, _ := encodeAll(t, Codec{}, c.Views, loopoc.PolicyCommits(c, policy))
		d := open(t, Codec{}, recs, c.Views)
		perGeometry := map[int]int{}
		for _, u := range d.units {
			if u.k == 0 {
				continue
			}
			g := geometry(c.Views[u.start-1])
			// The pattern (the k views before the piece) is part of the run.
			for s := u.start - int64(u.k); s < u.start+int64(u.n); s++ {
				if geometry(c.Views[s-1]) != g {
					t.Fatalf("%s: piece at %d (k=%d, n=%d) spans geometries", policy, u.start, u.k, u.n)
				}
			}
			perGeometry[g] += u.n
		}
		if len(perGeometry) != 3 {
			t.Fatalf("%s: runs in geometries %v, want all 3", policy, perGeometry)
		}
	}
}

// TestClockCoverage: S2's clock gives every second new screens, so the
// cycle is broken or re-keyed; exactness holds and coverage is reported.
func TestClockCoverage(t *testing.T) {
	c, _ := syntheticNamed(t, loopoc.S2Clock)
	for _, policy := range loopoc.Policies {
		if r := loopoc.MeasurePolicy(c, Codec{}, policy); !r.Exact {
			t.Fatalf("%s not exact: %s %s", policy, r.BadCheck, r.BadDetail)
		}
		st := statsOf(t, Codec{}, c, policy)
		t.Logf("syn-S2-clock %s: run coverage %.1f%%, %d pieces, mean piece %.1f views", policy, st.Coverage, st.Pieces, st.MeanPiece)
	}
}

// TestCyclePhaseSeek: SeekTime at the start of every cycle of every run
// piece, plus each phase offset and half-way into it, against a linear scan.
func TestCyclePhaseSeek(t *testing.T) {
	cs := []loopoc.Corpus{}
	for _, c := range append(slices.Clone(synthetic), captured(t)...) {
		if strings.Contains(c.Name, "S3") || strings.Contains(c.Name, "S5") || c.Name == "W5" || strings.Contains(c.Name, "S1") {
			cs = append(cs, c)
		}
	}
	linear := func(views []loopoc.View, at int64) int64 {
		best := int64(0)
		for _, v := range views {
			if v.AtNs <= at {
				best = v.Seq
			}
		}
		return best
	}
	for _, c := range cs {
		recs, _ := encodeAll(t, Codec{}, c.Views, c.Commits)
		d := open(t, Codec{}, recs, c.Views)
		checked := 0
		for _, u := range d.units {
			for cyc := 0; u.k > 0 && cyc*u.k < u.n && checked < 20000; cyc += 3 {
				for p := 0; p < u.k && cyc*u.k+p < u.n; p++ {
					seq := u.start + int64(cyc*u.k+p)
					for _, at := range []int64{d.at[seq], d.at[seq] + (d.at[seq]-d.at[seq-1])/2} {
						got, err := d.SeekTime(1, at)
						if want := linear(c.Views, at); err != nil || got != want {
							t.Fatalf("%s: SeekTime(%d) cycle %d phase %d = %d,%v want %d", c.Name, at, cyc, p, got, err, want)
						}
						checked++
					}
				}
			}
		}
		if checked == 0 {
			t.Fatalf("%s: no run pieces to seek into", c.Name)
		}
		t.Logf("%s: %d cycle-phase seeks match the linear scan", c.Name, checked)
	}
}

// Stats is the run shape of one encoding.
type Stats struct {
	Codec     string  `json:"codec"`
	Corpus    string  `json:"corpus"`
	Policy    string  `json:"policy"`
	Views     int64   `json:"views"`
	RunViews  int64   `json:"run_views"`
	Coverage  float64 `json:"run_coverage_pct"`
	Pieces    int     `json:"pieces"`
	MeanPiece float64 `json:"mean_piece_views"`
}

func statsOf(t *testing.T, codec Codec, c loopoc.Corpus, policy string) Stats {
	recs, _ := encodeAll(t, codec, c.Views, loopoc.PolicyCommits(c, policy))
	d := open(t, codec, recs, c.Views)
	st := Stats{Codec: codec.Name(), Corpus: c.Name, Policy: policy, Views: d.Len()}
	for _, u := range d.units {
		if u.k > 0 {
			st.RunViews += int64(u.n)
			st.Pieces++
		}
	}
	if st.Views > 0 {
		st.Coverage = 100 * float64(st.RunViews) / float64(st.Views)
	}
	if st.Pieces > 0 {
		st.MeanPiece = float64(st.RunViews) / float64(st.Pieces)
	}
	return st
}

// TestCyclerleReport writes results/cyclerle.json (every variant, every
// corpus, both policies) and results/cyclerle-stats.json (run coverage and
// mean piece length).
func TestCyclerleReport(t *testing.T) {
	codecs := Codecs()
	loopoc.RunReportTo(t, "cyclerle.json", codecs...)
	var stats []Stats
	for _, codec := range codecs {
		for _, c := range captured(t) {
			for _, policy := range loopoc.Policies {
				st := statsOf(t, codec.(Codec), c, policy)
				stats = append(stats, st)
				t.Logf("%-18s %-11s %-5s coverage=%5.1f%% pieces=%-4d mean piece=%.1f", st.Codec, st.Corpus, st.Policy, st.Coverage, st.Pieces, st.MeanPiece)
			}
		}
	}
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(loopoc.OutDir(), "results", "cyclerle-stats.json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
