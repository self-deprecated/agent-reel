package textindex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// TestMeasureLongRecording is the memo §7 phase-2 size/time measurement on a
// synthetic long recording (AREEL_TEXTINDEX_MEASURE=1; AREEL_TEXTINDEX_HOURS
// sets the length, default 8). Shape: 160x48, a view every 250 ms; every view
// scrolls one new log line in and updates a spinner status line whose elapsed
// counter changes every second, so the store holds ~5 distinct rows per
// second. One unique needle line is printed once at a known instant. It
// seals the store, indexes the sealed file and reports seal vs index time
// and size; it fails if the needle is not found at its instant and row.
// Speed and size are reported, not gated.
func TestMeasureLongRecording(t *testing.T) {
	if os.Getenv("AREEL_TEXTINDEX_MEASURE") != "1" {
		t.Skip("set AREEL_TEXTINDEX_MEASURE=1 to measure a long synthetic recording")
	}
	hours := 8.0
	if v := os.Getenv("AREEL_TEXTINDEX_HOURS"); v != "" {
		var err error
		if hours, err = strconv.ParseFloat(v, 64); err != nil || hours <= 0 {
			t.Fatal("AREEL_TEXTINDEX_HOURS", v)
		}
	}
	const cols, rows = 160, 48
	const step = 250 * time.Millisecond
	views := int(time.Duration(hours*float64(time.Hour)) / step)
	needleView := views * 2 / 3
	root := t.TempDir()
	must(t, os.Chmod(root, 0700))
	dir := filepath.Join(root, "recordings", "fixture")
	must(t, os.MkdirAll(dir, 0700))
	id := archive.Identity{Recording: "fixture", Backend: "fake", Instance: "test"}
	store, err := visualstore.Create(filepath.Join(dir, "visual"), id)
	must(t, err)
	must(t, store.BeginSegment(1, archive.Geometry{Columns: cols, Rows: rows}, "exact"))
	frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	words := []string{"INFO", "DEBUG", "WARN", "compile", "request", "cache", "fetch", "render", "test", "commit"}
	logLine := func(n int) string {
		if n == needleView {
			return "NEEDLE-4f2a the build finished with exit code 0"
		}
		return fmt.Sprintf("%06d %s %s id=%08x path=/api/items/%d took %dms user=u%d", n, words[n%len(words)], words[(n/7)%len(words)], uint32(n)*2654435761, n%9973, (n*37)%500, n%311)
	}
	fill := func(dst []castexport.Cell, text string) {
		c := 0
		for _, r := range text {
			if c >= len(dst) {
				break
			}
			dst[c] = castexport.Cell{Char: r, Width: 1}
			c++
		}
		for ; c < len(dst); c++ {
			dst[c] = castexport.Cell{Char: ' ', Width: 1}
		}
	}
	started := time.Now()
	var needleAt visualtime.Time
	needleY := -1
	for v := 0; v < views; v++ {
		screen := castexport.Screen{Cols: cols, Rows: rows, Cells: make([][]castexport.Cell, rows)}
		for r := 0; r < rows-1; r++ {
			screen.Cells[r] = make([]castexport.Cell, cols)
			n := v - (rows - 2 - r)
			if n >= 0 {
				fill(screen.Cells[r], logLine(n))
			} else {
				fill(screen.Cells[r], "")
			}
		}
		screen.Cells[rows-1] = make([]castexport.Cell, cols)
		elapsed := int(time.Duration(v) * step / time.Second)
		fill(screen.Cells[rows-1], fmt.Sprintf("%c working (%ds · esc to interrupt)", frames[v%len(frames)], elapsed))
		at := visualtime.Time(time.Duration(v) * step)
		if v == needleView {
			needleAt, needleY = at, rows-2
		}
		p := visualtime.Point{Line: int64(v + 1), At: at, Known: true}
		ok, err := store.Record(visualstore.View{Segment: 1, Point: p, Screen: screen})
		must(t, err)
		if !ok {
			t.Fatal("view coalesced", v)
		}
		if (v+1)%2000 == 0 {
			must(t, store.Commit(visualtime.Cutoff{Segment: 1, Source: p, Presentations: uint64(v + 1)}))
		}
	}
	end := visualtime.Point{Line: int64(views + 1), At: visualtime.Time(time.Duration(views) * step), Known: true}
	must(t, store.Complete(visualtime.Cutoff{Segment: 1, Source: end, SourceComplete: true, Presentations: uint64(views)}))
	must(t, store.Close())
	writeTime := time.Since(started)
	r, err := visualstore.OpenReader(filepath.Join(dir, "visual"))
	must(t, err)
	sel, err := r.Snapshot(context.Background())
	must(t, err)
	must(t, r.Close())
	ack := visualcapture.Acknowledged{Format: visualstore.Format, Views: sel.ViewSeq, Chain: sel.Chain, SourceEnds: map[int]visualtime.Time{1: end.At}, Segment: 1, Complete: true}
	must(t, visualcapture.WriteAcknowledged(filepath.Join(dir, visualcapture.AcknowledgedName), ack))
	must(t, os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"id":"fixture","target":{"backend":"fake","instance":"test"},"format":"visual/1","status":"stopped","pending":"","segments":[{"path":"visual"}],"future":"accepted"}`), 0600))
	must(t, os.WriteFile(filepath.Join(dir, "launch.json"), []byte(`{"target":{"backend":"fake","instance":"test"},"future":true}`), 0600))

	sealed := filepath.Join(root, "fixture.areel")
	result, err := seal.Seal(context.Background(), seal.Source{RecordingDir: dir}, seal.Destination{Path: sealed}, seal.Options{Limits: visualstore.DefaultLimits(), ActivityLimits: activitystore.DefaultLimits(), Sealer: "measure"})
	must(t, err)
	f, err := seal.Open(sealed)
	must(t, err)
	digest := DigestHex(f.Digest)
	must(t, f.Close())
	visual, err := visualstore.OpenSealed(sealed)
	must(t, err)
	indexDir := filepath.Join(root, "index")
	must(t, os.Mkdir(indexDir, 0700))
	path := filepath.Join(indexDir, "fixture.sqlite")
	stats, err := Build(context.Background(), Source{Recording: "fixture", Digest: digest, Visual: visual}, path)
	must(t, err)
	must(t, visual.Close())
	ix, err := OpenFresh(path, digest)
	must(t, err)
	defer ix.Close()
	queryStarted := time.Now()
	hits, err := ix.Search(context.Background(), Query{Text: "needle-4f2a"})
	must(t, err)
	queryTime := time.Since(queryStarted)
	fuzzyStarted := time.Now()
	fuzzy, err := ix.Search(context.Background(), Query{Text: "NEEDLE-4f2b the bild", Fuzzy: true})
	must(t, err)
	fuzzyTime := time.Since(fuzzyStarted)
	common := time.Now()
	many, err := ix.Search(context.Background(), Query{Text: "path=/api/items/42 ", Limit: LimitMax})
	must(t, err)
	commonTime := time.Since(common)
	report := map[string]any{
		"hours": hours, "views": views, "geometry": fmt.Sprintf("%dx%d", cols, rows),
		"storeWriteSeconds": writeTime.Seconds(),
		"sealSeconds":       result.Elapsed.Seconds(), "sealedBytes": result.FileSize, "sourceAllocatedBytes": result.SourceAllocatedBytes,
		"indexSeconds": stats.Elapsed.Seconds(), "indexBytes": stats.Bytes, "indexRows": stats.Meta.Rows,
		"indexToSealedSize": float64(stats.Bytes) / float64(result.FileSize), "indexToSealTime": stats.Elapsed.Seconds() / result.Elapsed.Seconds(),
		"needleQueryMs": float64(queryTime.Microseconds()) / 1000, "fuzzyQueryMs": float64(fuzzyTime.Microseconds()) / 1000,
		"commonQueryMs": float64(commonTime.Microseconds()) / 1000, "commonHits": len(many),
	}
	raw, _ := json.MarshalIndent(report, "", "  ")
	t.Logf("measurement:\n%s", raw)
	if keep := os.Getenv("AREEL_TEXTINDEX_KEEP"); keep != "" {
		raw, err := os.ReadFile(path)
		must(t, err)
		must(t, os.WriteFile(keep, raw, 0600))
	}
	if out := os.Getenv("AREEL_TEXTINDEX_MEASURE_OUT"); out != "" {
		must(t, os.WriteFile(out, append(raw, '\n'), 0600))
	}
	if len(hits) != 1 || hits[0].AtNs != int64(needleAt) || hits[0].Y != needleY || hits[0].ViewSeq != int64(needleView+1) {
		t.Fatalf("needle %+v, want at %d y %d", hits, needleAt, needleY)
	}
	if len(fuzzy) == 0 || fuzzy[0].AtNs != int64(needleAt) {
		t.Fatalf("fuzzy needle %+v", fuzzy)
	}
}
