//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// visualFixture is a synthetic visual/1 recording in a private temporary
// library: state.json, launch.json, one directory with process metadata per
// launched segment, the visual store and its acknowledged record. Nothing is
// launched; every process identity is a fixture.
type visualFixture struct {
	root, dir, store, ack string
	state                 State
}

func visualAt(line int64, ns visualtime.Time) visualtime.Point {
	return visualtime.Point{Line: line, At: ns, Known: true}
}

func visualScreen(text string) castexport.Screen {
	const cols, rows = 16, 4
	s := castexport.Screen{Cols: cols, Rows: rows, Cursor: castexport.Cursor{Visible: true}, Cells: make([][]castexport.Cell, rows)}
	for i := range s.Cells {
		s.Cells[i] = make([]castexport.Cell, cols)
		for j := range s.Cells[i] {
			s.Cells[i][j] = castexport.Cell{Char: ' ', Width: 1}
		}
	}
	for j, r := range text {
		s.Cells[0][j] = castexport.Cell{Char: r, Width: 1}
	}
	return s
}

// newVisualFixture makes the library and recording directories and the state
// with the given segments (all launched, each with absent-process metadata).
// The store and the acknowledged record are the caller's.
func newVisualFixture(t *testing.T, segments int, identity func(int) ProcessIdentity, ids ...string) *visualFixture {
	t.Helper()
	root := filepath.Join(shortTestDir(t), "library")
	store, e := OpenStore(root)
	if e != nil {
		t.Fatal(e)
	}
	state := State{ID: strings.Repeat("c", 32), Target: fakeTarget("visual-instance"), Status: "paused", Format: activityFormatIndexed, Holder: identity(201), Segments: []Segment{}, Events: []Event{}}
	if len(ids) > 0 {
		state.ID = ids[0]
	}
	dir, _ := store.dir(state.ID)
	if e = privateDir(dir); e != nil {
		t.Fatal(e)
	}
	for n := 1; n <= segments; n++ {
		state.Segments = append(state.Segments, Segment{Number: n, Launched: true, Path: visualcapture.StoreDirName})
		segment := filepath.Join(dir, fmt.Sprintf("segment-%04d", n))
		if e = os.Mkdir(segment, 0700); e != nil {
			t.Fatal(e)
		}
		for name, value := range map[string]any{"recorder.json": identity(200 + 10*n + 1), "watcher.json": identity(200 + 10*n + 2), "watch.json": watchSpec{Executable: "never-executed", Target: state.Target}} {
			if e = writeJSON(filepath.Join(segment, name), value); e != nil {
				t.Fatal(e)
			}
		}
	}
	if e = store.save(state); e != nil {
		t.Fatal(e)
	}
	if e = writeJSON(filepath.Join(dir, "launch.json"), launchSpec{Target: state.Target}); e != nil {
		t.Fatal(e)
	}
	return &visualFixture{root: root, dir: dir, store: filepath.Join(dir, visualcapture.StoreDirName), ack: filepath.Join(dir, visualcapture.AcknowledgedName), state: state}
}

func (f *visualFixture) save(t *testing.T) {
	t.Helper()
	if e := (&Store{Root: f.root}).save(f.state); e != nil {
		t.Fatal(e)
	}
}

func (f *visualFixture) create(t *testing.T) *visualstore.Store {
	t.Helper()
	if e := visualcapture.WriteAcknowledged(f.ack, visualcapture.Acknowledged{Format: visualstore.Format, Chain: make([]byte, sha256.Size), SourceEnds: map[int]visualtime.Time{}}); e != nil {
		t.Fatal(e)
	}
	s, e := visualstore.Create(f.store, archive.Identity{Recording: f.state.ID, Segment: 1, Backend: f.state.Target.Backend, Instance: f.state.Target.Instance})
	if e != nil {
		t.Fatal(e)
	}
	return s
}

