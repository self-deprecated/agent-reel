//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// writeLegacyRecording leaves a recording directory in the removed legacy raw
// format in the store's library, exactly as older binaries wrote it: a
// state.json without a format member, launch.json, the target index entry, a
// raw segment archive directory and an activity sidecar. Nothing reads its
// contents; they only prove every command leaves it untouched.
func writeLegacyRecording(t *testing.T, store *Store, target Target, status string) (State, string) {
	t.Helper()
	id := newID()
	dir, _ := store.dir(id)
	for _, d := range []string{filepath.Join(store.Root, "recordings"), filepath.Join(store.Root, "targets"), dir, filepath.Join(dir, "segment-0001"), filepath.Join(dir, "segment-0001", "archive"), filepath.Join(dir, "activity")} {
		if e := privateDir(d); e != nil {
			t.Fatal(e)
		}
	}
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ended := started.Add(time.Minute)
	state := State{ID: id, Target: target, Status: status, Segments: []Segment{{Number: 1, Path: "segment-0001/archive", Launched: true, Started: started, Ended: &ended}}, Events: []Event{}}
	if status == "paused" || status == "stopped" {
		state.Suppressed = true
	}
	if e := writeJSON(filepath.Join(dir, "state.json"), state); e != nil {
		t.Fatal(e)
	}
	if raw, e := os.ReadFile(filepath.Join(dir, "state.json")); e != nil || bytes.Contains(raw, []byte(`"format"`)) {
		t.Fatalf("legacy fixture carries a format: %s %v", raw, e)
	}
	if e := writeJSON(filepath.Join(dir, "launch.json"), launchSpec{Target: target, Token: newID()}); e != nil {
		t.Fatal(e)
	}
	if e := writeJSON(store.targetPath(target), id); e != nil {
		t.Fatal(e)
	}
	for path, data := range map[string]string{
		filepath.Join(dir, "segment-0001", "archive", "archive.json"): `{"identity":{}}`,
		filepath.Join(dir, "segment-0001", "archive", "000000.zst"):   "opaque raw frame",
		filepath.Join(dir, "activity", "000001.json"):                 `{"recording":"legacy"}`,
	} {
		if e := os.WriteFile(path, []byte(data), 0600); e != nil {
			t.Fatal(e)
		}
	}
	return state, dir
}

