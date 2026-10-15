package loopoc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

// Commit policies. prod replays the corpus's real commit boundaries; ideal is
// one commit at the end, the upper bound if sealing did not exist.
const (
	PolicyProd  = "prod"
	PolicyIdeal = "ideal"
)

// Policies is every policy Measure is run under by the reports.
var Policies = []string{PolicyProd, PolicyIdeal}

// PolicyCommits returns the commit boundaries of c under a policy.
func PolicyCommits(c Corpus, policy string) []int64 {
	switch policy {
	case PolicyProd:
		return slices.Clone(c.Commits)
	case PolicyIdeal:
		if len(c.Views) == 0 {
			return nil
		}
		return []int64{int64(len(c.Views))}
	}
	panic("loopoc: unknown policy " + policy)
}

// Result is one codec measured on one corpus under one commit policy.
type Result struct {
	Codec   string `json:"codec"`
	Corpus  string `json:"corpus"`
	Kind    string `json:"kind"`
	Policy  string `json:"policy"`
	Views   int64  `json:"views"`
	Commits int    `json:"commits"`
	SpanNs  int64  `json:"span_ns"`

	// Bytes. StoredBytes is Σ(len(record) + RecordOverhead) over the final
	// records, or the SizedEncoder figure; ByteMethod says which.
	Records        int     `json:"records"`
	RecordBytes    int64   `json:"record_bytes"`
	StoredBytes    int64   `json:"stored_bytes"`
	ByteMethod     string  `json:"byte_method"`
	RewrittenBytes int64   `json:"rewritten_bytes"`
	Rewrites       int     `json:"rewrites"`
	BytesPerView   float64 `json:"bytes_per_view"`
	BytesPerHour   float64 `json:"bytes_per_logical_hour"`

	// Costs. EncodeNsPerView is wall time inside Append and Commit only.
	// PeakHeapBytes is the largest runtime.MemStats.HeapAlloc seen at a
	// commit point during encoding, minus HeapAlloc after a GC just before
	// encoding (includes the records the harness holds; excludes memory a
	// codec gets outside the Go heap, e.g. SQLite's allocator).
	EncodeNsPerView float64 `json:"encode_ns_per_view"`
	PeakHeapBytes   int64   `json:"peak_heap_bytes"`
	SeekP50Ns       int64   `json:"seek_p50_ns"`
	SeekP99Ns       int64   `json:"seek_p99_ns"`
	PhaseSeeks      int     `json:"phase_seeks"`
	PhaseSeekP50Ns  int64   `json:"phase_seek_p50_ns"`
	PhaseSeekP99Ns  int64   `json:"phase_seek_p99_ns"`
	ExpandNsPerView float64 `json:"expand_ns_per_view"`

	// Exactness. FirstBadSeq is the first differing seq (0 when not
	// seq-specific); BadCheck names the check that failed.
	Exact           bool   `json:"exact"`
	FirstBadSeq     int64  `json:"first_bad_seq,omitempty"`
	BadCheck        string `json:"bad_check,omitempty"`
	BadDetail       string `json:"bad_detail,omitempty"`
	PrefixesChecked int    `json:"prefixes_checked"`
	Err             string `json:"error,omitempty"`
}

// Seeks per Measure run.
const (
	randomSeeks = 1000
	randomAts   = 1000
	phaseSeeks  = 1000
)

// Measure encodes c with codec, calling Commit at every boundary in commits
// (which must end at len(c.Views)), then decodes and checks exactness:
// Range(1, Len) field by field, At on 1,000 seeded seqs, SeekTime on 1,000
// seeded random times and at cycle phases against ReferenceSeek, and every
// Prefixes truncation encoded alone (policy-consistent commits, then the lost
// tail appended without a Commit) and decoded exactly.
func Measure(c Corpus, codec Codec, commits []int64) Result {
	r := Result{Codec: codec.Name(), Corpus: c.Name, Kind: c.Kind, Policy: policyName(c, commits),
		Views: int64(len(c.Views)), Commits: len(commits), SpanNs: c.SpanNs, Exact: true}
	bad := func(seq int64, check, detail string) {
		if r.Exact {
			r.Exact, r.FirstBadSeq, r.BadCheck, r.BadDetail = false, seq, check, detail
		}
	}
	fail := func(check string, err error) Result {
		r.Err = fmt.Sprintf("%s: %v", check, err)
		bad(0, check, err.Error())
		return r
	}

	run, err := encode(codec, c.Views, commits, nil, true)
	if err != nil {
		run.close(nil)
		return fail("encode", err)
	}
	r.Records, r.RecordBytes, r.StoredBytes, r.ByteMethod = len(run.records), run.recordBytes, run.stored, run.method
	r.RewrittenBytes, r.Rewrites = run.rewrittenBytes, run.rewrites
	if n := len(c.Views); n > 0 {
		r.BytesPerView = float64(r.StoredBytes) / float64(n)
		r.EncodeNsPerView = float64(run.encodeNs) / float64(n)
	}
	if c.SpanNs > 0 {
		r.BytesPerHour = float64(r.StoredBytes) / float64(c.SpanNs) * 3.6e12
	}
	r.PeakHeapBytes = run.peakHeap

	dec, err := codec.Open(run.records, Screens(c.Views))
	if err != nil {
		run.close(nil)
		return fail("open", err)
	}
	checkDecoder(c, dec, &r, bad, true)
	run.close(dec)

	for _, p := range Prefixes(c) {
		if !r.Exact {
			break
		}
		checkPrefix(codec, p, commits, bad)
		r.PrefixesChecked++
	}
	return r
}

