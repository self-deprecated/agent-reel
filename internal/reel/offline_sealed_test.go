package reel

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

func sealedControlFixture(t *testing.T, interrupted bool) (*visualGUI, string) {
	t.Helper()
	g := newVisualGUI(t, "stopped")
	g.begin(1, 16, 4)
	g.view(0, visualScreen("sealed"))
	g.completeAt(1)
	g.acknowledge()
	g.state.Suppressed = true
	if interrupted {
		g.state.Status = "interrupted"
	}
	g.saveState()
	g.closeWriter()
	if e := os.Mkdir(filepath.Join(g.root, "targets"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := writeJSON((&Store{Root: g.root}).targetPath(g.state.Target), g.id); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(g.root, g.id+".areel")
	opts := seal.Options{Limits: visualstore.DefaultLimits(), ActivityLimits: activitystore.DefaultLimits(), Sealer: "test"}
	if interrupted {
		opts.Kind = seal.KindInterrupted
		opts.Recovery = seal.RecoveryInput{SourceStatus: "recording", RecoveredAt: time.Now(), Recoverer: "test"}
	}
	if _, e := seal.Seal(context.Background(), seal.Source{RecordingDir: g.dir}, seal.Destination{Path: path}, opts); e != nil {
		t.Fatal(e)
	}
	return g, path
}

func TestSealedDoctorAndStandalone(t *testing.T) {
	g, path := sealedControlFixture(t, false)
	report, e := offlineRecording(context.Background(), g.root, g.id, "", visualstore.DefaultLimits(), offlineObservation{})
	if e != nil {
		t.Fatal(e)
	}
	v := report
	if v.ArtifactStatus != "ok" || v.Sealed.Kind != seal.KindComplete || v.Shadowed == nil || v.Shadowed.Dir != g.dir || len(v.Ownership) != 0 || v.Diagnosis != "sealed file; no process owns it" || !v.Completion.Complete {
		t.Fatalf("doctor: %+v", v)
	}
	var out bytes.Buffer
	if e = formatOffline(&out, report); e != nil || !strings.Contains(out.String(), g.id[:8]) || !strings.Contains(out.String(), "sealed") {
		t.Fatalf("sealed doctor output: %q %v", out.String(), e)
	}
	out.Reset()
	if e = CLI([]string{"doctor", path, "--json"}, &out, &out); e != nil {
		t.Fatal(e)
	}
	var actual seal.Report
	if e = json.Unmarshal(out.Bytes(), &actual); e != nil {
		t.Fatal(e)
	}
	expected, e := seal.Verify(context.Background(), path, visualstore.DefaultLimits())
	if e != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("standalone differs: %v", e)
	}
}

func TestSealedDoctorDamage(t *testing.T) {
	for _, item := range []string{"screen 1", "seal"} {
		t.Run(item, func(t *testing.T) {
			g, path := sealedControlFixture(t, false)
			db, e := sql.Open("sqlite", path)
			if e != nil {
				t.Fatal(e)
			}
			if item == "seal" {
				_, e = db.Exec("UPDATE seal SET manifest=manifest||' '")
			} else {
				var payload []byte
				e = db.QueryRow("SELECT payload FROM row_chunks WHERE first_id=1").Scan(&payload)
				if e == nil {
					payload[len(payload)/2] ^= 1
					_, e = db.Exec("UPDATE row_chunks SET payload=? WHERE first_id=1", payload)
				}
			}
			if e != nil {
				t.Fatal(e)
			}
			if e = db.Close(); e != nil {
				t.Fatal(e)
			}
			r, e := offlineRecording(context.Background(), g.root, g.id, "", visualstore.DefaultLimits(), offlineObservation{})
			if e != nil || r.ArtifactStatus != "damaged" || r.Sealed == nil || r.Sealed.FailedItem != item {
				t.Fatalf("doctor %s: %+v %v", item, r.Sealed, e)
			}
		})
	}
}

func TestSealedControlReadsAndListing(t *testing.T) {
	g, path := sealedControlFixture(t, false)
	if e := os.RemoveAll(g.dir); e != nil {
		t.Fatal(e)
	}
	store := &Store{Root: g.root}
	before, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	info, e := os.Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	names := func() []string {
		var out []string
		e := filepath.WalkDir(g.root, func(p string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			out = append(out, p)
			return nil
		})
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	entries := names()
	v, e := store.read(g.id)
	if e != nil || !reflect.DeepEqual(v, g.state) {
		t.Fatalf("read: %+v %v", v, e)
	}
	for _, command := range []string{"ensure", "status", "pause", "stop"} {
		v, e = store.Control(context.Background(), g.state.Target, command, "", "")
		if e != nil || v.Status != "stopped" || v.ID != g.id {
			t.Fatalf("%s: %+v %v", command, v, e)
		}
	}
	for _, command := range []string{"status", "stop"} {
		if _, e = store.ControlBound(g.id, command); e != nil {
			t.Fatal(e)
		}
	}
	rows, e := listRecordings(context.Background(), g.root, "", 1, listDefaults())
	if e != nil || len(rows.Items) != 1 || rows.Items[0].Holder != "sealed" {
		t.Fatalf("ls: %+v %v", rows, e)
	}
	var out bytes.Buffer
	if e = CLI([]string{"resume", g.id, "--library", g.root}, &out, &out); e == nil {
		t.Fatal("bound resume unexpectedly accepted")
	}
	if got := names(); !reflect.DeepEqual(entries, got) {
		t.Fatalf("read created entries: before %v after %v", entries, got)
	}
	after, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	afterInfo, e := os.Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(before, after) || !os.SameFile(info, afterInfo) || !info.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("sealed file changed")
	}
	if got := recordingSize(g.root, g.id); got != info.Size() {
		t.Fatalf("size=%d want %d", got, info.Size())
	}
}

func TestSealedStart(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "start", true: "recreate"}[interrupted], func(t *testing.T) {
			g, _ := sealedControlFixture(t, interrupted)
			if e := os.RemoveAll(g.dir); e != nil {
				t.Fatal(e)
			}
			launchErr := errors.New("fixture launch reached")
			newID := ""
			store := &Store{Root: g.root, Launch: func(root, id string) error { newID = id; return launchErr }}
			var e error
			if interrupted {
				_, e = store.Recreate(context.Background(), g.state.Target, "")
			} else {
				_, e = store.Control(context.Background(), g.state.Target, "start", "", "")
			}
			if !errors.Is(e, launchErr) || newID == "" || newID == g.id {
				t.Fatalf("new recording not launched: %s %v", newID, e)
			}
			v, e := store.read(newID)
			if e != nil || v.Target != g.state.Target {
				t.Fatalf("new state: %+v %v", v, e)
			}
		})
	}
}

