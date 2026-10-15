//go:build linux || darwin

package reel

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
)

const stopFixture = `{"version":3,"term":{"cols":80,"rows":24},"idle_time_limit":1.7976931348623157e308}` + "\n[0,\"o\",\"owned synthetic bytes\"]\n[0,\"x\",\"0\"]\n"

func gatedCapture(t *testing.T, closedMaster bool) (*capture, *io.PipeWriter) {
	t.Helper()
	root := t.TempDir()
	reader, writer := io.Pipe()
	sink := startStreamSink(reader, archive.Geometry{Columns: 80, Rows: 24})
	var master *os.File
	if closedMaster {
		r, w, e := os.Pipe()
		if e != nil {
			t.Fatal(e)
		}
		_ = r.Close()
		_ = w.Close()
		master = w
	} else {
		path := filepath.Join(root, "read-only")
		if e := os.WriteFile(path, nil, 0600); e != nil {
			t.Fatal(e)
		}
		var e error
		if master, e = os.Open(path); e != nil {
			t.Fatal(e)
		}
	}
	c := &capture{master: master, done: make(chan struct{}), sink: sink}
	go func() { <-sink.Done(); c.mu.Lock(); c.err = sink.Err(); c.mu.Unlock(); close(c.done) }()
	t.Cleanup(func() {
		_ = writer.CloseWithError(errors.New("scoped test cleanup"))
		_ = master.Close()
		select {
		case <-c.done:
		case <-time.After(3 * time.Second):
			t.Error("gated sink did not finish")
		}
	})
	return c, writer
}
func TestStopWaitsThroughClosedMasterFinalization(t *testing.T) {
	for _, outcome := range []string{"clean", "sink-failure", "deadline"} {
		t.Run(outcome, func(t *testing.T) {
			c, writer := gatedCapture(t, true)
			if outcome == "deadline" {
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
				defer cancel()
				e := c.Stop(ctx)
				if !errors.Is(e, context.DeadlineExceeded) || errors.Is(e, os.ErrClosed) {
					t.Fatalf("closed master bypassed finalization deadline: %v", e)
				}
				select {
				case <-c.done:
					t.Fatal("deadline falsely completed sink")
				default:
				}
				if c.Err() != nil {
					t.Fatal("command deadline became terminal error", c.Err())
				}
				_, _ = io.WriteString(writer, stopFixture)
				_ = writer.Close()
				<-c.done
				if e := c.Stop(context.Background()); e != nil {
					t.Fatal("later clean completion failed", e)
				}
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- c.Stop(ctx) }()
			select {
			case e := <-result:
				t.Fatalf("returned before gated sink completion: %v", e)
			case <-time.After(40 * time.Millisecond):
			}
			failure := errors.New("injected real sink failure")
			if outcome == "clean" {
				_, _ = io.WriteString(writer, stopFixture)
				_ = writer.Close()
			} else {
				_ = writer.CloseWithError(failure)
			}
			select {
			case e := <-result:
				if outcome == "clean" && e != nil {
					t.Fatal(e)
				}
				if outcome != "clean" && !errors.Is(e, failure) {
					t.Fatal("actual sink failure lost", e)
				}
				select {
				case <-c.done:
				default:
					t.Fatal("Stop returned before Done")
				}
			case <-ctx.Done():
				t.Fatal("Stop did not settle after sink")
			}
		})
	}
}
func TestStopPreservesUnrelatedSendError(t *testing.T) {
	c, _ := gatedCapture(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e := c.Stop(ctx)
	if e == nil || errors.Is(e, context.DeadlineExceeded) || errors.Is(e, os.ErrClosed) || !strings.Contains(e.Error(), "write") {
		t.Fatalf("unrelated read-only transport error swallowed: %v", e)
	}
	select {
	case <-c.done:
		t.Fatal("transport failure released unfinished sink")
	default:
	}
	actual := errors.New("genuine recorder exit failure")
	c.mu.Lock()
	c.err = actual
	c.mu.Unlock()
	if e := c.Stop(ctx); !errors.Is(e, actual) {
		t.Fatal("transport error hid recorder failure", e)
	}
}
