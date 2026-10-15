package sealsplit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/loopoc"
	"github.com/self-deprecated/agent-reel/internal/loopoc/blockz"
	"github.com/self-deprecated/agent-reel/internal/loopoc/cyclerle"
	"github.com/self-deprecated/agent-reel/internal/qualify"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// TestEncodeBytesMatchesMeasure: EncodeBytes charges exactly what
// loopoc.Measure charges and decodes exactly, on the synthetic corpora.
func TestEncodeBytesMatchesMeasure(t *testing.T) {
	for _, c := range loopoc.SyntheticCorpora() {
		for _, codec := range []loopoc.Codec{blockz.Blockz{}, cyclerle.Codec{}, loopoc.Raw{}} {
			for _, policy := range loopoc.Policies {
				commits := loopoc.PolicyCommits(c, policy)
				got, err := EncodeBytes(codec, c.Views, commits, true)
				if err != nil {
					t.Fatalf("%s %s %s: %v", codec.Name(), c.Name, policy, err)
				}
				want := loopoc.Measure(c, codec, commits)
				if !want.Exact || got.Stored != want.StoredBytes || got.Records != want.Records {
					t.Errorf("%s %s %s: EncodeBytes %d B / %d records, Measure %d B / %d records (exact %v)",
						codec.Name(), c.Name, policy, got.Stored, got.Records, want.StoredBytes, want.Records, want.Exact)
				}
			}
		}
	}
}

func TestDeltas(t *testing.T) {
	// A 3-screen spinner at 100 ms with one 101.5 ms step (Δt 5) and one
	// 100.4 ms step (Δt 8).
	var vs []loopoc.View
	var t0 int64
	for i := range 12 {
		switch i {
		case 0:
		case 5:
			t0 += 101_500_000
		case 8:
			t0 += 100_400_000
		default:
			t0 += 100_000_000
		}
		vs = append(vs, loopoc.View{Seq: int64(i + 1), Segment: 1, AtNs: t0, ScreenID: int64(i%3 + 1)})
	}
	s := Deltas(vs)
	if s.Deltas != 11 || s.P50Ns != 100e6 || s.P99Ns != 101_500_000 {
		t.Fatalf("deltas %+v", s)
	}
	// Δt 1..3 open the three transitions; Δt 4..11 recur. Against the same
	// phase of the previous cycle, Δt 5 misses (1.5 ms) and Δt 8 misses
	// (100.4 vs 101.5 ms); Δt 11 (100 vs 100.4 ms) hits.
	if s.RecurDeltas != 8 || s.WithinPrevCycle1msRecurring != 6.0/8 || s.WithinPrevCycle1ms != 6.0/11 {
		t.Fatalf("deltas %+v", s)
	}
}

// TestSealSplitCaptured captures a short S2-clock stream and checks the split
// is complete and the sealed re-encode is smaller than the store.
func TestSealSplitCaptured(t *testing.T) {
	h := os.Getenv("AREEL_TEST_AVT")
	if h == "" {
		t.Skip("AREEL_TEST_AVT is unset")
	}
	s, err := loopoc.Workload(loopoc.S2Clock, 0x5639_0009, 30*qualify.Second)
	if err != nil {
		t.Fatal(err)
	}
	rec := filepath.Join(t.TempDir(), "rec")
	c, _, err := Capture(h, rec, "s2", s.Cols, s.Rows, s.WriteCast)
	if err != nil {
		t.Fatal(err)
	}
	if bad := loopoc.CheckChains(c.Views); bad != 0 {
		t.Fatalf("chain mismatch at seq %d", bad)
	}
	db, err := OpenRO(filepath.Join(rec, visualcapture.StoreDirName, visualstore.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sp, err := Measure("s2", "captured", s.SpanNs, db, c.Views)
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range []string{TViews, TRows, TScreens, TOthers} {
		if sp.Stored[tb] <= 0 || sp.Sealed[tb] <= 0 {
			t.Errorf("%s: stored %d, sealed %d", tb, sp.Stored[tb], sp.Sealed[tb])
		}
	}
	if sp.SealedTotal >= sp.StoredTotal || int(sp.Counts.Views) != len(c.Views) {
		t.Errorf("sealed %d B vs stored %d B, %d views", sp.SealedTotal, sp.StoredTotal, sp.Counts.Views)
	}
	t.Logf("S2 30 s: stored %v, sealed %v", sp.Stored, sp.Sealed)
}

// reportCorpora is the Part 1 order: content-changing corpora first, then
// the contrast rows.
var reportCorpora = []string{loopoc.S2Clock, loopoc.S6Mixed, qualify.W3, qualify.W4, qualify.W6, qualify.W7, qualify.W5, loopoc.S3Jitter}

// TestSealSplitReport measures the stores TestLoopocCorpus left under
// $AREEL_LOOPOC_OUT/stores into results/sealsplit.json. It opens every store
// read-only and immutable.
func TestSealSplitReport(t *testing.T) {
	out := loopoc.OutDir()
	if out == "" {
		t.Skip("AREEL_LOOPOC_OUT is unset")
	}
	data, err := os.ReadFile(filepath.Join(out, "corpus", "manifest.json"))
	if err != nil {
		t.Skipf("no captured corpora: %v", err)
	}
	var manifest []struct {
		Name   string `json:"name"`
		SpanNs int64  `json:"span_ns"`
		Views  int    `json:"views"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	var splits []Split
	for _, name := range reportCorpora {
		i := slices.IndexFunc(manifest, func(m struct {
			Name   string `json:"name"`
			SpanNs int64  `json:"span_ns"`
			Views  int    `json:"views"`
		}) bool {
			return m.Name == name
		})
		if i < 0 {
			t.Errorf("%s: not in the manifest", name)
			continue
		}
		db, err := OpenRO(filepath.Join(out, "stores", name, visualcapture.StoreDirName, visualstore.DatabaseName))
		if err != nil {
			t.Fatal(err)
		}
		views, err := ReadViews(db)
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		if len(views) != manifest[i].Views {
			t.Errorf("%s: store has %d views, manifest %d", name, len(views), manifest[i].Views)
		}
		sp, err := Measure(name, "captured", manifest[i].SpanNs, db, views)
		db.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		splits = append(splits, sp)
		t.Logf("%-9s stored %8d B  sealed %8d B  sealed views %5.1f%% rows %5.1f%% screens %5.1f%%", name, sp.StoredTotal, sp.SealedTotal,
			100*sp.SealedShare[TViews], 100*sp.SealedShare[TRows], 100*sp.SealedShare[TScreens])
	}
	if err := WriteJSON(filepath.Join(out, "results", "sealsplit.json"), splits); err != nil {
		t.Fatal(err)
	}
}
