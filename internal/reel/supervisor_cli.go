package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// `areel service supervisor` is the bounded entrypoint the packaged user service runs:
// it loads one explicit allowlist file, binds the supervisor state machine to
// one explicit private library through NewStoreController and runs cycles
// until SIGTERM/SIGINT. It adds no verb to the supervisor contract: nothing
// here pauses, stops, resumes, discovers ambiently or activates anything.
//
// Discovery is the qualification boundary. supervisorBackends lists the
// discovery backends this build can bind, and it is empty: the Zellij adapter
// implements no Discover (SUPERVISOR.md), so an allowlist naming any backend
// is refused at startup as a configuration error until a later task adds and
// qualifies real discovery. The packaged service therefore cannot capture yet;
// it can only be built, validated and refused truthfully.
const (
	supervisorReportName = "supervisor.json"
	supervisorAllowMax   = 64 << 10
	launchDetached       = "detached"
	launchSystemdRun     = "systemd-run"
)

// configError marks a refused configuration. The service must not restart on
// it; ExitCode maps it to status 2, which the unit lists as restart-preventing.
type configError struct{ err error }

func (e *configError) Error() string { return "configuration: " + e.err.Error() }
func (e *configError) Unwrap() error { return e.err }
func configErr(format string, args ...any) error {
	return &configError{fmt.Errorf(format, args...)}
}

// ExitCode maps a CLI error to the process exit status: 0 for none, 2 for a
// refused configuration or a usage error, 1 for everything else.
func ExitCode(err error) int {
	var c *configError
	var u *usageError
	var status *ExitStatus
	switch {
	case err == nil:
		return 0
	case errors.As(err, &status):
		return status.Code
	case errors.As(err, &c), errors.As(err, &u):
		return 2
	}
	return 1
}

// supervisorBackends returns the discovery backends the production build can
// bind. Empty until a backend implements and qualifies Discover.
func supervisorBackends() []SupervisedBackend { return nil }

// readAllowlistFile reads a bounded explicit allowlist. Unlike library state
// the allowlist is declarative configuration that names only backends and
// target names, so it may live in the world-readable Nix store and be reached
// through symlinks; it must still be a regular file of at most 64KiB.
func readAllowlistFile(path string) ([]AllowlistEntry, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("allowlist must be a regular file")
	}
	if st.Size() > supervisorAllowMax {
		return nil, fmt.Errorf("allowlist exceeds 64KiB")
	}
	raw, e := io.ReadAll(io.LimitReader(f, supervisorAllowMax+1))
	if e != nil {
		return nil, e
	}
	return ParseAllowlist(raw)
}

// supervisorDeps are the process-level bindings of the command: discovery
// backends, the holder executable and an optional hook applied to the opened
// store. Production uses supervisorProduction; tests inject fakes so that no
// subprocess, capture tool or service is involved.
type supervisorDeps struct {
	backends []SupervisedBackend
	self     func() (string, error)
	bind     func(*Store)
}

func supervisorProduction() supervisorDeps {
	return supervisorDeps{backends: supervisorBackends(), self: os.Executable}
}

func supervisorCLI(args []string, out, errOut io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return supervisorCommand(ctx, args, out, errOut, supervisorProduction())
}

var supervisorHelp = cliCommand{Name: "service supervisor",
	Summary: `Integration: the user-service entrypoint of SUPERVISOR.md. Loads the
supervisor.allowlist file ({"entries":[{"backend":..,"target":..}]}, 1..64
targets), binds the library and reconciles the allowlisted targets through the
ordinary ensure control every supervisor.interval. It never pauses, stops or
resumes. A refused configuration exits 2, which the unit must not restart.
This build has no discovery backend, so every allowlist entry is refused.`,
	Examples: []string{"areel service supervisor", "areel service supervisor --once", "areel service supervisor --runtime $XDG_RUNTIME_DIR/agent-reel"},
	Docs:     "SUPERVISOR.md"}