// acknowledge writes the record of the store's committed state, as the capture
// does after a commit returned nil.
func (f *visualFixture) acknowledge(t *testing.T, complete bool) visualcapture.Acknowledged {
	t.Helper()
	r, e := visualstore.OpenReader(f.store)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	sel, e := r.Snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	ack := visualcapture.Acknowledged{Format: visualstore.Format, Views: sel.ViewSeq, Chain: sel.Chain, SourceEnds: map[int]visualtime.Time{}, Segment: sel.Cutoff.Segment, Complete: complete}
	if ack.Views == 0 {
		ack.Chain = make([]byte, sha256.Size)
	}
	for _, s := range sel.Segments {
		ack.SourceEnds[s.Segment] = s.SourceEnd.At
	}
	if e = visualcapture.WriteAcknowledged(f.ack, ack); e != nil {
		t.Fatal(e)
	}
	return ack
}

// segment records n views and an exit into one segment, closes it at the exit
// and commits: a pause boundary.
func visualSegment(t *testing.T, s *visualstore.Store, segment, n int) {
	t.Helper()
	if e := s.BeginSegment(segment, archive.Geometry{Columns: 16, Rows: 4}, visualcapture.Precision); e != nil {
		t.Fatal(e)
	}
	for i := 1; i <= n; i++ {
		if _, e := s.Record(visualstore.View{Segment: segment, Point: visualAt(int64(i), visualtime.Time(10*i)), Screen: visualScreen(strings.Repeat("x", i))}); e != nil {
			t.Fatal(e)
		}
	}
	exit := visualAt(int64(n+1), visualtime.Time(10*(n+1)))
	if e := s.Event(visualstore.Event{Segment: segment, Point: exit, Kind: visualstore.EventExit, Data: "0"}); e != nil {
		t.Fatal(e)
	}
	if e := s.CloseSegment(segment, exit); e != nil {
		t.Fatal(e)
	}
	if e := s.Commit(visualtime.Cutoff{Segment: segment, Source: exit, Presentations: uint64(n), Events: 1}); e != nil {
		t.Fatal(e)
	}
}

// pausedVisual is a store paused after one segment of three views, with a
// matching acknowledged record.
func pausedVisual(t *testing.T, identity func(int) ProcessIdentity) *visualFixture {
	t.Helper()
	f := newVisualFixture(t, 1, identity)
	s := f.create(t)
	visualSegment(t, s, 1, 3)
	f.acknowledge(t, false)
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	return f
}

// completedVisual is a stopped recording: the segment committed at its exit,
// then Complete and an acknowledged complete:true record.
func completedVisual(t *testing.T, identity func(int) ProcessIdentity, acknowledgeCompletion bool, ids ...string) *visualFixture {
	t.Helper()
	f := newVisualFixture(t, 1, identity, ids...)
	s := f.create(t)
	visualSegment(t, s, 1, 3)
	f.acknowledge(t, false)
	if e := s.Complete(visualtime.Cutoff{Segment: 1, Source: visualAt(4, 40), Presentations: 3, Events: 1, SourceComplete: true}); e != nil {
		t.Fatal(e)
	}
	if acknowledgeCompletion {
		f.acknowledge(t, true)
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	f.state.Status = "stopped"
	f.save(t)
	return f
}

func visualDoctor(t *testing.T, f *visualFixture, destination string, observe offlineObservation) (offlineVisualReport, error) {
	t.Helper()
	return offlineRecording(context.Background(), f.root, f.state.ID, destination, visualstore.DefaultLimits(), observe)
}

func visualFileHashes(t *testing.T, store string) map[string][32]byte {
	t.Helper()
	hashes := map[string][32]byte{}
	for _, name := range []string{visualstore.DatabaseName, visualstore.DatabaseName + "-wal"} {
		data, e := os.ReadFile(filepath.Join(store, name))
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			t.Fatal(e)
		}
		// An absent -wal and the empty one SQLite may create on open are the
		// same content: nothing.
		if len(data) == 0 {
			continue
		}
		hashes[name] = sha256.Sum256(data)
	}
	return hashes
}

// visualLibraryHashes hashes every file of the library except the store's
// shared-memory index, which SQLite rebuilds when any reader opens a WAL
// database.
func visualLibraryHashes(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	hashes := treeHashes(t, root)
	for path := range hashes {
		if strings.HasSuffix(path, "-shm") {
			delete(hashes, path)
		}
		if strings.HasSuffix(path, "-wal") {
			if data, _ := os.ReadFile(path); len(data) == 0 {
				delete(hashes, path)
			}
		}
	}
	return hashes
}

