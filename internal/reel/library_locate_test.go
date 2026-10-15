package reel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

func locateFixture(t *testing.T, tpl string) (string, string, context.Context) {
	t.Helper()
	root := t.TempDir()
	if e := os.Chmod(root, 0700); e != nil {
		t.Fatal(e)
	}
	if _, e := OpenStore(root); e != nil {
		t.Fatal(e)
	}
	id := strings.Repeat("a", 32)
	dir := filepath.Join(root, "recordings", id)
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := writeJSON(filepath.Join(dir, "launch.json"), launchSpec{Target: fakeTarget("locate"), SealName: tpl}); e != nil {
		t.Fatal(e)
	}
	return root, id, withSealName(context.Background(), tpl)
}

func TestLocateFileWinsAndGuardTransition(t *testing.T) {
	for _, tpl := range []string{"{id}", "{date}-{target}-{id}"} {
		t.Run(tpl, func(t *testing.T) {
			root, id, ctx := locateFixture(t, tpl)
			g := newLibraryGuard(ctx)
			loc, e := libraryLocate(root, id, g)
			if e != nil || loc.Kind != "dir" {
				t.Fatalf("%+v %v", loc, e)
			}
			name := id + ".areel"
			if tpl != "{id}" {
				name = "2026-09-24-target-" + name
			}
			path := filepath.Join(root, name)
			if e = os.WriteFile(path, []byte("locator does not verify payload"), 0600); e != nil {
				t.Fatal(e)
			}
			if e = g.check(); !errors.Is(e, errActivityListChanged) {
				t.Fatalf("publication guard: %v", e)
			}
			loc, e = libraryLocate(root, id, newLibraryGuard(ctx))
			if e != nil || loc.Kind != "sealed" || loc.Path != path || loc.Dir != filepath.Join(root, "recordings", id) {
				t.Fatalf("%+v %v", loc, e)
			}
			// The source-pinned template wins over current settings.
			loc, e = libraryLocate(root, id, newLibraryGuard(withSealName(ctx, "prefix-{id}")))
			if e != nil || loc.Path != path {
				t.Fatalf("launch template lost: %+v %v", loc, e)
			}
			g = newLibraryGuard(ctx)
			if _, e = libraryLocate(root, id, g); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(path, []byte("changed"), 0600); e != nil {
				t.Fatal(e)
			}
			if e = g.check(); !errors.Is(e, errActivityListChanged) {
				t.Fatalf("immutable guard: %v", e)
			}
		})
	}
}

func TestLocateAmbiguousEveryReader(t *testing.T) {
	root, id, ctx := locateFixture(t, "{target}-{id}")
	for _, prefix := range []string{"one", "two"} {
		if e := os.WriteFile(filepath.Join(root, prefix+"-"+id+".areel"), nil, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := writeJSON((&Store{Root: root}).targetPath(fakeTarget("locate")), id); e != nil {
		t.Fatal(e)
	}
	readers := map[string]func() error{
		"control-status": func() error {
			_, e := (&Store{Root: root, SealName: "{target}-{id}"}).Control(ctx, fakeTarget("locate"), "status", "", id)
			return e
		},
		"control-bound": func() error {
			_, e := (&Store{Root: root, SealName: "{target}-{id}"}).ControlBound(id, "status")
			return e
		},

		"doctor": func() error {
			_, e := offlineRecording(ctx, root, id, "", visualstore.DefaultLimits(), offlineObservation{}, "{target}-{id}")
			return e
		},
		"preview": func() error { _, e := loadVisualDurableStill(ctx, root, id); return e },
		"facts":   func() error { _, e := libraryVisualObserve(ctx, root, id, time.Now, nil); return e },
		"history": func() error {
			_, e := buildVisualHistory(ctx, t.TempDir(), root, id, 1, visualHistoryConfig{budgets: defaultVisualHistoryBudgets()})
			return e
		},
		"export":   func() error { _, e := freezeVisualExport(ctx, root, exportTestSpec(id)); return e },
		"locator":  func() error { _, e := libraryLocate(root, id, newLibraryGuard(ctx)); return e },
		"state":    func() error { _, _, e := libraryState(root, id, newLibraryGuard(ctx)); return e },
		"visual":   func() error { _, _, e := libraryVisualReader(ctx, root, id, newLibraryGuard(ctx)); return e },
		"detail":   func() error { _, e := libraryDetail(ctx, root, id); return e },
		"activity": func() error { _, e := listActivityContext(ctx, root, id, 0, 20, activityListOutputMax, nil); return e },
		"marks":    func() error { _, e := libraryMarks(ctx, root, id, 0, 0, 20, libraryOutputMax); return e },
		"control":  func() error { _, e := (&Store{Root: root, SealName: "{target}-{id}"}).read(id); return e },
	}
	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			if e := read(); e == nil || !strings.Contains(e.Error(), "ambiguous sealed file") {
				t.Fatalf("got %v", e)
			}
		})
	}
	// Other readers refuse the ambiguous ID; ls isolates it as an unavailable row.
	t.Run("ls", func(t *testing.T) {
		r, e := listRecordings(ctx, root, "", 128, listDefaults())
		if e != nil {
			t.Fatal(e)
		}
		if len(r.Items) != 1 || r.Items[0].Metadata != "unavailable" || !strings.Contains(r.Items[0].Reason, "ambiguous sealed file") {
			t.Fatalf("row %+v", r)
		}
	})
	if _, e := os.Lstat(filepath.Join(root, "lock")); !os.IsNotExist(e) {
		t.Fatalf("ambiguous reads created lock: %v", e)
	}
	page, e := libraryList(ctx, root, "", 128)
	if e != nil || len(page.Items) != 0 || page.UnexpectedEntries != 2 {
		t.Fatalf("list %+v %v", page, e)
	}
	ids, e := recordingIDs(root, "{target}-{id}")
	if e != nil || len(ids) != 0 {
		t.Fatalf("ids %v %v", ids, e)
	}
}

