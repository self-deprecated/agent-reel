package sealsplit

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/loopoc"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// Capture runs one asciicast v3 stream through the real pipeline exactly as
// the loopoc harness captures its corpora (internal/loopoc/capture_test.go):
// visualcapture.New with the pinned helper, production Limits, the header
// swapped for idle_time_limit = MaxFloat64, and QualifyHooks.LogicalFlush so
// commits follow the production cadence on logical time. write streams the
// cast (header line first) and must return when done. The store is created
// under rec, which is emptied first.
func Capture(helper, rec, name string, cols, rows int, write func(io.Writer) error) (loopoc.Corpus, CaptureInfo, error) {
	return captureIdentity(helper, rec, name, cols, rows, archive.Identity{Recording: "sealsplit-" + name, Backend: "loopoc", Instance: "poc"}, write)
}

func captureIdentity(helper, rec, name string, cols, rows int, identity archive.Identity, write func(io.Writer) error) (loopoc.Corpus, CaptureInfo, error) {
	var info CaptureInfo
	if err := os.RemoveAll(rec); err != nil {
		return loopoc.Corpus{}, info, err
	}
	if err := os.MkdirAll(rec, 0o700); err != nil {
		return loopoc.Corpus{}, info, err
	}
	limits := visualcapture.Defaults()
	var mu sync.Mutex
	var commits []int64
	cfg := visualcapture.Config{
		Store:        filepath.Join(rec, visualcapture.StoreDirName),
		Acknowledged: filepath.Join(rec, visualcapture.AcknowledgedName),
		Identity:     identity,
		Segment:      1,
		Geometry:     archive.Geometry{Columns: cols, Rows: rows},
		Helper:       helper,
		Limits:       limits,
		Qualify: &visualcapture.QualifyHooks{LogicalFlush: limits.FlushAge, OnBatch: func(b visualcapture.BatchSample) {
			mu.Lock()
			defer mu.Unlock()
			info.Batches++
			if len(commits) == 0 || b.Views > commits[len(commits)-1] {
				commits = append(commits, b.Views)
			}
		}},
	}
	start := time.Now()
	c, err := visualcapture.New(cfg)
	if err != nil {
		return loopoc.Corpus{}, info, err
	}
	c.SetFinal(true)
	pr, pw := io.Pipe()
	c.Start(pr)
	counter := &countWriter{}
	fed := make(chan error, 1)
	go func() {
		h := &headerSwap{w: io.MultiWriter(pw, counter), header: fmt.Sprintf("{\"version\":3,\"term\":{\"cols\":%d,\"rows\":%d},\"idle_time_limit\":%g}\n", cols, rows, math.MaxFloat64)}
		err := write(h)
		fed <- err
		pw.CloseWithError(err)
	}()
	<-c.Done()
	if err := c.Err(); err != nil {
		pr.CloseWithError(err)
		<-fed
		return loopoc.Corpus{}, info, fmt.Errorf("capture: %w", err)
	}
	if err := <-fed; err != nil {
		return loopoc.Corpus{}, info, fmt.Errorf("feed: %w", err)
	}
	info.WallMs, info.CastBytes, info.StoreDir = time.Since(start).Milliseconds(), counter.n, rec
	db, err := OpenRO(filepath.Join(cfg.Store, visualstore.DatabaseName))
	if err != nil {
		return loopoc.Corpus{}, info, err
	}
	defer db.Close()
	views, err := ReadViews(db)
	if err != nil {
		return loopoc.Corpus{}, info, err
	}
	mu.Lock()
	commits = slices.Clone(commits)
	mu.Unlock()
	return loopoc.Corpus{Name: name, Kind: "captured", Cols: cols, Rows: rows, Views: views, Commits: commits}, info, nil
}

// CaptureInfo describes one Capture run.
type CaptureInfo struct {
	WallMs    int64  `json:"capture_wall_ms"`
	CastBytes int64  `json:"cast_bytes"`
	Batches   int    `json:"batches"`
	StoreDir  string `json:"-"`
}

// headerSwap replaces the first line with header (copied from the harness).
type headerSwap struct {
	w      io.Writer
	header string
	done   bool
}

func (h *headerSwap) Write(p []byte) (int, error) {
	n := len(p)
	if !h.done {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			return n, nil
		}
		if _, err := io.WriteString(h.w, h.header); err != nil {
			return 0, err
		}
		h.done, p = true, p[i+1:]
	}
	if _, err := h.w.Write(p); err != nil {
		return 0, err
	}
	return n, nil
}
