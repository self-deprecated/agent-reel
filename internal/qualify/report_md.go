package qualify

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// RenderMarkdown renders report.md. Its first line is always
// "ROLLOUT: BLOCKED"; budget amendments follow immediately.
func RenderMarkdown(r *Report) []byte {
	var b bytes.Buffer
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	p("ROLLOUT: BLOCKED\n\n")

	if len(r.Budgets.Amendments) == 0 {
		p("Budget amendments: none.\n\n")
	} else {
		p("## Budget amendments\n\n")
		p("| Date | ID | Old | New | Reason | Approved by |\n|---|---|---|---|---|---|\n")
		for _, a := range r.Budgets.Amendments {
			p("| %s | %s | %s | %s | %s | %s |\n", cell(a.Date), cell(a.ID), cell(a.Old), cell(a.New), cell(a.Reason), cell(a.ApprovedBy))
		}
		p("\n")
	}

	p("**%s**\n\n", strings.ToUpper(r.Automated.Statement[:1])+r.Automated.Statement[1:])
	p("- Automated result: %s (correctness: %s; budgets: %s)\n", r.Automated.Result, r.Automated.Correctness, r.Automated.Budgets)
	p("- Rollout: %s\n", r.Rollout.Status)
	p("- Profile: %s\n", r.ProfileLabel)
	match := "matches the frozen hash"
	if !r.Budgets.MatchFrozen {
		match = "DOES NOT match the frozen hash " + r.Budgets.FrozenSHA256
	}
	p("- Budgets: `%s` (%s)\n", r.Budgets.SHA256, match)
	if r.Budgets.AmendedSinceGenesis {
		p("- Budgets differ from the Run 1 genesis `%s`: %d amendment(s) listed above\n", r.Budgets.GenesisSHA256, len(r.Budgets.Amendments))
	} else {
		p("- Budgets equal the Run 1 genesis `%s`\n", r.Budgets.GenesisSHA256)
	}
	if r.Source != nil {
		id := "jj identity not supplied"
		if r.Source.ChangeID != "" {
			id = "jj change `" + r.Source.ChangeID + "`, commit `" + r.Source.CommitID + "`"
		}
		p("- Source: %s; tree manifest `%s` (%d files)", id, r.Source.TreeSHA256, r.Source.TreeFiles)
		if r.Source.NixSource != "" {
			p("; Nix source `%s`", r.Source.NixSource)
		}
		p("\n")
	} else {
		p("- Source: missing\n")
	}
	suites := "not collected"
	if string(r.Suites) != "null" {
		suites = "see the Suites section"
	}
	p("- Suites: %s\n\n", suites)

	p("## Blocked on\n\n| # | ID | Kind | Reason | Evidence | Next input |\n|---|---|---|---|---|---|\n")
	for i, blk := range r.Rollout.BlockedOn {
		p("| %d | %s | %s | %s | %s | %s |\n", i+1, cell(blk.ID), blk.Kind, cell(blk.Reason), cell(blk.Evidence), cell(blk.NextInput))
	}
	p("\n")

	p("## Budgets\n\n")
	if r.Profile == ProfileSmoke {
		p("Every verdict below is labelled **%s**.\n\n", SmokeLabel)
	}
	p("| ID | Metric | Budget | Verdict | Worst observation | Basis |\n|---|---|---|---|---|---|\n")
	for _, v := range r.Verdicts {
		verdict := v.Verdict
		if v.Label != "" {
			verdict += " (" + v.Label + ")"
		}
		p("| %s | %s | %s | %s | %s | %s |\n", v.ID, cell(v.Metric), cell(v.Budget), verdict, cell(worstText(v)), cell(strings.Join(v.Basis.Kinds, ", ")+": "+v.Basis.Cite))
	}
	p("\n")
	var nonPass []string
	for _, v := range r.Verdicts {
		for _, l := range v.Limits {
			for _, o := range l.NonPass {
				nonPass = append(nonPass, fmt.Sprintf("| %s | %s | %s | %s | %s |", l.Key, o.Verdict, cell(o.Scenario+" "+o.Cell+targetText(o)), cell(o.Reason), cell(o.Evidence)))
			}
		}
	}
	if len(nonPass) > 0 {
		p("### Failed and not-evaluated limits\n\n| Limit | Verdict | Cell | Reason | Evidence |\n|---|---|---|---|---|\n")
		p("%s\n\n", strings.Join(nonPass, "\n"))
	}

	p("## Scenarios\n\n| Scenario | State | Planned | Cells (planned) | Checks pass/fail | Label |\n|---|---|---|---|---|---|\n")
	for _, s := range r.Scenarios {
		planned := "yes"
		if !s.Planned {
			planned = "no"
		}
		if s.ReportOnly {
			planned += " (report-only)"
		}
		label := s.Label
		if s.State == ScenarioNotRun && s.Error != "" {
			label = s.Error
		}
		p("| %s | %s | %s | %d (%d) | %d/%d | %s |\n", s.ID, s.State, planned, s.Cells, s.PlannedCell, s.ChecksPass, s.ChecksFail, cell(label))
	}
	p("\n")
	if len(r.HarnessRuns) > 0 {
		p("### Harness runs\n\n| Package | Exit | Wall | Top-level tests pass/fail/skip | Skipped by name | Log |\n|---|---|---|---|---|---|\n")
		for _, h := range r.HarnessRuns {
			var named []string
			for _, sk := range h.SkippedByName {
				named = append(named, sk.Test+" ("+sk.Scenario+"): "+sk.Reason)
			}
			skip := "—"
			if len(named) > 0 {
				skip = strings.Join(named, "; ")
			}
			p("| %s | %d | %s s | %d/%d/%d | %s | %s |\n", h.Package, h.ExitCode, formatNumber(math.Round(h.WallSeconds)), len(h.Passed), len(h.Failed), len(h.Skipped), cell(skip), cell(h.Log))
		}
		p("\n")
	}
	var failed []string
	for _, c := range r.Correctness {
		if c.Result != CheckPass {
			failed = append(failed, fmt.Sprintf("| %s | %s | %s | %s |", cell(c.Where), cell(c.ID), cell(c.Description), cell(c.Evidence)))
		}
	}
	if len(failed) > 0 {
		p("### Failed correctness checks\n\n| Where | Check | Description | Evidence |\n|---|---|---|---|\n%s\n\n", strings.Join(failed, "\n"))
	} else {
		p("All %d correctness checks pass.\n\n", len(r.Correctness))
	}

	if sf, err := decodeSuites(r.Suites); err == nil {
		p("## Suites\n\n| Suite | Result | Exit | Wall | Tests pass/fail/skip | Packages ok/fail | Log |\n|---|---|---|---|---|---|---|\n")
		for _, st := range sf.Suites {
			p("| %s | %s | %d | %s s | %d/%d/%d | %d/%d | %s |\n", st.Name, st.Result, st.ExitCode, formatNumber(math.Round(st.WallSeconds)),
				st.Counts.TestsPassed, st.Counts.TestsFailed, st.Counts.TestsSkipped, st.Counts.PackagesOK, st.Counts.PackagesFailed, cell(st.Log))
		}
		p("\n")
	}

	p("## Report-only metrics\n\n")
	renderMetrics(&b, r.ReportOnly)
	p("## Findings\n\n")
	if len(r.Findings) == 0 {
		p("None recorded.\n\n")
	} else {
		p("| Scenario | ID | Evidence | Suspected cause | Proposed remedy |\n|---|---|---|---|---|\n")
		for _, f := range r.Findings {
			p("| %s | %s | %s | %s | %s |\n", f.Scenario, cell(f.Finding.ID), cell(f.Finding.Evidence), cell(f.Finding.SuspectedCause), cell(f.Finding.ProposedRemedy))
		}
		p("\n")
	}
	p("## What accelerated logical time does not reproduce\n\n")
	for _, s := range r.NotReproduced {
		p("- %s\n", s)
	}
	p("\n## Known limits\n\n")
	for _, s := range r.KnownLimits {
		p("- %s\n", s)
	}
	p("\n## V9 input\n\n")
	renderMetrics(&b, r.V9Input)
	return b.Bytes()
}

