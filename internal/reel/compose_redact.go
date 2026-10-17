package reel

import (
	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/redact"
)

// Compose redaction and search reporting (docs/compose.md, "Redaction" and
// "Timerefs"). An EDL's `redact` rules, plus any `areel compose --redact`
// presets, travel in the frozen EDL; every source's frozen input masks its
// screens with them (freezeVisualExport), so the render, --still, --proxy and
// the plan's projected chunk inputs are all masked alike.

// composeRedactFlag adds --redact presets to doc's rules (the frozen EDL
// carries them, so a re-render of it masks the same way).
func composeRedactFlag(doc *editmap.EDL, list string) error {
	if list == "" {
		return nil
	}
	presets, err := redact.ParsePresets(list)
	if err != nil {
		return usageErr("compose: --redact: %v", err)
	}
	merged, err := redact.Canonical(redact.Merge(doc.Redact, presets))
	if err != nil {
		return usageErr("compose: --redact: %v", err)
	}
	doc.Redact = merged
	return nil
}

// searchAndRedaction adds the resolved {search} hits and the redaction
// report (counted from each used source's text index) to m. Counts are
// cached per recording and digest for the job (the rules are fixed), and a
// --still skips them.
func (j *composeJob) searchAndRedaction(m *composeManifest, mode string) {
	m.SearchHits = j.srcs.searches
	report := newRedactionReport(j.comp.EDL.Redact)
	if report == nil {
		return
	}
	if j.redactCounts == nil {
		j.redactCounts = map[string]redactionSourceReport{}
	}
	for _, name := range composeSortedKeys(j.srcs.open) {
		src := j.srcs.open[name]
		if mode == "still" {
			report.Sources = append(report.Sources, redactionSourceReport{Source: name, Recording: src.id, Status: redactSkipped, Reason: "--still does not count; run --plan"})
			continue
		}
		key := src.id + " " + src.digest
		counted, ok := j.redactCounts[key]
		if !ok {
			counted = countRedactions(j.ctx, j.root, name, src.id, report.Rules)
			j.redactCounts[key] = counted
		}
		counted.Source = name
		report.Sources = append(report.Sources, counted)
	}
	m.Redaction = report
	m.Notice += " " + redactNotice
	// Pattern text may itself be secret: the plan and metadata show the
	// frozen EDL with patterns withheld (edl.sha256 still hashes the real
	// frozen EDL, which only --freeze writes).
	m.EDL.Frozen.Redact = redact.Public(m.EDL.Frozen.Redact)
}
