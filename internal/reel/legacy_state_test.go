//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// legacyMember rewrites a private JSON file with member inserted right after
// its opening brace, as older AgentReel writers stamped `"version": 1`.
func legacyMember(t *testing.T, path, member string) []byte {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	b = bytes.Replace(b, []byte("{"), []byte("{"+member), 1)
	if e = os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	return b
}

// An existing recording whose state.json carries the obsolete `version: 1`
// member lists and details; the file is never rewritten.
func TestLegacyVersionStateLists(t *testing.T) {
	h, s := libraryFixture(t)
	dir := filepath.Join(h.root, "recordings", s.ID)
	statePath := filepath.Join(dir, "state.json")
	stateBytes := legacyMember(t, statePath, "\"version\": 1,")
	before := treeHashes(t, h.root)
	page, e := libraryList(context.Background(), h.root, "", 8)
	if e != nil || len(page.Items) != 1 || page.Items[0].Metadata != "observed" || page.Items[0].PersistedStatus != "recording" {
		t.Fatalf("legacy state listing %+v %v", page, e)
	}
	detail := libraryRequest(h, "/api/recordings/"+s.ID, nil)
	var v libraryRecording
	if detail.Code != 200 || json.Unmarshal(detail.Body.Bytes(), &v) != nil || v.PersistedStatus != "recording" || v.Segments[0].Media != "visual-store" {
		t.Fatalf("legacy state detail %d %s", detail.Code, detail.Body)
	}
	var state State
	data, e := offlineJSON(statePath, 4<<20, &state)
	if e != nil || state.ID != s.ID || !bytes.Equal(data, stateBytes) {
		t.Fatalf("legacy state offline decode %v %q", e, state.ID)
	}
	if !reflect.DeepEqual(before, treeHashes(t, h.root)) {
		t.Fatal("legacy metadata rewritten")
	}
	// Current writers stay unversioned.
	if e = writeJSON(statePath, state); e != nil {
		t.Fatal(e)
	}
	if fresh, e := os.ReadFile(statePath); e != nil || bytes.Contains(fresh, []byte("version")) {
		t.Fatalf("writer emitted a version field: %v %s", e, fresh)
	}
}

func TestLegacyVersionRejectsOtherUnknownMembersAndShapes(t *testing.T) {
	cases := map[string]struct{ file, member string }{
		"state-version-2":      {"state.json", "\"version\": 2,"},
		"state-version-string": {"state.json", "\"version\": \"1\","},
		"state-version-float":  {"state.json", "\"version\": 1.0,"},
		"state-version-twice":  {"state.json", "\"version\": 1, \"version\": 1,"},
		"state-unknown-key":    {"state.json", "\"legacy\": 1,"},
		"state-nested":         {"state.json", ""},
		"launch-version":       {"launch.json", "\"version\": 1,"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h, s := libraryFixture(t)
			dir := filepath.Join(h.root, "recordings", s.ID)
			path := filepath.Join(dir, c.file)
			if name == "state-nested" {
				b, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				b = bytes.Replace(b, []byte(`"target": {`), []byte(`"target": {"version": 1,`), 1)
				if e = os.WriteFile(path, b, 0600); e != nil {
					t.Fatal(e)
				}
			} else {
				legacyMember(t, path, c.member)
			}
			if detail := libraryRequest(h, "/api/recordings/"+s.ID, nil); detail.Code == 200 {
				t.Fatalf("noncanonical %s accepted: %s", c.file, detail.Body)
			}
			if c.file == "state.json" {
				var state State
				if _, e := offlineJSON(path, 4<<20, &state); e == nil {
					t.Fatal("offline decode accepted noncanonical state")
				}
			}
		})
	}
	// Activity records keep no schema-version field: a reintroduced one is unknown.
	_, s := libraryFixture(t)
	record, e := json.Marshal(activityRecord{Recording: s.ID, Backend: s.Target.Backend, Instance: s.Target.Instance, Receipt: activityReceipt{ID: "activity-1", Sequence: 1, At: time.Now().UTC()}, Transport: "private-unix", Attribution: "declared-or-unknown", Visibility: "unknown", Envelope: activityFixture("activity-1")})
	if e != nil {
		t.Fatal(e)
	}
	record = bytes.Replace(record, []byte("{"), []byte(`{"version":1,`), 1)
	if e := activityListJSON(record, &activityRecord{}); e == nil {
		t.Fatal("activity record version accepted")
	}
}
