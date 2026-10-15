package videoexport

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func request(t *testing.T) Request {
	t.Helper()
	return Request{Cast: strings.NewReader("cast"), Output: filepath.Join(t.TempDir(), "out.mp4"), Codec: "h264", FPS: 2, Width: 640, Height: 480, Spans: []Span{{Start: 0, End: 1}, {Start: 10, End: 11}}}
}
func TestTimingOmittedGapsAndFractionalEnd(t *testing.T) {
	q := request(t)
	q.Spans[1].End = 10.75
	got, err := timing(q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Frames != 4 || got.VideoSeconds != 2 || got.Timing[1].FirstFrame != 2 || got.Timing[1].Start != 10 || got.Timing[1].End != 10.75 {
		t.Fatalf("%+v", got)
	}
	for _, span := range got.Timing {
		for n := 0; n < span.Frames; n++ {
			if span.Start+float64(n)/float64(got.FPS) >= span.End {
				t.Fatal("sample outside half-open selection")
			}
		}
	}
}
func TestFractionalFrameBoundary(t *testing.T) {
	q := request(t)
	q.FPS = 10
	q.Spans = []Span{{Start: 1, End: 1.1}}
	got, err := timing(q)
	if err != nil || got.Frames != 1 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestCastRendererAdmission(t *testing.T) {
	for _, text := range []string{
		`{"version":3,"term":{"cols":99999,"rows":24}}` + "\n[0,\"o\",\"x\"]\n",
		`{"version":3,"term":{"cols":80,"rows":24}}` + "\n[0,\"r\",\"100000x24\"]\n",
		`{"version":3,"term":{"cols":80,"rows":24}}` + "\n[0,\"r\",\"80x24junk\"]\n",
		`{"version":3,"term":{"cols":80,"rows":24}}` + "\n[0,\"o\"]\n",
	} {
		path := filepath.Join(t.TempDir(), "source.cast")
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if err := admitCast(context.Background(), path); err == nil {
			t.Fatal("unsafe input admitted")
		}
	}
}

func TestTimingRefusesUnboundedAndAmbiguous(t *testing.T) {
	for _, name := range []string{"codec", "fps", "geometry", "nan", "infinity", "overlap", "empty", "frames"} {
		t.Run(name, func(t *testing.T) {
			q := request(t)
			switch name {
			case "codec":
				q.Codec = "unknown"
			case "fps":
				q.FPS = 0
			case "geometry":
				q.Width = 641
			case "nan":
				q.Spans[0].Start = math.NaN()
			case "infinity":
				q.Spans[0].End = math.Inf(1)
			case "overlap":
				q.Spans[1].Start = 0.5
			case "empty":
				q.Spans = nil
			case "frames":
				q.Spans = []Span{{End: 1e8}}
			}
			if _, err := timing(q); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
}
func TestPrerequisitesExplicit(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("missing prerequisites accepted")
	}
}
func TestAdmissionCancellationAndInputFailurePublishNothing(t *testing.T) {
	r := &Renderer{}
	q := request(t)
	permit <- struct{}{}
	if _, err := r.Render(context.Background(), q); !errors.Is(err, ErrBusy) {
		t.Fatalf("%v", err)
	}
	<-permit
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Render(ctx, q); err == nil {
		t.Fatal("canceled job accepted")
	}
	q.Cast = failedReader{}
	if _, err := r.Render(context.Background(), q); err == nil {
		t.Fatal("source failure accepted")
	}
	entries, err := os.ReadDir(filepath.Dir(q.Output))
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial output/staging retained: %v %v", entries, err)
	}
	if len(permit) != 0 {
		t.Fatal("permit leaked")
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, errors.New("synthetic source error") }
func TestExistingOutputIsNeverReplaced(t *testing.T) {
	q := request(t)
	if err := os.WriteFile(q.Output, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Renderer{}).Render(context.Background(), q); err == nil {
		t.Fatal("existing output accepted")
	}
	got, _ := os.ReadFile(q.Output)
	if string(got) != "existing" {
		t.Fatal("existing output changed")
	}
}
func TestBoundedDiagnostics(t *testing.T) {
	b := &boundedLog{}
	n, err := b.Write([]byte(strings.Repeat("x", 10000)))
	if err != nil || n != 10000 || len(b.String()) != 4096 {
		t.Fatal("unbounded diagnostics")
	}
}
func TestConfigCopiesAssets(t *testing.T) {
	if !platformSupported() {
		return
	}
	asset := []byte("x")
	r, err := New(Config{Chromium: os.Args[0], FFmpeg: os.Args[0], FFprobe: os.Args[0], Assets: Assets{asset, asset, asset, nil}})
	if err != nil {
		t.Fatal(err)
	}
	asset[0] = 'y'
	if !reflect.DeepEqual(r.config.Assets.PlayerJS, []byte("x")) {
		t.Fatal("caller can mutate assets")
	}
}
