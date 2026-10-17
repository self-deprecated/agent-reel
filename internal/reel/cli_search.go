package reel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/self-deprecated/agent-reel/internal/textindex"
)

var indexHelp = cliCommand{Name: "index", Args: "[REC]",
	Summary: `Build the text search index of a sealed recording (or --all of them) in
<library>/index/ID.sqlite. The index is a deletable cache keyed by the sealed
file's digest: an up-to-date index is kept unless --force. Recordings are
indexed automatically after sealing; live recordings are not indexed yet.`,
	Examples: []string{"areel index latest", "areel index --all", "areel index 3f2a --force --json"},
	Docs:     "docs/library.md (Search)"}

var searchHelp = cliCommand{Name: "search", Args: "TEXT [REC]",
	Summary: `Search the text shown in sealed recordings: one recording, or the whole
library without REC. Matches are case-insensitive substrings of one terminal
row (a phrase wrapped over two lines is not found) and labelled marks. Each
hit is the row's first appearance: segment, time, view, row (y) and columns.
Missing or stale indexes are built first. --fuzzy tolerates typos.`,
	Examples: []string{`areel search "All tests passed"`, `areel search "panic:" latest --json`, `areel search "conection refused" --fuzzy`},
	Docs:     "docs/library.md (Search)"}

func textIndexCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(indexHelp, out)
	f.library()
	all := f.Bool("all", false, "index every sealed recording in the library")
	force := f.Bool("force", false, "rebuild even when the index is up to date")
	asJSON := f.json()
	pos, help, e := f.parse(args[1:], 0, 1)
	if help || e != nil {
		return e
	}
	if *all == (len(pos) == 1) {
		return usageErr("index: give either REC or --all; see areel index --help")
	}
	if !*all {
		if e = checkREC(pos[0]); e != nil {
			return e
		}
	}
	s, e := f.load(nil)
	if e != nil {
		return e
	}
	root, e := requireLibrary(s)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(withSealName(context.Background(), s.SealName), indexCLIDeadline)
	defer cancel()
	var ids []string
	var reports []indexReport
	if *all {
		sealed, live, failed, e := librarySealedIDs(ctx, root)
		if e != nil {
			return e
		}
		for _, src := range sealed {
			ids = append(ids, src.id)
		}
		for _, id := range live {
			reports = append(reports, indexReport{Recording: id, Status: indexLive, Reason: errIndexNotSealed.Error()})
		}
		reports = append(reports, failed...)
	} else {
		id, e := resolveREC(root, pos[0], currentRecording(root, s), s.SealName)
		if e != nil {
			return e
		}
		ids = []string{id}
	}
	var built []indexReport
	failures := 0
	for _, id := range ids {
		r := ensureRecordingIndex(ctx, root, id, *force)
		if r.Status == indexUnavailable {
			failures++
		}
		built = append(built, r)
		if !*asJSON {
			if e := formatIndexReport(out, r); e != nil {
				return e
			}
		}
	}
	reports = append(built, reports...)
	if *asJSON {
		if e := writeJSONLine(out, map[string]any{"recordings": reports}); e != nil {
			return e
		}
	} else if *all {
		for _, r := range reports[len(built):] {
			if e := formatIndexReport(out, r); e != nil {
				return e
			}
		}
	}
	if !*all && len(built) == 1 && built[0].Status == indexLive {
		return fmt.Errorf("recording %s is %s", shortID(built[0].Recording), errIndexNotSealed)
	}
	if failures > 0 {
		return fmt.Errorf("%d recording(s) could not be indexed", failures)
	}
	return nil
}

// CLI deadlines: a search (including any index builds it needs) and an
// index run of the whole library.
const (
	searchCLIDeadline = 15 * time.Minute
	indexCLIDeadline  = 2 * time.Hour
)

func formatIndexReport(out io.Writer, r indexReport) error {
	var line string
	switch r.Status {
	case indexBuilt:
		line = fmt.Sprintf("%s  indexed: %d rows, %d marks from %d views; %s (sealed %s) in %s", shortID(r.Recording), r.Rows, r.Activity, r.Views, humanBytes(r.Bytes), humanBytes(r.SealedBytes), (time.Duration(r.Seconds * float64(time.Second))).Round(time.Millisecond))
	case indexFresh:
		line = fmt.Sprintf("%s  up to date: %d rows, %d marks; %s", shortID(r.Recording), r.Rows, r.Activity, humanBytes(r.Bytes))
	case indexLive:
		line = fmt.Sprintf("%s  skipped: %s", shortID(r.Recording), errIndexNotSealed)
	default:
		line = fmt.Sprintf("%s  failed: %s", shortID(r.Recording), r.Reason)
	}
	_, err := fmt.Fprintln(out, line)
	return err
}

func searchCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(searchHelp, out)
	f.library()
	fuzzy := f.Bool("fuzzy", false, "rank rows by edit distance (tolerates typos)")
	limit := f.Int("limit", "N", textindex.LimitDefault, fmt.Sprintf("at most N hits in total (1..%d)", textindex.LimitMax))
	asJSON := f.json()
	pos, help, e := f.parse(args[1:], 1, 2)
	if help || e != nil {
		return e
	}
	text, e := textindex.NormalizeQuery(pos[0])
	if e != nil {
		return usageErr("search: %v", e)
	}
	if *limit < 1 || *limit > textindex.LimitMax {
		return usageErr("search: --limit must be 1..%d", textindex.LimitMax)
	}
	if len(pos) == 2 {
		if e = checkREC(pos[1]); e != nil {
			return e
		}
	}
	s, e := f.load(nil)
	if e != nil {
		return e
	}
	root, e := requireLibrary(s)
	if e != nil {
		return e
	}
	id := ""
	if len(pos) == 2 {
		if id, e = resolveREC(root, pos[1], currentRecording(root, s), s.SealName); e != nil {
			return e
		}
	}
	// A search may build missing indexes first; bound the whole command.
	ctx, cancel := context.WithTimeout(withSealName(context.Background(), s.SealName), searchCLIDeadline)
	defer cancel()
	progress := func(id string) { fmt.Fprintf(errOut, "areel: indexing %s…\n", shortID(id)) }
	report, e := searchLibrary(ctx, root, id, textindex.Query{Text: text, Fuzzy: *fuzzy, Limit: *limit}, buildNow(root, progress))
	if e != nil {
		return e
	}
	if *asJSON {
		if e = writeJSONLine(out, report); e != nil {
			return e
		}
	} else if e = formatSearch(out, report); e != nil {
		return e
	}
	if id != "" && len(report.Recordings) == 1 && report.Recordings[0].Status == indexLive {
		return fmt.Errorf("recording %s is %s", shortID(id), errIndexNotSealed)
	}
	return nil
}

func formatSearch(out io.Writer, r searchReport) error {
	var b strings.Builder
	byRecording := map[string]searchRecording{}
	for _, rec := range r.Recordings {
		byRecording[rec.Recording] = rec
	}
	current := ""
	for _, h := range r.Hits {
		if h.Recording != current {
			current = h.Recording
			rec := byRecording[current]
			label := strings.TrimSpace(strings.Join([]string{rec.Repository, rec.Workspace, rec.Session}, " "))
			if rec.RecordedAt != nil {
				label += " · " + rec.RecordedAt.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(&b, "%s  %s\n", shortID(current), label)
		}
		where := fmt.Sprintf("row %d", h.Y)
		if h.Kind == textindex.KindActivity {
			where = "mark"
		}
		seen := ""
		if h.Count > 1 {
			seen = fmt.Sprintf("  (shown %d times)", h.Count)
		}
		fmt.Fprintf(&b, "  %d %s  %-7s  %s%s\n", h.Segment, searchClock(h.AtNs), where, h.Text, seen)
	}
	if len(r.Hits) == 0 {
		b.WriteString("no matches\n")
	}
	if r.More {
		fmt.Fprintf(&b, "(first %d hits; use --limit for more)\n", r.Limit)
	}
	var notes []string
	for _, rec := range r.Recordings {
		switch rec.Status {
		case indexLive:
			notes = append(notes, shortID(rec.Recording)+" not searched: "+errIndexNotSealed.Error())
		case indexUnavailable:
			notes = append(notes, shortID(rec.Recording)+" not searched: "+rec.Reason)
		}
	}
	for _, n := range notes {
		b.WriteString(n + "\n")
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// searchClock is H:MM:SS.mmm (or M:SS.mmm) of a footage position.
func searchClock(ns int64) string {
	d := time.Duration(ns)
	ms := d.Milliseconds() % 1000
	s := int64(d / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d.%03d", s/3600, s/60%60, s%60, ms)
	}
	return fmt.Sprintf("%d:%02d.%03d", s/60, s%60, ms)
}

// holderIndexAfterSeal builds the text index of a just-sealed recording on the
// sealing thread (already at lowered priority). Failure never affects the
// seal; the index is a cache that `areel index` or a search rebuilds.
func holderIndexAfterSeal(ctx context.Context, root, id string) error {
	r := ensureRecordingIndex(ctx, root, id, false)
	if r.Status == indexUnavailable || r.Status == indexLive {
		return errors.New(r.Reason)
	}
	return nil
}