func TestSealedProbe(t *testing.T) {
	g, _ := sealedControlFixture(t, false)
	if e := os.RemoveAll(g.dir); e != nil {
		t.Fatal(e)
	}
	other := strings.Repeat("b", 32)
	dir := filepath.Join(g.root, "recordings", other)
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	state := g.state
	state.ID = other
	state.Status = "paused"
	state.Target = fakeTarget("other")
	if e := writeJSON(filepath.Join(dir, "state.json"), state); e != nil {
		t.Fatal(e)
	}
	if e := writeJSON(filepath.Join(dir, "launch.json"), launchSpec{Target: state.Target}); e != nil {
		t.Fatal(e)
	}
	if e := writeJSON((&Store{Root: g.root}).targetPath(state.Target), other); e != nil {
		t.Fatal(e)
	}
	listener, e := net.Listen("unix", filepath.Join(dir, "control.sock"))
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	rows, e := listRecordings(context.Background(), g.root, "", 2, listDefaults())
	if e != nil {
		t.Fatal(e)
	}
	for _, row := range rows.Items {
		want := "sealed"
		if row.ID == other {
			want = "reachable"
		}
		if row.Holder != want {
			t.Fatalf("holder %+v", row)
		}
	}
	listener.(*net.UnixListener).SetDeadline(time.Now().Add(time.Second))
	conn, e := listener.Accept()
	if e != nil {
		t.Fatal("directory was not probed:", e)
	}
	conn.Close()
}
