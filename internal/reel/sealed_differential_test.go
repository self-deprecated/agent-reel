//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

const sealedDifferentialRecipe = `{"schema":"agent-reel/recipe/1","rate":{"num":10,"den":1},"clips":[{"segment":1,"start":"0.2","end":"14.5","speed":{"num":2,"den":1}},{"segment":2,"start":"0","end":"15"}]}`

func sealedDifferentialOptions() seal.Options {
	return seal.Options{Limits: visualstore.DefaultLimits(), ActivityLimits: activitystore.DefaultLimits(), Sealer: "areel 0.1.0"}
}

func sealedDifferentialRemoval(dir, path string) seal.Removal {
	o := sealedDifferentialOptions()
	return seal.Removal{RecordingDir: dir, SealedPath: path, Limits: o.Limits, ActivityLimits: o.ActivityLimits}
}

func sealedDifferentialBlocks(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM view_blocks").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Directory-mode immutable-prefix verification can leave empty SQLite WAL
// and SHM artifacts. All fixture handles are closed here. Remove only those
// empty-WAL pairs generated during A/sealing so B starts with no sidecars;
// every sealed route must preserve that baseline without recreating them.
func sealedDifferentialCleanSourceSidecars(t *testing.T, dir string) {
	t.Helper()
	for _, db := range []string{
		filepath.Join(dir, visualcapture.StoreDirName, visualstore.DatabaseName),
		filepath.Join(dir, activityIndexDir, activitystore.DatabaseName),
	} {
		if info, err := os.Stat(db + "-wal"); err == nil {
			if info.Size() != 0 {
				t.Fatalf("closed fixture has nonempty WAL: %s", db)
			}
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			if err := os.Remove(db + suffix); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
	}
}

// No recorder, helper, terminal, service, or external media tool is involved.
// There are 3000 distinct presentations, source block boundaries in each of
// two segments, a pause, a resize, markers, fidelity and all placement kinds.
func newSealedDifferentialFixture(t *testing.T, tpl string) *visualGUI {
	t.Helper()
	g := newVisualGUI(t, "stopped")
	identity := offlineIdentity
	g.state.Holder = identity(101)
	g.state.Suppressed = true
	for segment := 1; segment <= 2; segment++ {
		g.begin(segment, 20, 2)
		for n := 0; n < 1500; n++ {
			at := float64(n) / 100
			cols := 20
			if segment == 2 && n >= 750 {
				cols = 24
			}
			if segment == 2 && n == 750 {
				g.event(at, visualstore.EventResize, "24x2")
			}
			g.view(at, visualGUIScreen(cols, 2, fmt.Sprintf("segment %d view %04d", segment, n)))
			if n == 200 {
				g.marker(at, fmt.Sprintf("marker-%d", segment))
				g.finding(at, fidelity.ClassSGRUnsupported, fidelity.KindPoint, "SGR 58")
			}
			if n == 1000 {
				g.commit(at)
			}
		}
		if segment == 1 {
			g.closeSegment(15)
		} else {
			ended := visualGUIStart.Add(time.Hour + 15*time.Second)
			g.state.Segments[segment-1].Ended = &ended
		}
		segDir := filepath.Join(g.dir, fmt.Sprintf("segment-%04d", segment))
		if err := os.Mkdir(segDir, 0700); err != nil {
			t.Fatal(err)
		}
		for name, v := range map[string]any{"recorder.json": identity(10 * segment), "watcher.json": identity(10*segment + 1), "watch.json": watchSpec{Executable: "never-executed", Target: g.state.Target}} {
			if err := writeJSON(filepath.Join(segDir, name), v); err != nil {
				t.Fatal(err)
			}
		}
	}
	g.completeAt(15)
	ack := g.acknowledge()
	if ack.Views != 3000 || !ack.Complete {
		t.Fatalf("fixture acknowledgment: %+v", ack)
	}
	g.state.Events = []Event{
		{ID: g.id + ":1", Kind: "start", Segment: 1, At: visualGUIStart},
		{ID: g.id + ":2", Kind: "pause", Segment: 1, At: visualGUIStart.Add(15 * time.Second)},
		{ID: g.id + ":3", Kind: "resume", Segment: 2, At: visualGUIStart.Add(time.Hour)},
		{ID: g.id + ":4", Kind: "stop", Segment: 2, At: visualGUIStart.Add(time.Hour + 15*time.Second)},
	}
	g.saveState()
	if err := writeJSON(filepath.Join(g.dir, "launch.json"), map[string]any{"target": g.state.Target, "token": "", "sealName": tpl}); err != nil {
		t.Fatal(err)
	}
	a := newVisualActivity(t, g.dir, g.state)
	for n := 1; n <= 24; n++ {
		p := estimatedAt(1+(n%2), 1, 0, float64(n)/2)
		if n%5 == 0 {
			p = unknownPlacement()
		}
		a.add(indexedEnvelope(fmt.Sprintf("activity-%02d", n), fmt.Sprintf("Activity %02d", n)), n%3 == 0, nil, p)
	}
	a.commit()
	a.close()
	g.closeWriter()
	store, err := OpenStore(g.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(store.targetPath(g.state.Target), g.id); err != nil {
		t.Fatal(err)
	}
	return g
}

func sealedDifferentialJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sealedDifferentialObject(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("JSON: %v: %s", err, b)
	}
	return v
}

// sealedDifferentialStorageMask removes the viewer's storage-dependent parts:
// the size on disk and the original .areel download (or its live notice).
var sealedDifferentialStorageMask = regexp.MustCompile(` · <span class="viewer-size"[^>]*>[^<]*</span>|<a href="/download/[0-9a-f]+" download data-download-areel[^>]*>[^<]*</a>|<span class="menu-disabled"[^>]*data-download-areel-unavailable[^>]*>[^<]*</span>`)

// These are the only export masks: request time and input freeze time.
// TestSealedDifferential additionally exports the SAME directory twice and
// proves that these exact fields differ, and that no other field differs.
func sealedDifferentialExportMask(t *testing.T, b []byte) []byte {
	t.Helper()
	v := sealedDifferentialObject(t, b)
	delete(v, "requestedAt")
	if in, ok := v["input"].(map[string]any); ok {
		delete(in, "frozenAt")
	}
	return sealedDifferentialJSON(t, v)
}

type sealedDifferentialIdentity struct {
	info os.FileInfo
	sum  [32]byte
}

func sealedDifferentialFile(t *testing.T, path string) sealedDifferentialIdentity {
	t.Helper()
	i, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sealedDifferentialIdentity{i, sha256.Sum256(b)}
}

func sealedDifferentialEntries(t *testing.T, root string) []string {
	t.Helper()
	var entries []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries = append(entries, rel)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(entries)
	return entries
}

func sealedDifferentialCopy(t *testing.T, root string) string {
	t.Helper()
	dst := t.TempDir()
	if err := os.Chmod(dst, 0700); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.Mkdir(to, 0700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("unexpected fixture entry %s", rel)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(to, b, 0600)
	}); err != nil {
		t.Fatal(err)
	}
	return dst
}

func sealedDifferentialSnapshot(t *testing.T, root, id, tpl, sealedPath string, unexpected int) map[string][]byte {
	t.Helper()
	started := time.Now()
	ctx := withSealName(context.Background(), tpl)
	h, err := newLibraryHandler(root, libraryTestHost, tpl)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	h.visualNow = func() time.Time { return visualGUIStart.Add(48 * time.Hour) }
	out := map[string][]byte{}
	entries := sealedDifferentialEntries(t, root)
	var identity sealedDifferentialIdentity
	if sealedPath != "" {
		identity = sealedDifferentialFile(t, sealedPath)
		for _, name := range entries {
			if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") || strings.HasSuffix(name, "-journal") || strings.HasSuffix(name, ".tmp") {
				t.Fatalf("sealed snapshot starts with SQLite/temp sidecar %s", name)
			}
		}
	}
	unchanged := func(route string) {
		t.Helper()
		if sealedPath == "" {
			return
		}
		got := sealedDifferentialFile(t, sealedPath)
		if got.sum != identity.sum || !os.SameFile(got.info, identity.info) || !got.info.ModTime().Equal(identity.info.ModTime()) || got.info.Size() != identity.info.Size() {
			t.Fatalf("%s changed sealed bytes, identity, size or mtime", route)
		}
		if got := sealedDifferentialEntries(t, root); !reflect.DeepEqual(got, entries) {
			t.Fatalf("%s changed root/recordings entries:\nbefore %v\nafter %v", route, entries, got)
		}
	}
	put := func(key string, data []byte) {
		t.Helper()
		out[key] = append([]byte(nil), data...)
		unchanged(key)
	}
	get := func(route string) []byte {
		t.Helper()
		w := libraryRequest(h, route, func(r *http.Request) { *r = *r.WithContext(ctx) })
		if w.Code != 200 {
			t.Fatalf("%s: HTTP %d: %s", route, w.Code, w.Body)
		}
		unchanged(route)
		return w.Body.Bytes()
	}
	api := "/api/recordings/" + id
	pageBytes := get("/api/recordings")
	var page libraryPage
	if err := json.Unmarshal(pageBytes, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != id || page.UnexpectedEntries != unexpected {
		t.Fatalf("library list: %+v, unexpected want %d", page, unexpected)
	}
	item := page.Items[0]
	if item.SizeBytes == nil || item.Sealed != (sealedPath != "") {
		t.Fatalf("library list storage: size %v sealed %v", item.SizeBytes, item.Sealed)
	}
	// Size on disk and the original download legitimately follow the storage
	// (directory or sealed file); every other projection must not.
	item.SizeBytes, item.Sealed = nil, false
	put("/api/recordings:item", sealedDifferentialJSON(t, item))
	put(api, get(api))
	put(api+"/preview-durable", get(api+"/preview-durable"))
	put("viewer", sealedDifferentialStorageMask.ReplaceAll(get("/recordings/"+id+"?mode=live"), nil))
	put("facts", sealedDifferentialJSON(t, libraryVisualFactsFor(ctx, root, id, h.visualNow)))
	for after, n := 0, 0; ; n++ {
		route := fmt.Sprintf("%s/activity?after=%d&limit=7", api, after)
		b := get(route)
		put(fmt.Sprintf("activity/page-%d", n), b)
		var p activityListReport
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		if !p.MoreObserved {
			break
		}
		if p.NextAfter <= after || n > 30 {
			t.Fatal("activity pagination did not advance")
		}
		after = p.NextAfter
	}
	for _, backward := range []bool{false, true} {
		cursor := 0
		if backward {
			cursor = 25
		}
		for n := 0; ; n++ {
			key := "after"
			if backward {
				key = "before"
			}
			route := fmt.Sprintf("%s/marks?%s=%d&limit=7", api, key, cursor)
			b := get(route)
			put(fmt.Sprintf("marks/%s/page-%d", key, n), b)
			var p libraryMarksPage
			if err := json.Unmarshal(b, &p); err != nil {
				t.Fatal(err)
			}
			if (!backward && !p.MoreAfter) || (backward && !p.MoreBefore) {
				break
			}
			next := p.NextAfter
			if backward {
				next = p.PrevBefore
			}
			if next == cursor || n > 30 {
				t.Fatal("marks pagination did not advance")
			}
			cursor = next
		}
	}
	for segment := 1; segment <= 2; segment++ {
		c, err := buildVisualHistory(ctx, t.TempDir(), root, id, segment, visualHistoryConfig{budgets: defaultVisualHistoryBudgets()})
		if err != nil {
			t.Fatal(err)
		}
		b, err := c.Index(ctx, "/history/stable/window/")
		if err != nil {
			t.Fatal(err)
		}
		put(fmt.Sprintf("history/%d/index", segment), b)
		put(fmt.Sprintf("history/%d/facts", segment), sealedDifferentialJSON(t, map[string]any{"windows": c.WindowCount(), "duration": c.Duration(), "markers": c.markers}))
		for n := 0; n < c.WindowCount(); n++ {
			b, err := c.Window(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			put(fmt.Sprintf("history/%d/window/%d", segment, n), b)
			file, err := os.ReadFile(filepath.Join(c.dir, fmt.Sprintf("window-%d.json", n)))
			if err != nil || !bytes.Equal(file, b) {
				t.Fatalf("history window file %d/%d differs from rendered bytes: %v", segment, n, err)
			}
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	cli := func(args ...string) []byte {
		t.Helper()
		args = append(args, "--library", root)
		stdout, stderr, err := cliRun(t, args...)
		if err != nil {
			t.Fatalf("CLI %v: %v: %s %s", args, err, stdout, stderr)
		}
		unchanged(strings.Join(args, " "))
		return []byte(stdout)
	}
	recipe := filepath.Join(t.TempDir(), "recipe.json")
	if err := os.WriteFile(recipe, []byte(sealedDifferentialRecipe), 0600); err != nil {
		t.Fatal(err)
	}
	put("export/plan", sealedDifferentialExportMask(t, cli("export", id, "--format", "cast", "--recipe", recipe, "--plan")))
	zipPath := filepath.Join(t.TempDir(), "recording.zip")
	cli("export", id, zipPath, "--format", "cast", "--recipe", recipe)
	_, files := visualExportRead(t, zipPath)
	for name, b := range files {
		if name == "metadata.json" {
			b = sealedDifferentialExportMask(t, b)
		}
		put("export/zip/"+name, b)
	}
	spec := exportTestSpec(id)
	spec.Format = "h264"
	spec.Recipe = visualTestRecipe(t, sealedDifferentialRecipe)
	in, err := freezeVisualExport(ctx, root, spec)
	if err != nil {
		t.Fatal(err)
	}
	p, err := prepareVisualExport(ctx, root, spec, in)
	if err != nil {
		in.close()
		t.Fatal(err)
	}
	if len(p.manifest.Chapters) == 0 {
		t.Fatal("chapter comparison would be empty")
	}
	put("export/chapters", sealedDifferentialJSON(t, p.manifest.Chapters))
	resolve, err := p.resolve.Encode()
	if err != nil {
		t.Fatal(err)
	}
	put("export/resolve-markers", resolve)
	in.close()
	put("activity/ls", cli("activity", "ls", id))
	list := sealedDifferentialObject(t, cli("ls", "--json"))
	items := list["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("ls items: %v", items)
	}
	row := items[0].(map[string]any)
	wantHolder := "not-expected"
	if sealedPath != "" {
		wantHolder = "sealed"
	}
	if row["holder"] != wantHolder {
		t.Fatalf("holder=%v want %s", row["holder"], wantHolder)
	}
	// Explicitly permitted presentation-only ls mask.
	delete(row, "holder")
	put("ls/json", sealedDifferentialJSON(t, list))
	var doctorOut, doctorErr bytes.Buffer
	if err := offlineCLIWithObservation([]string{"doctor", id, "--library", root, "--json"}, &doctorOut, &doctorErr, absentObservation()); err != nil {
		t.Fatalf("doctor: %v %s %s", err, doctorOut.String(), doctorErr.String())
	}
	unchanged("doctor")
	doctor := sealedDifferentialObject(t, doctorOut.Bytes())
	if doctor["visual"] == nil || doctor["activity"] == nil {
		t.Fatalf("doctor omitted verified sections: %s", doctorOut.String())
	}
	visual := doctor["visual"].(map[string]any)
	activity := doctor["activity"].(map[string]any)
	activityReport := activity["report"].(map[string]any)
	put("doctor/compared", sealedDifferentialJSON(t, map[string]any{
		"visual.counts": visual["counts"], "visual.verifiedPrefix": visual["verifiedPrefix"], "visual.complete": visual["complete"],
		"acknowledged": doctor["acknowledged"], "completion": doctor["completion"],
		"activity.count": activity["count"], "activity.report.prefix": activityReport["verifiedPrefix"],
	}))
	if visual["counts"] == nil || visual["verifiedPrefix"] == nil || visual["complete"] != true || doctor["acknowledged"] == nil || doctor["completion"] == nil || activity["count"] != float64(24) || activityReport["verifiedPrefix"] == nil {
		t.Fatalf("doctor comparison must include verified nonempty fields: %s", doctorOut.String())
	}
	if sealedPath != "" {
		section, ok := doctor["sealed"].(map[string]any)
		if !ok || section["status"] != "ok" || section["kind"] != "complete" {
			t.Fatalf("doctor sealed section: %v", doctor)
		}
	}
	store := &Store{Root: root, SealName: tpl}
	state, err := store.read(id)
	if err != nil {
		t.Fatal(err)
	}
	put("Store.read", sealedDifferentialJSON(t, map[string]any{"status": state.Status, "segments": state.Segments, "events": state.Events}))
	// Config and status are read-only routes too; their output is not part of
	// the differential contract, but their effects are checked immediately.
	cli("status", id, "--json")
	stdout, stderr, err := cliRun(t, "config", "--json")
	if err != nil {
		t.Fatalf("config: %v %s %s", err, stdout, stderr)
	}
	unchanged("config")
	t.Logf("snapshot: %d compared route outputs in %s", len(out), time.Since(started))
	return out
}

func sealedDifferentialEqual(t *testing.T, want, got map[string][]byte) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("snapshot keys: got %d want %d", len(got), len(want))
	}
	for key, a := range want {
		if b, ok := got[key]; !ok || !bytes.Equal(a, b) {
			t.Errorf("differential %s:\nA %s\nphase %s", key, a, b)
		}
	}
}

func TestSealedDifferential(t *testing.T) {
	for _, tpl := range []string{"{id}", "{date}-{target}-{id}"} {
		t.Run(tpl, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "config.json")
			if err := writeJSON(config, map[string]any{"seal": map[string]any{"name": tpl}}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AREEL_CONFIG", config)
			t.Setenv("AREEL_SEAL_NAME", tpl)
			g := newSealedDifferentialFixture(t, tpl)
			name, err := renderSealName(tpl, g.state)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(g.root, name+".areel")
			sourceBlocks := sealedDifferentialBlocks(t, filepath.Join(g.dir, visualcapture.StoreDirName, visualstore.DatabaseName))
			if sourceBlocks < 4 {
				t.Fatalf("fixture has %d source blocks; need multiple blocks per segment", sourceBlocks)
			}
			a := sealedDifferentialSnapshot(t, g.root, g.id, tpl, "", 0)
			aprime := sealedDifferentialCopy(t, g.root)
			spec := exportTestSpec(g.id)
			spec.Recipe = visualTestRecipe(t, sealedDifferentialRecipe)
			var exports [2]map[string][]byte
			for i := range exports {
				bundle, err := buildExport(withSealName(context.Background(), tpl), g.root, spec, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				_, exports[i] = visualExportRead(t, bundle)
			}
			m0 := sealedDifferentialObject(t, exports[0]["metadata.json"])
			m1 := sealedDifferentialObject(t, exports[1]["metadata.json"])
			if m0["requestedAt"] == m1["requestedAt"] || m0["input"].(map[string]any)["frozenAt"] == m1["input"].(map[string]any)["frozenAt"] {
				t.Fatal("same-directory export clock masks not proven to vary")
			}
			for i := range exports {
				exports[i]["metadata.json"] = sealedDifferentialExportMask(t, exports[i]["metadata.json"])
			}
			sealedDifferentialEqual(t, exports[0], exports[1])
			started := time.Now()
			if _, err := seal.Seal(context.Background(), seal.Source{RecordingDir: g.dir}, seal.Destination{Path: path}, sealedDifferentialOptions()); err != nil {
				t.Fatal(err)
			}
			t.Logf("seal 3000 views/24 activity rows: %s; file bytes=%d", time.Since(started), sealedDifferentialFile(t, path).info.Size())
			if blocks := sealedDifferentialBlocks(t, path); blocks != 2 || blocks >= sourceBlocks {
				t.Fatalf("source blocks %d were not merged into two sealed segment blocks: %d", sourceBlocks, blocks)
			}
			sealedDifferentialCleanSourceSidecars(t, g.dir)
			sealedDifferentialEqual(t, a, sealedDifferentialSnapshot(t, g.root, g.id, tpl, path, 0))
			bprime := sealedDifferentialCopy(t, g.root)
			if _, err := seal.RemoveSource(context.Background(), sealedDifferentialRemoval(g.dir, path)); err != nil {
				t.Fatal(err)
			}
			sealedDifferentialEqual(t, a, sealedDifferentialSnapshot(t, g.root, g.id, tpl, path, 0))
			leftover := filepath.Join(bprime, "recordings", "."+g.id+".deleting-0123456789abcdef")
			if err := os.Rename(filepath.Join(bprime, "recordings", g.id), leftover); err != nil {
				t.Fatal(err)
			}
			sealedDifferentialEqual(t, a, sealedDifferentialSnapshot(t, bprime, g.id, tpl, filepath.Join(bprime, name+".areel"), 1))
			if err := os.WriteFile(filepath.Join(aprime, "."+name+".areel.tmp-0123456789abcdef"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			sealedDifferentialEqual(t, a, sealedDifferentialSnapshot(t, aprime, g.id, tpl, "", 1))
		})
	}
}
