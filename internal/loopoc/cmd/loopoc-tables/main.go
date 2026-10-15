// Command loopoc-tables turns the V9 PoC result files (results/*.json written
// by TestLoopocReport, TestBlockzReport and TestCyclerleReport) into the
// Markdown tables of docs/visual-recording-loop-poc.md.
//
//	go run ./internal/loopoc/cmd/loopoc-tables "$AREEL_LOOPOC_OUT/results"
//
// It only reads. The output is a pure function of the JSON: codecs and corpora
// are printed in a fixed order (unknown names sorted after the known ones), and
// nothing depends on map order, the clock or the environment. Timing columns
// (encode, seek, expand) are whatever the last report run measured; they vary
// between runs, the byte and exactness columns do not.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// result mirrors the fields of loopoc.Result that the tables use.
type result struct {
	Codec           string  `json:"codec"`
	Corpus          string  `json:"corpus"`
	Policy          string  `json:"policy"`
	Views           int64   `json:"views"`
	Commits         int     `json:"commits"`
	SpanNs          int64   `json:"span_ns"`
	Records         int     `json:"records"`
	StoredBytes     int64   `json:"stored_bytes"`
	ByteMethod      string  `json:"byte_method"`
	RewrittenBytes  int64   `json:"rewritten_bytes"`
	Rewrites        int     `json:"rewrites"`
	BytesPerView    float64 `json:"bytes_per_view"`
	BytesPerHour    float64 `json:"bytes_per_logical_hour"`
	EncodeNsPerView float64 `json:"encode_ns_per_view"`
	PeakHeapBytes   int64   `json:"peak_heap_bytes"`
	SeekP50Ns       int64   `json:"seek_p50_ns"`
	SeekP99Ns       int64   `json:"seek_p99_ns"`
	PhaseSeeks      int     `json:"phase_seeks"`
	PhaseSeekP50Ns  int64   `json:"phase_seek_p50_ns"`
	ExpandNsPerView float64 `json:"expand_ns_per_view"`
	Exact           bool    `json:"exact"`
	FirstBadSeq     int64   `json:"first_bad_seq"`
	BadCheck        string  `json:"bad_check"`
	PrefixesChecked int     `json:"prefixes_checked"`
	Err             string  `json:"error"`
}

// stat mirrors one entry of cyclerle-stats.json.
type stat struct {
	Codec          string  `json:"codec"`
	Corpus         string  `json:"corpus"`
	Policy         string  `json:"policy"`
	Views          int64   `json:"views"`
	RunViews       int64   `json:"run_views"`
	RunCoveragePct float64 `json:"run_coverage_pct"`
	Pieces         int     `json:"pieces"`
	MeanPieceViews float64 `json:"mean_piece_views"`
}

const (
	baseline = "sqlite-v8"
	prod     = "prod"
	ideal    = "ideal"
)

var (
	codecOrder = []string{baseline, "raw", "a0-sqlite-nochain", "a-blockz", "a-blockz-dict",
		"b-cyclerle", "b-cyclerle-plain", "b-cyclerle-extend"}
	corpusOrder = []string{"W1", "W2", "W3", "W4", "W5", "W6", "W7",
		"S2-clock", "S3-jitter", "S4-resize", "S5-diverge", "S6-mixed"}
	headline = []string{"W5", "S3-jitter", "S2-clock"}
	// candidates are the codecs compared against the baseline in the
	// headline (raw is a sanity reference only).
	notCandidates = map[string]bool{baseline: true, "raw": true}
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: loopoc-tables RESULTS_DIR")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "loopoc-tables:", err)
		os.Exit(1)
	}
}

type data struct {
	results map[[3]string]result // codec, corpus, policy
	stats   map[[3]string]stat
	codecs  []string
	corpora []string
}

func load(dir string) (*data, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	d := &data{results: map[[3]string]result{}, stats: map[[3]string]stat{}}
	codecs, corpora := map[string]bool{}, map[string]bool{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(p, "-stats.json") {
			var ss []stat
			if err := json.Unmarshal(raw, &ss); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			for _, s := range ss {
				k := [3]string{s.Codec, s.Corpus, s.Policy}
				if _, dup := d.stats[k]; dup {
					return nil, fmt.Errorf("%s: duplicate stats %v", p, k)
				}
				d.stats[k] = s
			}
			continue
		}
		var rs []result
		if err := json.Unmarshal(raw, &rs); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		for _, r := range rs {
			k := [3]string{r.Codec, r.Corpus, r.Policy}
			if _, dup := d.results[k]; dup {
				return nil, fmt.Errorf("%s: duplicate result %v", p, k)
			}
			d.results[k] = r
			codecs[r.Codec], corpora[r.Corpus] = true, true
		}
	}
	if len(d.results) == 0 {
		return nil, errors.New("no results in " + dir)
	}
	d.codecs, d.corpora = ordered(codecs, codecOrder), ordered(corpora, corpusOrder)
	return d, nil
}

