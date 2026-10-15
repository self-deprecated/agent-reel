//go:build linux || darwin

package reel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// These tests cover the visual/1 capture wiring (V5 Run 2) without Zellij or
// asciinema: the owned PTY runs /bin/sh, and Zellij.Start runs against stub
// executables that only answer version/geometry queries or copy a fixture into
// the FIFO. The readout helper is the pinned areel-avt-checkpoint when
// AREEL_TEST_AVT names it, otherwise the fake helper below.

// fakeAVTArg makes this test binary serve the helper protocol instead of
// running tests. It is reached only through the wrapper script written by
// visualHelper, never through the environment, so no other subprocess of the
// test binary can enter it.
const fakeAVTArg = "_fake_avt"

func init() {
	if len(os.Args) == 2 && os.Args[1] == fakeAVTArg {
		serveFakeAVT(os.Stdin, os.Stdout)
		os.Exit(0)
	}
}

// fakeAVT is NOT an emulator. Printable text is written at the cursor, CR and
// LF move it, escape sequences are skipped, and every write dirties its row.
// It speaks the helper's exact wire protocol so the real castexport.AVT client
// and the real visualcapture pipeline run unchanged against it.
type fakeAVT struct {
	cols, rows int
	col, row   int
	cells      [][]castexport.Cell
	dirty      map[int]bool
	esc        int // 0 none, 1 after ESC, 2 CSI, 3 OSC
}

func (f *fakeAVT) reset(cols, rows int) {
	f.cols, f.rows, f.col, f.row = cols, rows, 0, 0
	f.cells = make([][]castexport.Cell, rows)
	f.dirty = map[int]bool{}
	for r := range f.cells {
		f.cells[r] = make([]castexport.Cell, cols)
		for c := range f.cells[r] {
			f.cells[r][c] = castexport.Cell{Char: ' ', Width: 1}
		}
		f.dirty[r] = true
	}
}

func (f *fakeAVT) feed(data []byte) {
	for len(data) > 0 {
		r, n := utf8.DecodeRune(data)
		data = data[n:]
		switch f.esc {
		case 1:
			switch r {
			case '[':
				f.esc = 2
			case ']':
				f.esc = 3
			default:
				f.esc = 0
			}
			continue
		case 2:
			if r >= 0x40 && r <= 0x7e {
				f.esc = 0
			}
			continue
		case 3:
			if r == 0x07 || r == 0x1b {
				f.esc = 0
			}
			continue
		}
		switch {
		case r == 0x1b:
			f.esc = 1
		case r == '\r':
			f.col = 0
		case r == '\n':
			f.row = min(f.row+1, f.rows-1)
		case r < 0x20 || r == 0x7f:
		default:
			if f.col >= f.cols {
				f.col = f.cols - 1
			}
			f.cells[f.row][f.col] = castexport.Cell{Char: r, Width: 1}
			f.dirty[f.row] = true
			f.col++
		}
	}
}

func (f *fakeAVT) header(w *bufio.Writer) {
	for _, v := range []int{f.cols, f.rows, f.col, f.row} {
		_ = binary.Write(w, binary.BigEndian, uint32(v))
	}
	_ = w.WriteByte(1)
}

