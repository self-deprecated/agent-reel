package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

// Human output formatters. The machine (--json) forms are the JSON shapes the
// commands printed before the CLI overhaul; these only render them.

// stateOutput is the --json form of the lifecycle commands: the recording
// state plus the library it lives in, so an integration can bind to the
// library the CLI actually used.
type stateOutput struct {
	State
	Library     string `json:"library"`
	RecoverHint bool   `json:"recoverHint,omitempty"`
}

func writeJSONLine(out io.Writer, v any) error {
	return json.NewEncoder(out).Encode(v)
}

// printLifecycle prints the result of record/pause/resume/stop/status. A
// state is printed only when one exists: an error never prints an empty state.
func printLifecycle(out io.Writer, command, library string, v State, err error, asJSON bool, templates ...string) error {
	if v.ID == "" && v.Status == "" {
		return err
	}
	if asJSON {
		if e := writeJSONLine(out, stateOutput{State: v, Library: library, RecoverHint: command == "status" && statusRecoverHint(library, v, templates...)}); e != nil {
			return e
		}
		return err
	}
	var e error
	if v.ID == "" {
		_, e = fmt.Fprintf(out, "%s: no recording for %s\n", v.Status, targetLabel(v.Target))
	} else if command == "status" {
		e = formatStatus(out, library, v, time.Now(), templates...)
	} else {
		e = formatStateLine(out, v)
	}
	if e != nil {
		return e
	}
	return err
}

func targetLabel(t Target) string {
	if t.Name == "" {
		return t.Backend
	}
	return t.Backend + ":" + t.Name
}

func formatStateLine(out io.Writer, v State) error {
	line := fmt.Sprintf("%s %s on %s", v.Status, shortID(v.ID), targetLabel(v.Target))
	if n := len(v.Segments); n > 0 {
		line += fmt.Sprintf(" (segment %d)", v.Segments[n-1].Number)
	}
	if v.Pending != "" {
		line += "; pending " + v.Pending
	}
	if _, e := fmt.Fprintln(out, line); e != nil {
		return e
	}
	if v.Error != "" {
		_, e := fmt.Fprintf(out, "error: %s\n", shortDiagnostic(v.Error))
		return e
	}
	return nil
}

func statusRecoverHint(root string, v State, templates ...string) bool {
	if !recoveryStatus(v.Status) {
		return false
	}
	if _, err := os.Lstat(filepath.Join(root, "recordings", v.ID)); os.IsNotExist(err) {
		return false
	}
	if loc, e := libraryLocate(root, v.ID, newLibraryGuard(withSealName(context.Background(), firstSealName(templates)))); e == nil && loc.Kind == "sealed" {
		return false
	}
	return true
}

func formatStatus(out io.Writer, library string, v State, now time.Time, templates ...string) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "recording\t%s (%s)\n", shortID(v.ID), v.ID)
	fmt.Fprintf(w, "target\t%s\n", targetLabel(v.Target))
	status := v.Status
	if v.Pending != "" {
		status += " (pending " + v.Pending + ")"
	}
	if v.Suppressed {
		status += ", suppressed"
	}
	fmt.Fprintf(w, "status\t%s\n", status)
	if at := recordingStarted(v); !at.IsZero() {
		fmt.Fprintf(w, "started\t%s (%s ago)\n", at.Local().Format("2006-01-02 15:04:05"), humanDuration(now.Sub(at)))
	}
	fmt.Fprintf(w, "footage\t%s in %d segment(s)\n", humanDuration(recordingDuration(v, now)), len(v.Segments))
	fmt.Fprintf(w, "storage\t%s\n", listStorage(v.Storage))
	if e := requireVisualFormat(v.Format); e != nil {
		fmt.Fprintf(w, "format\tunsupported: %s\n", e)
	} else {
		fmt.Fprintf(w, "format\t%s\n", v.Format)
	}
	fmt.Fprintf(w, "library\t%s\n", library)
	if v.Error != "" {
		fmt.Fprintf(w, "error\t%s\n", shortDiagnostic(v.Error))
	}
	if statusRecoverHint(library, v, templates...) {
		fmt.Fprintf(w, "run: areel recover %s\n", v.ID)
	}
	return w.Flush()
}

