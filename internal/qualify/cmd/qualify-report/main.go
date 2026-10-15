// Command qualify-report rewrites report.json and report.md in a V8
// qualification artifact directory from its scenario fragments.
//
//	go run ./internal/qualify/cmd/qualify-report --in DIR [--suites FILE] [--profile smoke|full]
//	    [--source-root DIR] [--exit-on none|correctness|automated]
//
// With --source-root and no source.json in DIR, it first writes source.json
// from the tree manifest of that module root and $AREEL_QUALIFY_JJ
// ("<change_id> <commit_id>") and $AREEL_QUALIFY_NIX_SOURCE. It never runs a
// VCS command. The report is always written before any non-zero exit.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/self-deprecated/agent-reel/internal/qualify"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "qualify-report:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("qualify-report", flag.ContinueOnError)
	in := fs.String("in", "", "artifact directory")
	suites := fs.String("suites", "", "suites.json to include")
	profile := fs.String("profile", "", "profile override (smoke or full)")
	sourceRoot := fs.String("source-root", "", "module root for source.json when absent")
	exitOn := fs.String("exit-on", "none", "exit non-zero on: none, correctness or automated")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || fs.NArg() != 0 {
		return errors.New("usage: qualify-report --in DIR [--suites FILE] [--profile P] [--source-root DIR] [--exit-on none|correctness|automated]")
	}
	switch *exitOn {
	case "none", "correctness", "automated":
	default:
		return fmt.Errorf("--exit-on %q", *exitOn)
	}
	if *sourceRoot != "" {
		path := filepath.Join(*in, "source.json")
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			src, err := qualify.SourceIdentity(*sourceRoot, os.Getenv("AREEL_QUALIFY_JJ"), os.Getenv("AREEL_QUALIFY_NIX_SOURCE"))
			if err != nil {
				return err
			}
			b, err := json.MarshalIndent(src, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
				return err
			}
		}
	}
	r, err := qualify.BuildReport(qualify.Options{Dir: *in, SuitesPath: *suites, Profile: *profile})
	if err != nil {
		return err
	}
	if err := qualify.WriteReport(*in, r); err != nil {
		return err
	}
	fmt.Println(filepath.Join(*in, "report.md"))
	fmt.Println(r.Automated.Statement)
	switch {
	case *exitOn == "correctness" && r.Automated.Correctness != qualify.CheckPass:
		return errors.New("correctness checks failed")
	case *exitOn == "automated" && r.Automated.Result != qualify.VerdictPass:
		return errors.New("automated result is " + r.Automated.Result)
	}
	return nil
}
