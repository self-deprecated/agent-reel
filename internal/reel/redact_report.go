package reel

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/self-deprecated/agent-reel/internal/redact"
	"github.com/self-deprecated/agent-reel/internal/textindex"
)

// The redaction report of --plan and metadata.json: per source, per rule,
// how many distinct rows of the source recording the rule masks, how many
// matches that is and up to redactExamplesMax masked examples (the whole
// row masked by every rule, never the secret). Counts come from the text
// index (one evaluation per distinct indexed row, the same text the render
// masks); a missing or stale index is built first, as `areel search` does.
// Live recordings are not indexed, so their counts are unavailable; the
// render masks them all the same.

const (
	redactExamplesMax    = 5
	redactExampleRunes   = 120
	redactExampleContext = 32
)

type redactionRuleReport struct {
	Rule     redact.Rule `json:"rule"`
	Label    string      `json:"label"`
	Rows     int64       `json:"rows"`
	Matches  int64       `json:"matches"`
	Shown    int64       `json:"shown"`
	Examples []string    `json:"examples"`
}

type redactionSourceReport struct {
	Source    string                `json:"source,omitempty"`
	Recording string                `json:"recording"`
	Status    string                `json:"status"`
	Reason    string                `json:"reason,omitempty"`
	Index     string                `json:"index,omitempty"`
	RowsRead  int64                 `json:"rowsRead,omitempty"`
	Rules     []redactionRuleReport `json:"rules,omitempty"`
}

// redactionReport never carries pattern text: Public lists the rules with
// patterns withheld (redact.Public); Rules is the rule set it counts with.
type redactionReport struct {
	Rules   []redact.Rule           `json:"-"`
	Public  []redact.Rule           `json:"rules"`
	Scope   string                  `json:"scope"`
	Sources []redactionSourceReport `json:"sources"`
}

const redactionScope = "counts are distinct rows of each whole source recording (rows: distinct masked rows; matches: rule matches in them; shown: how often those rows appeared); every rendered frame of the output is masked. Not covered: secrets wrapped across two terminal rows, overlay/card text, mark labels, the library viewer."

// Redaction report states.
const (
	redactCounted     = "counted"
	redactUnavailable = "unavailable"
	redactSkipped     = "skipped"
)

// newRedactionReport starts a report for rules (nil when there are none).
func newRedactionReport(rules []redact.Rule) *redactionReport {
	if len(rules) == 0 {
		return nil
	}
	return &redactionReport{Rules: rules, Public: redact.Public(rules), Scope: redactionScope, Sources: []redactionSourceReport{}}
}

// add counts recording id (as EDL source name, or "" for an export).
func (r *redactionReport) add(ctx context.Context, root, name, id string) {
	if r == nil {
		return
	}
	r.Sources = append(r.Sources, countRedactions(ctx, root, name, id, r.Rules))
}

// countRedactions evaluates rules once per distinct indexed row of id.
func countRedactions(ctx context.Context, root, name, id string, rules []redact.Rule) redactionSourceReport {
	out := redactionSourceReport{Source: name, Recording: id, Status: redactUnavailable}
	set, err := redact.Compile(rules)
	if err != nil {
		out.Reason = err.Error()
		return out
	}
	ix, status, _, err := openRecordingIndex(ctx, root, id)
	if err != nil {
		out.Reason = err.Error()
		if errors.Is(err, errIndexNotSealed) {
			out.Reason = "live recording: " + errIndexNotSealed.Error() + ", so no counts; the render still masks every row"
		}
		return out
	}
	defer ix.Close()
	out.Index = status
	reports := make([]redactionRuleReport, set.Len())
	public := redact.Public(rules)
	for i := range reports {
		reports[i] = redactionRuleReport{Rule: public[i], Label: set.Rule(i).Label(i), Examples: []string{}}
	}
	err = ix.Rows(ctx, textindex.KindScreen, func(row textindex.Row) error {
		out.RowsRead++
		spans, counts := set.Find(row.Text)
		if len(spans) == 0 {
			return nil
		}
		masked := ""
		for i, n := range counts {
			if n == 0 {
				continue
			}
			rr := &reports[i]
			rr.Rows++
			rr.Matches += int64(n)
			rr.Shown += row.Count
			if len(rr.Examples) < redactExamplesMax {
				if masked == "" {
					masked = redact.ApplyText(row.Text, spans)
				}
				ex := redactExample(masked, spans, i)
				if !slices.Contains(rr.Examples, ex) {
					rr.Examples = append(rr.Examples, ex)
				}
			}
		}
		return nil
	})
	if err != nil {
		out.Reason = fmt.Sprintf("reading the text index: %v", err)
		return out
	}
	out.Status, out.Rules = redactCounted, reports
	return out
}

// redactExample is a window of the masked row around rule's first span.
// Substitutions shorten the masked text, so the window is taken in masked
// runes, starting a little before the span's original start.
func redactExample(masked string, spans []redact.Span, rule int) string {
	runes := []rune(masked)
	start := 0
	for _, sp := range spans {
		if slices.Contains(sp.Rules, rule) {
			start = max(0, min(sp.Start, len(runes))-redactExampleContext)
			break
		}
	}
	end := min(len(runes), start+redactExampleRunes)
	ex := string(runes[start:end])
	if start > 0 {
		ex = "…" + ex
	}
	if end < len(runes) {
		ex += "…"
	}
	return ex
}

// openRecordingIndex opens recording id's fresh text index, building it
// first when it is missing or stale (the `areel search` path). status is
// indexFresh or indexBuilt; digest is the sealed manifest digest it matches
// ("sha256:<hex>", as EDL sources pin it).
func openRecordingIndex(ctx context.Context, root, id string) (_ *textindex.Index, status, digest string, err error) {
	src, err := sealedIndexKey(ctx, root, id)
	if err != nil {
		return nil, "", "", err
	}
	path := libraryIndexPath(root, id)
	status = indexFresh
	ix, err := textindex.OpenFresh(path, src.digest)
	if err != nil {
		if _, err = buildSealedIndex(ctx, root, src); err != nil {
			return nil, "", "", fmt.Errorf("building the text index: %s", sealDiagnostic(err))
		}
		status = indexBuilt
		if ix, err = textindex.OpenFresh(path, src.digest); err != nil {
			return nil, "", "", fmt.Errorf("text index: %s", sealDiagnostic(err))
		}
	}
	if ix.Meta().Recording != id {
		_ = ix.Close()
		return nil, "", "", errors.New("text index names another recording")
	}
	return ix, status, "sha256:" + src.digest, nil
}