// ordered returns the names in set, the known ones in known's order first, the
// rest sorted.
func ordered(set map[string]bool, known []string) []string {
	var out, rest []string
	for _, k := range known {
		if set[k] {
			out = append(out, k)
		}
	}
	for k := range set {
		if !slices.Contains(known, k) {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func (d *data) get(codec, corpus, policy string) (result, bool) {
	r, ok := d.results[[3]string{codec, corpus, policy}]
	return r, ok
}

func (d *data) candidates() []string {
	var out []string
	for _, c := range d.codecs {
		if !notCandidates[c] {
			out = append(out, c)
		}
	}
	return out
}

func run(dir string, w io.Writer) error {
	d, err := load(dir)
	if err != nil {
		return err
	}
	p := &printer{w: w}
	p.headline(d, prod)
	p.headline(d, ideal)
	p.perView(d, prod)
	p.exactness(d)
	p.costs(d)
	p.runs(d)
	return p.err
}

type printer struct {
	w   io.Writer
	err error
}

func (p *printer) f(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, args...)
	}
}

func (p *printer) table(header []string, rows [][]string) {
	p.f("| %s |\n", strings.Join(header, " | "))
	seps := make([]string, len(header))
	for i := range seps {
		seps[i] = "---"
		if i > 0 {
			seps[i] = "---:"
		}
	}
	p.f("|%s|\n", strings.Join(seps, "|"))
	for _, r := range rows {
		p.f("| %s |\n", strings.Join(r, " | "))
	}
	p.f("\n")
}

// kbh formats bytes per logical hour as decimal kB/h.
func kbh(b float64) string { return fmt.Sprintf("%.1f", b/1e3) }

func factor(v, base float64) string {
	if base == 0 {
		return "—"
	}
	if f := v / base; f < 0.01 {
		return fmt.Sprintf("%.4f×", f)
	}
	return fmt.Sprintf("%.3f×", v/base)
}

func exactMark(r result) string {
	if r.Exact {
		return ""
	}
	return " ✗"
}

func label(corpus string) string {
	if corpus == "W5" {
		return "W5 (S1-pure)"
	}
	return corpus
}

// headline: kB per logical hour and the factor against sqlite-v8 for every
// candidate on the spinner corpora.
func (p *printer) headline(d *data, policy string) {
	p.f("#### Headline (%s): view bytes per logical hour, kB/h (× sqlite-v8 %s)\n\n", policy, prod)
	header := []string{"codec"}
	for _, c := range headline {
		header = append(header, label(c))
	}
	var rows [][]string
	for _, codec := range append([]string{baseline}, d.candidates()...) {
		row := []string{codec}
		for _, corpus := range headline {
			r, ok := d.get(codec, corpus, policy)
			b, bok := d.get(baseline, corpus, prod)
			switch {
			case !ok:
				row = append(row, "—")
			case !bok:
				row = append(row, kbh(r.BytesPerHour)+exactMark(r))
			default:
				row = append(row, fmt.Sprintf("%s (%s)%s", kbh(r.BytesPerHour), factor(r.BytesPerHour, b.BytesPerHour), exactMark(r)))
			}
		}
		rows = append(rows, row)
	}
	p.table(header, rows)
}

// perView: bytes per view for every corpus and codec under one policy.
func (p *printer) perView(d *data, policy string) {
	p.f("#### Bytes per view, all corpora (%s); W1–W7 are the regression rows, ✗ marks an inexact result\n\n", policy)
	header := append([]string{"corpus", "views", "commits"}, d.codecs...)
	var rows [][]string
	for _, corpus := range d.corpora {
		b, _ := d.get(baseline, corpus, policy)
		row := []string{label(corpus), fmt.Sprint(b.Views), fmt.Sprint(b.Commits)}
		for _, codec := range d.codecs {
			r, ok := d.get(codec, corpus, policy)
			if !ok {
				row = append(row, "—")
				continue
			}
			row = append(row, fmt.Sprintf("%.2f%s", r.BytesPerView, exactMark(r)))
		}
		rows = append(rows, row)
	}
	p.table(header, rows)
}

// exactness: per codec, how many corpus × policy results are exact, how many
// prefix truncations were decoded, and the first failure if any.
func (p *printer) exactness(d *data) {
	p.f("#### Exactness (all corpora × both policies, including every Prefixes truncation)\n\n")
	var rows [][]string
	for _, codec := range d.codecs {
		total, exact, prefixes := 0, 0, 0
		var first []string
		for _, corpus := range d.corpora {
			for _, policy := range []string{prod, ideal} {
				r, ok := d.get(codec, corpus, policy)
				if !ok {
					continue
				}
				total++
				prefixes += r.PrefixesChecked
				if r.Exact {
					exact++
				} else if len(first) == 0 {
					first = append(first, fmt.Sprintf("%s/%s: %s at seq %d %s", corpus, policy, r.BadCheck, r.FirstBadSeq, r.Err))
				}
			}
		}
		verdict := "exact"
		if exact != total {
			verdict = "NOT exact"
		}
		note := "—"
		if len(first) > 0 {
			note = first[0]
		}
		rows = append(rows, []string{codec, verdict, fmt.Sprintf("%d/%d", exact, total), fmt.Sprint(prefixes), note})
	}
	p.table([]string{"codec", "verdict", "exact results", "prefix decodes", "first failure"}, rows)
}

// costs: records, encode, seek and expansion on the spinner corpora under
// prod, for the baseline and the candidates.
func (p *printer) costs(d *data) {
	p.f("#### Costs (%s); timings are wall-clock from the last report run and vary between runs\n\n", prod)
	var rows [][]string
	for _, corpus := range []string{"W5", "S3-jitter"} {
		for _, codec := range append([]string{baseline}, d.candidates()...) {
			r, ok := d.get(codec, corpus, prod)
			if !ok {
				continue
			}
			phase := "—"
			if r.PhaseSeeks > 0 {
				phase = fmt.Sprintf("%.1f", float64(r.PhaseSeekP50Ns)/1e3)
			}
			rows = append(rows, []string{label(corpus), codec,
				fmt.Sprint(r.Records),
				fmt.Sprintf("%.1f", float64(r.Views)/float64(max(r.Records, 1))),
				fmt.Sprintf("%.1f", float64(r.StoredBytes)/float64(max(r.Records, 1))),
				fmt.Sprintf("%.0f", r.EncodeNsPerView),
				fmt.Sprintf("%.1f", float64(r.SeekP50Ns)/1e3),
				fmt.Sprintf("%.1f", float64(r.SeekP99Ns)/1e3),
				phase,
				fmt.Sprintf("%.0f", r.ExpandNsPerView),
				fmt.Sprint(r.RewrittenBytes),
			})
		}
	}
	p.table([]string{"corpus", "codec", "records", "views/record", "stored B/record", "encode ns/view",
		"seek p50 µs", "seek p99 µs", "phase seek p50 µs", "expand ns/view", "rewritten B"}, rows)
}

// runs: candidate B's run coverage and piece lengths per corpus.
func (p *printer) runs(d *data) {
	if len(d.stats) == 0 {
		return
	}
	p.f("#### Candidate B runs (b-cyclerle; extend = b-cyclerle-extend)\n\n")
	var rows [][]string
	for _, corpus := range d.corpora {
		s, ok := d.stats[[3]string{"b-cyclerle", corpus, prod}]
		if !ok {
			continue
		}
		si := d.stats[[3]string{"b-cyclerle", corpus, ideal}]
		ext, _ := d.get("b-cyclerle-extend", corpus, prod)
		rows = append(rows, []string{label(corpus), fmt.Sprint(s.Views),
			fmt.Sprintf("%.1f", s.RunCoveragePct),
			fmt.Sprint(s.Pieces), fmt.Sprintf("%.1f", s.MeanPieceViews),
			fmt.Sprintf("%.1f", si.MeanPieceViews),
			fmt.Sprint(ext.RewrittenBytes), fmt.Sprint(ext.Rewrites)})
	}
	p.table([]string{"corpus", "views", "run coverage %", "pieces (prod)", "mean piece views (prod)",
		"mean piece views (ideal)", "extend rewritten B (prod)", "extend rewrites"}, rows)
}
