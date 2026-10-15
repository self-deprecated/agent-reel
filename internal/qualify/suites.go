package qualify

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// SuitesSchema identifies suites.json (C6: the legacy suites run beside the
// qualification, with their pass/fail and counts).
const SuitesSchema = "agent-reel/qualification-suites/1"

// SuitesFile is suites.json.
type SuitesFile struct {
	Schema string  `json:"schema"`
	Method string  `json:"method"`
	Suites []Suite `json:"suites"`
}

// Suite is one sd suite run.
type Suite struct {
	Name        string      `json:"name"`
	Command     string      `json:"command"`
	Result      string      `json:"result"` // pass or fail (the exit status)
	ExitCode    int         `json:"exitCode"`
	WallSeconds float64     `json:"wallSeconds"`
	Log         string      `json:"log"`
	Counts      SuiteCounts `json:"counts"`
}

// SuiteCounts are parsed from the suite's log. Test counts include subtests
// (every "--- PASS/FAIL/SKIP:" line at any depth).
type SuiteCounts struct {
	TestsPassed     int `json:"testsPassed"`
	TestsFailed     int `json:"testsFailed"`
	TestsSkipped    int `json:"testsSkipped"`
	PackagesOK      int `json:"packagesOk"`
	PackagesFailed  int `json:"packagesFailed"`
	PackagesNoTests int `json:"packagesNoTests"`
	// ArchiveSuccess is archive_integration.py's final {"success": …}
	// line, when the log has one.
	ArchiveSuccess *bool `json:"archiveSuccess,omitempty"`
}

// SuitesMethod describes how suites.json is produced.
const SuitesMethod = "each suite runs as its sd task body (sd areel <name>) with stdout+stderr logged; result is the exit status; " +
	"counts are parsed from the log: \"--- PASS:\"/\"--- FAIL:\"/\"--- SKIP:\" lines at any depth (subtests included), " +
	"\"ok <pkg>\", \"FAIL <pkg>\" and \"? <pkg> [no test files]\" package lines, and archive_integration.py's final {\"success\": …} line; " +
	"a suite whose log reports success false fails even with exit status 0, and archive-integration fails without that line"

var (
	suiteTestLine  = regexp.MustCompile(`^\s*--- (PASS|FAIL|SKIP): `)
	suiteOKLine    = regexp.MustCompile(`^ok\s+\S+`)
	suiteFailLine  = regexp.MustCompile(`^FAIL\s+\S+`)
	suiteNoTests   = regexp.MustCompile(`^\?\s+\S+\s+\[no test files\]`)
	archiveSuccess = regexp.MustCompile(`^\{"success": (true|false), "report": `)
)

// ParseSuiteLog counts a suite log.
func ParseSuiteLog(r io.Reader) (SuiteCounts, error) {
	var c SuiteCounts
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if m := suiteTestLine.FindStringSubmatch(line); m != nil {
			switch m[1] {
			case "PASS":
				c.TestsPassed++
			case "FAIL":
				c.TestsFailed++
			default:
				c.TestsSkipped++
			}
			continue
		}
		switch {
		case suiteOKLine.MatchString(line):
			c.PackagesOK++
		case suiteFailLine.MatchString(line):
			c.PackagesFailed++
		case suiteNoTests.MatchString(line):
			c.PackagesNoTests++
		}
		if m := archiveSuccess.FindStringSubmatch(line); m != nil {
			ok := m[1] == "true"
			c.ArchiveSuccess = &ok
		}
	}
	return c, sc.Err()
}

// BuildSuites reads dir/suites.tsv, one line per suite run
// "<name>\t<exit code>\t<wall seconds>\t<log file relative to dir>\t<command>",
// parses each log, and returns suites.json.
func BuildSuites(dir string) (SuitesFile, error) {
	out := SuitesFile{Schema: SuitesSchema, Method: SuitesMethod, Suites: []Suite{}}
	data, err := os.ReadFile(filepath.Join(dir, "suites.tsv"))
	if err != nil {
		return out, err
	}
	for n, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 5)
		if len(f) != 5 {
			return out, fmt.Errorf("suites.tsv line %d: want 5 tab-separated fields", n+1)
		}
		code, err := strconv.Atoi(f[1])
		if err != nil {
			return out, fmt.Errorf("suites.tsv line %d: exit code %q", n+1, f[1])
		}
		wall, err := strconv.ParseFloat(f[2], 64)
		if err != nil {
			return out, fmt.Errorf("suites.tsv line %d: wall %q", n+1, f[2])
		}
		s := Suite{Name: f[0], ExitCode: code, WallSeconds: wall, Log: f[3], Command: f[4], Result: CheckPass}
		if code != 0 {
			s.Result = CheckFail
		}
		lf, err := os.Open(filepath.Join(dir, filepath.FromSlash(f[3])))
		if err != nil {
			return out, err
		}
		s.Counts, err = ParseSuiteLog(lf)
		lf.Close()
		if err != nil {
			return out, err
		}
		// archive_integration.py reports its own verdict on its last line:
		// false (or, for archive-integration, no verdict line) fails the suite
		// even when the exit status is 0.
		if a := s.Counts.ArchiveSuccess; (a != nil && !*a) || (a == nil && s.Name == "archive-integration") {
			s.Result = CheckFail
		}
		out.Suites = append(out.Suites, s)
	}
	return out, nil
}

// decodeSuites reads suites.json when it carries SuitesSchema.
func decodeSuites(data []byte) (*SuitesFile, error) {
	var f SuitesFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	if f.Schema != SuitesSchema {
		return nil, fmt.Errorf("suites schema %q, want %s", f.Schema, SuitesSchema)
	}
	if len(f.Suites) == 0 {
		return nil, errors.New("suites.json lists no suite")
	}
	return &f, nil
}
