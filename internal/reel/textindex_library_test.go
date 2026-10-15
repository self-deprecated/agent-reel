//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/textindex"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// searchFixture: an 80x24 recording whose shell prints "All tests passed" once
// at 12.5 s on row 5, with a labelled manual mark ("start refactor", declared
// 2 s earlier than its 20 s placement) and Pi-style marks, sealed with its
// source removed.
func searchFixture(t *testing.T) *visualGUI {
	t.Helper()
	g := newVisualGUIState(t, State{ID: strings.Repeat("5", 32), Target: fakeTarget("search-instance"), Status: "stopped"})
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("$ make test"))
	g.view(3, visualGUIText("$ make test", "running 12 tests"))
	g.view(12.5, visualGUIText("$ make test", "running 12 tests", "", "", "", "All tests passed"))
	g.view(14, visualGUIText("$ make test", "running 12 tests", "", "", "", "All tests passed", "$ git commit"))
	g.view(25, visualGUIText("$ vim notes.md"))
	g.completeAt(30)
	g.acknowledge()
	a := newVisualActivity(t, g.dir, g.state)
	// Marks exactly as main stores them: `areel mark --ago 2s "start
	// refactor"` (markEnvelope) and Pi's `/reel mark` (kind pi:mark, the
	// label in the envelope, no data).
	raw, e := markEnvelope("start refactor", 2*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	var mark activityEnvelope
	if e = json.Unmarshal(raw, &mark); e != nil {
		t.Fatal(e)
	}
	a.add(mark, true, nil, estimatedAt(1, 4, 14, 20))
	pi := activityFixture("pi-mark-1")
	pi.Source, pi.Kind, pi.Label, pi.Data = "pi:agent-reel", "pi:mark", strptr("review checkpoint"), nil
	a.add(pi, true, nil, estimatedAt(1, 5, 25, 26))
	a.add(roleEnvelope("agent-1", "assistant"), true, nil, estimatedAt(1, 3, 12.5, 13))
	a.add(indexedEnvelope("row-1", "bulk not a marker"), false, nil, estimatedAt(1, 1, 0, 1))
	a.commit()
	a.close()
	g.state.Suppressed = true
	g.saveState()
	g.closeWriter()
	path := filepath.Join(g.root, g.id+".areel")
	opts := seal.Options{Limits: visualstore.DefaultLimits(), ActivityLimits: activitystore.DefaultLimits(), Sealer: "test"}
	if _, e := seal.Seal(context.Background(), seal.Source{RecordingDir: g.dir}, seal.Destination{Path: path}, opts); e != nil {
		t.Fatal(e)
	}
	sealedDifferentialCleanSourceSidecars(t, g.dir)
	if _, e := seal.RemoveSource(context.Background(), seal.Removal{RecordingDir: g.dir, SealedPath: path, Limits: opts.Limits, ActivityLimits: opts.ActivityLimits}); e != nil {
		t.Fatal(e)
	}
	return g
}