func TestOfflineVisualDoctorVerdicts(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		f := completedVisual(t, offlineIdentity, true)
		before := visualLibraryHashes(t, f.root)
		r, e := visualDoctor(t, f, "", absentObservation())
		if e != nil {
			t.Fatal(e)
		}
		if r.Format != "visual/1" || r.Visual == nil || r.Visual.Status != visualstore.StatusOK || r.ArtifactStatus != "ok" || r.Visual.Acknowledged != visualstore.AcknowledgedPresent {
			t.Fatalf("completed store %+v %+v", r, r.Visual)
		}
		if r.Acknowledged == nil || r.Acknowledged.Views != 3 || !r.Acknowledged.Complete || r.Acknowledged.SourceEnds["1"] != 40 || r.UnacknowledgedViews != 0 {
			t.Fatalf("acknowledged %+v", r.Acknowledged)
		}
		if c := r.Completion; c == nil || !c.Store || !c.Acknowledged || !c.HolderStopped || !c.Complete {
			t.Fatalf("completion %+v", r.Completion)
		}
		if !reflect.DeepEqual(before, visualLibraryHashes(t, f.root)) {
			t.Fatal("doctor mutated the source library")
		}
	})
	t.Run("paused", func(t *testing.T) {
		f := pausedVisual(t, offlineIdentity)
		r, e := visualDoctor(t, f, "", absentObservation())
		if e != nil {
			t.Fatal(e)
		}
		if r.Visual.Status != visualstore.StatusIncomplete || r.Visual.Acknowledged != visualstore.AcknowledgedPresent || r.ArtifactStatus != "incomplete" {
			t.Fatalf("paused store %+v", r.Visual)
		}
		if c := r.Completion; c.Store || c.Acknowledged || c.HolderStopped || c.Complete {
			t.Fatalf("paused completion %+v", c)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		f := pausedVisual(t, offlineIdentity)
		// The owner was told of one more view than the store holds, with a
		// chain the store does not carry.
		claim := visualcapture.Acknowledged{Format: visualstore.Format, Views: 4, Chain: bytes.Repeat([]byte{7}, sha256.Size), SourceEnds: map[int]visualtime.Time{1: 40}, Segment: 1}
		if e := visualcapture.WriteAcknowledged(f.ack, claim); e != nil {
			t.Fatal(e)
		}
		r, e := visualDoctor(t, f, "", absentObservation())
		if e != nil {
			t.Fatal(e)
		}
		if r.Visual.Status != visualstore.StatusTruncated || r.Visual.Acknowledged != visualstore.AcknowledgedMissing || r.ArtifactStatus != "truncated" || r.UnacknowledgedViews != 0 {
			t.Fatalf("truncated store %+v", r.Visual)
		}
	})
	t.Run("unacknowledged-tail", func(t *testing.T) {
		f := newVisualFixture(t, 2, offlineIdentity)
		s := f.create(t)
		visualSegment(t, s, 1, 3)
		f.acknowledge(t, false)
		// A second segment committed but never acknowledged: a crash between
		// Commit and the record write.
		visualSegment(t, s, 2, 2)
		if e := s.Close(); e != nil {
			t.Fatal(e)
		}
		f.state.Status = "interrupted"
		f.save(t)
		r, e := visualDoctor(t, f, "", absentObservation())
		if e != nil {
			t.Fatal(e)
		}
		if r.Visual.Status != visualstore.StatusIncomplete || r.Visual.Acknowledged != visualstore.AcknowledgedPresent || r.UnacknowledgedViews != 2 || r.Acknowledged.Views != 3 {
			t.Fatalf("unacknowledged tail %+v %+v", r.Visual, r.Acknowledged)
		}
	})
	t.Run("zero-segment", func(t *testing.T) {
		// The recorder launched but the store never saw a header: New+Start
		// created the store and the initial record, and no segment began.
		f := newVisualFixture(t, 1, offlineIdentity)
		s := f.create(t)
		if e := s.Close(); e != nil {
			t.Fatal(e)
		}
		f.state.Status = "failed"
		f.save(t)
		r, e := visualDoctor(t, f, "", absentObservation())
		if e != nil {
			t.Fatal(e)
		}
		if r.Visual.Status != visualstore.StatusIncomplete || r.Visual.FailedItem != "" || r.Visual.Counts.Segments != 0 || r.Acknowledged.Segment != 0 || r.Completion.Complete {
			t.Fatalf("zero-segment store %+v", r.Visual)
		}
	})
	t.Run("temporary-record-files-ignored", func(t *testing.T) {
		f := pausedVisual(t, offlineIdentity)
		if e := os.WriteFile(filepath.Join(f.dir, ".visual-acknowledged.json.123456.tmp"), []byte("{torn"), 0600); e != nil {
			t.Fatal(e)
		}
		r, e := visualDoctor(t, f, "", absentObservation())
		if e != nil || r.Visual.Status != visualstore.StatusIncomplete || r.Visual.Acknowledged != visualstore.AcknowledgedPresent {
			t.Fatalf("temporary record file disturbed the doctor: %v %+v", e, r.Visual)
		}
	})
	t.Run("segment-flag-removed", func(t *testing.T) {
		f := pausedVisual(t, offlineIdentity)
		if e := CLI([]string{"doctor", f.state.ID, "--library", f.root, "--segment", "1"}, io.Discard, io.Discard); e == nil || !strings.Contains(e.Error(), "segment") {
			t.Fatalf("removed --segment flag accepted: %v", e)
		}
	})
}

