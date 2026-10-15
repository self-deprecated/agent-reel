package loopoc

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/qualify"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// V8SpinnerViewBytesPerHour is the V8 reference: W5 view bytes (views table
// plus views_at, dbstat) of about 3 MB per logical hour
// (internal/visualcapture/qualify_timeline_test.go:1523).
const V8SpinnerViewBytesPerHour = 3e6

// TestLoopocReport measures the baselines on every corpus in
// $AREEL_LOOPOC_OUT/corpus into results/sqlite-v8.json and results/raw.json,
// then checks sqlite-v8 on W5 (S1-pure) under prod against V8's figure and
// against dbstat of the captured production store itself.
func TestLoopocReport(t *testing.T) {
	RunReport(t, SQLiteV8{Dir: t.TempDir()}, Raw{})
	data, err := os.ReadFile(filepath.Join(OutDir(), "results", "sqlite-v8.json"))
	if err != nil {
		t.Fatal(err)
	}
	var results []Result
	if err := json.Unmarshal(data, &results); err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Corpus != qualify.W5 || r.Policy != PolicyProd {
			continue
		}
		ratio := r.BytesPerHour / V8SpinnerViewBytesPerHour
		t.Logf("sqlite-v8 W5 prod: %.0f B/logical-h = %.3f × V8's 3 MB/h (%.2f MiB/h)", r.BytesPerHour, ratio, r.BytesPerHour/(1<<20))
		if ratio < 0.85 || ratio > 1.15 {
			t.Errorf("sqlite-v8 on W5 is %.3f × the V8 figure, outside ±15%%", ratio)
		}
		store := filepath.Join(OutDir(), "stores", qualify.W5, visualcapture.StoreDirName, visualstore.DatabaseName)
		if _, err := os.Stat(store); err != nil {
			t.Logf("no captured W5 store to compare (%v)", err)
			continue
		}
		db, err := sql.Open("sqlite", "file:"+store+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		real, err := dbstatBytes(db, "view_blocks", "view_blocks_at", "view_blocks_last")
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("captured production W5 store: view_blocks+indexes dbstat %d B; sqlite-v8 baseline %d B (%.3f ×)", real, r.StoredBytes, float64(r.StoredBytes)/float64(real))
	}
}
