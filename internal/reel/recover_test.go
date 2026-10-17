//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

func recoverDefaults() recoverOptions {
	return recoverOptions{Template: "{id}", Limits: visualstore.DefaultLimits()}
}

func TestRecoverHints(t *testing.T) {
	for _, status := range []string{"interrupted", "failed", "recording", "stopped"} {
		t.Run(status, func(t *testing.T) {
			f := completedVisual(t, offlineIdentity, true)
			f.state.Status = status
			f.save(t)
			if err := writeJSON((&Store{Root: f.root}).targetPath(f.state.Target), f.state.ID); err != nil {
				t.Fatal(err)
			}
			report, err := listRecordings(context.Background(), f.root, "", 128, listDefaults())
			if err != nil {
				t.Fatal(err)
			}
			want := status != "stopped"
			if len(report.Items) != 1 || listRecoverHint(report.Items[0]) != want {
				t.Fatal(report)
			}
			encoded, err := json.Marshal(report)
			if err != nil || strings.Contains(string(encoded), `"recoverHint":true`) != want {
				t.Fatal(string(encoded), err)
			}
			var out bytes.Buffer
			if err = formatList(&out, f.root, report, time.Now()); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "run: areel recover "+f.state.ID) != want {
				t.Fatal(out.String())
			}
			out.Reset()
			if err = formatStatus(&out, f.root, f.state, time.Now()); err != nil {
				t.Fatal(err)
			}
			// Unreachable control is marked interrupted by the lifecycle control
			// path before formatting; a raw recording state alone has no such fact.
			statusHint := status == "interrupted" || status == "failed"
			if strings.Contains(out.String(), "run: areel recover "+f.state.ID) != statusHint {
				t.Fatal(out.String())
			}
			out.Reset()
			if err = printLifecycle(&out, "status", f.root, f.state, nil, true); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), `"recoverHint":true`) != statusHint {
				t.Fatal(out.String())
			}
			doc, err := visualDoctor(t, f, "", absentObservation())
			if err != nil {
				t.Fatal(err)
			}
			if doc.RecoverHint != want {
				t.Fatal(doc)
			}
			out.Reset()
			if err = formatOffline(&out, doc); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "run: areel recover "+f.state.ID) != want {
				t.Fatal(out.String())
			}
		})
	}
	t.Run("directory-current-template-leftover", func(t *testing.T) {
		f := completedVisual(t, offlineIdentity, true)
		doc, err := offlineRecording(context.Background(), f.root, f.state.ID, "", visualstore.DefaultLimits(), absentObservation(), "new-{id}")
		if err != nil || doc.RecoverHint {
			t.Fatal(doc, err)
		}
		tmp := filepath.Join(f.root, ".new-"+f.state.ID+".areel.tmp-0123456789abcdef")
		if err = os.WriteFile(tmp, nil, 0600); err != nil {
			t.Fatal(err)
		}
		doc, err = offlineRecording(context.Background(), f.root, f.state.ID, "", visualstore.DefaultLimits(), absentObservation(), "new-{id}")
		if err != nil || !doc.RecoverHint {
			t.Fatal(doc, err)
		}
	})
	t.Run("sealed-no-hint-unless-leftover", func(t *testing.T) {
		f := pausedVisual(t, offlineIdentity)
		r := recoverRun(t, f, recoverDefaults())
		if err := writeJSON((&Store{Root: f.root}).targetPath(f.state.Target), f.state.ID); err != nil {
			t.Fatal(err)
		}
		report, err := listRecordings(context.Background(), f.root, "", 128, listDefaults())
		if err != nil || listRecoverHint(report.Items[0]) {
			t.Fatal(report, err)
		}
		state, err := (&Store{Root: f.root}).read(f.state.ID)
		if err != nil {
			t.Fatal(err)
		}
		if statusRecoverHint(f.root, state) {
			t.Fatal("sealed status hint")
		}
		doc, err := offlineRecording(context.Background(), f.root, f.state.ID, "", visualstore.DefaultLimits(), absentObservation())
		if err != nil || doc.RecoverHint {
			t.Fatal(doc, err)
		}
		tmp := filepath.Join(f.root, "."+filepath.Base(r.SealedPath)+".tmp-0123456789abcdef")
		if err = os.WriteFile(tmp, nil, 0600); err != nil {
			t.Fatal(err)
		}
		doc, err = offlineRecording(context.Background(), f.root, f.state.ID, "", visualstore.DefaultLimits(), absentObservation())
		if err != nil || !doc.RecoverHint {
			t.Fatal(doc, err)
		}
	})
}
func TestRecoverAmbiguousRowFeed(t *testing.T) {
	root, id, ctx := locateFixture(t, "{target}-{id}")
	if err := writeJSON((&Store{Root: root}).targetPath(fakeTarget("locate")), id); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(root, prefix+"-"+id+".areel"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	healthy := strings.Repeat("b", 32)
	target := fakeTarget("healthy")
	dir := filepath.Join(root, "recordings", healthy)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "launch.json"), launchSpec{Target: target}); err != nil {
		t.Fatal(err)
	}
	if err := (&Store{Root: root}).save(State{ID: healthy, Target: target, Status: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON((&Store{Root: root}).targetPath(target), healthy); err != nil {
		t.Fatal(err)
	}
	r, err := listRecordings(ctx, root, "", 128, listDefaults())
	if err != nil || len(r.Items) != 2 {
		t.Fatal(r, err)
	}
	for _, row := range r.Items {
		if row.ID == id && (row.Metadata != "unavailable" || !strings.Contains(row.Reason, "ambiguous sealed file")) {
			t.Fatal(row)
		}
	}
	service := &feedService{library: root, limits: feedDefaults(time.Second)}
	page, err := service.observe(ctx)
	if err != nil || len(page.Items) != 2 {
		t.Fatal(page, err)
	}
}

func recoverRun(t *testing.T, f *visualFixture, opts recoverOptions) recoverReport {
	t.Helper()
	r, err := recoverRecording(context.Background(), f.root, f.state.ID, opts, absentObservation())
	if err != nil {
		t.Fatalf("recover: %v %+v", err, r)
	}
	t.Logf("recover elapsed=%s kind=%s views=%d freed=%d retained=%s", r.Elapsed, r.Kind, r.Views, r.FreedBytes, r.Retained)
	return r
}
func recoverAssertRemoved(t *testing.T, f *visualFixture, r recoverReport) {
	t.Helper()
	if !r.RemovedDirectory || r.Verify == nil || r.Verify.Status != "ok" {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Lstat(f.dir); !os.IsNotExist(err) {
		t.Fatalf("source remains: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(f.root, "recordings"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("recordings: %v %v", entries, err)
	}
	leftovers, err := recoverLeftovers(f.root, f.state.ID, "{id}")
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("leftovers %v %v", leftovers, err)
	}
	if r.SealedPath != filepath.Join(f.root, f.state.ID+".areel") {
		t.Fatal(r.SealedPath)
	}
}
func recoverBuild(t *testing.T, f *visualFixture) string {
	t.Helper()
	path := filepath.Join(f.root, f.state.ID+".areel")
	if _, err := seal.Seal(context.Background(), seal.Source{RecordingDir: f.dir}, seal.Destination{Path: path}, sealedDifferentialOptions()); err != nil {
		t.Fatal(err)
	}
	return path
}
func recoverSQL(t *testing.T, path, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(stmt, args...)
	ce := db.Close()
	if err != nil || ce != nil {
		t.Fatal(err, ce)
	}
}
func recoverTailFixture(t *testing.T) *visualFixture {
	f := newVisualFixture(t, 2, offlineIdentity)
	s := f.create(t)
	visualSegment(t, s, 1, 3)
	f.acknowledge(t, false)
	visualSegment(t, s, 2, 2)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	f.state.Status = "recording"
	f.save(t)
	return f
}

func TestRecoverMatrix(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		f := completedVisual(t, offlineIdentity, true)
		verifies := 0
		ctx := seal.WithProbe(context.Background(), func(part string, n int64) error {
			if part == "verify" && n == 1 {
				verifies++
			}
			return nil
		})
		r, err := recoverRecording(ctx, f.root, f.state.ID, recoverDefaults(), absentObservation())
		if err != nil || verifies != 1 {
			t.Fatalf("whole-file verifies=%d error=%v", verifies, err)
		}
		recoverAssertRemoved(t, f, r)
		if r.Kind != seal.KindComplete || !r.Sealed {
			t.Fatal(r)
		}
		before := treeHashes(t, f.root)
		again := recoverRun(t, f, recoverDefaults())
		if !reflect.DeepEqual(before, treeHashes(t, f.root)) || !reflect.DeepEqual(again.Actions, []string{"nothing to do"}) {
			t.Fatal(again)
		}
	})
	t.Run("equal-file", func(t *testing.T) {
		f := completedVisual(t, offlineIdentity, true)
		recoverBuild(t, f)
		r := recoverRun(t, f, recoverDefaults())
		recoverAssertRemoved(t, f, r)
		if r.Sealed {
			t.Fatal("rebuilt")
		}
	})
	t.Run("tail", func(t *testing.T) {
		f := recoverTailFixture(t)
		r := recoverRun(t, f, recoverDefaults())
		if r.RemovedDirectory || r.Kind != seal.KindInterrupted || r.Views != 3 || r.DroppedViews != 2 || !strings.Contains(r.Retained, "--discard-tail") {
			t.Fatalf("%+v", r)
		}
		sf, err := seal.Open(r.SealedPath)
		if err != nil {
			t.Fatal(err)
		}
		defer sf.Close()
		raw, err := os.ReadFile(filepath.Join(f.dir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		var marked State
		_ = json.Unmarshal(raw, &marked)
		if marked.Status != "interrupted" || !bytes.Equal(raw, sf.SourceFiles["state.json"]) || sf.Manifest.Recovery.SourceStatus != "recording" {
			t.Fatal("mark not embedded verbatim")
		}
		r = recoverRun(t, f, recoverDefaults())
		if r.RemovedDirectory {
			t.Fatal("implicit tail discard")
		}
		opts := recoverDefaults()
		opts.DiscardTail = true
		r = recoverRun(t, f, opts)
		recoverAssertRemoved(t, f, r)
	})
	t.Run("no-tail", func(t *testing.T) {
		f := pausedVisual(t, offlineIdentity)
		r := recoverRun(t, f, recoverDefaults())
		recoverAssertRemoved(t, f, r)
		if r.Kind != seal.KindInterrupted {
			t.Fatal(r)
		}
	})
	t.Run("complete-content-interrupted-lifecycle", func(t *testing.T) {
		f := completedVisual(t, offlineIdentity, true)
		f.state.Status = "stopping"
		f.state.Pending = "stop"
		f.save(t)
		r := recoverRun(t, f, recoverDefaults())
		recoverAssertRemoved(t, f, r)
		if r.Kind != seal.KindInterrupted || r.Verify.Manifest.Recovery.SourceVisualStatus != "ok" {
			t.Fatal(r)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		f := pausedVisual(t, offlineIdentity)
		err := visualcapture.WriteAcknowledged(f.ack, visualcapture.Acknowledged{Format: visualstore.Format, Views: 4, Chain: bytes.Repeat([]byte{7}, sha256.Size), SourceEnds: map[int]visualtime.Time{1: 40}, Segment: 1})
		if err != nil {
			t.Fatal(err)
		}
		r := recoverRun(t, f, recoverDefaults())
		if r.RemovedDirectory || r.Verify.Manifest.Recovery.SourceVisualStatus != "truncated" || r.Retained == "" {
			t.Fatal(r)
		}
		opts := recoverDefaults()
		opts.DiscardTail = true
		recoverAssertRemoved(t, f, recoverRun(t, f, opts))
	})
	t.Run("damaged", func(t *testing.T) {
		f := recoverTailFixture(t)
		recoverSQL(t, filepath.Join(f.store, visualstore.DatabaseName), "UPDATE view_blocks SET last_chain=zeroblob(32) WHERE first_seq>3")
		r := recoverRun(t, f, recoverDefaults())
		if r.RemovedDirectory || r.Verify.Manifest.Recovery.SourceVisualStatus != "damaged" {
			t.Fatal(r)
		}
	})
	t.Run("legacy", func(t *testing.T) {
		f := pausedVisual(t, offlineIdentity)
		f.state.Format = ""
		f.save(t)
		before := treeHashes(t, f.root)
		_, err := recoverRecording(context.Background(), f.root, f.state.ID, recoverDefaults(), absentObservation())
		if !errors.Is(err, errLegacyRecording) || !reflect.DeepEqual(before, treeHashes(t, f.root)) {
			t.Fatal(err)
		}
	})
	t.Run("live-and-uncertain", func(t *testing.T) {
		for _, present := range []bool{true, false} {
			f := pausedVisual(t, offlineIdentity)
			observe := absentObservation()
			observe.identity = func(pid int) (ProcessIdentity, error) {
				if present {
					return offlineIdentity(pid), nil
				}
				return ProcessIdentity{}, errors.New("uncertain")
			}
			before := treeHashes(t, f.root)
			_, err := recoverRecording(context.Background(), f.root, f.state.ID, recoverDefaults(), observe)
			if err == nil || !strings.Contains(err.Error(), "not-checked-active-or-uncertain") || !reflect.DeepEqual(before, treeHashes(t, f.root)) {
				t.Fatal(err)
			}
		}
	})
	t.Run("truncated-activity-changes-nothing", func(t *testing.T) {
		f := pausedVisual(t, offlineIdentity)
		a := newVisualActivity(t, f.dir, f.state)
		a.add(indexedEnvelope("one", "one"), false, nil, unknownPlacement())
		a.commit()
		a.close()
		ackPath := filepath.Join(f.dir, activityIndexDir, "acknowledged.json")
		raw, err := os.ReadFile(ackPath)
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.Replace(raw, []byte(`"seq":1`), []byte(`"seq":2`), 1)
		if err = os.WriteFile(ackPath, raw, 0600); err != nil {
			t.Fatal(err)
		}
		_, _ = visualDoctor(t, f, "", absentObservation()) // establish read-side SHM
		before := treeHashes(t, f.root)
		_, err = recoverRecording(context.Background(), f.root, f.state.ID, recoverDefaults(), absentObservation())
		if err == nil || !strings.Contains(err.Error(), "activity truncated") || !reflect.DeepEqual(before, treeHashes(t, f.root)) {
			t.Fatal(err)
		}
	})
}

func TestRecoverLeftovers(t *testing.T) {
	for _, kind := range []string{"ok", "missing", "damaged", "partial"} {
		t.Run(kind, func(t *testing.T) {
			f := completedVisual(t, offlineIdentity, true)
			path := ""
			if kind != "missing" {
				path = recoverBuild(t, f)
			}
			name, _ := seal.DeletingName(f.state.ID)
			left := filepath.Join(f.root, "recordings", name)
			if err := os.Rename(f.dir, left); err != nil {
				t.Fatal(err)
			}
			if kind == "partial" {
				if err := os.Remove(filepath.Join(left, "state.json")); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "damaged" {
				recoverSQL(t, path, "UPDATE seal SET manifest=manifest||' '")
			}
			before := treeHashes(t, f.root)
			r := recoverRun(t, f, recoverDefaults())
			if kind == "ok" || kind == "partial" {
				if len(r.RemovedLeftovers) != 1 || r.Verify.Status != "ok" {
					t.Fatal(r)
				}
				if _, err := os.Stat(left); !os.IsNotExist(err) {
					t.Fatal(err)
				}
			} else if !strings.Contains(r.Retained, "mv -T -n -- "+shellQuote(left)+" "+shellQuote(f.dir)) || !reflect.DeepEqual(before, treeHashes(t, f.root)) {
				t.Fatal(r)
			}
		})
	}
	t.Run("root-reserved-only", func(t *testing.T) {
		f := completedVisual(t, offlineIdentity, true)
		tmp := filepath.Join(f.root, "."+f.state.ID+".areel.tmp-0123456789abcdef")
		scratch := filepath.Join(f.root, "."+f.state.ID+".areel.salvage-0123456789abcdef")
		if err := os.WriteFile(tmp, []byte("torn"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(scratch, 0700); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(f.root, "."+strings.Repeat("d", 32)+".areel.tmp-0123456789abcdef")
		if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		r := recoverRun(t, f, recoverDefaults())
		if len(r.RemovedLeftovers) != 2 {
			t.Fatal(r)
		}
		recoverAssertRemoved(t, f, r)
		data, err := os.ReadFile(keep)
		if err != nil || string(data) != "keep" {
			t.Fatal(err)
		}
	})
}

func TestRecoverDamagedFile(t *testing.T) {
	for _, source := range []bool{true, false} {
		t.Run(fmt.Sprint(source), func(t *testing.T) {
			f := completedVisual(t, offlineIdentity, true)
			path := recoverBuild(t, f)
			if !source {
				if _, err := seal.RemoveSource(context.Background(), sealedDifferentialRemoval(f.dir, path)); err != nil {
					t.Fatal(err)
				}
			}
			// A completion defect leaves the visual verified prefix intact.
			recoverSQL(t, path, "DELETE FROM completion")
			before := treeHashes(t, f.root)
			r, err := recoverRecording(context.Background(), f.root, f.state.ID, recoverDefaults(), absentObservation())
			if source {
				if err != nil || !strings.Contains(r.Retained, "remove this file by hand") || !reflect.DeepEqual(before, treeHashes(t, f.root)) {
					t.Fatal(r, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "--out") {
				t.Fatal(r, err)
			}
			opts := recoverDefaults()
			opts.Out = filepath.Join(f.root, "out")
			if _, err = recoverRecording(context.Background(), f.root, f.state.ID, opts, absentObservation()); err == nil {
				t.Fatal("inside accepted")
			}
			opts.Out = offlineDestination(t)
			r, err = recoverRecording(context.Background(), f.root, f.state.ID, opts, absentObservation())
			if err != nil || r.Verify == nil || r.Verify.Status != "ok" || r.Kind != seal.KindInterrupted || r.Verify.Manifest.Recovery.RecoveredFrom != path {
				t.Fatal(r, err)
			}
			if !reflect.DeepEqual(before, treeHashes(t, f.root)) {
				t.Fatal("damaged source changed")
			}
		})
	}
}

func TestRecoverRootPrivacyAndHelp(t *testing.T) {
	f := completedVisual(t, offlineIdentity, true)
	info, err := os.Stat(f.root)
	if err != nil || info.Mode() != os.ModeDir|0700 || !privateOwner(info) {
		t.Fatal(info, err)
	}
	if err = os.Chmod(f.root, 0750); err != nil {
		t.Fatal(err)
	}
	before := treeHashes(t, f.root)
	_, err = recoverRecording(context.Background(), f.root, f.state.ID, recoverDefaults(), absentObservation())
	if err == nil || !strings.Contains(err.Error(), "chmod 700 "+shellQuote(f.root)) || !reflect.DeepEqual(before, treeHashes(t, f.root)) {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = CLI([]string{"help"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "salvage") || !strings.Contains(out.String(), "recover") {
		t.Fatal(out.String())
	}
	out.Reset()
	if err = CLI([]string{"recover", "--help"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--out", "--discard-tail", "--json", "--decoded-bytes", "--work", "docs/recording.md"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal(want, out.String())
		}
	}
	if err = CLI([]string{"salvage"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), `did you mean "recover"`) {
		t.Fatal(err)
	}
}

func TestRecoverLocatorCapacity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "library")
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	const count = 2305
	started := time.Now()
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%032x", i)
		dir, _ := store.dir(id)
		if err = os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err = writeJSON(filepath.Join(dir, "launch.json"), launchSpec{SealName: "{id}"}); err != nil {
			t.Fatal(err)
		}
		if err = store.save(State{ID: id, Target: fakeTarget(id), Status: "stopped"}); err != nil {
			t.Fatal(err)
		}
	}
	seen, after := 0, ""
	for {
		page, e := libraryList(context.Background(), root, after, 128)
		if e != nil {
			t.Fatal(e)
		}
		seen += len(page.Items)
		after = page.NextAfter
		if !page.MoreObserved {
			break
		}
	}
	if seen != count {
		t.Fatal(seen)
	}
	ids, err := recordingIDs(root)
	if err != nil || len(ids) != count {
		t.Fatal(len(ids), err)
	}
	id, err := resolveREC(root, fmt.Sprintf("%031x", 0), nil)
	if err == nil || id != "" {
		t.Fatal("ambiguous prefix should stay ambiguous")
	}
	// A unique 31-byte prefix near the high end still scans every directory.
	id, err = resolveREC(root, "0000000000000000000000000000090", nil)
	if err != nil || id != "00000000000000000000000000000900" {
		t.Fatal(id, err)
	}
	t.Logf("capacity %d directories; pages+resolution+fixture elapsed %s", seen, time.Since(started))
}

func TestRecoverCurrentTemplateAndUnreadable(t *testing.T) {
	t.Run("current-template-tail", func(t *testing.T) {
		f := recoverTailFixture(t)
		opts := recoverDefaults()
		opts.Template = "new-{id}"
		r := recoverRun(t, f, opts)
		loc, err := libraryLocate(f.root, f.state.ID, newLibraryGuard(withSealName(context.Background(), opts.Template)))
		if err != nil || loc.Kind != "sealed" || loc.Path != r.SealedPath {
			t.Fatal(loc, err)
		}
		state, err := (&Store{Root: f.root, SealName: opts.Template}).read(f.state.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, asJSON := range []bool{false, true} {
			var output bytes.Buffer
			if err = printLifecycle(&output, "status", f.root, state, nil, asJSON, opts.Template); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), "recoverHint") || strings.Contains(output.String(), "run: areel recover") {
				t.Fatalf("sealed current-template status hinted recovery: %s", output.String())
			}
		}
		opts.DiscardTail = true
		r = recoverRun(t, f, opts)
		if !r.RemovedDirectory {
			t.Fatal(r)
		}
	})
	t.Run("unreadable-existing-source", func(t *testing.T) {
		f := completedVisual(t, offlineIdentity, true)
		recoverBuild(t, f)
		fp, err := os.OpenFile(filepath.Join(f.store, visualstore.DatabaseName), os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = fp.WriteAt([]byte("corrupt!"), 0)
		_ = fp.Close()
		if err != nil {
			t.Fatal(err)
		}
		r := recoverRun(t, f, recoverDefaults())
		if r.RemovedDirectory || !strings.Contains(r.Retained, "--discard-tail") {
			t.Fatal(r)
		}
		opts := recoverDefaults()
		opts.DiscardTail = true
		recoverAssertRemoved(t, f, recoverRun(t, f, opts))
	})
	t.Run("created-fallback", func(t *testing.T) {
		f := completedVisual(t, offlineIdentity, true)
		// Fixture segment start is zero; renderer must obtain store meta.created.
		opts := recoverDefaults()
		opts.Template = "{date}-{time}-{id}"
		r := recoverRun(t, f, opts)
		if !r.RemovedDirectory || r.Kind != seal.KindComplete || !strings.HasSuffix(r.SealedPath, f.state.ID+".areel") {
			t.Fatal(r)
		}
	})
}

func TestRecoverLiveSocket(t *testing.T) {
	f := pausedVisual(t, offlineIdentity)
	endpoint := filepath.Join(f.dir, "control.sock")
	tmp := filepath.Join(f.root, "."+f.state.ID+".areel.tmp-0123456789abcdef")
	if err := os.WriteFile(tmp, []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	before := treeHashes(t, f.root)
	ln, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	observe := absentObservation()
	observe.identity = func(pid int) (ProcessIdentity, error) { return offlineIdentity(pid), nil }
	r, err := recoverRecording(context.Background(), f.root, f.state.ID, recoverDefaults(), observe)
	if err == nil || !strings.Contains(err.Error(), "not-checked-active-or-uncertain") {
		t.Fatal(r, err)
	}
	_ = ln.(*net.UnixListener).SetDeadline(time.Now().Add(10 * time.Millisecond))
	if conn, e := ln.Accept(); e == nil {
		conn.Close()
		t.Fatal("recovery contacted holder")
	}
	_ = ln.Close()
	if !reflect.DeepEqual(before, treeHashes(t, f.root)) {
		t.Fatal("live refusal changed fixture")
	}
}
func TestRecoverActivityTailAndUntrustedEnvelope(t *testing.T) {
	t.Run("activity-tail", func(t *testing.T) {
		f := pausedVisual(t, offlineIdentity)
		a := newVisualActivity(t, f.dir, f.state)
		a.add(indexedEnvelope("one", "one"), false, nil, unknownPlacement())
		a.commit()
		path := filepath.Join(f.dir, activityIndexDir, "acknowledged.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		a.add(indexedEnvelope("two", "two"), false, nil, unknownPlacement())
		a.commit()
		a.close()
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		r := recoverRun(t, f, recoverDefaults())
		if r.RemovedDirectory || r.DroppedActivityRows != 1 || r.Verify.Manifest.Activity.Rows != 1 {
			t.Fatal(r)
		}
		opts := recoverDefaults()
		opts.DiscardTail = true
		recoverAssertRemoved(t, f, recoverRun(t, f, opts))
	})
	t.Run("untrusted-envelope", func(t *testing.T) {
		f := completedVisual(t, offlineIdentity, true)
		r := recoverRun(t, f, recoverDefaults())
		recoverSQL(t, r.SealedPath, "UPDATE seal SET manifest=manifest||' '")
		before := treeHashes(t, f.root)
		opts := recoverDefaults()
		opts.Out = offlineDestination(t)
		r = recoverRun(t, f, opts)
		if !strings.Contains(r.Retained, "untrusted sealed envelope") || !reflect.DeepEqual(before, treeHashes(t, f.root)) {
			t.Fatal(r)
		}
		if _, err := os.Stat(opts.Out); !os.IsNotExist(err) {
			t.Fatal("created output for untrusted envelope", err)
		}
	})
}

func TestRecoverLeftoverHolderProof(t *testing.T) {
	f := completedVisual(t, offlineIdentity, true)
	scratch := filepath.Join(f.root, "."+f.state.ID+".areel.salvage-0123456789abcdef")
	if err := os.Mkdir(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	state := f.state
	state.Holder = offlineIdentity(202)
	if err := writeJSON(filepath.Join(scratch, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	observe := absentObservation()
	observe.identity = func(pid int) (ProcessIdentity, error) {
		if pid == 202 {
			return offlineIdentity(pid), nil
		}
		return ProcessIdentity{}, errProcessAbsent
	}
	before := treeHashes(t, f.root)
	_, err := recoverRecording(context.Background(), f.root, f.state.ID, recoverDefaults(), observe)
	if err == nil || !strings.Contains(err.Error(), "not-checked-active-or-uncertain") || !reflect.DeepEqual(before, treeHashes(t, f.root)) {
		t.Fatal(err)
	}
}