// MeasurePolicy is Measure under a named policy.
func MeasurePolicy(c Corpus, codec Codec, policy string) Result {
	r := Measure(c, codec, PolicyCommits(c, policy))
	r.Policy = policy
	return r
}

func policyName(c Corpus, commits []int64) string {
	switch {
	case slices.Equal(commits, c.Commits):
		return PolicyProd
	case slices.Equal(commits, PolicyCommits(c, PolicyIdeal)):
		return PolicyIdeal
	}
	return "custom"
}

type encoded struct {
	enc                 Encoder
	records             [][]byte
	recordBytes, stored int64
	method              string
	rewrittenBytes      int64
	rewrites            int
	encodeNs, peakHeap  int64
}

func (e *encoded) close(dec Decoder) {
	if c, ok := dec.(io.Closer); ok {
		_ = c.Close()
	}
	if c, ok := e.enc.(io.Closer); ok {
		_ = c.Close()
	}
}

// encode appends views, committing at each boundary, then appends tail
// without a commit. Records are copied on receipt.
func encode(codec Codec, views []View, commits []int64, tail []View, measure bool) (*encoded, error) {
	e := &encoded{enc: codec.NewEncoder()}
	var ms runtime.MemStats
	var base uint64
	if measure {
		runtime.GC()
		runtime.ReadMemStats(&ms)
		base = ms.HeapAlloc
	}
	next := 0
	for _, b := range commits {
		if b < int64(next) || b > int64(len(views)) {
			return e, fmt.Errorf("commit boundary %d outside %d..%d", b, next, len(views))
		}
		start := time.Now()
		for ; next < int(b); next++ {
			if err := e.enc.Append(views[next]); err != nil {
				return e, fmt.Errorf("append seq %d: %w", views[next].Seq, err)
			}
		}
		recs, rewritten, err := e.enc.Commit()
		e.encodeNs += time.Since(start).Nanoseconds()
		if err != nil {
			return e, fmt.Errorf("commit at %d: %w", b, err)
		}
		for _, rec := range recs {
			e.records = append(e.records, slices.Clone(rec))
		}
		for i, rec := range rewritten {
			if i < 0 || i >= len(e.records) {
				return e, fmt.Errorf("commit at %d rewrites record %d of %d", b, i, len(e.records))
			}
			e.records[i] = slices.Clone(rec)
			e.rewrittenBytes += int64(len(rec))
			e.rewrites++
		}
		if measure {
			runtime.ReadMemStats(&ms)
			if d := int64(ms.HeapAlloc) - int64(base); d > e.peakHeap {
				e.peakHeap = d
			}
		}
	}
	if next != len(views) {
		return e, fmt.Errorf("commits end at %d of %d views", next, len(views))
	}
	for _, rec := range e.records {
		e.recordBytes += int64(len(rec))
	}
	if s, ok := e.enc.(SizedEncoder); ok {
		n, err := s.StoredBytes()
		if err != nil {
			return e, fmt.Errorf("stored bytes: %w", err)
		}
		e.stored, e.method = n, s.ByteMethod()
	} else {
		e.stored = e.recordBytes + RecordOverhead*int64(len(e.records))
		e.method = fmt.Sprintf("sum of record lengths + %d B per record", RecordOverhead)
	}
	// The lost tail goes in last, after the byte count, and is never committed.
	for _, v := range tail {
		if err := e.enc.Append(v); err != nil {
			return e, fmt.Errorf("append tail seq %d: %w", v.Seq, err)
		}
	}
	return e, nil
}

