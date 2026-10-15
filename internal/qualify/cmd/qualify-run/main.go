// Command qualify-run is the V8 qualification runner behind
// `sd areel qualify-smoke`, `sd areel qualify` and Nix checks.qualify. One
// invocation always produces a complete report in a fresh artifact directory:
//
//	go run ./internal/qualify/cmd/qualify-run --profile smoke|full [--suites FILE]
//	    [--skip TEST=SCENARIO=REASON]... [--exit-on none|correctness|automated]
//	    [--timeout-visualcapture D] [--timeout-reel D] [--min-free-gib N]
//
// It creates OUT with mkdtemp as ${TMPDIR:-/tmp}/areel-qualify-XXXXXXXXX,
// falling back to /tmp when that would be longer than MaxOutLen bytes (the
// Tier B libraries live at OUT/lib/<tag> under the product's 104-byte
// control.sock bound), and refuses an OUT on tmpfs (fsync and allocation
// would not be real; the Tier B harness refuses a tmpfs library too). It
// admits OUT with the qualify artifact guard, then runs
//
//	go test -tags qualify -p 1 -count=1 -timeout T -run '^TestQualify' -v ./internal/visualcapture
//	go test -tags qualify -p 1 -count=1 -timeout T -run '^TestQualify' -v ./internal/reel
//
// one after the other (TestQualifyHarnessAdmission included), each logged to
// OUT/logs, with ZELLIJ*/TMUX* and inherited AREEL_QUALIFY_* removed and a
// private XDG_RUNTIME_DIR under OUT. It then writes harness-runs.json,
// environment.json and source.json (the tree manifest, $AREEL_QUALIFY_JJ and
// $AREEL_QUALIFY_NIX_SOURCE; it never runs a VCS command) and generates
// report.json and report.md. The exit status follows --exit-on (default:
// correctness for smoke, automated for full, plan decision 8), always after
// the report is written.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/self-deprecated/agent-reel/internal/qualify"
)

// MaxOutLen bounds the artifact directory path: OUT/lib/<tag>/recordings/
// <32 hex>/control.sock must stay under the product's 104-byte socket bound.
const MaxOutLen = 40

// Default go test timeouts. Observed full-profile walls on the V2 host:
// visualcapture about 6 h (A.matrix 114 min, 3-day timelines about 2 h,
// faults 6.5 min, plus non-growth/seek/recovery), reel about 2.2 h
// (realtime 22, export 76, readers 15, history 16 min).
var defaultTimeouts = map[string][2]time.Duration{
	qualify.ProfileSmoke: {60 * time.Minute, 60 * time.Minute},
	qualify.ProfileFull:  {600 * time.Minute, 240 * time.Minute},
}

// defaultMinFreeGiB is the free space required under OUT before a run. The
// full profile's peak is the 3-day@32 timeline cell (about 30 GiB observed
// in Run 3) plus the other scenarios' measured summaries.
var defaultMinFreeGiB = map[string]int64{qualify.ProfileSmoke: 2, qualify.ProfileFull: 40}

var packages = []string{"./internal/visualcapture", "./internal/reel"}

type skipList []qualify.NamedSkip

func (s *skipList) String() string { return fmt.Sprint(*s) }

func (s *skipList) Set(v string) error {
	parts := strings.SplitN(v, "=", 3)
	if len(parts) != 3 || !strings.HasPrefix(parts[0], "TestQualify") || !strings.Contains(parts[0], "/") || parts[1] == "" || strings.TrimSpace(parts[2]) == "" {
		return errors.New("--skip wants TestQualifyX/Sub=SCENARIO=REASON (a named subtest, its scenario and a non-empty reason)")
	}
	*s = append(*s, qualify.NamedSkip{Test: parts[0], Scenario: parts[1], Reason: parts[2]})
	return nil
}

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "qualify-run:", err)
	}
	os.Exit(code)
}

