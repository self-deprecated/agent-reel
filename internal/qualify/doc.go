// Package qualify is the V8 qualification foundation for the visual/1
// recorder, GUI and exporter. It holds no scenarios; the tagged scenario tests
// in visualcapture and reel use it.
//
// It provides:
//   - deterministic, seeded workload generators W1-W9, the mixed day profile
//     and the Tier B real-time mix, each with an analytic Expectation
//     (workload.go, expect.go);
//   - an exact asciicast v3 writer whose intervals are differences of absolute
//     integer nanoseconds (cast.go);
//   - the frozen budgets file, embedded and pinned by TestBudgetsFrozen
//     (budgets.json, budgets.go);
//   - the scenario fragment schema with measured/derived/estimate labels
//     (result.go) and the profile plan of required cells (plan.go);
//   - /proc and rusage samplers (proc_linux.go) and statistics (stats.go);
//   - the artifact-directory guard that refuses the user's library (guard.go);
//   - the V0 tree-manifest source identity (source.go);
//   - the report generator, which applies the verdict rules and always
//     reports rollout: blocked (report.go, report_md.go, cmd/qualify-report).
package qualify