func TestLibraryConfigFormat(t *testing.T) {
	store := testStore(t)
	path := filepath.Join(store.Root, "config.json")
	for _, bad := range []string{`{"format":""}`, `{"format":"legacy"}`, `{"format":"visual/2"}`, `{"format":1}`, `{"format":null,"extra":1}`} {
		if e := os.WriteFile(path, []byte(bad), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := readLibraryConfig(store.Root); e == nil || !strings.Contains(e.Error(), "library config") {
			t.Fatalf("config %s accepted: %v", bad, e)
		}
		if _, e := store.create(fakeTarget("refused")); e == nil {
			t.Fatalf("recording created under config %s", bad)
		}
	}
	for _, good := range []string{`{}`, `{"format":"visual/1"}`, `{"format":null}`, `{"backend":"zellij","format":"visual/1"}`} {
		if e := os.WriteFile(path, []byte(good), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := readLibraryConfig(store.Root); e != nil {
			t.Fatalf("config %s refused: %v", good, e)
		}
	}
	// Without any config file, and with each accepted file, every new
	// recording is visual/1.
	for i, config := range []string{"", `{}`, `{"format":"visual/1"}`} {
		if config == "" {
			_ = os.Remove(path)
		} else if e := os.WriteFile(path, []byte(config), 0600); e != nil {
			t.Fatal(e)
		}
		state, e := store.create(fakeTarget("created-" + string(rune('a'+i))))
		if e != nil || state.Format != activityFormatIndexed {
			t.Fatalf("config %q created %q: %v", config, state.Format, e)
		}
		persisted, e := store.read(state.ID)
		if e != nil || persisted.Format != activityFormatIndexed {
			t.Fatalf("persisted format %q: %v", persisted.Format, e)
		}
	}
}

func TestLegacyRecordingListedUnsupported(t *testing.T) {
	store := testStore(t)
	legacy, dir := writeLegacyRecording(t, store, listTarget("legacy"), "stopped")
	before := activityTree(t, dir)
	report, e := listRecordings(context.Background(), store.Root, "", 8, listDefaults())
	if e != nil || len(report.Items) != 1 {
		t.Fatalf("listing %+v %v", report, e)
	}
	row := report.Items[0]
	if row.ID != legacy.ID || row.Metadata != "unsupported" || !strings.Contains(row.Reason, "legacy recording") || row.Holder != "not-checked" || listRecoverHint(row) {
		t.Fatalf("legacy row %+v", row)
	}
	var out bytes.Buffer
	if e = formatList(&out, store.Root, report, time.Now()); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "unsupported") || !strings.Contains(out.String(), shortID(legacy.ID)+": legacy recording") || strings.Contains(out.String(), "areel recover") {
		t.Fatalf("ls output:\n%s", out.String())
	}
	page, e := libraryList(context.Background(), store.Root, "", 8)
	if e != nil || len(page.Items) != 1 || page.Items[0].Metadata != "unsupported" || !strings.Contains(page.Items[0].Reason, "legacy recording") || page.Items[0].Format != "" {
		t.Fatalf("library page %+v %v", page, e)
	}
	card := libraryView(page.Items[0])
	if card.CardPreviewURL() != "" || card.Status() != "unsupported" || !strings.Contains(card.CardPreviewCaption(), "legacy recording") {
		t.Fatalf("card %q %q %q", card.CardPreviewURL(), card.Status(), card.CardPreviewCaption())
	}
	h, e := newLibraryHandler(store.Root, libraryTestHost)
	if e != nil {
		t.Fatal(e)
	}
	defer h.close()
	if w := libraryRequest(h, "/", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "legacy recording") {
		t.Fatalf("catalog %d %s", w.Code, w.Body)
	}
	if !reflect.DeepEqual(before, activityTree(t, dir)) {
		t.Fatal("listing touched the legacy recording")
	}
}

func TestLegacyRecordingRefusedEverywhere(t *testing.T) {
	store := testStore(t)
	legacy, dir := writeLegacyRecording(t, store, fakeTarget("legacy-refused"), "stopped")
	before := activityTree(t, dir)
	ctx := context.Background()
	refused := func(what string, e error) {
		t.Helper()
		if !errors.Is(e, errLegacyRecording) {
			t.Fatalf("%s: %v", what, e)
		}
	}
	_, _, e := libraryState(store.Root, legacy.ID, newLibraryGuard(ctx))
	refused("library state", e)
	_, e = libraryDetail(ctx, store.Root, legacy.ID)
	refused("library detail", e)
	_, e = listActivity(store.Root, legacy.ID, 0, 8, activityListOutputDefault, nil)
	refused("activity listing", e)
	_, e = libraryMarks(ctx, store.Root, legacy.ID, 0, 0, 8, libraryOutputMax)
	refused("marks", e)
	_, e = buildExport(ctx, store.Root, exportTestSpec(legacy.ID), t.TempDir())
	refused("export", e)
	_, e = offlineRecording(ctx, store.Root, legacy.ID, "", visualstore.DefaultLimits(), absentObservation())
	refused("doctor", e)
	_, e = recoverRecording(ctx, store.Root, legacy.ID, recoverOptions{Limits: visualstore.DefaultLimits()}, absentObservation())
	refused("recover", e)
	refused("export CLI", CLI([]string{"export", "--library", store.Root, legacy.ID, filepath.Join(t.TempDir(), "out.zip")}, io.Discard, io.Discard))
	refused("doctor CLI", CLI([]string{"doctor", "--library", store.Root, legacy.ID}, io.Discard, io.Discard))
	refused("activity ls CLI", CLI([]string{"activity", "ls", "--library", store.Root, legacy.ID}, io.Discard, io.Discard))

	h, e := newLibraryHandler(store.Root, libraryTestHost)
	if e != nil {
		t.Fatal(e)
	}
	defer h.close()
	for _, path := range []string{"/api/recordings/" + legacy.ID, "/api/recordings/" + legacy.ID + "/activity", "/api/recordings/" + legacy.ID + "/marks", "/api/recordings/" + legacy.ID + "/segments/1/preview", "/api/recordings/" + legacy.ID + "/preview-durable", "/recordings/" + legacy.ID, "/recordings/" + legacy.ID + "?mode=history&segment=1&at=0", "/activity/" + legacy.ID} {
		if w := libraryRequest(h, path, nil); w.Code == 200 || strings.Contains(w.Body.String(), "opaque raw frame") {
			t.Fatalf("%s served a legacy recording: %d %s", path, w.Code, w.Body)
		}
	}
	for _, path := range []string{"/api/recordings/" + legacy.ID + "/segments/1/cast", "/api/recordings/" + legacy.ID + "/segments/1/frames"} {
		if w := libraryRequest(h, path, nil); w.Code != 400 {
			t.Fatalf("%s: raw route still exists: %d", path, w.Code)
		}
	}
	exports, e := newLibraryExports(store.Root, libraryTestHost)
	if e != nil {
		t.Fatal(e)
	}
	defer exports.Close()
	if w := exportsRequest(exports, "GET", "/exports/new?recording="+legacy.ID, "", nil); w.Code != 400 {
		t.Fatalf("export form for a legacy recording: %d", w.Code)
	}
	if w := exportsRequest(exports, "POST", "/exports", "recording="+legacy.ID, nil); w.Code != 400 {
		t.Fatalf("export job for a legacy recording: %d", w.Code)
	}
	// A holder never runs for a legacy recording, whatever its status.
	var state State
	if e = readJSON(filepath.Join(dir, "state.json"), &state); e != nil {
		t.Fatal(e)
	}
	state.Status = "starting"
	if e = store.save(state); e != nil {
		t.Fatal(e)
	}
	refused("holder", Hold(store.Root, legacy.ID, &fakeBackend{target: legacy.Target}))
	if _, e = os.Stat(filepath.Join(dir, "control.sock")); !os.IsNotExist(e) {
		t.Fatal("legacy holder opened a control socket", e)
	}
	state.Status = "stopped"
	if e = store.save(state); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(before, activityTree(t, dir)) {
		t.Fatal("a refusal touched the legacy recording")
	}
}

func TestLegacyRecordingControls(t *testing.T) {
	for _, status := range []string{"paused", "recording", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			store := testStore(t)
			target := fakeTarget("legacy-controls-" + status)
			legacy, dir := writeLegacyRecording(t, store, target, status)
			before := activityTree(t, dir)
			for _, command := range []string{"resume", "start", "pause"} {
				if _, e := store.Control(context.Background(), target, command, "", ""); !errors.Is(e, errLegacyRecording) || !strings.Contains(e.Error(), "stop its holder if one is still running") {
					t.Fatalf("%s on a legacy recording: %v", command, e)
				}
			}
			// status, ensure and stop reach no holder: proven holder loss is
			// reported in memory and never written to the legacy state.json.
			for _, command := range []string{"status", "ensure", "stop"} {
				v, e := store.Control(context.Background(), target, command, "", "")
				if command == "stop" {
					if !errors.Is(e, errHolderAbsent) {
						t.Fatalf("stop without a holder: %v", e)
					}
				} else if e != nil {
					t.Fatalf("%s: %v", command, e)
				}
				if v.ID != legacy.ID || v.Status != "interrupted" || !v.Suppressed {
					t.Fatalf("%s reported %+v", command, v)
				}
			}
			if _, e := store.ControlBound(legacy.ID, "status"); e != nil {
				t.Fatal("bound status", e)
			}
			if !reflect.DeepEqual(before, activityTree(t, dir)) {
				t.Fatal("a control touched the legacy recording")
			}
			var out bytes.Buffer
			if e := formatStatus(&out, store.Root, legacy, time.Now()); e != nil || !strings.Contains(out.String(), "unsupported: legacy recording") {
				t.Fatalf("status output %v:\n%s", e, out.String())
			}
		})
	}
}

