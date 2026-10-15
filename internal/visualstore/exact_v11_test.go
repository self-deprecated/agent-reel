package visualstore_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// Reuse the deterministic V14 fixture and its exactRecord oracle. This build
// follows its original commit cuts, then finishes it for seal eligibility.
func TestSealedV11DeterministicExact(t *testing.T) {
	f := deterministicFixture()
	dir := filepath.Join(t.TempDir(), "visual")
	s, err := visualstore.Create(dir, archive.Identity{Recording: "exact-v11", Backend: "oracle", Instance: "v14"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	segment := 0
	segmentViews := uint64(0)
	var cutoff visualtime.Cutoff
	commits := map[int64]bool{}
	for _, n := range f.corpus.Commits {
		commits[n] = true
	}
	for _, v := range f.corpus.Views {
		if v.Segment != segment {
			if segment != 0 {
				must(s.CloseSegment(segment, cutoff.Source))
				must(s.Commit(cutoff))
			}
			scr := f.screens[v.ScreenID]
			must(s.BeginSegment(v.Segment, archive.Geometry{Columns: scr.Cols, Rows: scr.Rows}, "exact"))
			segment, segmentViews = v.Segment, 0
		}
		exactRecord(t, s, f, v)
		segmentViews++
		cutoff = visualtime.Cutoff{Segment: segment, Source: exactPoint(v), Presentations: segmentViews}
		if commits[v.Seq] {
			must(s.Commit(cutoff))
		}
	}
	cutoff.SourceComplete = true
	must(s.Complete(cutoff))
	must(s.Close())
	path := filepath.Join(t.TempDir(), "exact.areel")
	db, err := visualstore.CreateSealedFile(path)
	must(err)
	_, err = visualstore.SealVisual(context.Background(), dir, db, visualstore.DefaultLimits())
	must(err)
	must(db.Close())
	a, err := visualstore.OpenReader(dir)
	must(err)
	defer a.Close()
	b, err := visualstore.OpenSealed(path)
	must(err)
	defer b.Close()
	ctx := context.Background()
	as, err := a.Snapshot(ctx)
	must(err)
	bs, err := b.Snapshot(ctx)
	must(err)
	if !reflect.DeepEqual(as, bs) {
		t.Fatal("snapshot differs")
	}
	visualstore.V11CompareViewOracle(t, a, b, as, bs)
	for id := range f.screens {
		x, err := a.Screen(ctx, as, id)
		must(err)
		y, err := b.Screen(ctx, bs, id)
		must(err)
		if !reflect.DeepEqual(x, y) {
			t.Fatalf("screen %d differs", id)
		}
	}
	t.Logf("deterministicFixture: %d views, all views and selected pages/seeks exactly match directory", len(f.corpus.Views))
}
