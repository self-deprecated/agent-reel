package reel

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Recording arguments (REC): a full 32-hex ID, a unique prefix of at least 4
// hex characters, "@" (the recording bound to the current target, resolved by
// the caller's context as `areel status` does) or "latest" (the most recently
// started recording). Resolution only reads the library.

const (
	recCurrent       = "@"
	recLatest        = "latest"
	recPrefixMin     = 4
	recScanMax       = 1 << 16 // recording directories inspected by one resolution
	recCandidatesMax = 8
)

var recPrefix = regexp.MustCompile(`^[a-f0-9]{4,31}$`)

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// checkREC validates the syntax of a REC argument.
func checkREC(arg string) error {
	if arg == recCurrent || arg == recLatest || validID.MatchString(arg) || recPrefix.MatchString(arg) {
		return nil
	}
	return usageErr("REC must be a recording ID, a unique prefix of at least %d hex characters, @ or latest (got %q)", recPrefixMin, arg)
}

// resolveREC resolves REC to a full recording ID in library root. current is
// called for "@" only.
func resolveREC(root, arg string, current func() (string, error), templates ...string) (string, error) {
	if e := checkREC(arg); e != nil {
		return "", e
	}
	switch {
	case validID.MatchString(arg):
		return arg, nil
	case arg == recCurrent:
		if current == nil {
			return "", fmt.Errorf("@ is not available here; pass a recording ID")
		}
		return current()
	}
	ids, e := recordingIDs(root, templates...)
	if e != nil {
		return "", e
	}
	if arg == recLatest {
		return latestRecording(root, ids, templates...)
	}
	var matches []string
	for _, id := range ids {
		if strings.HasPrefix(id, arg) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no recording matches %q in %s (see areel ls)", arg, root)
	case 1:
		return matches[0], nil
	}
	shown := matches
	if len(shown) > recCandidatesMax {
		shown = shown[:recCandidatesMax]
	}
	more := ""
	if len(matches) > len(shown) {
		more = fmt.Sprintf(" and %d more", len(matches)-len(shown))
	}
	return "", usageErr("recording prefix %q is ambiguous: %s%s; use more characters", arg, strings.Join(shown, ", "), more)
}

// recordingIDs lists the recording directory names of a library, sorted.
func firstSealName(templates []string) string {
	if len(templates) > 0 && templates[0] != "" {
		return templates[0]
	}
	return "{id}"
}

func recordingIDs(root string, templates ...string) ([]string, error) {
	g := newLibraryGuard(withSealName(context.Background(), firstSealName(templates)))
	names, _, _, e := libraryNames(root, g, recScanMax)
	if e != nil {
		if os.IsNotExist(e) {
			return nil, nil
		}
		return nil, e
	}
	var ids []string
	for id := range names {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// recordingStarted is when a recording started: its first segment, else its
// first lifecycle event. Zero when unknown.
func recordingStarted(v State) time.Time {
	var t time.Time
	for _, s := range v.Segments {
		if !s.Started.IsZero() && (t.IsZero() || s.Started.Before(t)) {
			t = s.Started
		}
	}
	if t.IsZero() {
		for _, ev := range v.Events {
			if !ev.At.IsZero() && (t.IsZero() || ev.At.Before(t)) {
				t = ev.At
			}
		}
	}
	return t
}

func latestRecording(root string, ids []string, templates ...string) (string, error) {
	store := &Store{Root: root, SealName: firstSealName(templates)}
	best, bestAt := "", time.Time{}
	for _, id := range ids {
		v, e := store.read(id)
		if e != nil {
			continue
		}
		at := recordingStarted(v)
		if best == "" || at.After(bestAt) {
			best, bestAt = id, at
		}
	}
	if best == "" {
		return "", fmt.Errorf("no recordings in %s", root)
	}
	return best, nil
}

// currentRecording resolves "@" read-only: the backend target of the caller's
// context (or the configured target) and its target index entry.
func currentRecording(root string, s *settings) func() (string, error) {
	return func() (string, error) {
		z := NewZellij()
		selection := contextSelection(s, "")
		if selection.Configured == "" {
			if config, e := readLibraryConfig(root); e == nil {
				selection.Configured = config.Backend
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		t, e := Resolve(ctx, selection, map[string]Backend{"zellij": z})
		if e != nil {
			return "", fmt.Errorf("@: %w", e)
		}
		var id string
		if e = readJSON((&Store{Root: root}).targetPath(t), &id); e != nil {
			if os.IsNotExist(e) {
				return "", fmt.Errorf("@: no recording for target %s", t.Name)
			}
			return "", fmt.Errorf("@: %w", e)
		}
		if !validID.MatchString(id) {
			return "", fmt.Errorf("@: invalid target index for %s", t.Name)
		}
		return id, nil
	}
}

// contextSelection builds the backend selection of lifecycle commands: an
// explicit TARGET, else the caller's context, else the configured target.
func contextSelection(s *settings, target string) Selection {
	env := map[string]string{"ZELLIJ_SESSION_NAME": os.Getenv("ZELLIJ_SESSION_NAME"), "TMUX": os.Getenv("TMUX")}
	if target == "" && env["ZELLIJ_SESSION_NAME"] == "" && env["TMUX"] == "" {
		target = s.Target
	}
	// A --backend flag is an explicit assertion (it conflicts with a recording
	// binding of another backend); AREEL_BACKEND and the config file only
	// configure the default, which outranks the library config.
	explicit, configured := "", s.Backend
	if s.source("backend") == sourceFlag {
		explicit, configured = s.Backend, ""
	}
	return Selection{ExplicitBackend: explicit, Configured: configured, Target: target, Environment: env}
}