func serveFakeAVT(in io.Reader, out io.Writer) {
	r := bufio.NewReader(in)
	w := bufio.NewWriter(out)
	u32 := func() (int, bool) {
		var b [4]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, false
		}
		return int(binary.BigEndian.Uint32(b[:])), true
	}
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != "AVT1" {
		return
	}
	cols, ok1 := u32()
	rows, ok2 := u32()
	if !ok1 || !ok2 {
		return
	}
	f := &fakeAVT{}
	f.reset(cols, rows)
	for {
		op, err := r.ReadByte()
		if err != nil {
			return
		}
		switch op {
		case 'F':
			n, ok := u32()
			if !ok {
				return
			}
			data := make([]byte, n)
			if _, err := io.ReadFull(r, data); err != nil {
				return
			}
			f.feed(data)
		case 'R':
			c, ok1 := u32()
			rr, ok2 := u32()
			if !ok1 || !ok2 {
				return
			}
			f.reset(c, rr)
		case 'V':
			f.header(w)
			var payload []byte
			for _, row := range f.cells {
				payload = append(payload, castexport.EncodeCells(row)...)
			}
			_ = binary.Write(w, binary.BigEndian, uint32(len(payload)))
			_, _ = w.Write(payload)
			f.dirty = map[int]bool{}
		case 'W':
			f.header(w)
			var rowsOut []int
			for i := 0; i < f.rows; i++ {
				if f.dirty[i] {
					rowsOut = append(rowsOut, i)
				}
			}
			_ = binary.Write(w, binary.BigEndian, uint32(len(rowsOut)))
			for _, i := range rowsOut {
				_ = binary.Write(w, binary.BigEndian, uint32(i))
				_, _ = w.Write(castexport.EncodeCells(f.cells[i]))
			}
			f.dirty = map[int]bool{}
		case 'P':
			_, _ = w.WriteString("AVTP")
			_ = binary.Write(w, binary.BigEndian, uint32(castexport.HelperProtocol))
		case 'D':
			for _, v := range []int{f.cols, f.rows, 0} {
				_ = binary.Write(w, binary.BigEndian, uint32(v))
			}
		default:
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

// visualHelper returns the readout helper for these tests: the pinned helper
// when AREEL_TEST_AVT names it, otherwise a wrapper that re-executes this test
// binary as the fake helper.
func visualHelper(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("AREEL_TEST_AVT"); p != "" {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("AREEL_TEST_AVT: %v", err)
		}
		return p
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fake-avt")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec "+shellQuote(self)+" "+fakeAVTArg+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// visualSentinel is fixture output that must reach no file outside the store.
const visualSentinel = "V5SENTINEL-7c1f0e"

func visualCastHeader(cols, rows int) string {
	return fmt.Sprintf("{\"version\":3,\"term\":{\"cols\":%d,\"rows\":%d},\"idle_time_limit\":%g}\n", cols, rows, math.MaxFloat64)
}
func visualCastEvent(delta, kind, data string) string {
	k, _ := json.Marshal(kind)
	d, _ := json.Marshal(data)
	return fmt.Sprintf("[%s,%s,%s]\n", delta, k, d)
}

// visualRecordingDir is a private recording directory with a short path.
func visualRecordingDir(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "ar-v-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dir := filepath.Join(root, "0123456789abcdef0123456789abcdef")
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func visualConfig(recording, id string, segment int, helper string) visualcapture.Config {
	return visualcapture.Config{
		Store:        filepath.Join(recording, visualcapture.StoreDirName),
		Acknowledged: filepath.Join(recording, visualcapture.AcknowledgedName),
		Identity:     archive.Identity{Recording: id, Segment: segment, Backend: "fake", Instance: "visual"},
		Segment:      segment, Geometry: archive.Geometry{Columns: 80, Rows: 24},
		Helper: helper, Limits: visualcapture.Defaults(),
	}
}

func visualSnapshot(t *testing.T, recording string) visualstore.Selection {
	t.Helper()
	r, err := visualstore.OpenReader(filepath.Join(recording, visualcapture.StoreDirName))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sel, err := r.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func visualVerify(t *testing.T, recording string) (visualcapture.Acknowledged, visualstore.Report) {
	t.Helper()
	ack, err := visualcapture.LoadAcknowledged(filepath.Join(recording, visualcapture.AcknowledgedName))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := visualstore.Verify(context.Background(), filepath.Join(recording, visualcapture.StoreDirName), ack.Expectation(), visualstore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return ack, rep
}

// requireNoRawDuplicate is B7: no segment archive directory exists and the
// sentinel appears in no file outside visual/visual.sqlite*.
func requireNoRawDuplicate(t *testing.T, recording string) {
	t.Helper()
	store := filepath.Join(recording, visualcapture.StoreDirName)
	sawStore := false
	err := filepath.WalkDir(recording, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(recording, path)
		if d.IsDir() {
			if d.Name() == "archive" && strings.HasPrefix(rel, "segment-") {
				t.Errorf("raw archive directory %s exists for a visual/1 recording", rel)
			}
			return nil
		}
		if filepath.Dir(path) == store && strings.HasPrefix(d.Name(), "visual.sqlite") {
			sawStore = true
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(visualSentinel)) {
			t.Errorf("fixture output duplicated outside the visual store in %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawStore {
		t.Fatal("visual store database missing")
	}
}

// requireVisualObserver checks the capability set of the production visual
// observer: progress, placement and stop intent, but no legacy footage anchor.
func requireVisualObserver(t *testing.T, o Observer) *visualCapture {
	t.Helper()
	v, ok := o.(*visualCapture)
	if !ok {
		t.Fatalf("observer is %T, not *visualCapture", o)
	}
	var o2 Observer = v
	if _, ok := o2.(visualProgressReporter); !ok {
		t.Fatal("visual observer does not report visual progress")
	}
	if _, ok := o2.(placementSource); !ok {
		t.Fatal("visual observer is not a placement source")
	}
	if _, ok := o2.(finalIntent); !ok {
		t.Fatal("visual observer does not take stop intent")
	}
	return v
}

// TestVisualOwnedPTYCapture is B2 (and B7): the real owned PTY/FIFO lifecycle
// feeding a real visualcapture.Capture, with /bin/sh as the recorder.
func TestVisualOwnedPTYCapture(t *testing.T) {
	helper := visualHelper(t)
	full := visualCastHeader(80, 24) + visualCastEvent("0.25", "o", visualSentinel+"\r\n") + visualCastEvent("0.5", "x", "0")
	noExit := visualCastHeader(80, 24) + visualCastEvent("0.25", "o", visualSentinel+"\r\n")
	for _, tc := range []struct {
		name, fixture string
		final, fails  bool
	}{
		{"pause", full, false, false},
		{"stop", full, true, false},
		{"missing-x0", noExit, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recording := visualRecordingDir(t)
			segment := filepath.Join(recording, "segment-0001")
			if err := privateDir(segment); err != nil {
				t.Fatal(err)
			}
			fixture := filepath.Join(t.TempDir(), "fixture.cast")
			if err := os.WriteFile(fixture, []byte(tc.fixture), 0600); err != nil {
				t.Fatal(err)
			}
			sink, err := visualcapture.New(visualConfig(recording, "0123456789abcdef0123456789abcdef", 1, helper))
			if err != nil {
				t.Fatal(err)
			}
			cast := filepath.Join(segment, "capture.fifo")
			fifo, err := archive.OpenFIFO(cast)
			if err != nil {
				_ = sink.Close()
				t.Fatal(err)
			}
			sink.Start(fifo.Reader)
			cmd := exec.Command("/bin/sh", "-c", "cat "+shellQuote(fixture)+" > "+shellQuote(cast)+"; sleep 0.2")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			observer, err := startVisualCapture(ctx, cmd, sink, fifo, Observation{Geometry: Geometry{80, 24}}, segment)
			if observer == nil {
				t.Fatalf("launch failed: %v", err)
			}
			defer func() {
				stopCtx, c := context.WithTimeout(context.Background(), 8*time.Second)
				defer c()
				_ = observer.Stop(stopCtx)
			}()
			if err != nil {
				t.Fatalf("startup: %v", err)
			}
			v := requireVisualObserver(t, observer)
			if !observer.Observation().OutputObserved {
				t.Fatal("output not observed at readiness")
			}
			if tc.final {
				v.SetFinal(true)
			}
			select {
			case <-observer.Done():
			case <-ctx.Done():
				t.Fatal("owned synthetic recorder did not finish")
			}
			if _, err := os.Stat(filepath.Join(segment, "recorder.json")); err != nil {
				t.Fatal("recorder identity not written", err)
			}
			if tc.fails {
				err := observer.Err()
				if err == nil || archive.IsStorageFailure(err) {
					t.Fatalf("missing x0 must be a plain capture failure: %v", err)
				}
				sel := visualSnapshot(t, recording)
				if sel.Complete || len(sel.Segments) != 1 || sel.Segments[0].State != visualstore.StateLive {
					t.Fatalf("recorder failure left %+v", sel)
				}
				return
			}
			if err := observer.Err(); err != nil {
				t.Fatal(err)
			}
			sel := visualSnapshot(t, recording)
			ack, rep := visualVerify(t, recording)
			if sel.ViewSeq != 2 || ack.Views != 2 || v.VisualProgress().Views != 2 {
				t.Fatalf("views: store %d, acknowledged %d, stats %+v", sel.ViewSeq, ack.Views, v.VisualProgress())
			}
			want := visualstore.StateClosed
			if tc.final {
				want = visualstore.StateComplete
			}
			if len(sel.Segments) != 1 || sel.Segments[0].State != want || sel.Complete != tc.final || ack.Complete != tc.final {
				t.Fatalf("segment boundary %+v ack %+v", sel, ack)
			}
			if tc.final && rep.Status != visualstore.StatusOK {
				t.Fatalf("stopped capture verifies %+v", rep)
			}
			if !tc.final && rep.Status == visualstore.StatusDamaged {
				t.Fatalf("paused capture verifies %+v", rep)
			}
			if v.VisualProgress().DurableSourceNs != 750_000_000 {
				t.Fatalf("durable footage %+v", v.VisualProgress())
			}
			requireNoRawDuplicate(t, recording)
		})
	}
}

// zellijStubs writes stub zellij and asciinema executables. The asciinema stub
// records each invocation and, for a session, copies the fixture into its
// --output-file before exiting like a recorder that wrote x0.
func zellijStubs(t *testing.T, root string, cols, rows int, fixture string) (*Zellij, string) {
	t.Helper()
	calls := filepath.Join(root, "asciinema-calls")
	zscript := fmt.Sprintf(`#!/bin/sh
case "$*" in
--version) echo 'zellij 0.44.3';;
*list-clients*) printf 'CLIENT_ID ZELLIJ_PANE_ID RUNNING_COMMAND\n1 terminal_0 N/A\n';;
*list-tabs*) echo '[{"active":true,"display_area_columns":%d,"display_area_rows":%d}]';;
esac
`, cols, rows)
	ascript := `#!/bin/sh
if [ "$1" = --version ]; then echo 'asciinema 3.2.1'; exit; fi
echo "$*" >> ` + shellQuote(calls) + `
out=
while [ $# -gt 0 ]; do
  if [ "$1" = --output-file ]; then out=$2; fi
  shift
done
cat ` + shellQuote(fixture) + ` > "$out"
sleep 0.2
`
	zpath := filepath.Join(root, "zellij")
	apath := filepath.Join(root, "asciinema")
	if err := os.WriteFile(zpath, []byte(zscript), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(apath, []byte(ascript), 0700); err != nil {
		t.Fatal(err)
	}
	return &Zellij{Executable: zpath, Asciinema: apath, SocketRoot: root, Self: zpath}, calls
}

// TestZellijStartVisual is B2's backend half: Zellij.Start for visual/1 with
// stub executables. A missing helper or an unsupported geometry refuses before
// any recorder process or storage exists; a supported start records into the
// recording's visual store with no raw archive.
func TestZellijStartVisual(t *testing.T) {
	helper := visualHelper(t)
	for _, tc := range []struct {
		name       string
		helper     string
		cols, rows int
	}{
		{"missing-helper", "/nonexistent/areel-avt-checkpoint", 80, 24},
		{"unsupported-geometry", helper, 600, 24},
		{"stop", helper, 80, 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recording := visualRecordingDir(t)
			stubs := t.TempDir()
			fixture := filepath.Join(stubs, "fixture.cast")
			body := visualCastHeader(80, 24) + visualCastEvent("0.1", "o", visualSentinel+"\r\n") + visualCastEvent("0.1", "x", "0")
			if err := os.WriteFile(fixture, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			z, calls := zellijStubs(t, stubs, tc.cols, tc.rows, fixture)
			z.Helper = tc.helper
			target, err := z.Resolve(context.Background(), "target")
			if err != nil {
				t.Fatal(err)
			}
			segment := filepath.Join(recording, "segment-0001")
			spec := CaptureSpec{Directory: segment, RecordingID: filepath.Base(recording), Segment: 1, Format: activityFormatIndexed}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			observer, err := z.Start(ctx, target, spec)
			if tc.name != "stop" {
				if observer != nil || err == nil || archive.IsStorageFailure(err) {
					t.Fatalf("refusal must be a plain prelaunch error with no observer: %v %v", observer, err)
				}
				if _, e := os.Stat(calls); !os.IsNotExist(e) {
					t.Fatal("recorder launched despite refusal")
				}
				for _, name := range []string{visualcapture.StoreDirName, visualcapture.AcknowledgedName} {
					if _, e := os.Lstat(filepath.Join(recording, name)); !os.IsNotExist(e) {
						t.Fatalf("refusal created %s", name)
					}
				}
				return
			}
			if observer == nil || err != nil {
				t.Fatalf("visual start: %v %v", observer, err)
			}
			v := requireVisualObserver(t, observer)
			v.SetFinal(true)
			select {
			case <-observer.Done():
			case <-ctx.Done():
				t.Fatal("stub recorder did not finish")
			}
			if err := observer.Err(); err != nil {
				t.Fatal(err)
			}
			if _, e := os.Lstat(filepath.Join(segment, "archive")); !os.IsNotExist(e) {
				t.Fatal("visual/1 start created a raw archive directory")
			}
			for _, name := range []string{"watch.json", "recorder.json", filepath.Join("config", "config.toml")} {
				if _, e := os.Stat(filepath.Join(segment, name)); e != nil {
					t.Fatalf("segment %s missing: %v", name, e)
				}
			}
			sel := visualSnapshot(t, recording)
			ack, rep := visualVerify(t, recording)
			if !sel.Complete || !ack.Complete || rep.Status != visualstore.StatusOK || sel.ViewSeq != 2 {
				t.Fatalf("stopped visual start: %+v %+v %+v", sel, ack, rep)
			}
			requireNoRawDuplicate(t, recording)
			// The recorder follows its PTY's size: a fixed --window-size would
			// pin the segment's geometry and defeat following the session.
			argv, err := os.ReadFile(calls)
			if err != nil || !strings.HasPrefix(string(argv), "session ") || strings.Contains(string(argv), "--window-size") {
				t.Fatalf("recorder argv %q %v", argv, err)
			}
			if f := observer.Observation().Follow; f == nil || f.Current != (Geometry{80, 24}) {
				t.Fatalf("follow account %+v", f)
			}
		})
	}
}
