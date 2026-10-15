package reel

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Target selection is backend-qualified. Instance and Address are opaque to core.
// Instance need not attest a server generation (Zellij uses namespace+name).
// Name alone must never be used as a deduplication/suppression key.
type Target struct {
	Backend  string `json:"backend"`
	Instance string `json:"instance"`
	Name     string `json:"name"`
	Address  string `json:"address"`
}

func (t Target) Equal(u Target) bool { return t.Backend == u.Backend && t.Instance == u.Instance }

type Geometry struct {
	Columns int `json:"columns"`
	Rows    int `json:"rows"`
}
type Observation struct {
	At              time.Time `json:"at"`
	Geometry        Geometry  `json:"geometry"`
	GeometrySupport string    `json:"geometrySupport"`
	Readiness       string    `json:"readiness"`
	OutputObserved  bool      `json:"outputObserved"`
	Visibility      string    `json:"visibility"`
	Clients         int       `json:"clients"`
	Uncertainty     string    `json:"uncertainty"`
	// Follow is the capture's account of following the target's effective
	// geometry during the segment. Absent for captures that do not follow
	// (older recordings, synthetic observers).
	Follow *GeometryFollow `json:"follow,omitempty"`
}
type Observer interface {
	Observation() Observation
	// A caller deadline bounds waiting, not ownership. Actual terminal/sink
	// failures belong in Err; completion is proved only by Done.
	Stop(context.Context) error
	Done() <-chan struct{}
	// Err may report archive failure before Done; ownership lasts until Done.
	Err() error
}

// CaptureSpec is normalized by the holder; store identity never discovers sessions.
// Format is the recording's State.Format, always "visual/1": the capture goes
// into the recording's visual store (visualcapture). Backends refuse any other
// value.
type CaptureSpec struct {
	Directory, RecordingID string
	Segment                int
	Format                 string
}

type Backend interface {
	Name() string
	Resolve(context.Context, string) (Target, error)
	Observe(context.Context, Target) (Observation, error)
	// Start returns nil only when no capture process was launched. Once a
	// process exists it MUST return its observer, including on startup failure,
	// so core cannot mistake an interrupted launch for a safe refusal to retry.
	Start(context.Context, Target, CaptureSpec) (Observer, error)
}

type Selection struct {
	Configured, ExplicitBackend, Target string
	Environment                         map[string]string
	Binding                             *Target
}

// validateBinding checks explicit CLI contradictions, never incidental caller context.
func (s Selection) validateBinding() error {
	if s.Binding != nil && ((s.ExplicitBackend != "" && s.ExplicitBackend != s.Binding.Backend) || (s.Target != "" && s.Target != s.Binding.Name)) {
		return fmt.Errorf("explicit target/backend conflicts with recording binding")
	}
	return nil
}

// Resolve is used only by the CLI. Integrations supply recording IDs, not backend discovery.
func Resolve(ctx context.Context, s Selection, backends map[string]Backend) (Target, error) {
	if err := s.validateBinding(); err != nil {
		return Target{}, err
	}
	backend := s.ExplicitBackend
	if backend == "" && s.Binding != nil {
		backend = s.Binding.Backend
	}
	if backend == "" {
		backend = s.Configured
	}
	if backend == "" {
		z, tm := s.Environment["ZELLIJ_SESSION_NAME"] != "", s.Environment["TMUX"] != ""
		if z && tm {
			return Target{}, fmt.Errorf("ambiguous nested environment: configure AREEL_BACKEND or --backend")
		}
		if z {
			backend = "zellij"
		}
		if tm {
			backend = "tmux"
		}
	}
	if backend == "" {
		return Target{}, fmt.Errorf("no backend selected: configure AREEL_BACKEND or --backend")
	}
	b, ok := backends[backend]
	if !ok {
		return Target{}, fmt.Errorf("backend %q unsupported (only Zellij is implemented)", backend)
	}
	name := s.Target
	if name == "" && s.Binding != nil {
		name = s.Binding.Name
	}
	if name == "" && backend == "zellij" {
		name = s.Environment["ZELLIJ_SESSION_NAME"]
	}
	if name == "" {
		return Target{}, fmt.Errorf("no capture target: use --target or a backend caller environment")
	}
	t, err := b.Resolve(ctx, name)
	if err != nil {
		return t, err
	}
	if s.Binding != nil && !t.Equal(*s.Binding) {
		return Target{}, fmt.Errorf("stale or mismatched recording binding")
	}
	return t, nil
}

// shutdownPending distinguishes a control wait deadline from a terminal/sink
// failure. Err() on the observer remains authoritative for actual failure.
type shutdownPending struct{ cause error }

func (e *shutdownPending) Error() string {
	return "owned observer finalization pending: " + e.cause.Error()
}
func (e *shutdownPending) Unwrap() error { return e.cause }
func isShutdownPending(ctx context.Context, err error) bool {
	var pending *shutdownPending
	return errors.As(err, &pending) || (ctx.Err() != nil && err == ctx.Err())
}
func waitForShutdown(ctx context.Context, o Observer) error {
	select {
	case <-o.Done():
		return o.Err()
	case <-ctx.Done():
		if err := o.Err(); err != nil {
			return err
		}
		return &shutdownPending{ctx.Err()}
	}
}
