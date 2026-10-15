package reel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/links"
)

// sidecarLibrary is the sidecar view's fixture library: a stopped parent with
// two segments (a pause between them), two child recordings, a copied
// screenshot and a path-only video, all linked post hoc (declared anchors):
//
//	child-a  recording  segment 1 at 10s, child footage 20s (views every 5s)
//	child-b  recording  segment 1 at 30s, child footage 50s: it runs on past
//	                    the parent's 60s segment end, un-anchored in segment 2
//	shot-1   screenshot segment 1 at 45s
//	video-1  video      segment 1 at 5s, no stop, never served
type sidecarLibrary struct {
	g                *visualGUI
	parent, a, b     string
	root, videoPath  string
	parentSegmentEnd float64
}

func newSidecarLibrary(t *testing.T) *sidecarLibrary {
	t.Helper()
	parentID, aID, bID := strings.Repeat("3", 32), strings.Repeat("4", 32), strings.Repeat("5", 32)
	g := newVisualGUIState(t, State{ID: parentID, Target: linkTarget("sidecar-parent"), Status: "stopped"})
	g.begin(1, 80, 24)
	for _, at := range []float64{0, 20, 40} {
		g.view(at, visualGUIText(sidecarText("PARENT-S1", at)))
	}
	g.closeSegment(60)
	g.begin(2, 80, 24)
	g.view(0, visualGUIText("PARENT-S2-000"))
	g.closeSegment(40)
	g.acknowledge()
	a := addVisualGUI(t, g.root, State{ID: aID, Target: linkTarget("sidecar-child-a"), Status: "stopped"})
	a.begin(1, 80, 24)
	for _, at := range []float64{0, 5, 10, 15} {
		a.view(at, visualGUIText(sidecarText("CHILD-A", at)))
	}
	a.closeSegment(20)
	a.acknowledge()
	b := addVisualGUI(t, g.root, State{ID: bID, Target: linkTarget("sidecar-child-b"), Status: "stopped"})
	b.begin(1, 80, 24)
	for _, at := range []float64{0, 8, 30} {
		b.view(at, visualGUIText(sidecarText("CHILD-B", at)))
	}
	b.closeSegment(50)
	b.acknowledge()
	a.closeWriter()
	b.closeWriter()
	g.closeWriter()
	files := t.TempDir()
	shot := filepath.Join(files, "login.png")
	if e := os.WriteFile(shot, sidecarPNG, 0600); e != nil {
		t.Fatal(e)
	}
	video := filepath.Join(files, "SIDECAR-SECRET-VIDEO.mkv")
	if e := os.WriteFile(video, []byte("video"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{
		{"--recording", aID, "--at", "10", "--id", "child-a", "--label", "worker A"},
		{"--recording", bID, "--at", "30", "--id", "child-b", "--label", "worker B"},
		{"--kind", "screenshot", "--path", shot, "--at", "45", "--id", "shot-1", "--label", "login page"},
		{"--kind", "video", "--path", video, "--at", "5", "--id", "video-1", "--label", "OBS"},
	} {
		if e := linkRun(t, nil, append([]string{"add", "--library", g.root, "--parent", parentID}, args...)...); e != nil {
			t.Fatalf("add %v: %v", args, e)
		}
	}
	return &sidecarLibrary{g: g, parent: parentID, a: aID, b: bID, root: g.root, videoPath: video, parentSegmentEnd: 60}
}

func sidecarText(prefix string, at float64) string { return fmt.Sprintf("%s-%03d", prefix, int(at)) }

// sidecarPNG is a valid 1x1 PNG.
var sidecarPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\xcf\xc0\xf0\x1f\x00\x05\x00\x01\xff\x89\x99=\x1d\x00\x00\x00\x00IEND\xaeB`\x82")

// The links API carries each link's anchor pairs and a recording child's
// footage duration; the positioned still and the child frame routes answer
// exactly what the sidecar view needs, and nothing names the video path.
func TestLibrarySidecarRoutes(t *testing.T) {
	s := newSidecarLibrary(t)
	h := s.g.h
	w := libraryRequest(h, "/api/recordings/"+s.parent+"/links", nil)
	var page libraryLinksPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Links) != 4 {
		t.Fatalf("links %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "SIDECAR-SECRET-VIDEO") {
		t.Fatal("links API names the video path")
	}
	byID := map[string]libraryLinkView{}
	for _, l := range page.Links {
		byID[l.ID] = l
	}
	a, b := byID["child-a"], byID["child-b"]
	if len(a.Anchors) != 1 || a.Anchors[0] != (linkPairView{Phase: "declared", ParentSegment: 1, ParentSeconds: 10, ChildSeconds: 0, Precision: links.PrecisionDeclared}) || a.ChildSeconds == nil || *a.ChildSeconds != 20 {
		t.Fatalf("child-a %+v", a)
	}
	if b.ChildSeconds == nil || *b.ChildSeconds != 50 || !containsString(b.Issues, "segment-unanchored:2") {
		t.Fatalf("child-b %+v", b)
	}
	if byID["video-1"].ChildSeconds != nil || byID["shot-1"].ChildSeconds != nil || len(byID["shot-1"].Anchors) != 1 {
		t.Fatalf("sidecars %+v %+v", byID["video-1"], byID["shot-1"])
	}
	// The Go alignment and the API pairs agree (the page applies these pairs).
	res, err := ResolveLinks(withSealName(context.Background(), "{id}"), s.root, s.parent, false)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := res.Find("child-a").Alignment.ParentToChild(links.ParentTime{Segment: 1, AtNs: int64(17 * time.Second)}); err != nil || c != int64(7*time.Second) {
		t.Fatalf("ParentToChild %d %v", c, err)
	}
	// preview-at: the view at or before the position, with its own source.
	for _, tc := range []struct {
		at, want, view string
	}{{"7", "CHILD-A-005", "segment=1;at=5"}, {"15", "CHILD-A-015", "segment=1;at=15"}, {"19.5", "CHILD-A-015", "segment=1;at=15"}} {
		w := libraryRequest(h, "/api/recordings/"+s.a+"/preview-at?segment=1&at="+tc.at, nil)
		if w.Code != 200 || w.Header().Get("AgentReel-Preview-Source") != sidecarPreviewSource || w.Header().Get("AgentReel-Preview-View") != tc.view || !strings.Contains(w.Body.String(), tc.want) || w.Header().Get("Content-Type") != "application/x-asciicast" {
			t.Fatalf("preview-at %s: %d %v %q", tc.at, w.Code, w.Header(), w.Body.String())
		}
	}
	for _, path := range []string{
		"/api/recordings/" + s.a + "/preview-at",
		"/api/recordings/" + s.a + "/preview-at?at=1",
		"/api/recordings/" + s.a + "/preview-at?segment=1",
		"/api/recordings/" + s.a + "/preview-at?segment=1&at=01",
		"/api/recordings/" + s.a + "/preview-at?segment=1&at=-1",
		"/api/recordings/" + s.a + "/preview-at?segment=0&at=1",
		"/api/recordings/" + s.a + "/preview-at?segment=1&at=1&x=1",
		"/api/recordings/" + s.a + "/sidecar-frame?at=1",
	} {
		if w := libraryRequest(h, path, nil); w.Code != 400 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	if w := libraryRequest(h, "/api/recordings/"+s.a+"/preview-at?segment=2&at=1", nil); w.Code != 422 {
		t.Fatalf("missing segment: %d", w.Code)
	}
	// sidecar-frame: a pinned segment-1 player and its cutoff's duration.
	w = libraryRequest(h, "/api/recordings/"+s.b+"/sidecar-frame", nil)
	var frame librarySidecarFrame
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &frame) != nil || frame.Duration != 50 || !strings.HasPrefix(frame.Player, "/player/"+s.b+"?mode=history&segment=1&cutoff=") {
		t.Fatalf("sidecar-frame %d %s", w.Code, w.Body.String())
	}
	if p := libraryRequest(h, frame.Player+"&at=8", nil); p.Code != 200 || !strings.Contains(p.Body.String(), `data-mode="history"`) {
		t.Fatalf("pinned player %d", p.Code)
	}
	// The parent's viewer loads the sidecar module; the embedded assets are served.
	v := libraryRequest(h, "/recordings/"+s.parent+"?mode=history&segment=1&at=0", nil)
	if v.Code == 303 {
		v = libraryRequest(h, v.Header().Get("Location"), nil)
	}
	if v.Code != 200 || !strings.Contains(v.Body.String(), `src="/assets/sidecar-view.mjs"`) {
		t.Fatalf("viewer %d", v.Code)
	}
	if v := libraryRequest(h, "/recordings/"+s.parent+"?mode=history&segment=1&at=0", nil); v.Code == 200 && strings.Contains(v.Body.String(), "SIDECAR-SECRET-VIDEO") {
		t.Fatal("viewer names the video path")
	}
	for _, asset := range []string{"/assets/sidecar-view.mjs", "/assets/snapshot-renderer.mjs", "/assets/sidecar.css"} {
		if w := libraryRequest(h, asset, nil); w.Code != 200 {
			t.Fatalf("%s: %d", asset, w.Code)
		}
	}
}

// A still-recording child (segment 1 open, no end) gets a sidecar frame pinned
// at its committed footage, the links API reports no child duration for it,
// and a re-GET pins newer committed footage only when there is some: the
// cutoff the page's Live panel re-pins to ("Back to history").
func TestLibrarySidecarFrameRecordingChild(t *testing.T) {
	s := newSidecarLibrary(t)
	h := s.g.h
	id := strings.Repeat("6", 32)
	c := addVisualGUI(t, s.root, State{ID: id, Target: linkTarget("sidecar-child-c"), Status: "recording"})
	c.begin(1, 80, 24)
	c.view(0, visualGUIText("CHILD-C-000"))
	c.view(5, visualGUIText("CHILD-C-005"))
	c.commit(10)
	c.acknowledge()
	if e := linkRun(t, nil, "add", "--library", s.root, "--parent", s.parent, "--recording", id, "--at", "20", "--id", "child-c", "--label", "worker C"); e != nil {
		t.Fatal(e)
	}
	frame := func() librarySidecarFrame {
		t.Helper()
		w := libraryRequest(h, "/api/recordings/"+id+"/sidecar-frame", nil)
		var frame librarySidecarFrame
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &frame) != nil {
			t.Fatalf("sidecar-frame %d %s", w.Code, w.Body.String())
		}
		return frame
	}
	prefix := "/player/" + id + "?mode=history&segment=1&cutoff="
	first := frame()
	if first.Duration != 10 || !strings.HasPrefix(first.Player, prefix) || len(first.Player) == len(prefix) {
		t.Fatalf("open child frame %+v", first)
	}
	w := libraryRequest(h, "/api/recordings/"+s.parent+"/links", nil)
	var page libraryLinksPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatalf("links %d %s", w.Code, w.Body.String())
	}
	var found bool
	for _, l := range page.Links {
		if l.ID == "child-c" {
			found = true
			if l.ChildSeconds != nil || l.Target.Recording != id {
				t.Fatalf("child-c %+v", l)
			}
		}
	}
	if !found || !strings.Contains(w.Body.String(), `"childSeconds":null`) {
		t.Fatalf("links %s", w.Body.String())
	}
	// Newer committed footage: a new cutoff and its duration.
	c.view(15, visualGUIText("CHILD-C-015"))
	c.commit(20)
	c.acknowledge()
	second := frame()
	if second.Duration != 20 || !strings.HasPrefix(second.Player, prefix) || second.Player == first.Player {
		t.Fatalf("grown child frame %+v (was %+v)", second, first)
	}
	// Nothing new: the same pinned player.
	if third := frame(); third != second {
		t.Fatalf("unchanged child frame %+v, want %+v", third, second)
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
