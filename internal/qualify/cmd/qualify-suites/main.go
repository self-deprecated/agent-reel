// Command qualify-suites writes DIR/suites.json from DIR/suites.tsv and the
// suite logs it names (qualify.BuildSuites). `sd areel qualify-suites DIR`
// runs the legacy suites and then this command.
//
//	go run ./internal/qualify/cmd/qualify-suites --dir DIR
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
		fmt.Fprintln(os.Stderr, "qualify-suites:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("qualify-suites", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory holding suites.tsv and the suite logs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || fs.NArg() != 0 {
		return errors.New("usage: qualify-suites --dir DIR")
	}
	f, err := qualify.BuildSuites(*dir)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(*dir, "suites.json")
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	for _, s := range f.Suites {
		fmt.Printf("%-20s %s (exit %d, %.0f s; tests %d pass, %d fail, %d skip; packages %d ok, %d fail)\n", s.Name, s.Result, s.ExitCode, s.WallSeconds,
			s.Counts.TestsPassed, s.Counts.TestsFailed, s.Counts.TestsSkipped, s.Counts.PackagesOK, s.Counts.PackagesFailed)
	}
	fmt.Println(path)
	return nil
}
