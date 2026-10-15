package reel

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// errVisualFormat reports a recording that is not visual/1 where a visual
// reader was asked for. It is a refusal, never a fallback to another reader.
var errVisualFormat = errors.New("recording is not visual/1")

// errLegacyRecording refuses a recording in the removed legacy raw format
// (State.Format ""). Such a directory is listed as unsupported and left
// untouched; nothing opens, exports, records into, recovers or deletes it.
var errLegacyRecording = errors.New("legacy recording: the raw recording format has been removed and is not supported")

// requireVisualFormat accepts only visual/1, the one recording format.
func requireVisualFormat(format string) error {
	switch format {
	case activityFormatIndexed:
		return nil
	case "":
		return errLegacyRecording
	}
	return fmt.Errorf("%w: %q", errVisualFormat, libraryText(format, 64))
}

// libraryVisualReader opens a visual/1 recording's store read-only for one
// library request. The library root, the recordings directory, the recording
// directory and the store directory are all guarded as growing: a live capture
// legitimately rewrites the acknowledged record and the store's WAL while a
// reader works, so only their identity is pinned, never their mtime. state.json
// and launch.json are read under the guard as for every other reader. The
// store's own identity must name this recording and its target, exactly as the
// visual doctor checks it. The caller owns Close.
func libraryVisualReader(ctx context.Context, root, id string, g *libraryGuard) (*visualstore.Reader, State, error) {
	if !validID.MatchString(id) {
		return nil, State{}, fmt.Errorf("invalid recording")
	}
	loc, e := libraryLocate(root, id, g)
	if e != nil {
		return nil, State{}, e
	}
	dir := loc.Dir
	if loc.Kind == "dir" {
		for _, p := range []string{root, filepath.Join(root, "recordings"), dir} {
			if e := g.growing(p); e != nil {
				return nil, State{}, e
			}
		}
	}
	state, _, e := libraryState(root, id, g)
	if e != nil {
		return nil, State{}, e
	}
	if state.Format != activityFormatIndexed {
		return nil, State{}, errVisualFormat
	}
	var r *visualstore.Reader
	if e = ctx.Err(); e != nil {
		return nil, State{}, e
	}
	if loc.Kind == "sealed" {
		r, e = visualstore.OpenSealed(loc.Path)
	} else {
		storeDir := filepath.Join(dir, visualcapture.StoreDirName)
		if e = g.growing(storeDir); e != nil {
			return nil, State{}, e
		}
		r, e = visualstore.OpenReader(storeDir)
	}
	if e != nil {
		return nil, State{}, e
	}
	identity := r.Identity()
	if identity.Recording != state.ID || identity.Backend != state.Target.Backend || identity.Instance != state.Target.Instance {
		return nil, State{}, errors.Join(fmt.Errorf("visual store identity mismatch"), r.Close())
	}
	return r, state, nil
}