// TestOfflineVisualCompletionDisagreements reports each of the three
// completion facts as found when they disagree, in both directions.
func TestOfflineVisualCompletionDisagreements(t *testing.T) {
	t.Run("store-complete-not-acknowledged", func(t *testing.T) {
		// A failure between Store.Complete and the acknowledged write.
		f := completedVisual(t, offlineIdentity, false)
		r, e := visualDoctor(t, f, "", absentObservation())
		if e != nil {
			t.Fatal(e)
		}
		if c := r.Completion; !c.Store || c.Acknowledged || !c.HolderStopped || c.Complete {
			t.Fatalf("unacknowledged completion reported complete %+v", c)
		}
	})
	t.Run("stopped-after-pause-boundary", func(t *testing.T) {
		// A stop escalated after the pause's Done: the holder settles stopped,
		// the store stays closed and incomplete.
		f := pausedVisual(t, offlineIdentity)
		f.state.Status = "stopped"
		f.save(t)
		r, e := visualDoctor(t, f, "", absentObservation())
		if e != nil {
			t.Fatal(e)
		}
		if c := r.Completion; c.Store || c.Acknowledged || !c.HolderStopped || c.Complete || r.Visual.Status != visualstore.StatusIncomplete {
			t.Fatalf("late stop reported complete %+v", c)
		}
	})
	t.Run("capture-complete-holder-interrupted", func(t *testing.T) {
		// SetFinal(true) and a clean x0, but a PTY drain error interrupted
		// the holder.
		f := completedVisual(t, offlineIdentity, true)
		f.state.Status = "interrupted"
		f.save(t)
		r, e := visualDoctor(t, f, "", absentObservation())
		if e != nil {
			t.Fatal(e)
		}
		if c := r.Completion; !c.Store || !c.Acknowledged || c.HolderStopped || c.Complete || r.Visual.Status != visualstore.StatusOK {
			t.Fatalf("interrupted holder reported complete %+v", c)
		}
	})
}