func TestLegacyStoppedTargetStartsVisual(t *testing.T) {
	store := testStore(t)
	target := fakeTarget("legacy-stopped")
	legacy, dir := writeLegacyRecording(t, store, target, "stopped")
	before := activityTree(t, dir)
	store.Launch = func(root, id string) error {
		go func() { _ = Hold(root, id, &fakeBackend{target: target}) }()
		return nil
	}
	started, e := store.Control(context.Background(), target, "start", "", "")
	if e != nil || started.ID == legacy.ID || started.Format != activityFormatIndexed {
		t.Fatalf("start after a stopped legacy recording: %+v %v", started, e)
	}
	newDir, _ := store.dir(started.ID)
	t.Cleanup(func() { _, _ = call(newDir, "stop") })
	if !reflect.DeepEqual(before, activityTree(t, dir)) {
		t.Fatal("start touched the legacy recording")
	}
}

// The supervisor's path: ensure reports a legacy recording without a holder
// as interrupted, and Recreate supersedes it with a new visual/1 recording,
// leaving the legacy directory byte-for-byte untouched.
func TestLegacyTargetRecreatedBySupervisorPath(t *testing.T) {
	for _, status := range []string{"interrupted", "recording", "failed"} {
		t.Run(status, func(t *testing.T) {
			store := testStore(t)
			target := fakeTarget("legacy-recreate-" + status)
			legacy, dir := writeLegacyRecording(t, store, target, status)
			before := activityTree(t, dir)
			v, e := store.Control(context.Background(), target, "ensure", "", "")
			if e != nil || v.Status != "interrupted" {
				t.Fatalf("ensure: %+v %v", v, e)
			}
			store.Launch = func(root, id string) error {
				go func() { _ = Hold(root, id, &fakeBackend{target: target}) }()
				return nil
			}
			created, e := store.Recreate(context.Background(), target, "")
			if e != nil || created.ID == legacy.ID || created.Format != activityFormatIndexed || created.Status != "recording" {
				t.Fatalf("recreate: %+v %v", created, e)
			}
			newDir, _ := store.dir(created.ID)
			t.Cleanup(func() { _, _ = call(newDir, "stop") })
			if current, e := store.current(target); e != nil || current.ID != created.ID {
				t.Fatalf("target index %+v %v", current, e)
			}
			if !reflect.DeepEqual(before, activityTree(t, dir)) {
				t.Fatal("recreate touched the legacy recording")
			}
		})
	}
}