func renderMetrics(b *bytes.Buffer, ms []ReportMetric) {
	if len(ms) == 0 {
		b.WriteString("None recorded.\n\n")
		return
	}
	b.WriteString("| Scenario | Cell | Metric | Value | Kind | Method / formula |\n|---|---|---|---|---|---|\n")
	for _, m := range ms {
		how := m.Metric.Method
		if m.Metric.Formula != "" {
			how += "; " + m.Metric.Formula
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s %s | %s | %s |\n", m.Scenario, cell(m.Cell), m.Metric.ID, formatNumber(m.Metric.Value), m.Metric.Unit, m.Metric.Kind, cell(how))
	}
	b.WriteString("\n")
}

func worstText(v BudgetVerdict) string {
	var parts []string
	for _, l := range v.Limits {
		if l.Worst == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s %s vs %s %s (%s%s)", l.Key, formatNumber(*l.Worst.Observed), l.Unit, l.Comparator,
			formatNumber(*l.Worst.Threshold), l.Worst.Cell, targetText(*l.Worst)))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "; ")
}

func targetText(o Observation) string {
	if o.Target == nil {
		return ""
	}
	return " target " + strconv.Itoa(*o.Target)
}

// cell escapes text for a Markdown table cell.
func cell(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "|", `\|`)
}