func TestOfflineVisualAcknowledgedRecordRequired(t *testing.T) {
	for _, kind := range []string{"missing", "trailing", "unknown-member", "widened", "symlink", "temporary-only"} {
		t.Run(kind, func(t *testing.T) {
			// Real procfs observation of fixture identities that cannot exist,
			// so the unmodified CLI reaches the acknowledged record.
			f := pausedVisual(t, realAbsentIdentity(t))
			observe := offlineObservation{currentProcView, observeIdentity}
			if r, e := visualDoctor(t, f, "", observe); e != nil || r.ArtifactStatus != "incomplete" {
				t.Fatalf("fixture is not checkable: %v %+v", e, r)
			}
			switch kind {
			case "missing":
				os.Remove(f.ack)
			case "trailing":
				data, _ := os.ReadFile(f.ack)
				os.WriteFile(f.ack, append(data, []byte("{}\n")...), 0600)
			case "unknown-member":
				data, _ := os.ReadFile(f.ack)
				os.WriteFile(f.ack, bytes.Replace(data, []byte(`{"format"`), []byte(`{"extra":1,"format"`), 1), 0600)
			case "widened":
				os.Chmod(f.ack, 0644)
			case "symlink":
				os.Rename(f.ack, f.ack+".real")
				os.Symlink(f.ack+".real", f.ack)
			case "temporary-only":
				os.Rename(f.ack, filepath.Join(f.dir, ".visual-acknowledged.json.42.tmp"))
			}
			r, e := visualDoctor(t, f, "", observe)
			if e == nil {
				t.Fatalf("doctor accepted a %s acknowledged record: %+v", kind, r)
			}
			if r.Visual != nil || r.ArtifactStatus == "ok" || r.ArtifactStatus == "incomplete" {
				t.Fatalf("verdict without an acknowledged record %+v", r)
			}
			var out bytes.Buffer
			if e = CLI([]string{"doctor", f.state.ID, "--library", f.root, "--json"}, &out, io.Discard); e == nil {
				t.Fatal("CLI doctor succeeded without an acknowledged record")
			}
			if strings.Contains(out.String(), `"visual"`) || strings.Contains(out.String(), `"status":"ok"`) {
				t.Fatalf("CLI printed a verdict %s", out.String())
			}
		})
	}
}

func TestOfflineVisualOwnershipFailsClosed(t *testing.T) {
	for _, kind := range []string{"present", "missing-recorder", "unreadable-view", "stale-launch-flag", "earlier-unlaunched-attempt"} {
		t.Run(kind, func(t *testing.T) {
			f := pausedVisual(t, offlineIdentity)
			observe := absentObservation()
			segment := filepath.Join(f.dir, "segment-0001")
			wantChecked := false
			switch kind {
			case "present":
				observe.identity = func(pid int) (ProcessIdentity, error) { return offlineIdentity(pid), nil }
			case "missing-recorder":
				os.Remove(filepath.Join(segment, "recorder.json"))
			case "unreadable-view":
				observe.view = func() (processView, error) { return processView{}, os.ErrPermission }
			case "stale-launch-flag":
				// The newest attempt's launched:false is not proof of absence.
				os.Remove(filepath.Join(segment, "recorder.json"))
				f.state.Segments[0].Launched = false
				f.save(t)
			case "earlier-unlaunched-attempt":
				// A refused attempt that left no directory, followed by a
				// later attempt, contributes no ownership evidence.
				f.state.Segments = append([]Segment{{Number: 1, Path: visualcapture.StoreDirName}}, f.state.Segments...)
				f.state.Segments[1].Number = 2
				os.Rename(segment, filepath.Join(f.dir, "segment-0002"))
				f.save(t)
				wantChecked = true
			}
			before := visualLibraryHashes(t, f.root)
			r, e := visualDoctor(t, f, "", observe)
			if e != nil {
				t.Fatal(e)
			}
			if wantChecked {
				if r.ArtifactStatus != "incomplete" {
					t.Fatalf("settled attempt blocked the doctor %+v", r)
				}
				return
			}
			if r.ArtifactStatus != "not-checked-active-or-uncertain" || r.Visual != nil {
				t.Fatalf("uncertain ownership verified %+v", r)
			}
			dst := offlineDestination(t)
			if _, e = visualDoctor(t, f, dst, observe); e == nil {
				t.Fatal("uncertain salvage accepted")
			}
			if _, e = os.Stat(dst); !os.IsNotExist(e) {
				t.Fatal("refusal created destination")
			}
			if !reflect.DeepEqual(before, visualLibraryHashes(t, f.root)) {
				t.Fatal("source changed")
			}
		})
	}
}

