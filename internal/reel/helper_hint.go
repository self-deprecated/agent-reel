package reel

import (
	"errors"
	"os"

	"github.com/self-deprecated/agent-reel/internal/castexport"
)

// helperEnv overrides the visual/1 readout helper otherwise found on PATH.
const helperEnv = "AREEL_AVT_CHECKPOINT"

// helperHint adds where a mismatched checkpoint helper came from: a stale
// AREEL_AVT_CHECKPOINT (for example exported by an older dev shell)
// silently overrides the helper bundled with an installed areel.
func (z *Zellij) helperHint(err error) error { return helperHintFor(z.Helper, err) }

// helperHintFor is helperHint for a backend whose explicit helper override is
// override.
func helperHintFor(override string, err error) error {
	if override != "" {
		return err
	}
	return annotateHelperMismatch(err, os.Getenv(helperEnv))
}

// annotateHelperMismatch returns the self-describing mismatch itself, with the
// environment hint, in place of err when the variable selected the helper.
// Wrapping messages are fixed when created, so the hint cannot be added in
// place underneath them.
func annotateHelperMismatch(err error, fromEnv string) error {
	var mismatch *castexport.HelperMismatchError
	if fromEnv == "" || !errors.As(err, &mismatch) || mismatch.Hint != "" {
		return err
	}
	hinted := *mismatch
	hinted.Hint = helperEnv + " points at it (a dev shell may set it); unset " + helperEnv + " or rebuild that helper"
	return &hinted
}