// checkDecoder runs every exactness check on a full-corpus decoder and, when
// timed, fills the cost fields of r.
func checkDecoder(c Corpus, dec Decoder, r *Result, bad func(int64, string, string), timed bool) {
	n := int64(len(c.Views))
	if got := dec.Len(); got != n {
		bad(min(got, n)+1, "len", fmt.Sprintf("Len()=%d, want %d", got, n))
		return
	}
	start := time.Now()
	got, err := dec.Range(1, int(n))
	if timed && n > 0 {
		r.ExpandNsPerView = float64(time.Since(start).Nanoseconds()) / float64(n)
	}
	if err != nil {
		bad(1, "range", err.Error())
		return
	}
	if seq, detail := firstDiff(got, c.Views); seq != 0 {
		bad(seq, "range", detail)
		return
	}
	if n == 0 {
		return
	}
	rng := rand.New(rand.NewPCG(c.Seed, 0xa7))
	for range randomAts {
		seq := 1 + rng.Int64N(n)
		v, err := dec.At(seq)
		if err != nil {
			bad(seq, "at", err.Error())
			return
		}
		if s, detail := firstDiff([]View{v}, c.Views[seq-1:seq]); s != 0 {
			bad(seq, "at", detail)
			return
		}
	}
	segment := c.Views[0].Segment
	seek := func(check string, at int64) (int64, bool) {
		want, werr := ReferenceSeek(c.Views, segment, at)
		t0 := time.Now()
		got, gerr := dec.SeekTime(segment, at)
		d := time.Since(t0).Nanoseconds()
		switch {
		case werr != nil && errors.Is(gerr, ErrNoView):
		case werr != nil:
			bad(got, check, fmt.Sprintf("SeekTime(%d)=%d,%v; want ErrNoView", at, got, gerr))
			return d, false
		case gerr != nil || got != want:
			bad(want, check, fmt.Sprintf("SeekTime(%d)=%d,%v; want %d", at, got, gerr, want))
			return d, false
		}
		return d, true
	}
	var times []int64
	for range randomSeeks {
		at := rng.Int64N(c.SpanNs + 1)
		d, ok := seek("seek", at)
		if !ok {
			return
		}
		times = append(times, d)
	}
	if timed {
		r.SeekP50Ns, r.SeekP99Ns = percentiles(times)
	}
	times = times[:0]
	for _, at := range PhaseTimes(c) {
		d, ok := seek("phase-seek", at)
		if !ok {
			return
		}
		times = append(times, d)
	}
	if timed {
		r.PhaseSeeks = len(times)
		r.PhaseSeekP50Ns, r.PhaseSeekP99Ns = percentiles(times)
	}
}

// checkPrefix encodes one Prefixes entry alone with the commits of the full
// policy that fall inside it plus its own boundary, appends its lost tail
// without committing, and checks the decoded result is exactly p.Views.
func checkPrefix(codec Codec, p Corpus, commits []int64, bad func(int64, string, string)) {
	b := int64(len(p.Views))
	var own []int64
	for _, x := range commits {
		if x < b {
			own = append(own, x)
		}
	}
	if b > 0 {
		own = append(own, b)
	}
	run, err := encode(codec, p.Views, own, p.Tail, false)
	if err != nil {
		run.close(nil)
		bad(0, "prefix "+p.Name, err.Error())
		return
	}
	dec, err := codec.Open(run.records, Screens(p.Views))
	if err != nil {
		run.close(nil)
		bad(0, "prefix "+p.Name, "open: "+err.Error())
		return
	}
	defer run.close(dec)
	if got := dec.Len(); got != b {
		bad(min(got, b)+1, "prefix "+p.Name, fmt.Sprintf("Len()=%d, want %d", got, b))
		return
	}
	got, err := dec.Range(1, int(b))
	if err != nil {
		bad(1, "prefix "+p.Name, err.Error())
		return
	}
	if seq, detail := firstDiff(got, p.Views); seq != 0 {
		bad(seq, "prefix "+p.Name, detail)
		return
	}
	if b > 0 {
		v, err := dec.At(b)
		if err != nil {
			bad(b, "prefix "+p.Name, "At(head): "+err.Error())
			return
		}
		if seq, detail := firstDiff([]View{v}, p.Views[b-1:]); seq != 0 {
			bad(seq, "prefix "+p.Name, "At(head): "+detail)
		}
	}
}

