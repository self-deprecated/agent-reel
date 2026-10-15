package seal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func options() Options {
	return Options{Limits: visualstore.DefaultLimits(), ActivityLimits: activitystore.DefaultLimits(), Sealer: "test/v11"}
}
func point(n int) visualtime.Point {
	return visualtime.Point{Line: int64(n), At: visualtime.Time(n * 10), Known: true}
}
func fixture(t *testing.T, activity bool, n int) string {
	t.Helper()
	return fixtureShape(t, activity, n, "")
}

func fixtureShape(t *testing.T, activity bool, n int, shape string) string {
	t.Helper()
	dir := t.TempDir()
	id := archive.Identity{Recording: "fixture", Backend: "fake", Instance: "test"}
	s, err := visualstore.Create(filepath.Join(dir, "visual"), id)
	must(t, err)
	t.Cleanup(func() { _ = s.Close() })
	cols, rows := 16, 4
	if shape == "new-screens" || shape == "scroll" {
		cols, rows = 200, 50
	}
	if shape == "resize-scroll" {
		cols, rows = 80, 24
	}
	must(t, s.BeginSegment(1, archive.Geometry{Columns: cols, Rows: rows}, "exact"))
	must(t, s.Event(visualstore.Event{Segment: 1, Point: point(1), Kind: visualstore.EventMarker, Data: "start"}))
	must(t, s.Fidelity(visualstore.FidelityRecord{Segment: 1, Point: point(1), Finding: fidelity.Finding{Class: "fixture", Kind: fidelity.KindPoint, Detail: "synthetic"}}))
	events := uint64(1)
	for i := 1; i <= n; i++ {
		if shape == "resize-scroll" && i == 2 {
			cols, rows = 200, 50
			must(t, s.Event(visualstore.Event{Segment: 1, Point: point(i), Kind: visualstore.EventResize, Data: "200x50"}))
			events++
		}
		screen := castexport.Screen{Cols: cols, Rows: rows, Cursor: castexport.Cursor{Visible: true, Col: i % cols}, Cells: make([][]castexport.Cell, rows)}
		for r := range screen.Cells {
			screen.Cells[r] = make([]castexport.Cell, cols)
			id := i + r
			switch shape {
			case "few-screens":
				id = ((i-1)/10)%2*rows + r
			case "new-screens":
				id = (i-1)*rows + r
			case "spread-rows":
				id = (i-1)*rows + r + 1
				if i > 6*visualstore.SealChunkRecords/4 {
					j := (i - 1 - 6*visualstore.SealChunkRecords/4) % 300
					id = ((j*4+r)%6)*visualstore.SealChunkRecords + j + 1
				}
			}
			line := fmt.Sprintf("%0*d", cols, id)
			for c := range screen.Cells[r] {
				screen.Cells[r][c] = castexport.Cell{Char: rune(line[c]), Width: 1}
				if shape == "scroll" {
					screen.Cells[r][c].FG = castexport.Color{Kind: castexport.ColorRGB, R: 1, G: 2, B: 3}
					screen.Cells[r][c].BG = castexport.Color{Kind: castexport.ColorRGB, R: 4, G: 5, B: 6}
				}
			}
		}
		ok, err := s.Record(visualstore.View{Segment: 1, Point: point(i), Screen: screen})
		must(t, err)
		if !ok {
			t.Fatal("view coalesced")
		}
		if i%1000 == 0 {
			must(t, s.Commit(visualtime.Cutoff{Segment: 1, Source: point(i), Presentations: uint64(i), Events: events}))
		}
	}
	must(t, s.Complete(visualtime.Cutoff{Segment: 1, Source: point(n + 1), SourceComplete: true, Presentations: uint64(n), Events: events}))
	must(t, s.Close())
	r, err := visualstore.OpenReader(filepath.Join(dir, "visual"))
	must(t, err)
	sel, err := r.Snapshot(context.Background())
	must(t, err)
	must(t, r.Close())
	ack := visualcapture.Acknowledged{Format: visualstore.Format, Views: sel.ViewSeq, Chain: sel.Chain, SourceEnds: map[int]visualtime.Time{1: point(n + 1).At}, Segment: 1, Complete: true}
	must(t, visualcapture.WriteAcknowledged(filepath.Join(dir, visualcapture.AcknowledgedName), ack))
	must(t, os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"id":"fixture","target":{"backend":"fake","instance":"test"},"format":"visual/1","status":"stopped","pending":"","segments":[{"path":"visual"}],"future":"accepted"}`), 0600))
	must(t, os.WriteFile(filepath.Join(dir, "launch.json"), []byte(`{"target":{"backend":"fake","instance":"test"},"future":true}`), 0600))
	if activity {
		a, err := activitystore.Create(filepath.Join(dir, activityDirName), id)
		must(t, err)
		t.Cleanup(func() { _ = a.Close() })
		for i := 1; i <= 50; i++ {
			p := visualtime.Placement{Precision: activitystore.PrecisionUnknown}
			if i%2 == 0 {
				p = visualtime.Placement{Precision: activitystore.PrecisionEstimated, Segment: 1, Basis: point(i), ReceiptTick: int64(i + 1), BasisReceiptTick: int64(i), Estimated: visualtime.Time(i*10 + 1)}
			}
			_, err := a.Append(int64(i), activitystore.Record{ID: fmt.Sprintf("event-%d", i), Envelope: []byte(fmt.Sprintf(`{"note":%d}`, i)), ReceivedAt: time.Unix(1700000000, int64(i)), Placement: p, Marker: i%3 == 0})
			must(t, err)
		}
		must(t, a.Commit())
		must(t, a.Close())
	}
	return dir
}
func destination(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must(t, os.Chmod(dir, 0700))
	return filepath.Join(dir, "fixture.areel")
}
func build(t *testing.T, src, dst string) Result {
	t.Helper()
	r, err := Seal(context.Background(), Source{src}, Destination{dst}, options())
	must(t, err)
	return r
}
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	must(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func mutate(t *testing.T, path, q string, args ...any) {
	t.Helper()
	db := rawDB(t, path)
	_, err := db.Exec(q, args...)
	must(t, err)
	must(t, db.Close())
}
func copySeal(t *testing.T, path string) string {
	t.Helper()
	dst := destination(t)
	b, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(dst, b, 0600))
	return dst
}
func assertEmpty(t *testing.T, dst string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(dst))
	must(t, err)
	if len(entries) != 0 {
		t.Fatalf("destination directory not empty: %v", entries)
	}
}
func TestSealRoundTripAndBareV12(t *testing.T) {
	for _, withActivity := range []bool{true, false} {
		t.Run(fmt.Sprint(withActivity), func(t *testing.T) {
			src := fixture(t, withActivity, 80)
			dst := destination(t)
			result := build(t, src, dst)
			if result.Path != dst || result.FileSize <= 0 || result.SourceAllocatedBytes <= 0 || result.Elapsed <= 0 || result.PeakRSSAfterBytes <= 0 {
				t.Fatalf("result: %+v", result)
			}
			rep, err := Verify(context.Background(), dst, visualstore.DefaultLimits())
			must(t, err)
			if rep.Status != StatusOK {
				t.Fatalf("%+v", rep)
			}
			if rep.Counts.Visual.Views != 80 || rep.Manifest.Activity.Absent == withActivity {
				t.Fatalf("counts: %+v", rep.Counts)
			}
			must(t, visualstore.CheckSealedFile(dst))
			entries, err := os.ReadDir(filepath.Dir(dst))
			must(t, err)
			if len(entries) != 1 {
				t.Fatalf("publication left files: %v", entries)
			}
			db, err := immutableDB(dst)
			must(t, err)
			defer db.Close()
			rows, err := db.Query("SELECT name,data FROM source_files ORDER BY name")
			must(t, err)
			files := 0
			for rows.Next() {
				var name string
				var data []byte
				must(t, rows.Scan(&name, &data))
				want, err := os.ReadFile(filepath.Join(src, name))
				must(t, err)
				if !bytes.Equal(want, data) {
					t.Fatal(name)
				}
				files++
			}
			must(t, rows.Err())
			must(t, rows.Close())
			if files != 3+btoi(withActivity) {
				t.Fatalf("source files=%d", files)
			}
			var metaCount int
			must(t, db.QueryRow("SELECT COUNT(*) FROM meta").Scan(&metaCount))
			if metaCount != 8+btoi(withActivity) {
				t.Fatalf("meta count=%d", metaCount)
			}
			assertPinnedLayout(t, db, withActivity)
			vr, err := visualstore.OpenSealed(dst)
			must(t, err)
			defer vr.Close()
			sel, err := vr.Snapshot(context.Background())
			must(t, err)
			views, err := vr.Views(context.Background(), sel, 1, 1, 100)
			must(t, err)
			if len(views) != 80 {
				t.Fatal(len(views))
			}
			_, err = vr.Screen(context.Background(), sel, views[0].ScreenID)
			must(t, err)
			ar, err := activitystore.OpenSealed(dst)
			must(t, err)
			defer ar.Close()
			as, err := ar.Snapshot(context.Background())
			must(t, err)
			events, err := ar.Events(context.Background(), as, 1, 100)
			must(t, err)
			if len(events) != 50*btoi(withActivity) {
				t.Fatalf("activity rows=%d", len(events))
			}
			if withActivity {
				ack, err := activitystore.LoadAcknowledged(filepath.Join(src, activityDirName))
				must(t, err)
				if !bytes.Equal(as.Chain, ack.Chain) {
					t.Fatal("activity chain")
				}
				got, err := ar.Get(context.Background(), as, "event-2")
				must(t, err)
				if got.Placement.Precision != activitystore.PrecisionEstimated {
					t.Fatal(got)
				}
				placed, err := ar.Placed(context.Background(), as, 1, 0, 100, 1, 100)
				must(t, err)
				if len(placed) != 25 {
					t.Fatal(len(placed))
				}
			} else if as.Seq != 0 || !bytes.Equal(as.Chain, make([]byte, 32)) {
				t.Fatal(as)
			}
			// Determinism includes every provenance field except the seal time.
			second := build(t, src, destination(t))
			a, b := result.Manifest, second.Manifest
			a.SealedAt = ""
			b.SealedAt = ""
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("non-deterministic manifest\n%+v\n%+v", a, b)
			}
		})
	}
}
func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
func editState(t *testing.T, src, key string, value any) {
	t.Helper()
	path := filepath.Join(src, "state.json")
	b, err := os.ReadFile(path)
	must(t, err)
	var m map[string]any
	must(t, json.Unmarshal(b, &m))
	m[key] = value
	b, err = json.Marshal(m)
	must(t, err)
	must(t, os.WriteFile(path, b, 0600))
}
func editAck(t *testing.T, src string, fn func(*visualcapture.Acknowledged)) {
	t.Helper()
	path := filepath.Join(src, visualcapture.AcknowledgedName)
	a, err := visualcapture.LoadAcknowledged(path)
	must(t, err)
	fn(&a)
	must(t, visualcapture.WriteAcknowledged(path, a))
}
func TestSealRefusalsBeforeAnyFile(t *testing.T) {
	cases := map[string]func(*testing.T, string, string){
		"pending": func(t *testing.T, s, d string) { editState(t, s, "pending", "stop") },
		"incomplete_ack": func(t *testing.T, s, d string) {
			editAck(t, s, func(a *visualcapture.Acknowledged) { a.Complete = false })
		},
		"missing_completion": func(t *testing.T, s, d string) {
			mutate(t, filepath.Join(s, "visual", visualstore.DatabaseName), "DELETE FROM completion")
		},
		"visual_tail": func(t *testing.T, s, d string) {
			vr, err := visualstore.OpenReader(filepath.Join(s, "visual"))
			must(t, err)
			sel, err := vr.Snapshot(context.Background())
			must(t, err)
			must(t, vr.Close())
			// Use an actually authenticated earlier source-block chain.
			db := rawDB(t, filepath.Join(s, "visual", visualstore.DatabaseName))
			var chain []byte
			var last int64
			must(t, db.QueryRow("SELECT last_seq,last_chain FROM view_blocks ORDER BY first_seq LIMIT 1").Scan(&last, &chain))
			must(t, db.Close())
			if last >= sel.ViewSeq {
				t.Fatal("fixture needs multiple source blocks")
			}
			editAck(t, s, func(a *visualcapture.Acknowledged) { a.Views = last; a.Chain = chain })
		},
		"ack_chain": func(t *testing.T, s, d string) {
			editAck(t, s, func(a *visualcapture.Acknowledged) { a.Chain[0] ^= 1 })
		},
		"visual_damaged": func(t *testing.T, s, d string) {
			mutate(t, filepath.Join(s, "visual", visualstore.DatabaseName), "UPDATE view_blocks SET payload=x'80' WHERE first_seq=1")
		},
		"activity_tail": func(t *testing.T, s, d string) {
			path := filepath.Join(s, activityAckName)
			db := rawDB(t, filepath.Join(s, activityDirName, activitystore.DatabaseName))
			var chain []byte
			must(t, db.QueryRow("SELECT chain FROM activity WHERE seq=49").Scan(&chain))
			must(t, db.Close())
			must(t, os.WriteFile(path, []byte(fmt.Sprintf("{\"seq\":49,\"chain\":\"%x\"}\n", chain)), 0600))
		},
		"activity_damaged": func(t *testing.T, s, d string) {
			mutate(t, filepath.Join(s, activityDirName, activitystore.DatabaseName), "UPDATE activity SET envelope='changed' WHERE seq=1")
		},
		"identity": func(t *testing.T, s, d string) { editState(t, s, "id", "other") },
		"activity_identity": func(t *testing.T, s, d string) {
			mutate(t, filepath.Join(s, activityDirName, activitystore.DatabaseName), "UPDATE meta SET value='other' WHERE key='recording'")
		},
		"missing_launch": func(t *testing.T, s, d string) { must(t, os.Remove(filepath.Join(s, "launch.json"))) },
		"launch_target": func(t *testing.T, s, d string) {
			must(t, os.WriteFile(filepath.Join(s, "launch.json"), []byte("{}"), 0600))
		},
		"segment_path": func(t *testing.T, s, d string) {
			editState(t, s, "segments", []any{map[string]any{"path": "segment-0001"}})
		},
		"parent_mode": func(t *testing.T, s, d string) { must(t, os.Chmod(filepath.Dir(d), 0755)) },
	}
	for _, status := range []string{"recording", "paused", "interrupted", "failed", "stopping"} {
		cases["status_"+status] = func(t *testing.T, s, d string) { editState(t, s, "status", status) }
	}
	for name, modify := range cases {
		t.Run(name, func(t *testing.T) {
			n := 12
			if name == "visual_tail" {
				n = 1001
			}
			src := fixture(t, true, n)
			dst := destination(t)
			modify(t, src, dst)
			_, err := Seal(context.Background(), Source{src}, Destination{dst}, options())
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("want refusal: %v", err)
			}
			var refusal *RefusalError
			if !errors.As(err, &refusal) {
				t.Fatalf("not typed refusal: %v", err)
			}
			assertEmpty(t, dst)
		})
	}
	t.Run("sealed_source", func(t *testing.T) {
		src := fixture(t, false, 8)
		sealed := destination(t)
		build(t, src, sealed)
		dst := destination(t)
		_, err := Seal(context.Background(), Source{sealed}, Destination{dst}, options())
		if !errors.Is(err, ErrRefused) {
			t.Fatal(err)
		}
		assertEmpty(t, dst)
	})
}
func TestSealExistingDestinationAndPublishRace(t *testing.T) {
	src := fixture(t, false, 12)
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprint(race), func(t *testing.T) {
			dst := destination(t)
			sentinel := []byte("existing")
			ctx := context.Background()
			if race {
				ctx = context.WithValue(ctx, hookKey{}, func(part string, n int64) error {
					if part == "publish" {
						return os.WriteFile(dst, sentinel, 0600)
					}
					return nil
				})
			} else {
				must(t, os.WriteFile(dst, sentinel, 0600))
			}
			_, err := Seal(ctx, Source{src}, Destination{dst}, options())
			if !errors.Is(err, ErrExists) {
				t.Fatal(err)
			}
			got, err := os.ReadFile(dst)
			must(t, err)
			if !bytes.Equal(got, sentinel) {
				t.Fatal("overwrote destination")
			}
			entries, err := os.ReadDir(filepath.Dir(dst))
			must(t, err)
			if len(entries) != 1 {
				t.Fatal(entries)
			}
		})
	}
}
func TestSealSourceChanged(t *testing.T) {
	src := fixture(t, true, 12)
	dst := destination(t)
	ctx := context.WithValue(context.Background(), hookKey{}, func(part string, n int64) error {
		if part == "source_recheck" {
			path := filepath.Join(src, visualcapture.AcknowledgedName)
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(path, append(b, ' '), 0600)
		}
		return nil
	})
	_, err := Seal(ctx, Source{src}, Destination{dst}, options())
	if !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	assertEmpty(t, dst)
}
func TestSealCorruption(t *testing.T) {
	src := fixture(t, true, 20)
	original := destination(t)
	build(t, src, original)
	cases := []struct{ name, q, item string }{
		{"manifest", "UPDATE seal SET manifest=manifest||' '", "seal"},
		{"digest", "UPDATE seal SET digest=zeroblob(32)", "seal"},
		{"state", "UPDATE source_files SET data=x'7b7d' WHERE name='state.json'", "source_files"},
		{"activity", "UPDATE activity SET envelope='changed' WHERE seq=1", "row 1"},
		{"activity_progress", "UPDATE activity_progress SET chain=zeroblob(32)", "progress"},
		{"meta_added", "INSERT INTO meta VALUES('other','value')", "meta"},
		{"meta_removed", "DELETE FROM meta WHERE key='created'", "meta"},
		{"schema_extra", "CREATE TABLE extra(id INTEGER) STRICT", "schema"},
		{"schema_trigger", "CREATE TRIGGER extra AFTER INSERT ON source_files BEGIN SELECT 1; END", "schema"},
		{"rows", "UPDATE row_chunks SET payload=x'80' WHERE first_id=1", "row 1"},
		{"screens", "UPDATE screen_chunks SET payload=x'80' WHERE first_id=1", "screen 1"},
		{"heads", "UPDATE screen_heads SET heads=zeroblob(length(heads)) WHERE first_id=1", "screen 1"},
		{"views", "UPDATE view_blocks SET payload=x'80' WHERE first_seq=1", "view block 1"},
		{"events", "UPDATE events SET data='changed'", "seal"},
		{"fidelity", "UPDATE fidelity SET detail='changed'", "seal"},
		{"segments", "UPDATE segments SET precision='changed'", "seal"},
		{"completion_missing", "DELETE FROM completion; UPDATE segments SET state='closed'; UPDATE progress SET source_complete=0", "completion"},
		{"provenance", "UPDATE progress SET logical_bytes=logical_bytes+1; UPDATE completion SET logical_bytes=logical_bytes+1", "completion"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dst := copySeal(t, original)
			mutate(t, dst, c.q)
			rep, err := Verify(context.Background(), dst, visualstore.DefaultLimits())
			must(t, err)
			if rep.Status != StatusDamaged || rep.FailedItem != c.item {
				t.Fatalf("want %s: %s %s %s", c.item, rep.Status, rep.FailedItem, rep.Reason)
			}
		})
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			dst := copySeal(t, original)
			must(t, os.WriteFile(dst+suffix, nil, 0600))
			rep, err := Verify(context.Background(), dst, visualstore.DefaultLimits())
			must(t, err)
			if rep.Status != StatusDamaged || rep.FailedItem != "file" {
				t.Fatal(rep)
			}
		})
	}
}
func TestManifestStrictnessAndBudgets(t *testing.T) {
	src := fixture(t, false, 8)
	original := destination(t)
	result := build(t, src, original)
	data, _, err := manifestBytes(result.Manifest)
	must(t, err)
	for _, bad := range []string{
		strings.Replace(string(data), `"schema":`, `"extra":1,"schema":`, 1),
		strings.Replace(string(data), `"schema":`, `"schema":"duplicate","schema":`, 1),
		strings.Replace(string(data), `"count":8,`, ``, 1),
		string(data) + " {}",
		strings.Replace(string(data), `"activity":{`, `"activity":{"extra":1,`, 1),
	} {
		if _, err := parseManifest([]byte(bad)); err == nil {
			t.Fatalf("accepted malformed manifest: %s", bad)
		}
	}
	for _, l := range []visualstore.Limits{{Work: 1, DecodedBytes: 1 << 20}, {Work: 100000, DecodedBytes: 1}} {
		rep, err := Verify(context.Background(), original, l)
		if !errors.Is(err, visualstore.ErrLimit) || rep.Status != StatusUnverified {
			t.Fatalf("%+v %v", rep, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := Verify(ctx, original, visualstore.DefaultLimits())
	if !errors.Is(err, context.Canceled) || rep.Status != StatusUnverified {
		t.Fatalf("%+v %v", rep, err)
	}
	dst := destination(t)
	_, err = Seal(ctx, Source{src}, Destination{dst}, options())
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertEmpty(t, dst)
}

// Assert the published names independently of the schema-generating constants
// and exactSchema, so accidentally omitting an object there cannot bless it.
func assertPinnedLayout(t *testing.T, db *sql.DB, activity bool) {
	t.Helper()
	want := []string{
		"table:meta", "table:segments", "table:view_blocks", "table:events", "table:fidelity",
		"table:progress", "table:completion", "table:row_chunks", "table:screen_chunks", "table:screen_heads",
		"table:activity", "table:activity_progress", "table:source_files", "table:seal",
		"index:view_blocks_last", "index:view_blocks_at", "index:events_at", "index:fidelity_at", "index:activity_placed",
		"index:sqlite_autoindex_meta_1", "index:sqlite_autoindex_activity_1", "index:sqlite_autoindex_source_files_1",
	}
	sort.Strings(want)
	rows, err := db.Query("SELECT type||':'||name FROM sqlite_schema ORDER BY type,name")
	must(t, err)
	var got []string
	for rows.Next() {
		var name string
		must(t, rows.Scan(&name))
		got = append(got, name)
	}
	must(t, rows.Err())
	must(t, rows.Close())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema names: %v", got)
	}
	want = []string{"format", "recording", "backend", "instance", "created", "source_format", "sealed", "sealer"}
	if activity {
		want = append(want, "activity_format")
	}
	sort.Strings(want)
	rows, err = db.Query("SELECT key FROM meta ORDER BY key")
	must(t, err)
	got = nil
	for rows.Next() {
		var key string
		must(t, rows.Scan(&key))
		got = append(got, key)
	}
	must(t, rows.Err())
	must(t, rows.Close())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("meta keys: %v", got)
	}
}