// recordingDuration sums the wall time of launched segments; an open segment
// counts up to now only while the recording is recording.
func recordingDuration(v State, now time.Time) time.Duration {
	var d time.Duration
	for _, s := range v.Segments {
		if s.Started.IsZero() {
			continue
		}
		switch {
		case s.Ended != nil:
			d += s.Ended.Sub(s.Started)
		case v.Status == "recording":
			d += now.Sub(s.Started)
		}
	}
	return max(d, 0)
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// recordingSize is the bounded on-disk size of a recording directory; -1 when
// it cannot be measured within the bound.
func recordingSize(root, id string, templates ...string) int64 {
	g := newLibraryGuard(withSealName(context.Background(), firstSealName(templates)))
	loc, err := libraryLocate(root, id, g)
	if err != nil {
		return -1
	}
	if loc.Kind == "sealed" {
		info, err := os.Lstat(loc.Path)
		if err != nil {
			return -1
		}
		return info.Size()
	}
	const entriesMax = 1 << 16
	var total int64
	entries := 0
	e := filepath.WalkDir(loc.Dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if entries > entriesMax {
			return errors.New("too many entries")
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	if e != nil {
		return -1
	}
	return total
}

// formatList renders one listing page as a table.
func formatList(out io.Writer, library string, r listReport, now time.Time, templates ...string) error {
	if len(r.Items) == 0 {
		if r.After != "" {
			_, e := fmt.Fprintf(out, "no more recordings in %s\n", library)
			return e
		}
		_, e := fmt.Fprintf(out, "no recordings in %s (start one with areel record)\n", library)
		return e
	}
	store := &Store{Root: library, SealName: firstSealName(templates)}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTARGET\tSTATUS\tSTARTED\tDURATION\tSIZE\tSTORAGE")
	for _, row := range listTree(r.Items) {
		item := row.item
		id, target, started, duration, size := "?", "?", "-", "-", "-"
		if item.ID != "" {
			id = row.prefix + shortID(item.ID)
			if v, e := store.read(item.ID); e == nil {
				if at := recordingStarted(v); !at.IsZero() {
					started = at.Local().Format("2006-01-02 15:04")
				}
				duration = humanDuration(recordingDuration(v, now))
			}
			if n := recordingSize(library, item.ID, firstSealName(templates)); n >= 0 {
				size = humanBytes(n)
			}
		}
		if item.Target != nil {
			target = item.Target.Backend + ":" + item.Target.Name
		}
		status := item.Status
		if item.Metadata == "unsupported" {
			status = "unsupported"
		}
		if item.Status == "recording" && item.Holder != "reachable" && item.Holder != "not-checked" {
			status += " (holder " + item.Holder + ")"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", id, target, status, started, duration, size, item.Storage)
	}
	if e := w.Flush(); e != nil {
		return e
	}
	for _, item := range r.Items {
		if item.Reason != "" {
			if _, e := fmt.Fprintf(out, "%s: %s\n", shortID(item.ID), item.Reason); e != nil {
				return e
			}
		}
		if listRecoverHint(item) {
			if _, e := fmt.Fprintf(out, "run: areel recover %s\n", item.ID); e != nil {
				return e
			}
		}
	}
	if r.MoreObserved && r.NextAfter != "" {
		_, e := fmt.Fprintf(out, "more: areel ls --after %s\n", r.NextAfter)
		return e
	}
	return nil
}

// formatConfig renders `areel config`.
func formatConfig(out io.Writer, s *settings) error {
	state := "absent"
	if s.FileExists {
		state = "present"
	}
	file := s.File
	if file == "" {
		file = "(none: HOME and XDG_CONFIG_HOME unset)"
	}
	if _, e := fmt.Fprintf(out, "config file: %s (%s)\n\n", file, state); e != nil {
		return e
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tVALUE\tSOURCE")
	for _, k := range settingKeys {
		value := s.values[k.name]
		if value == "" {
			switch k.name {
			case "backend":
				value = "(unset: library config or caller context)"
			case "target":
				value = "(unset: caller context)"
			case "export.directory":
				value = "(unset: " + exportDirectoryHelp + ")"
			default:
				value = "(unset)"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", k.name, value, s.sources[k.name])
	}
	return w.Flush()
}

// formatOffline renders an offline doctor report.
func formatOffline(out io.Writer, v offlineVisualReport) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "recording\t%s (%s)\n", shortID(v.Recording), v.Format)
	if v.Sealed != nil {
		fmt.Fprintf(w, "sealed\t%s (%s; %s)\n", v.Sealed.Path, v.Sealed.Kind, v.Sealed.Status)
		if v.Sealed.FailedItem != "" {
			fmt.Fprintf(w, "failed\t%s: %s\n", v.Sealed.FailedItem, shortDiagnostic(v.Sealed.Reason))
		}
	}
	fmt.Fprintf(w, "status\t%s\n", v.PersistedStatus)
	fmt.Fprintf(w, "diagnosis\t%s\n", v.Diagnosis)
	fmt.Fprintf(w, "store\t%s\n", v.ArtifactStatus)
	if v.Acknowledged != nil {
		fmt.Fprintf(w, "acknowledged\t%d views (complete: %t); %d unacknowledged\n", v.Acknowledged.Views, v.Acknowledged.Complete, v.UnacknowledgedViews)
	}
	if v.Completion != nil {
		fmt.Fprintf(w, "completion\t%s\n", v.Completion.Reason)
	}
	if v.Activity != nil {
		fmt.Fprintf(w, "activity\t%s (%d rows)\n", v.Activity.Status, v.Activity.Count)
	}
	if v.Salvage != nil {
		fmt.Fprintf(w, "salvage\twritten\n")
	}
	if v.PersistedError != "" {
		fmt.Fprintf(w, "error\t%s\n", shortDiagnostic(v.PersistedError))
	}
	if v.RecoverHint {
		fmt.Fprintf(w, "run: areel recover %s\n", v.Recording)
	}
	fmt.Fprintf(w, "details\tareel doctor ... --json\n")
	return w.Flush()
}

// doctorCheck is one line of the environment doctor.
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | missing | mismatch | unvalidated | not-ready | ...
	Detail string `json:"detail"`
}
type doctorEnvironment struct {
	Platform string        `json:"platform"`
	Capture  string        `json:"capture"` // ready | not-ready
	Checks   []doctorCheck `json:"checks"`
}

func formatDoctor(out io.Writer, d doctorEnvironment) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, c := range d.Checks {
		fmt.Fprintf(w, "%s\t%s\t%s\n", c.Name, c.Status, c.Detail)
	}
	if e := w.Flush(); e != nil {
		return e
	}
	summary := "capture prerequisites: ready; no session attached; first-frame readiness remains unverified"
	if d.Capture != "ready" {
		summary = "capture prerequisites: not ready (see above)"
	}
	_, e := fmt.Fprintln(out, summary)
	return e
}

// exportTimestamp is the local time used in default export names.
func exportTimestamp(t time.Time) string { return t.Local().Format("20060102-1504") }

func pathLabel(p string) string {
	if home, e := os.UserHomeDir(); e == nil && home != "" && strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}