// firstDiff returns the first seq where got differs from want (field by
// field) and what differs, or 0.
func firstDiff(got, want []View) (int64, string) {
	for i := range min(len(got), len(want)) {
		g, w := got[i], want[i]
		if g == w {
			continue
		}
		field := "?"
		switch {
		case g.Seq != w.Seq:
			field = fmt.Sprintf("Seq %d != %d", g.Seq, w.Seq)
		case g.Segment != w.Segment:
			field = fmt.Sprintf("Segment %d != %d", g.Segment, w.Segment)
		case g.Line != w.Line:
			field = fmt.Sprintf("Line %d != %d", g.Line, w.Line)
		case g.AtNs != w.AtNs:
			field = fmt.Sprintf("AtNs %d != %d", g.AtNs, w.AtNs)
		case g.ScreenID != w.ScreenID:
			field = fmt.Sprintf("ScreenID %d != %d", g.ScreenID, w.ScreenID)
		case g.ScreenHash != w.ScreenHash:
			field = "ScreenHash"
		case g.CursorCol != w.CursorCol || g.CursorRow != w.CursorRow || g.CursorVisible != w.CursorVisible:
			field = fmt.Sprintf("Cursor %d,%d,%v != %d,%d,%v", g.CursorCol, g.CursorRow, g.CursorVisible, w.CursorCol, w.CursorRow, w.CursorVisible)
		case g.Chain != w.Chain:
			field = "Chain"
		}
		return w.Seq, field
	}
	if len(got) != len(want) {
		i := min(len(got), len(want))
		return int64(i + 1), fmt.Sprintf("%d views, want %d", len(got), len(want))
	}
	return 0, ""
}

func percentiles(ns []int64) (int64, int64) {
	if len(ns) == 0 {
		return 0, 0
	}
	s := slices.Clone(ns)
	slices.Sort(s)
	at := func(q float64) int64 { return s[min(len(s)-1, int(q*float64(len(s))))] }
	return at(0.50), at(0.99)
}

// PhaseTimes are the cycle-phase seek times of c: for the first screen (in
// seq order) that occurs at least three times, the at_ns of up to 1,000 of its
// occurrences (evenly strided), each also plus half the median Δt between
// consecutive views. Empty when no screen recurs.
func PhaseTimes(c Corpus) []int64 {
	count := map[int64]int{}
	for _, v := range c.Views {
		count[v.ScreenID]++
	}
	idx := slices.IndexFunc(c.Views, func(v View) bool { return count[v.ScreenID] >= 3 })
	if idx < 0 {
		return nil
	}
	first := c.Views[idx].ScreenID
	var occ []int64
	for _, v := range c.Views {
		if v.ScreenID == first {
			occ = append(occ, v.AtNs)
		}
	}
	var deltas []int64
	for i := 1; i < len(c.Views); i++ {
		deltas = append(deltas, c.Views[i].AtNs-c.Views[i-1].AtNs)
	}
	slices.Sort(deltas)
	half := deltas[len(deltas)/2] / 2
	stride := max(1, (len(occ)+phaseSeeks/2-1)/(phaseSeeks/2))
	var out []int64
	for i := 0; i < len(occ); i += stride {
		out = append(out, occ[i], occ[i]+half)
	}
	return out
}

// ---------------------------------------------------------------------------
// Reports.

// OutDir is $AREEL_LOOPOC_OUT, or "" when unset.
func OutDir() string { return os.Getenv("AREEL_LOOPOC_OUT") }

// RunReport measures each codec on every corpus in $AREEL_LOOPOC_OUT/corpus
// under both policies and writes $AREEL_LOOPOC_OUT/results/<codec name>.json
// (a JSON array of Result, one per corpus × policy). It skips when
// AREEL_LOOPOC_OUT is unset and fails the test for any inexact result.
func RunReport(t *testing.T, codecs ...Codec) {
	t.Helper()
	for _, codec := range codecs {
		RunReportTo(t, codec.Name()+".json", codec)
	}
}

// RunReportTo is RunReport writing all codecs' results into one file
// $AREEL_LOOPOC_OUT/results/<file>.
func RunReportTo(t *testing.T, file string, codecs ...Codec) {
	t.Helper()
	out := OutDir()
	if out == "" {
		t.Skip("AREEL_LOOPOC_OUT is unset")
	}
	corpora, err := LoadCorpora(filepath.Join(out, "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	if len(corpora) == 0 {
		t.Fatalf("no corpora in %s/corpus; run TestLoopocCorpus first", out)
	}
	var results []Result
	for _, codec := range codecs {
		for _, c := range corpora {
			for _, policy := range Policies {
				start := time.Now()
				r := MeasurePolicy(c, codec, policy)
				results = append(results, r)
				t.Logf("%-18s %-12s %-5s views=%-6d B/h=%-11.0f B/view=%-7.2f exact=%v prefixes=%d (%s)",
					r.Codec, r.Corpus, r.Policy, r.Views, r.BytesPerHour, r.BytesPerView, r.Exact, r.PrefixesChecked, time.Since(start).Round(time.Millisecond))
				if !r.Exact {
					t.Errorf("%s on %s/%s is not exact: seq %d %s: %s %s", r.Codec, r.Corpus, r.Policy, r.FirstBadSeq, r.BadCheck, r.BadDetail, r.Err)
				}
			}
		}
	}
	if err := WriteResults(filepath.Join(out, "results", file), results); err != nil {
		t.Fatal(err)
	}
}

// WriteResults writes results as indented JSON.
func WriteResults(path string, results []Result) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
