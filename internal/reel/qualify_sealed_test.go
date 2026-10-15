//go:build linux && qualify

package reel

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// Exercise the post-stop harness without the real-recorder live prerequisite.
// Every deletion and corruption below is confined to this test's fixture.
func TestQualifySealedReadersAndFault(t *testing.T) {
	if view, err := currentProcView(); err != nil {
		t.Logf("sandbox ownership observation unavailable: %v (%+v)", err, view)
	}
	g := newSealedDifferentialFixture(t, "{id}")
	before, views, err := tbSentinelSources(g.dir)
	if err != nil {
		t.Fatal(err)
	}
	path := tbSealedPath(g.dir)
	if _, err := seal.Seal(context.Background(), seal.Source{RecordingDir: g.dir}, seal.Destination{Path: path}, sealedDifferentialOptions()); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(g.root)
	if err != nil {
		t.Fatal(err)
	}
	target := &tbTarget{id: g.id, rec: g.dir, stopState: State{Status: "stopped"}, result: &tbWorkerResult{}}
	if v := tbVerifyTarget(store, target); v.err == nil || !strings.Contains(v.err.Error(), "retained source") {
		t.Fatalf("retained source accepted: %+v", v)
	}
	if err := os.RemoveAll(g.dir); err != nil {
		t.Fatal(err)
	}
	v := tbVerifyTarget(store, target)
	if v.err != nil || v.stateErr != nil || !v.ack.Complete || v.rep.Status != visualstore.StatusOK || v.rep.Acknowledged != visualstore.AcknowledgedPresent {
		t.Fatalf("sealed completion: verify %v, state %v, status %s, acknowledged %s", v.err, v.stateErr, v.rep.Status, v.rep.Acknowledged)
	}
	after, sealedViews, err := tbSentinelSources(g.dir)
	if err != nil || sealedViews != views || !reflect.DeepEqual(before, after) {
		t.Fatalf("sentinel scan changed: views %d/%d, %v", views, sealedViews, err)
	}
	ids, rep, err := tbActivityIDs(g.dir)
	if err != nil || rep.Status != activitystore.StatusOK || len(ids) != 24 {
		t.Fatalf("sealed activity: %d IDs, %+v, %v", len(ids), rep, err)
	}
	if row, err := tcFlipRow(g.dir); err != nil || row == 0 {
		t.Fatalf("flip: %d %v", row, err)
	}
	if reachable, err := tcRowReachable(g.dir); err != nil || !reachable {
		t.Fatalf("flipped chunk not read: %t %v", reachable, err)
	}
	if rep, err := seal.Verify(context.Background(), path, visualstore.DefaultLimits()); err == nil && rep.Status == seal.StatusOK {
		t.Fatal("flipped chunk verified")
	}
}