func supervisorCommand(ctx context.Context, args []string, out, errOut io.Writer, deps supervisorDeps) error {
	f := newCLIFlags(supervisorHelp, out)
	f.library()
	f.setting("allowlist", "supervisor.allowlist", "FILE", "allowlist file (default: supervisor.allowlist)")
	runtime := f.String("runtime", "DIR", "", "existing private directory for the supervisor.json cycle report")
	f.setting("interval", "supervisor.interval", "DURATION", "polling period, 100ms..60s (default: supervisor.interval, 5s)")
	f.setting("launch", "supervisor.launch", "MODE", "holder launch: detached or systemd-run (default: supervisor.launch)")
	once := f.Bool("once", false, "run one cycle, print its report and exit without serving")
	_, help, e := f.parse(args[1:], 0, 0)
	if help || e != nil {
		return e
	}
	cfg, e := f.load(nil)
	if e != nil {
		return e
	}
	if cfg.Library == "" {
		return configErr("no library configured: set library in your config or pass --library")
	}
	if cfg.SupervisorAllowlist == "" {
		return configErr("supervisor.allowlist is not set: set it in your config or pass --allowlist; the supervisor is enabled only by an explicit allowlist")
	}
	allowlist, launch := &cfg.SupervisorAllowlist, &cfg.SupervisorLaunch
	interval := &cfg.SupervisorInterval
	entries, e := readAllowlistFile(*allowlist)
	if e != nil {
		return &configError{fmt.Errorf("allowlist %s: %w", *allowlist, e)}
	}
	if len(entries) == 0 {
		return configErr("allowlist %s has no entries; the supervisor is enabled only by a non-empty explicit allowlist", *allowlist)
	}
	limits := SupervisorDefaults()
	limits.Interval = *interval
	if limits.BackoffMax < limits.Interval {
		limits.BackoffMax = limits.Interval
	}
	if e = limits.validate(); e != nil {
		return &configError{e}
	}
	var report string
	if *runtime != "" {
		dir, e := offlineAbsolute(*runtime)
		if e != nil {
			return &configError{e}
		}
		if e = existingPrivateDirectory(dir); e != nil {
			return configErr("runtime directory unavailable: existing explicit private directory required")
		}
		report = filepath.Join(dir, supervisorReportName)
	}
	library, e := creatableLibrary(cfg)
	if e != nil {
		return &configError{e}
	}
	store, e := OpenStore(library)
	if e != nil {
		return e
	}
	store.SealName = cfg.SealName
	if report != "" && (report == store.Root || filepath.Dir(report) == store.Root) {
		return configErr("runtime directory must be outside the library")
	}
	self, e := deps.self()
	if e != nil {
		return e
	}
	if *launch == launchSystemdRun {
		store.Launch = transientHolderLauncher(self, nil)
	}
	if deps.bind != nil {
		deps.bind(store)
	}
	s, e := NewSupervisor(entries, deps.backends, NewStoreController(store, self), limits, nil)
	if e != nil {
		return &configError{e}
	}
	run := &supervisorRun{supervisor: s, report: report, log: errOut}
	if *once {
		r := s.Cycle(ctx)
		e = run.publish(r)
		return errors.Join(json.NewEncoder(out).Encode(r), e)
	}
	fmt.Fprintf(errOut, "supervisor: %d allowlist entries, %d discovery backends, holders %s, interval %s\n", len(entries), len(deps.backends), *launch, limits.Interval)
	return run.serve(ctx)
}

// supervisorRun repeats cycles on the period, logs each entry's change of
// action, diagnostic or recording status to the journal (stderr) and, when a
// runtime directory was given, replaces RUNTIME/supervisor.json atomically
// (0600) with the last completed report. The report is the bounded privacy-safe
// projection of SUPERVISOR.md: no addresses, paths, process identity, segments,
// events or unlisted target names.
type supervisorRun struct {
	supervisor *Supervisor
	report     string
	log        io.Writer
	last       map[AllowlistEntry]string
	backends   map[string]string
}

func (r *supervisorRun) serve(ctx context.Context) error {
	for ctx.Err() == nil {
		report := r.supervisor.Cycle(ctx)
		if e := r.publish(report); e != nil {
			fmt.Fprintf(r.log, "supervisor: report: %v\n", e)
		}
		timer := time.NewTimer(r.supervisor.limits.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	fmt.Fprintln(r.log, "supervisor: stopping; holders keep recording and the next run adopts them")
	return nil
}

func (r *supervisorRun) publish(report SupervisorReport) error {
	r.logChanges(report)
	if r.report == "" {
		return nil
	}
	return writeJSON(r.report, report)
}

// logChanges prints one bounded line per backend or entry whose outcome
// differs from the previous cycle, so a steady state is silent.
func (r *supervisorRun) logChanges(report SupervisorReport) {
	if r.last == nil {
		r.last, r.backends = map[AllowlistEntry]string{}, map[string]string{}
	}
	for _, b := range report.Backends {
		line := b.Discovery
		if b.Error != "" {
			line += ": " + b.Error
		}
		if r.backends[b.Name] != line {
			r.backends[b.Name] = line
			fmt.Fprintf(r.log, "supervisor: backend %s: discovery %s\n", b.Name, line)
		}
	}
	for _, e := range report.Entries {
		line := e.Presence + " " + e.Action
		if e.Recording != nil {
			line += " recording " + e.Recording.Status
			if e.Recording.HolderLoss {
				line += " (holder loss)"
			}
		}
		if e.Error != "" {
			line += ": " + e.Error
		}
		key := AllowlistEntry{e.Backend, e.Target}
		if r.last[key] != line {
			r.last[key] = line
			fmt.Fprintf(r.log, "supervisor: %s/%s: %s\n", e.Backend, e.Target, line)
		}
	}
}