func run(args []string) (int, error) {
	fs := flag.NewFlagSet("qualify-run", flag.ContinueOnError)
	profile := fs.String("profile", "", "smoke or full")
	suites := fs.String("suites", "", "suites.json to include in the report (sd areel qualify-suites)")
	exitOn := fs.String("exit-on", "", "none, correctness or automated (default: correctness for smoke, automated for full)")
	tVC := fs.Duration("timeout-visualcapture", 0, "go test -timeout for ./internal/visualcapture")
	tReel := fs.Duration("timeout-reel", 0, "go test -timeout for ./internal/reel")
	minFree := fs.Int64("min-free-gib", -1, "refuse to start with less free space under OUT")
	sourceRoot := fs.String("source-root", ".", "module root (for the tree manifest and go test)")
	var skips skipList
	fs.Var(&skips, "skip", "exclude a named subtest: TestQualifyX/Sub=SCENARIO=REASON (repeatable; shown in the report)")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	timeouts, ok := defaultTimeouts[*profile]
	if !ok {
		return 2, fmt.Errorf("--profile %q: want smoke or full", *profile)
	}
	if *tVC > 0 {
		timeouts[0] = *tVC
	}
	if *tReel > 0 {
		timeouts[1] = *tReel
	}
	if *exitOn == "" {
		*exitOn = "correctness"
		if *profile == qualify.ProfileFull {
			*exitOn = "automated"
		}
	}
	switch *exitOn {
	case "none", "correctness", "automated":
	default:
		return 2, fmt.Errorf("--exit-on %q", *exitOn)
	}
	if err := validateSkips(skips, *profile); err != nil {
		return 2, err
	}
	if *minFree < 0 {
		*minFree = defaultMinFreeGiB[*profile]
	}
	if *suites != "" {
		abs, err := filepath.Abs(*suites)
		if err != nil {
			return 2, err
		}
		if _, err := os.Stat(abs); err != nil {
			return 2, fmt.Errorf("--suites: %w", err)
		}
		*suites = abs
	}
	root, err := filepath.Abs(*sourceRoot)
	if err != nil {
		return 2, err
	}

	helper, err := prerequisites()
	if err != nil {
		return 2, err
	}
	// The source identity is taken before the run, so it names the tree the
	// harness was built from.
	src, err := qualify.SourceIdentity(root, os.Getenv("AREEL_QUALIFY_JJ"), os.Getenv("AREEL_QUALIFY_NIX_SOURCE"))
	if err != nil {
		return 2, err
	}
	out, err := makeOut()
	if err != nil {
		return 2, err
	}
	fmt.Println("qualify-run: artifact directory", out)
	var st syscall.Statfs_t
	if err := syscall.Statfs(out, &st); err != nil {
		return 2, err
	}
	if free := int64(st.Bavail) * int64(st.Bsize); free < *minFree<<30 {
		os.Remove(out)
		return 2, fmt.Errorf("the %s profile needs at least %d GiB free under %s, have %d MiB", *profile, *minFree, filepath.Dir(out), free>>20)
	}
	admitted, err := qualify.OpenArtifactDir(out)
	if err != nil {
		return 2, err
	}
	out = admitted
	if err := os.WriteFile(filepath.Join(out, qualify.ArtifactMarker), []byte("agent-reel V8 qualification artifact directory\n"), 0o600); err != nil {
		return 2, err
	}
	// The budgets copy is written first, so the report's budgets-copy check
	// always applies.
	if err := os.WriteFile(filepath.Join(out, "budgets.json"), qualify.BudgetsJSON(), 0o600); err != nil {
		return 2, err
	}
	env := qualify.CollectEnvironment(out, *profile, helper)
	for _, d := range []string{"logs", "xdg-runtime"} {
		if err := os.Mkdir(filepath.Join(out, d), 0o700); err != nil {
			return 2, err
		}
	}
	childEnv := hygienicEnv(out, *profile)

	hr := qualify.HarnessRunsFile{Schema: qualify.HarnessRunsSchema}
	for i, pkg := range packages {
		r, err := runPackage(root, out, pkg, timeouts[i], skips, childEnv)
		if err != nil {
			return 2, err
		}
		hr.Runs = append(hr.Runs, r)
	}
	env.Finish()
	if err := writeJSON(filepath.Join(out, "harness-runs.json"), hr); err != nil {
		return 2, err
	}
	if err := writeJSON(filepath.Join(out, "environment.json"), env); err != nil {
		return 2, err
	}
	if err := writeJSON(filepath.Join(out, "source.json"), src); err != nil {
		return 2, err
	}
	rep, err := qualify.BuildReport(qualify.Options{Dir: out, SuitesPath: *suites})
	if err != nil {
		return 2, err
	}
	if err := qualify.WriteReport(out, rep); err != nil {
		return 2, err
	}
	md := filepath.Join(out, "report.md")
	fmt.Println()
	printSummary(md)
	fmt.Println("qualify-run: report", md)
	fmt.Println("qualify-run:", rep.Automated.Statement)
	switch {
	case *exitOn == "correctness" && rep.Automated.Correctness != qualify.CheckPass:
		return 1, errors.New("correctness checks failed (see the report)")
	case *exitOn == "automated" && rep.Automated.Result != qualify.VerdictPass:
		return 1, errors.New("automated result is " + rep.Automated.Result + " (see the report)")
	}
	return 0, nil
}

