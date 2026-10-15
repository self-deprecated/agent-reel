//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

func recoverPrefixSnapshot(t *testing.T, root, id string) map[string][]byte {
	t.Helper()
	ctx := context.Background()
	out := map[string][]byte{}
	h, err := newLibraryHandler(root, libraryTestHost)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	// Fixed fixture clock, as in the complete differential.
	h.visualNow = func() time.Time { return visualGUIStart.Add(2 * time.Hour) }
	api := "/api/recordings/" + id
	for _, route := range []string{api + "/preview-durable", api + "/activity?limit=128", api + "/marks?limit=128"} {
		response := libraryRequest(h, route, func(r *http.Request) { *r = *r.WithContext(ctx) })
		if response.Code != 200 {
			t.Fatalf("%s: %d %s", route, response.Code, response.Body)
		}
		out[route] = response.Body.Bytes()
	}

	for _, kind := range []string{"activity", "marks", "marks-backward"} {
		cursor := 0
		if kind == "marks-backward" {
			cursor = 25
		}
		for page := 0; page < 30; page++ {
			endpoint, key := "activity", "after"
			if kind != "activity" {
				endpoint = "marks"
			}
			if kind == "marks-backward" {
				key = "before"
			}
			route := fmt.Sprintf("%s/%s?%s=%d&limit=7", api, endpoint, key, cursor)
			response := libraryRequest(h, route, func(r *http.Request) { *r = *r.WithContext(ctx) })
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			out[fmt.Sprintf("%s/page-%d", kind, page)] = response.Body.Bytes()
			if kind == "activity" {
				var p activityListReport
				if e := json.Unmarshal(response.Body.Bytes(), &p); e != nil {
					t.Fatal(e)
				}
				if !p.MoreObserved {
					break
				}
				if p.NextAfter <= cursor {
					t.Fatal("activity cursor")
				}
				cursor = p.NextAfter
			} else {
				var p libraryMarksPage
				if e := json.Unmarshal(response.Body.Bytes(), &p); e != nil {
					t.Fatal(e)
				}
				if kind == "marks-backward" {
					if !p.MoreBefore {
						break
					}
					cursor = p.PrevBefore
				} else {
					if !p.MoreAfter {
						break
					}
					cursor = p.NextAfter
				}
			}
		}
	}
	out["facts"] = sealedDifferentialJSON(t, libraryVisualFactsFor(ctx, root, id, h.visualNow))
	history, err := buildVisualHistory(ctx, t.TempDir(), root, id, 1, visualHistoryConfig{budgets: defaultVisualHistoryBudgets()})
	if err != nil {
		t.Fatal(err)
	}
	out["history/facts"] = sealedDifferentialJSON(t, map[string]any{"windows": history.WindowCount(), "duration": history.Duration(), "markers": history.markers})
	for n := 0; n < history.WindowCount(); n++ {
		b, err := history.Window(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		disk, err := os.ReadFile(filepath.Join(history.dir, fmt.Sprintf("window-%d.json", n)))
		if err != nil || !bytes.Equal(disk, b) {
			t.Fatal(err)
		}
		out[fmt.Sprintf("history/%d", n)] = b
	}
	if err = history.Close(); err != nil {
		t.Fatal(err)
	}
	recipe := `{"schema":"agent-reel/recipe/1","rate":{"num":10,"den":1},"clips":[{"segment":1,"start":"0.2","end":"14.5"}]}`
	recipePath := filepath.Join(t.TempDir(), "recipe.json")
	if err = os.WriteFile(recipePath, []byte(recipe), 0600); err != nil {
		t.Fatal(err)
	}
	plan, stderr, err := cliRun(t, "export", id, "--library", root, "--format", "cast", "--recipe", recipePath, "--plan")
	if err != nil {
		t.Fatal(err, stderr)
	}
	out["plan"] = sealedDifferentialExportMask(t, []byte(plan))
	spec := exportTestSpec(id)
	spec.Recipe = visualTestRecipe(t, recipe)
	zip, err := buildExport(ctx, root, spec, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, files := visualExportRead(t, zip)
	for name, b := range files {
		if name == "metadata.json" {
			b = sealedDifferentialExportMask(t, b)
		}
		out["cast/"+name] = b
	}
	report, err := offlineRecording(ctx, root, id, "", visualstore.DefaultLimits(), absentObservation())
	if err != nil {
		t.Fatal(err)
	}
	v := report
	if v.Visual == nil || v.Activity == nil {
		t.Fatal(report)
	}
	// Compare the same content sections as the complete differential. Storage
	// format/allocation, overall verdict ("incomplete" directory versus "ok"
	// interrupted seal), and ownership are representation-specific. The explicit
	// counts, prefix, acknowledgment and incomplete lifecycle must agree.
	out["doctor"] = sealedDifferentialJSON(t, map[string]any{
		"prefix": v.Visual.Prefix, "counts": v.Visual.Counts, "completion": v.Completion, "acknowledged": v.Acknowledged,
		"complete": v.Visual.Complete, "activityCount": v.Activity.Count, "activityPrefix": v.Activity.Report.Prefix,
	})
	return out
}

func TestRecoverDifferentialR(t *testing.T) {
	g := newSealedDifferentialFixture(t, "{id}")
	// Simulate a crash after the first segment was acknowledged: the second
	// segment is committed but outside the recording's durable public prefix.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(g.dir, "visual", visualstore.DatabaseName)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var chain []byte
	err = db.QueryRow("SELECT last_chain FROM view_blocks WHERE last_seq=1500").Scan(&chain)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	ack := visualcapture.Acknowledged{Format: visualstore.Format, Views: 1500, Chain: chain, Segment: 1, SourceEnds: map[int]visualtime.Time{1: 15e9}}
	if err = visualcapture.WriteAcknowledged(filepath.Join(g.dir, visualcapture.AcknowledgedName), ack); err != nil {
		t.Fatal(err)
	}
	g.state.Status = "recording"
	g.saveState()
	prefix := sealedDifferentialCopy(t, g.root)
	prefixDir := filepath.Join(prefix, "recordings", g.id)
	// Build an independent ordinary prefix via the public capped salvage API.
	salvaged := filepath.Join(t.TempDir(), "visual")
	if _, err = visualstore.SalvageWith(context.Background(), filepath.Join(g.dir, "visual"), salvaged, visualstore.DefaultLimits(), visualstore.SalvageOptions{MaxViews: 1500}); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(filepath.Join(prefixDir, "visual")); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(salvaged, filepath.Join(prefixDir, "visual")); err != nil {
		t.Fatal(err)
	}
	r, err := recoverRecording(context.Background(), g.root, g.id, recoverDefaults(), absentObservation())
	if err != nil || r.RemovedDirectory || r.Views != 1500 || r.DroppedViews != 1500 {
		t.Fatal(r, err)
	}
	file, err := seal.Open(r.SealedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err = os.WriteFile(filepath.Join(prefixDir, "state.json"), file.SourceFiles["state.json"], 0600); err != nil {
		t.Fatal(err)
	}
	// The same state bytes and original acknowledgment are compared, not a
	// synthesized lifecycle state. Presentation-only ls holder/hints and the
	// whole-library/whole-recording routes are excluded from the prefix contract.
	var state State
	if err = json.Unmarshal(file.SourceFiles["state.json"], &state); err != nil || state.Status != "interrupted" {
		t.Fatal(err)
	}
	a := recoverPrefixSnapshot(t, prefix, g.id)
	b := recoverPrefixSnapshot(t, g.root, g.id)
	sealedDifferentialEqual(t, a, b)
	t.Logf("phase R: %d prefix route outputs; sealed %d views, retained %d views", len(a), r.Views, r.DroppedViews)
}
