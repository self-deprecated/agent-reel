package reel

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/self-deprecated/agent-reel/internal/transcript"
	"github.com/self-deprecated/agent-reel/internal/transcriptcast"
)

// importBackendName is the backend of recordings made by areel import.
const importBackendName = "import"

// importSpec is what an import recording draws. It travels as the target's
// Address (opaque to core), so the holder, the recorder and recovery all see
// the same, immutable description.
type importSpec struct {
	Source string                  `json:"source"` // absolute path
	Format transcript.Format       `json:"format"`
	Thread string                  `json:"thread,omitempty"` // resolved conversation ID
	Title  string                  `json:"title,omitempty"`
	Follow bool                    `json:"follow,omitempty"`
	Style  transcriptcast.Settings `json:"style"` // unused for an asciicast
	Cols   int                     `json:"cols"`
	Rows   int                     `json:"rows"`
}

func parseImportSpec(t Target) (importSpec, error) {
	var s importSpec
	if t.Backend != importBackendName {
		return s, fmt.Errorf("not an import target")
	}
	if e := json.Unmarshal([]byte(t.Address), &s); e != nil {
		return s, fmt.Errorf("import target: %w", e)
	}
	if !filepath.IsAbs(s.Source) || s.Cols < 1 || s.Rows < 1 {
		return s, fmt.Errorf("import target: invalid source or size")
	}
	return s, nil
}

// Import records an imported conversation or asciicast. Its capture process
// renders the source instead of watching a terminal; everything after the
// FIFO (visual store, holder, seal) is the same as for Zellij.
type Import struct {
	// Self is this executable (the recorder runs `Self _import-feed DIR`).
	Self string
	// Helper overrides the visual/1 readout helper lookup.
	Helper string
}

func NewImport(self string) *Import { return &Import{Self: self} }

func (b *Import) Name() string { return importBackendName }

func (b *Import) Resolve(_ context.Context, name string) (Target, error) {
	return Target{}, fmt.Errorf("import recordings are started by areel import, not by name (%q)", name)
}

func (b *Import) Observe(_ context.Context, t Target) (Observation, error) {
	s, e := parseImportSpec(t)
	if e != nil {
		return Observation{}, e
	}
	return Observation{Geometry: Geometry{Columns: s.Cols, Rows: s.Rows}, GeometrySupport: "fixed", Readiness: "unverified", Visibility: "unknown"}, nil
}

func (b *Import) helper() (string, error) {
	if b.Helper != "" {
		return b.Helper, nil
	}
	return exportExecutable(helperEnv, "areel-avt-checkpoint")
}