func TestOfflineVisualSalvage(t *testing.T) {
	for _, kind := range []string{"completed", "unacknowledged-tail"} {
		t.Run(kind, func(t *testing.T) {
			var f *visualFixture
			views := int64(3)
			if kind == "completed" {
				f = completedVisual(t, offlineIdentity, true)
			} else {
				f = newVisualFixture(t, 2, offlineIdentity)
				s := f.create(t)
				visualSegment(t, s, 1, 3)
				f.acknowledge(t, false)
				visualSegment(t, s, 2, 2)
				s.Close()
				views = 5
			}
			sourceFiles := visualFileHashes(t, f.store)
			before := visualLibraryHashes(t, f.root)
			dst := offlineDestination(t)
			r, e := visualDoctor(t, f, dst, absentObservation())
			if e != nil {
				t.Fatal(e)
			}
			if r.Salvage == nil || r.Salvage.Status != visualstore.SalvageComplete || r.ArtifactStatus != visualstore.SalvageComplete || r.Salvage.Copied.Views != views {
				t.Fatalf("salvage %+v", r.Salvage)
			}
			check, e := visualstore.Verify(context.Background(), dst, nil, visualstore.DefaultLimits())
			if e != nil || check.Status != visualstore.StatusIncomplete || check.Counts.Views != views || check.SalvageStatus != visualstore.SalvageComplete {
				t.Fatalf("salvaged store %v %+v", e, check)
			}
			reader, e := visualstore.OpenReader(dst)
			if e != nil {
				t.Fatal(e)
			}
			sel, e := reader.Snapshot(context.Background())
			reader.Close()
			if e != nil || len(sel.Segments) == 0 {
				t.Fatalf("salvaged segments %v %+v", e, sel)
			}
			for _, s := range sel.Segments {
				if s.State != visualstore.StateClosed {
					t.Fatalf("salvaged segment %d is %s", s.Segment, s.State)
				}
			}
			if !reflect.DeepEqual(sourceFiles, visualFileHashes(t, f.store)) || len(sourceFiles) == 0 {
				t.Fatal("salvage changed visual.sqlite or its WAL")
			}
			if !reflect.DeepEqual(before, visualLibraryHashes(t, f.root)) {
				t.Fatal("salvage wrote inside the source library")
			}
		})
	}
	t.Run("destination-refusals", func(t *testing.T) {
		f := pausedVisual(t, offlineIdentity)
		before := visualLibraryHashes(t, f.root)
		for _, dst := range []string{
			filepath.Join(f.root, "new"),
			filepath.Join(f.store, "new"),
			filepath.Join(f.dir, "salvage"),
			filepath.Join(t.TempDir(), "absent", "new"),
		} {
			if _, e := visualDoctor(t, f, dst, absentObservation()); e == nil {
				t.Fatalf("destination %s accepted", dst)
			}
		}
		if !reflect.DeepEqual(before, visualLibraryHashes(t, f.root)) {
			t.Fatal("refused salvage wrote inside the source library")
		}
	})
}

// realAbsentIdentity is a fixture identity in this process's own procfs view
// whose PID cannot exist (it is above any pid_max), so the unmodified CLI
// observes it as absent.
func realAbsentIdentity(t *testing.T) func(int) ProcessIdentity {
	t.Helper()
	view, e := currentProcView()
	if e != nil {
		t.Skipf("procfs view unavailable: %v", e)
	}
	return func(pid int) ProcessIdentity {
		pid += 2147480000
		return ProcessIdentity{PID: pid, Start: "12345", Boot: view.Boot, PIDNamespace: view.Namespace}
	}
}