func TestSealedTextIndexBuildSearchAndRebuild(t *testing.T) {
	g := searchFixture(t)
	ctx := context.Background()
	r := ensureRecordingIndex(ctx, g.root, g.id, false)
	if r.Status != indexBuilt || r.Views != 5 || r.Rows != 5 || r.Activity != 2 || r.Bytes == 0 {
		t.Fatalf("build %+v", r)
	}
	path := libraryIndexPath(g.root, g.id)
	for p, mode := range map[string]os.FileMode{filepath.Dir(path): os.ModeDir | 0700, path: 0600} {
		st, e := os.Lstat(p)
		if e != nil || st.Mode() != mode {
			t.Fatalf("%s mode %v %v", p, st.Mode(), e)
		}
	}
	if again := ensureRecordingIndex(ctx, g.root, g.id, false); again.Status != indexFresh || again.Rows != 5 {
		t.Fatalf("fresh %+v", again)
	}
	failBuild := func(context.Context, sealedIndexSource) (string, error) {
		t.Fatal("fresh index rebuilt")
		return "", nil
	}
	report, e := searchLibrary(ctx, g.root, g.id, textindex.Query{Text: "tests PASSED"}, failBuild)
	if e != nil || len(report.Hits) != 1 {
		t.Fatalf("search %+v %v", report, e)
	}
	h := report.Hits[0]
	if h.Recording != g.id || h.Kind != "screen" || h.Segment != 1 || h.At != "12.5" || h.ViewSeq != 3 || h.Y != 5 || h.Cols != [2]int{4, 16} || h.Text != "All tests passed" || h.Count != 1 {
		t.Fatalf("hit %+v", h)
	}
	if len(report.Recordings) != 1 || report.Recordings[0].Session == "" || report.Recordings[0].Hits != 1 {
		t.Fatalf("recordings %+v", report.Recordings)
	}
	// A labelled mark is searchable, placed at its estimate less its offset.
	report, e = searchLibrary(ctx, g.root, g.id, textindex.Query{Text: "refactor"}, failBuild)
	if e != nil || len(report.Hits) != 1 || report.Hits[0].Kind != "activity" || report.Hits[0].At != "18" || report.Hits[0].Y != -1 || report.Hits[0].ViewSeq != 4 {
		t.Fatalf("mark %+v %v", report.Hits, e)
	}
	if report, e = searchLibrary(ctx, g.root, g.id, textindex.Query{Text: "checkpoint"}, failBuild); e != nil || len(report.Hits) != 1 || report.Hits[0].Kind != "activity" || report.Hits[0].At != "26" || report.Hits[0].ViewSeq != 5 {
		t.Fatalf("pi mark %+v %v", report.Hits, e)
	}
	// Pi's phase/role enumerations are not text.
	if report, _ = searchLibrary(ctx, g.root, g.id, textindex.Query{Text: "assistant"}, failBuild); len(report.Hits) != 0 {
		t.Fatalf("role indexed %+v", report.Hits)
	}
	// A different sealed digest makes the index stale: it is rebuilt.
	db, e := sql.Open("sqlite", "file:"+path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`UPDATE meta SET value = 'other-digest' WHERE key = 'digest'`); e != nil {
		t.Fatal(e)
	}
	db.Close()
	if status, _ := textindex.Check(path, "other-digest"); status != textindex.StatusFresh {
		t.Fatal("tamper failed", status)
	}
	if rebuilt := ensureRecordingIndex(ctx, g.root, g.id, false); rebuilt.Status != indexBuilt {
		t.Fatalf("stale index kept %+v", rebuilt)
	}
	built := 0
	report, e = searchLibrary(ctx, g.root, "", textindex.Query{Text: "git commit"}, func(context.Context, sealedIndexSource) (string, error) { built++; return indexBuilt, nil })
	if e != nil || built != 0 || len(report.Hits) != 1 || report.Hits[0].At != "14" || report.Hits[0].Y != 6 {
		t.Fatalf("library search %+v %v", report, e)
	}
	// An index naming another recording is never served.
	db, e = sql.Open("sqlite", "file:"+path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`UPDATE meta SET value = 'ffffffffffffffffffffffffffffffff' WHERE key = 'recording'`); e != nil {
		t.Fatal(e)
	}
	db.Close()
	if report, e = searchLibrary(ctx, g.root, g.id, textindex.Query{Text: "git"}, failBuild); e != nil || len(report.Hits) != 0 || report.Recordings[0].Status != indexUnavailable {
		t.Fatalf("foreign index served %+v %v", report, e)
	}
	// Interrupted builds' temporary files are swept once an hour old.
	old, fresh := filepath.Join(filepath.Dir(path), "."+g.id+".sqlite.tmp-1"), filepath.Join(filepath.Dir(path), "."+g.id+".sqlite.tmp-2")
	for _, p := range []string{old, fresh} {
		if e = os.WriteFile(p, []byte("x"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e = os.Chtimes(old, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)); e != nil {
		t.Fatal(e)
	}
	// Deleting the cache loses nothing: the next index rebuilds it.
	if e = os.Remove(path); e != nil {
		t.Fatal(e)
	}
	if r := ensureRecordingIndex(ctx, g.root, g.id, false); r.Status != indexBuilt {
		t.Fatalf("after delete %+v", r)
	}
	if _, e = os.Stat(old); !os.IsNotExist(e) {
		t.Fatal("old leftover kept", e)
	}
	if _, e = os.Stat(fresh); e != nil {
		t.Fatal("fresh temporary file swept", e)
	}
}

func TestSearchAPIQueuesAndServes(t *testing.T) {
	g := searchFixture(t)
	built := make(chan string, 4)
	g.h.libraryIndexerFor().onBuilt = func(id string, err error) {
		if err != nil {
			t.Error(err)
		}
		built <- id
	}
	get := func(query string) searchReport {
		t.Helper()
		w := libraryRequest(g.h, "/api/search?"+query, nil)
		var r searchReport
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &r) != nil || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %s", query, w.Code, w.Body)
		}
		return r
	}
	q := url.Values{"q": {"All tests"}}.Encode()
	first := get(q)
	if first.Pending != 1 || len(first.Hits) != 0 || len(first.Recordings) != 1 || first.Recordings[0].Status != indexQueued || first.Recordings[0].Reason == "" {
		t.Fatalf("first %+v", first)
	}
	select {
	case id := <-built:
		if id != g.id {
			t.Fatal(id)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("background index not built")
	}
	r := get(q + "&recording=" + g.id + "&limit=5")
	if r.Pending != 0 || len(r.Hits) != 1 || r.Hits[0].At != "12.5" || r.Hits[0].Y != 5 || r.Recordings[0].Status != indexFresh || r.Recordings[0].Session == "" {
		t.Fatalf("second %+v", r)
	}
	if fuzzy := get(url.Values{"q": {"tests pased"}, "fuzzy": {"1"}}.Encode()); len(fuzzy.Hits) != 1 || fuzzy.Hits[0].Text != "All tests passed" {
		t.Fatalf("fuzzy %+v", fuzzy)
	}
	// Raw JSON carries no paths.
	body := libraryRequest(g.h, "/api/search?"+q, nil).Body.String()
	if strings.Contains(body, g.root) {
		t.Fatal("path in search output")
	}
	for _, bad := range []string{"", "q=", "q=%20%20", "q=a&q=b", "q=x&limit=0", "q=x&limit=201", "q=x&fuzzy=yes", "q=x&recording=nope", "q=x&other=1", "q=" + strings.Repeat("x", 257)} {
		if w := libraryRequest(g.h, "/api/search?"+bad, nil); w.Code != 400 {
			t.Fatal(bad, w.Code)
		}
	}
	// The Host/peer gate applies as to every route.
	if w := libraryRequest(g.h, "/api/search?"+q, func(r *http.Request) { r.Host = "evil.example:80" }); w.Code != 403 {
		t.Fatal("host gate", w.Code)
	}
	if w := libraryRequest(g.h, "/api/search?"+q, func(r *http.Request) { r.Method = "POST" }); w.Code != 405 {
		t.Fatal("method", w.Code)
	}
}

func TestSearchAndIndexCLI(t *testing.T) {
	g := searchFixture(t)
	var out, errOut bytes.Buffer
	if e := CLI([]string{"search", "all TESTS", "--library", g.root, "--json"}, &out, &errOut); e != nil {
		t.Fatal(e, errOut.String())
	}
	var r searchReport
	if e := json.Unmarshal(out.Bytes(), &r); e != nil || len(r.Hits) != 1 || r.Hits[0].At != "12.5" || !strings.Contains(errOut.String(), "indexing "+shortID(g.id)) {
		t.Fatalf("%s %s %v", out.String(), errOut.String(), e)
	}
	out.Reset()
	if e := CLI([]string{"search", "refactor", g.id[:8], "--library", g.root}, &out, &errOut); e != nil {
		t.Fatal(e)
	}
	if text := out.String(); !strings.Contains(text, "1 0:18.000  mark     start refactor") || !strings.Contains(text, shortID(g.id)) {
		t.Fatalf("text output %q", text)
	}
	out.Reset()
	if e := CLI([]string{"search", "nothing like this", "--library", g.root}, &out, &errOut); e != nil || !strings.Contains(out.String(), "no matches") {
		t.Fatal(e, out.String())
	}
	out.Reset()
	if e := CLI([]string{"index", "--all", "--library", g.root, "--json"}, &out, &errOut); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), `"status":"fresh"`) {
		t.Fatal(out.String())
	}
	out.Reset()
	if e := CLI([]string{"index", g.id, "--force", "--library", g.root}, &out, &errOut); e != nil || !strings.Contains(out.String(), "indexed: 5 rows, 2 marks from 5 views") {
		t.Fatal(e, out.String())
	}
	for _, args := range [][]string{{"index"}, {"index", g.id, "--all"}, {"search"}, {"search", "  "}, {"search", "x", "--limit", "0"}} {
		if e := CLI(append(args, "--library", g.root), &out, &errOut); e == nil {
			t.Fatal("accepted", args)
		}
	}
}