// prerequisites refuses a missing locked tool (the video-check guards plus
// the helper and asciinema) and returns the helper path.
func prerequisites() (string, error) {
	helper := os.Getenv("AREEL_TEST_AVT")
	if helper == "" {
		helper = os.Getenv("AREEL_AVT_CHECKPOINT")
	}
	if helper == "" {
		return "", errors.New("AREEL_TEST_AVT is not set: run inside the root devenv shell for the locked avt helper")
	}
	for name, path := range map[string]string{"AREEL_TEST_AVT": helper, "AREEL_TEST_ASCIINEMA": os.Getenv("AREEL_TEST_ASCIINEMA"),
		"AREEL_TEST_CHROMIUM": os.Getenv("AREEL_TEST_CHROMIUM"), "AREEL_TEST_FFMPEG": os.Getenv("AREEL_TEST_FFMPEG"), "AREEL_TEST_FFPROBE": os.Getenv("AREEL_TEST_FFPROBE")} {
		if path == "" {
			return "", fmt.Errorf("%s is not set: run inside the root devenv shell for the locked tools", name)
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("%s=%s is not an executable file (%v)", name, path, err)
		}
	}
	return helper, nil
}

// makeOut creates a fresh 0700 OUT (mkdtemp) under ${TMPDIR:-/tmp}, or /tmp
// when that is too long, and refuses a user library path or a tmpfs.
func makeOut() (string, error) {
	base := os.Getenv("TMPDIR")
	if base == "" {
		base = "/tmp"
	}
	base = filepath.Clean(base)
	const pattern = "areel-qualify-"
	// os.MkdirTemp appends up to 10 random digits.
	if len(base)+1+len(pattern)+10 > MaxOutLen {
		fmt.Printf("qualify-run: TMPDIR %s is too long for OUT (≤ %d bytes); using /tmp\n", base, MaxOutLen)
		base = "/tmp"
	}
	if err := qualify.RefuseLibraryLexical(filepath.Join(base, pattern)); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(base, pattern)
	if err != nil {
		return "", err
	}
	if len(dir) > MaxOutLen {
		os.Remove(dir)
		return "", fmt.Errorf("artifact directory %s is %d bytes, over %d", dir, len(dir), MaxOutLen)
	}
	fsType, err := qualify.FSType(dir)
	if err != nil {
		os.Remove(dir)
		return "", err
	}
	if fsType == "tmpfs" || fsType == "ramfs" {
		os.Remove(dir)
		return "", fmt.Errorf("artifact directory %s is on %s: qualification needs a disk-backed filesystem (real fsync and allocation; the Tier B harness refuses a tmpfs library). Set TMPDIR to a short directory on disk", dir, fsType)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// hygienicEnv is the go test environment: no ZELLIJ*/TMUX*, no inherited
// AREEL_QUALIFY_* (AREEL_QUALIFY_KEEP excepted), a private XDG_RUNTIME_DIR,
// and the qualification opt-in.
func hygienicEnv(out, profile string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(name, "ZELLIJ"), strings.HasPrefix(name, "TMUX"), name == "XDG_RUNTIME_DIR":
			continue
		case strings.HasPrefix(name, "AREEL_QUALIFY_") && name != "AREEL_QUALIFY_KEEP":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "AREEL_QUALIFY_OUT="+out, "AREEL_QUALIFY_PROFILE="+profile, "XDG_RUNTIME_DIR="+filepath.Join(out, "xdg-runtime"))
}

// skipPattern turns named subtests of one top-level test into one
// `go test -skip` pattern, "^Top$/^(A|B)$". go test splits -skip on "/" per
// level, so subtests of different top-level tests cannot share a pattern.
func skipPattern(skips []qualify.NamedSkip) (string, error) {
	var top string
	var subs []string
	for _, s := range skips {
		t, sub, _ := strings.Cut(s.Test, "/")
		if top != "" && t != top {
			return "", fmt.Errorf("--skip: %s and %s are under different top-level tests of one package", top, t)
		}
		top = t
		subs = append(subs, regexp.QuoteMeta(sub))
	}
	return "^" + regexp.QuoteMeta(top) + "$/^(" + strings.Join(subs, "|") + ")$", nil
}

func runPackage(root, out, pkg string, timeout time.Duration, skips []qualify.NamedSkip, env []string) (qualify.HarnessRun, error) {
	name := filepath.Base(pkg)
	logRel := "logs/" + name + ".log"
	args := []string{"test", "-tags", "qualify", "-p", "1", "-count=1", "-timeout", timeout.String(), "-run", "^TestQualify", "-v"}
	var mine []qualify.NamedSkip
	for _, s := range skips {
		top, _, _ := strings.Cut(s.Test, "/")
		if st, ok := qualify.LookupScenarioTest(top); ok && st.Package == pkg {
			mine = append(mine, s)
		}
	}
	if len(mine) > 0 {
		pat, err := skipPattern(mine)
		if err != nil {
			return qualify.HarnessRun{}, err
		}
		args = append(args, "-skip", pat)
	}
	args = append(args, pkg)
	r := qualify.HarnessRun{Package: pkg, Command: "go " + strings.Join(args, " "), Log: logRel, SkippedByName: mine}
	f, err := os.OpenFile(filepath.Join(out, filepath.FromSlash(logRel)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return r, err
	}
	defer f.Close()
	fmt.Fprintf(f, "# %s\n", r.Command)
	fmt.Printf("qualify-run: %s (log %s)\n", r.Command, filepath.Join(out, logRel))
	cmd := exec.Command("go", args...)
	cmd.Dir, cmd.Env = root, env
	w := io.MultiWriter(f, os.Stdout)
	cmd.Stdout, cmd.Stderr = w, w
	start := time.Now()
	err = cmd.Run()
	r.WallSeconds = time.Since(start).Seconds()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		r.ExitCode = exit.ExitCode()
	default:
		return r, err
	}
	fmt.Printf("qualify-run: %s exited %d after %s\n", pkg, r.ExitCode, time.Since(start).Round(time.Second))
	lf, err := os.Open(f.Name())
	if err != nil {
		return r, err
	}
	defer lf.Close()
	return r, qualify.ParseHarnessLog(lf, &r)
}

// validateSkips refuses a --skip whose top-level test is not a scenario test
// of the harness, or whose scenario is not required by the profile or not
// produced by that test: a skip must always name a scenario the report
// shows as skipped.
func validateSkips(skips []qualify.NamedSkip, profile string) error {
	bf, err := qualify.LoadBudgets()
	if err != nil {
		return err
	}
	required := qualify.RequiredScenarios(profile, bf)
	for _, sk := range skips {
		top, _, _ := strings.Cut(sk.Test, "/")
		st, ok := qualify.LookupScenarioTest(top)
		if !ok {
			return fmt.Errorf("--skip %s: %s is not a scenario test of the harness", sk.Test, top)
		}
		if !slices.Contains(required, sk.Scenario) {
			return fmt.Errorf("--skip %s: scenario %s is not required by the %s profile", sk.Test, sk.Scenario, profile)
		}
		if !slices.Contains(st.Scenarios, sk.Scenario) {
			return fmt.Errorf("--skip %s: %s does not produce scenario %s", sk.Test, top, sk.Scenario)
		}
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// printSummary prints report.md up to its budget table: the ROLLOUT line,
// amendments, the automated verdict and blockedOn.
func printSummary(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	head, _, _ := strings.Cut(string(data), "\n## Budgets\n")
	fmt.Println(strings.TrimRight(head, "\n"))
	fmt.Println()
}