func TestLocateReservedAndSymlink(t *testing.T) {
	root, id, ctx := locateFixture(t, "{id}")
	reserved := []string{"." + id + ".areel.tmp-" + strings.Repeat("b", 16), "." + id + ".areel.salvage-" + strings.Repeat("c", 16), "." + id + ".deleting-" + strings.Repeat("d", 16)}
	for _, parent := range []string{root, filepath.Join(root, "recordings")} {
		for _, name := range reserved {
			if e := os.WriteFile(filepath.Join(parent, name), nil, 0600); e != nil {
				t.Fatal(e)
			}
			if _, e := libraryLocate(root, name, newLibraryGuard(ctx)); e == nil {
				t.Fatalf("located reserved %s", name)
			}
		}
	}
	loc, e := libraryLocate(root, id, newLibraryGuard(ctx))
	if e != nil || loc.Kind != "dir" {
		t.Fatalf("%+v %v", loc, e)
	}
	page, e := libraryList(ctx, root, "", 128)
	if e != nil || page.UnexpectedEntries != 6 || len(page.Items) != 1 {
		t.Fatalf("reserved list %+v %v", page, e)
	}
	if e = os.Symlink(filepath.Join(root, reserved[0]), filepath.Join(root, id+".areel")); e != nil {
		t.Fatal(e)
	}
	if _, e = libraryLocate(root, id, newLibraryGuard(ctx)); e == nil || !strings.Contains(e.Error(), "symlink") {
		t.Fatalf("symlink accepted: %v", e)
	}
}

