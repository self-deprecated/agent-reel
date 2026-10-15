package activitystore

import (
	"context"
	"testing"
)

// Marker rows (every fifth test record) page in order within the selection,
// and the summary counts them and names the newest.
func TestReaderMarkersAndSummary(t *testing.T) {
	s, dir := create(t)
	fill(t, s, 1, 23, 4, 64)
	r := openReader(t, dir)
	sel := snapshot(t, r)
	ctx := context.Background()
	first, err := r.Markers(ctx, sel, 1, 2)
	if err != nil || len(first) != 2 || first[0].Seq != 5 || first[1].Seq != 10 || !first[0].Marker {
		t.Fatalf("first page %+v %v", first, err)
	}
	rest, err := r.Markers(ctx, sel, first[1].Seq+1, MaxPage)
	if err != nil || len(rest) != 2 || rest[0].Seq != 15 || rest[1].Seq != 20 {
		t.Fatalf("rest %+v %v", rest, err)
	}
	count, newest, ok, err := r.MarkerSummary(ctx, sel)
	if err != nil || !ok || count != 4 || newest.Seq != 20 {
		t.Fatalf("summary %d %+v %v %v", count, newest, ok, err)
	}
	// Rows appended after the selection stay invisible to it.
	fill(t, s, 24, 30, 7, 64)
	if count, newest, _, _ = r.MarkerSummary(ctx, sel); count != 4 || newest.Seq != 20 {
		t.Fatal("summary followed appends", count, newest.Seq)
	}
	if count, newest, _, _ = r.MarkerSummary(ctx, snapshot(t, r)); count != 6 || newest.Seq != 30 {
		t.Fatal("fresh summary", count, newest.Seq)
	}
}