func TestOfflineVisualCLI(t *testing.T) {
	identity := realAbsentIdentity(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("AREEL_RECORDING", "must-not-use")
	t.Setenv("AREEL_BACKEND", "must-not-discover")
	f := completedVisual(t, identity, true)
	before := visualLibraryHashes(t, f.root)
	var out bytes.Buffer
	if e := CLI([]string{"doctor", f.state.ID, "--library", f.root, "--json"}, &out, io.Discard); e != nil {
		t.Fatalf("%v %s", e, out.String())
	}
	var report struct {
		Format         string `json:"format"`
		ArtifactStatus string `json:"artifactStatus"`
		Visual         struct {
			Status string `json:"status"`
		} `json:"visual"`
		Completion struct {
			Complete bool `json:"complete"`
		} `json:"completion"`
	}
	if e := json.Unmarshal(out.Bytes(), &report); e != nil {
		t.Fatal(e)
	}
	if report.Format != "visual/1" || report.Visual.Status != "ok" || report.ArtifactStatus != "ok" || !report.Completion.Complete {
		t.Fatalf("CLI doctor %s", out.String())
	}
	out.Reset()
	if e := CLI([]string{"salvage", f.state.ID, "--library", f.root}, &out, io.Discard); e == nil || !strings.Contains(e.Error(), "recover") {
		t.Fatalf("removed salvage must suggest recover: %v", e)
	}

	if !reflect.DeepEqual(before, visualLibraryHashes(t, f.root)) {
		t.Fatal("CLI mutated the source library")
	}
}

// visualNeighbour adds a legacy raw recording (the removed format) beside the
// visual one, as an older binary left it: every reader refuses it and nothing
// changes it.
func visualNeighbour(t *testing.T, root string) State {
	t.Helper()
	id := strings.Repeat("d", 32)
	dir := filepath.Join(root, "recordings", id)
	archiveDir := filepath.Join(dir, "segment-0001", "archive")
	if e := os.MkdirAll(archiveDir, 0700); e != nil {
		t.Fatal(e)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	s := State{ID: id, Status: "paused", Target: fakeTarget("legacy-instance"), Segments: []Segment{{Number: 1, Path: "segment-0001/archive", Launched: true, Started: start, Ended: &end}}, Events: []Event{}}
	if e := writeJSON(filepath.Join(dir, "state.json"), s); e != nil {
		t.Fatal(e)
	}
	if e := writeJSON(filepath.Join(dir, "launch.json"), launchSpec{Target: s.Target}); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(archiveDir, "000000.zst"), []byte("opaque raw frame"), 0600); e != nil {
		t.Fatal(e)
	}
	return s
}

func TestLegacyNeighbourRefusedBesideVisual(t *testing.T) {
	f := pausedVisual(t, offlineIdentity)
	legacy := visualNeighbour(t, f.root)
	legacyDir := filepath.Join(f.root, "recordings", legacy.ID)
	legacyBefore := treeHashes(t, legacyDir)
	visualBefore := visualLibraryHashes(t, f.dir)
	ctx := context.Background()
	if _, e := buildExport(ctx, f.root, exportTestSpec(f.state.ID), t.TempDir()); e != nil {
		t.Fatalf("visual export as format=cast: %v", e)
	}
	if _, e := buildExport(ctx, f.root, exportTestSpec(legacy.ID), t.TempDir()); !errors.Is(e, errLegacyRecording) {
		t.Fatalf("legacy export %v", e)
	}
	h, e := newLibraryHandler(f.root, libraryTestHost)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(h.close)
	for _, id := range []string{f.state.ID, legacy.ID} {
		for _, kind := range []string{"cast", "frames"} {
			if w := libraryRequest(h, "/api/recordings/"+id+"/segments/1/"+kind, nil); w.Code != 400 {
				t.Fatalf("%s %s served %d", id, kind, w.Code)
			}
		}
	}
	if w := libraryRequest(h, "/api/recordings/"+legacy.ID, nil); w.Code != 422 {
		t.Fatalf("legacy detail served %d %s", w.Code, w.Body)
	}
	if w := libraryRequest(h, "/api/recordings/"+f.state.ID, nil); w.Code != 200 {
		t.Fatalf("visual detail refused %d %s", w.Code, w.Body)
	}
	if !reflect.DeepEqual(legacyBefore, treeHashes(t, legacyDir)) {
		t.Fatal("legacy neighbour changed")
	}
	if !reflect.DeepEqual(visualBefore, visualLibraryHashes(t, f.dir)) {
		t.Fatal("visual recording changed")
	}
}
