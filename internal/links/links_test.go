package links

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	parentID = "0123456789abcdef0123456789abcdef"
	childID  = "fedcba9876543210fedcba9876543210"
	otherID  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

var created = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

func library(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "lib")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func recordingLink(id, child string, seq int64) Link {
	return Link{ID: id, Kind: KindRecording, Ref: Ref{Recording: child}, StartAnchor: SeqAnchor(seq), Label: "summon test", Origin: "summon", CreatedAt: created}
}

func TestManifestRoundTripAndStrictness(t *testing.T) {
	root := library(t)
	m, err := Update(root, parentID, func(m *Manifest) error {
		m.Links = append(m.Links, recordingLink("rec-fedcba98", childID, 3))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, exists, err := Read(root, parentID)
	if err != nil || !exists || len(got.Links) != 1 || got.Links[0].Ref.Recording != childID || *got.Links[0].StartAnchor.Seq != 3 {
		t.Fatalf("read = %+v %v %v (wrote %+v)", got, exists, err, m)
	}
	st, err := os.Lstat(ManifestPath(root, parentID))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("manifest mode %v %v", st.Mode(), err)
	}
	data, _ := os.ReadFile(ManifestPath(root, parentID))
	for name, mutate := range map[string]func(string) string{
		"unknown field": func(s string) string { return strings.Replace(s, `"offsetNs"`, `"extra": 1, "offsetNs"`, 1) },
		"duplicate key": func(s string) string { return strings.Replace(s, `"offsetNs": 0`, `"offsetNs": 0, "offsetNs": 0`, 1) },
		"case":          func(s string) string { return strings.Replace(s, `"offsetNs"`, `"OffsetNs"`, 1) },
		"schema":        func(s string) string { return strings.Replace(s, Schema, "agent-reel/links/2", 1) },
		"self link":     func(s string) string { return strings.Replace(s, childID, parentID, 1) },
		"two refs": func(s string) string {
			return strings.Replace(s, `"recording": "`+childID+`"`, `"recording": "`+childID+`", "path": "/x"`, 1)
		},
		"trailing value": func(s string) string { return s + "{}" },
	} {
		if _, err := Decode([]byte(mutate(string(data)))); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	// A failing update writes nothing.
	if _, err := Update(root, parentID, func(m *Manifest) error { m.Links = nil; return errors.New("no") }); err == nil {
		t.Fatal("update error lost")
	}
	if got, _, _ := Read(root, parentID); len(got.Links) != 1 {
		t.Fatal("failed update wrote")
	}
	// An invalid result is refused.
	if _, err := Update(root, parentID, func(m *Manifest) error {
		m.Links = append(m.Links, recordingLink("rec-fedcba98", childID, 4))
		return nil
	}); err == nil {
		t.Fatal("duplicate link ID accepted")
	}
}

func TestReadRefusesSymlinkAndLooseModes(t *testing.T) {
	root := library(t)
	if _, err := Update(root, parentID, func(m *Manifest) error { return nil }); err != nil {
		t.Fatal(err)
	}
	path := ManifestPath(root, parentID)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read(root, parentID); err == nil {
		t.Fatal("0644 manifest read")
	}
	os.Chmod(path, 0600)
	target := filepath.Join(root, "elsewhere.json")
	os.Rename(path, target)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read(root, parentID); err == nil {
		t.Fatal("symlinked manifest read")
	}
}

func TestScanGrouping(t *testing.T) {
	root := library(t)
	grand := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	must := func(parent string, l Link) {
		t.Helper()
		if _, err := Update(root, parent, func(m *Manifest) error { m.Links = append(m.Links, l); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	must(parentID, recordingLink("a", childID, 1))
	must(childID, recordingLink("b", grand, 1))
	// A second parent of the same child: the smallest parent ID wins.
	must(otherID, recordingLink("c", childID, 1))
	os.WriteFile(filepath.Join(root, Dir, "cccccccccccccccccccccccccccccccc.json"), []byte("{"), 0600)
	g, err := Scan(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.ParentOf[childID] != parentID || g.LinkOf[childID] != "a" || g.ParentOf[grand] != childID || g.Invalid != 1 {
		t.Fatalf("grouping %+v", g)
	}
	if g.Role(parentID) != "parent" || g.Role(childID) != "child" || g.Role(otherID) != "standalone" {
		t.Fatalf("roles %s %s %s", g.Role(parentID), g.Role(childID), g.Role(otherID))
	}
	if a := g.Ancestors(grand); len(a) != 2 || a[0] != childID || a[1] != parentID {
		t.Fatalf("ancestors %v", a)
	}
	// A parent missing from the library falls back to the next parent, and
	// with none left the child is standalone.
	g, _ = Scan(root, func(id string) bool { return id != parentID })
	if g.Role(childID) != "child" || g.ParentOf[childID] != otherID {
		t.Fatalf("filtered %+v", g)
	}
	g, _ = Scan(root, func(id string) bool { return id != parentID && id != otherID })
	if g.ParentOf[childID] != "" || g.Role(childID) != "parent" {
		t.Fatalf("filtered %+v", g)
	}
	// Loops are dropped.
	must(grand, recordingLink("d", parentID, 1))
	g, _ = Scan(root, nil)
	if g.ParentOf[parentID] != "" || g.ParentOf[childID] != "" || g.ParentOf[grand] != "" {
		t.Fatalf("loop kept %+v", g.ParentOf)
	}
}

func TestAssetImportAndVerifiedOpen(t *testing.T) {
	root := library(t)
	src := filepath.Join(t.TempDir(), "Shot.PNG")
	os.WriteFile(src, []byte("\x89PNG fake"), 0644)
	ref, digest, err := ImportAsset(root, parentID, "shot-1", src)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "assets/"+parentID+"/shot-1.png" || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("ref %q digest %q", ref, digest)
	}
	if _, _, err := ImportAsset(root, parentID, "shot-1", src); err == nil {
		t.Fatal("overwrote an asset")
	}
	l := Link{ID: "shot-1", Kind: KindScreenshot, Ref: Ref{Asset: ref}, Digest: &digest, StartAnchor: Declared(1, "2.5"), Label: "shot", Origin: "agent", CreatedAt: created}
	data, ct, err := OpenAsset(root, parentID, l)
	if err != nil || string(data) != "\x89PNG fake" || ct != "image/png" {
		t.Fatalf("open %q %q %v", data, ct, err)
	}
	file := filepath.Join(root, ref)
	os.WriteFile(file, []byte("changed"), 0600)
	if _, _, err := OpenAsset(root, parentID, l); !errors.Is(err, ErrAssetDigest) {
		t.Fatalf("changed asset: %v", err)
	}
	if CheckAsset(root, parentID, l, true) != "asset-digest-mismatch" {
		t.Fatal("check digest")
	}
	os.Remove(file)
	os.Symlink(src, file)
	if _, _, err := OpenAsset(root, parentID, l); err == nil {
		t.Fatal("served a symlink")
	}
	video := Link{ID: "video-1", Kind: KindVideo, Ref: Ref{Path: "/tmp/obs.mkv"}, StartAnchor: SeqAnchor(1), Label: "v", Origin: "user", CreatedAt: created}
	if _, _, err := OpenAsset(root, parentID, video); err == nil {
		t.Fatal("a path link opened as an asset")
	}
	if _, _, err := ImportAsset(root, parentID, "shot-2", filepath.Join(t.TempDir(), "x.svg")); err == nil {
		t.Fatal("svg accepted")
	}
}

func sec(s float64) int64 { return int64(s * 1e9) }

func TestAlignResumeReanchorAndGapRefusal(t *testing.T) {
	t0 := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	l := recordingLink("rec-1", childID, 5)
	stopSeq := int64(9)
	l.StopAnchor = SeqAnchor(stopSeq)
	events := []Event{
		{Seq: 5, Link: "rec-1", Phase: PhaseStart, Placement: Placement{Segment: 1, AtNs: sec(10), Precision: PrecisionEstimated}, ReceivedAt: t0},
		// Parent paused 20s after the start, for 30s, then resumed as segment 2.
		{Seq: 7, Link: "rec-1", Phase: PhaseResume, Placement: Placement{Segment: 2, Precision: PrecisionUnknown}, ReceivedAt: t0.Add(50 * time.Second), Segment: 2, ResumedAt: t0.Add(50 * time.Second)},
		{Seq: 9, Link: "rec-1", Phase: PhaseStop, Placement: Placement{Segment: 2, AtNs: sec(15), Precision: PrecisionEstimated}, ReceivedAt: t0.Add(65 * time.Second)},
		{Seq: 6, Link: "other", Phase: PhaseStart, Placement: Placement{Segment: 1, AtNs: sec(1), Precision: PrecisionEstimated}, ReceivedAt: t0},
	}
	a, start, stop, issues := Align(l, events, 2)
	if len(issues) != 0 || start.Seq != 5 || stop.Segment != 2 || len(a.Pairs) != 2 {
		t.Fatalf("align %+v %+v %+v %v", a, start, stop, issues)
	}
	a.SegmentEndNs = map[int]int64{1: sec(30)}
	if c, err := a.ParentToChild(ParentTime{1, sec(25)}); err != nil || c != sec(15) {
		t.Fatalf("segment 1: %d %v", c, err)
	}
	// Segment 2 at 5s is 50+5 = 55s after the start: child 55s.
	if c, err := a.ParentToChild(ParentTime{2, sec(5)}); err != nil || c != sec(55) {
		t.Fatalf("segment 2: %d %v", c, err)
	}
	if p, err := a.ChildToParent(sec(55)); err != nil || p != (ParentTime{2, sec(5)}) {
		t.Fatalf("child→parent %v %v", p, err)
	}
	var span *SpanError
	if _, err := a.ChildToParent(sec(35)); !errors.As(err, &span) || span.Reason != "in-parent-gap" {
		t.Fatalf("child in the parent pause: %v", err)
	}
	if err := a.CheckSpan(ParentTime{1, sec(12)}, ParentTime{2, sec(3)}); !errors.As(err, &span) || span.Reason != "crosses-gap" || !strings.Contains(err.Error(), "split it there") {
		t.Fatalf("gap span: %v", err)
	}
	if err := a.CheckSpan(ParentTime{2, sec(1)}, ParentTime{2, sec(14)}); err != nil {
		t.Fatalf("in-segment span: %v", err)
	}
	if _, err := a.ParentToChild(ParentTime{2, sec(20)}); !errors.As(err, &span) || span.Reason != "after-stop" {
		t.Fatalf("after stop: %v", err)
	}
	if _, err := a.ParentToChild(ParentTime{1, sec(5)}); !errors.As(err, &span) || span.Reason != "before-start" {
		t.Fatalf("before start: %v", err)
	}
	// Without the resume record segment 2 is un-anchored, and the span refusal says so.
	a2, _, _, issues := Align(l, []Event{events[0], events[2]}, 2)
	if len(issues) != 1 || issues[0] != "segment-unanchored:2" {
		t.Fatalf("issues %v", issues)
	}
	if err := a2.CheckSpan(ParentTime{1, sec(12)}, ParentTime{2, sec(3)}); !errors.As(err, &span) || span.Reason != "crosses-unanchored-gap" {
		t.Fatalf("un-anchored gap: %v", err)
	}
	if _, err := a2.ParentToChild(ParentTime{2, sec(3)}); !errors.As(err, &span) || span.Reason != "unanchored" {
		t.Fatalf("un-anchored point: %v", err)
	}
}

func TestAlignDeclaredAndOffset(t *testing.T) {
	d := "sha256:" + strings.Repeat("0", 64)
	l := Link{ID: "shot-1", Kind: KindScreenshot, Ref: Ref{Asset: AssetRef(parentID, "shot-1.png")}, Digest: &d, StartAnchor: Declared(3, "42.5"), OffsetNs: sec(-1.5), Label: "s", Origin: "user", CreatedAt: created}
	if err := l.Validate(parentID); err != nil {
		t.Fatal(err)
	}
	a, start, _, issues := Align(l, nil, 5)
	if len(issues) != 0 || start.Precision != PrecisionDeclared || a.Pairs[0].Parent != (ParentTime{3, sec(41)}) || a.Precision(3) != PrecisionDeclared {
		t.Fatalf("declared %+v %+v %v", a, start, issues)
	}
}