func TestLocateRootEntriesDoNotCollide(t *testing.T) {
	root := t.TempDir()
	if e := os.Chmod(root, 0700); e != nil {
		t.Fatal(e)
	}
	s, e := OpenStore(root)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.withLock(func() error { return nil }); e != nil {
		t.Fatal(e)
	}
	// Store/OpenStore owns recordings/, targets/, and lock; readLibraryConfig
	// reads config.json. Other ordinary root entries must be silently ignored.
	if e = os.WriteFile(filepath.Join(root, "config.json"), []byte("{}"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "user-notes"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	// Compare every root reader before/after the holder's persistent control
	// file exists, using fresh catalog caches so this also exercises loading.
	readRoot := func() []any {
		t.Helper()
		var snapshot []any
		for _, tpl := range []string{"{id}", "{date}-{target}-{id}", "{target}{id}"} {
			ctx := withSealName(context.Background(), tpl)
			id := strings.Repeat("a", 32)
			loc, err := libraryLocate(root, id, newLibraryGuard(ctx))
			if err != nil {
				t.Fatal(err)
			}
			left, err := recoverLeftovers(root, id, tpl)
			if err != nil {
				t.Fatal(err)
			}
			page, err := libraryList(ctx, root, "", 128)
			if err != nil {
				t.Fatal(err)
			}
			cache, err := newLibraryCatalogCache(root)
			if err != nil {
				t.Fatal(err)
			}
			cached, err := cache.Page(ctx, "", 128)
			cache.Close()
			if err != nil {
				t.Fatal(err)
			}
			snapshot = append(snapshot, loc, left, page, cached.Page)
		}
		out, stderr, err := cliRun(t, "ls", "--library", root, "--json")
		if err != nil {
			t.Fatal(err)
		}
		return append(snapshot, out, stderr)
	}
	before := readRoot()
	if e = os.WriteFile(filepath.Join(root, "seal.lock"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if after := readRoot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("seal.lock changed root readers:\nbefore: %#v\nafter: %#v", before, after)
	}
	entries, e := os.ReadDir(root)
	if e != nil {
		t.Fatal(e)
	}
	if len(entries) != 6 {
		t.Fatalf("root entries: %v", entries)
	}
	for _, tpl := range []string{"{id}", "{date}-{target}-{id}", "{target}{id}"} {
		re, e := sealNameRegexp(tpl)
		if e != nil {
			t.Fatal(e)
		}
		for _, entry := range entries {
			if reservedRecordingName(entry.Name()) || re.MatchString(entry.Name()) {
				t.Fatalf("root collision %s %s", tpl, entry.Name())
			}
		}
		page, e := libraryList(withSealName(context.Background(), tpl), root, "", 128)
		if e != nil || page.UnexpectedEntries != 0 || len(page.Items) != 0 {
			t.Fatalf("%+v %v", page, e)
		}
	}
	if e = os.WriteFile(filepath.Join(root, "unexpected.areel"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	page, e := libraryList(context.Background(), root, "", 128)
	if e != nil || page.UnexpectedEntries != 1 {
		t.Fatalf("%+v %v", page, e)
	}
}

// A published interrupted prefix wins over a retained source with a real,
// committed extra view. Every visual content path consumes the sealed reader.
func TestSealedFileWinsOverCommittedTail(t *testing.T) {
	g := newVisualGUI(t, "interrupted")
	g.begin(1, 16, 4)
	g.view(0, visualScreen("sealed prefix"))
	g.commit(1)
	g.acknowledge()
	g.closeWriter()
	path := filepath.Join(g.root, g.id+".areel")
	_, e := seal.Seal(context.Background(), seal.Source{RecordingDir: g.dir}, seal.Destination{Path: path}, seal.Options{
		Kind: seal.KindInterrupted, Limits: visualstore.DefaultLimits(), ActivityLimits: activitystore.DefaultLimits(),
		Recovery: seal.RecoveryInput{SourceStatus: "recording", RecoveredAt: time.Now(), Recoverer: "test"},
	})
	if e != nil {
		t.Fatal(e)
	}
	// Reopen only our synthetic directory writer; the published file is untouched.
	g.store, e = visualstore.Open(filepath.Join(g.dir, visualcapture.StoreDirName))
	if e != nil {
		t.Fatal(e)
	}
	g.view(2, visualScreen("tail"))
	g.commit(3)
	g.closeWriter()
	source, e := visualstore.OpenReader(filepath.Join(g.dir, visualcapture.StoreDirName))
	if e != nil {
		t.Fatal(e)
	}
	tail, e := source.Snapshot(context.Background())
	source.Close()
	if e != nil || tail.ViewSeq != 2 {
		t.Fatalf("source tail %+v %v", tail, e)
	}
	ctx := context.Background()
	r, _, e := libraryVisualReader(ctx, g.root, g.id, newLibraryGuard(ctx))
	if e != nil {
		t.Fatal(e)
	}
	selected, e := r.Snapshot(ctx)
	r.Close()
	if e != nil || selected.ViewSeq != 1 {
		t.Fatalf("sealed reader %+v %v", selected, e)
	}
	facts := libraryVisualFactsFor(ctx, g.root, g.id, func() time.Time { return visualGUIStart })
	if facts.DurableViews != 1 || facts.Unacknowledged != 0 || facts.Unavailable != "" {
		t.Fatalf("tail appeared in facts: %+v", facts)
	}
	report, e := offlineRecording(ctx, g.root, g.id, "", visualstore.DefaultLimits(), offlineObservation{})
	if e != nil || report.Visual.Counts.Views != 1 {
		t.Fatalf("doctor %+v %v", report, e)
	}
	state, _, e := libraryState(g.root, g.id, newLibraryGuard(ctx))
	if e != nil || state.Status != "interrupted" {
		t.Fatalf("state %+v %v", state, e)
	}
	// Freeze and history are separate opens, each must retain the sealed count.
	in, e := freezeVisualExport(ctx, g.root, exportTestSpec(g.id))
	if e != nil {
		t.Fatal(e)
	}
	if in.sel.ViewSeq != 1 {
		t.Fatalf("export views %d", in.sel.ViewSeq)
	}
	in.close()
	history, e := buildVisualHistory(ctx, t.TempDir(), g.root, g.id, 1, visualHistoryConfig{budgets: defaultVisualHistoryBudgets()})
	if e != nil {
		t.Fatal(e)
	}
	defer history.Close()
	if history.Duration() > 2 {
		t.Fatalf("history served source tail: %v", history.Duration())
	}
}

func TestSealedFeedAndActivityStatus(t *testing.T) {
	g, path := sealedControlFixture(t, false)
	named := filepath.Join(g.root, "session-"+g.id+".areel")
	if e := os.Rename(path, named); e != nil {
		t.Fatal(e)
	}
	if e := os.RemoveAll(g.dir); e != nil {
		t.Fatal(e)
	}
	before := sealedDifferentialEntries(t, g.root)
	feed := &feedService{library: g.root, sealName: "{target}-{id}", limits: feedDefaults(time.Second)}
	page, e := feed.observe(context.Background())
	if e != nil || len(page.Items) != 1 || page.Items[0].Holder != "sealed" {
		t.Fatalf("feed %+v %v", page, e)
	}
	t.Setenv("AREEL_SEAL_NAME", "{target}-{id}")
	_, _, e = cliRun(t, "activity", "status", "--recording", g.id, "--library", g.root)
	if e == nil || !strings.Contains(e.Error(), "sealed file has no activity receiver") {
		t.Fatalf("activity status: %v", e)
	}
	after := sealedDifferentialEntries(t, g.root)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("read changed entries: %v %v", before, after)
	}
}
