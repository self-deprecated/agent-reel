package seal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealRehashedManifestAndEmbeddedValidation(t *testing.T) {
	src := fixture(t, true, 12)
	original := destination(t)
	build(t, src, original)
	cases := []struct {
		name, item string
		change     func(*testing.T, string, *Manifest)
	}{
		{"view_count", "seal", func(t *testing.T, p string, m *Manifest) { m.Views.Views++ }},
		{"row_digest", "seal", func(t *testing.T, p string, m *Manifest) { m.Rows.Digest[0] ^= 1 }},
		{"screen_digest", "seal", func(t *testing.T, p string, m *Manifest) { m.Screens.Digest[0] ^= 1 }},
		{"activity_chain", "seal", func(t *testing.T, p string, m *Manifest) { m.Activity.Chain[0] ^= 1 }},
		{"acknowledged", "seal", func(t *testing.T, p string, m *Manifest) { m.Acknowledged.ActivitySeq++ }},
		{"identity", "seal", func(t *testing.T, p string, m *Manifest) { m.Recording = "other" }},
		{"state_identity", "source_files", func(t *testing.T, p string, m *Manifest) {
			replaceEmbedded(t, p, m, "state.json", func(b []byte) []byte { return []byte(strings.Replace(string(b), `"id":"fixture"`, `"id":"other"`, 1)) })
		}},
		{"ack_incomplete", "source_files", func(t *testing.T, p string, m *Manifest) {
			replaceEmbedded(t, p, m, "visual-acknowledged.json", func(b []byte) []byte {
				return []byte(strings.Replace(string(b), `"complete":true`, `"complete":false`, 1))
			})
		}},
		{"activity_noncanonical", "source_files", func(t *testing.T, p string, m *Manifest) {
			replaceEmbedded(t, p, m, activityAckName, func(b []byte) []byte { return append(b, ' ') })
		}},
		{"oversized_launch", "source_files", func(t *testing.T, p string, m *Manifest) {
			replaceEmbedded(t, p, m, "launch.json", func(b []byte) []byte { return append(b, []byte(strings.Repeat(" ", 16<<10))...) })
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := copySeal(t, original)
			db := rawDB(t, path)
			var raw string
			must(t, db.QueryRow("SELECT manifest FROM seal").Scan(&raw))
			must(t, db.Close())
			m, err := parseManifest([]byte(raw))
			must(t, err)
			c.change(t, path, &m)
			data, sum, err := manifestBytes(m)
			must(t, err)
			mutate(t, path, "UPDATE seal SET manifest=?,digest=?", string(data), sum)
			rep, err := Verify(context.Background(), path, options().Limits)
			must(t, err)
			if rep.Status != StatusDamaged || rep.FailedItem != c.item {
				t.Fatalf("%+v", rep)
			}
		})
	}
}
func replaceEmbedded(t *testing.T, path string, m *Manifest, name string, change func([]byte) []byte) {
	t.Helper()
	db := rawDB(t, path)
	var b []byte
	must(t, db.QueryRow("SELECT data FROM source_files WHERE name=?", name).Scan(&b))
	must(t, db.Close())
	b = change(b)
	mutate(t, path, "UPDATE source_files SET data=? WHERE name=?", b, name)
	m.SourceFiles[name] = digest(b)
}
func TestSealSourceReadOnlyAndInvalidInputs(t *testing.T) {
	src := fixture(t, false, 12)
	must(t, os.Chmod(src, 0700))
	dst := filepath.Join(src, "nested.areel")
	_, err := Seal(context.Background(), Source{src}, Destination{dst}, options())
	if !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	// Resolve parent aliases too.
	parent := t.TempDir()
	alias := filepath.Join(parent, "alias")
	must(t, os.Symlink(src, alias))
	_, err = Seal(context.Background(), Source{src}, Destination{filepath.Join(alias, "nested.areel")}, options())
	if !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
	for _, l := range []Options{{}, {Limits: options().Limits}} {
		dst := destination(t)
		_, err := Seal(context.Background(), Source{src}, Destination{dst}, l)
		if err == nil {
			t.Fatal("accepted invalid limits")
		}
		assertEmpty(t, dst)
	}
}
func TestManifestRequiredAndNullMembers(t *testing.T) {
	src := fixture(t, false, 1)
	r := build(t, src, destination(t))
	b, _, err := manifestBytes(r.Manifest)
	must(t, err)
	var object map[string]json.RawMessage
	must(t, json.Unmarshal(b, &object))
	for k := range object {
		t.Run(k, func(t *testing.T) {
			saved := object[k]
			delete(object, k)
			missing, err := json.Marshal(object)
			must(t, err)
			if _, err := parseManifest(missing); err == nil {
				t.Fatal("accepted missing member")
			}
			object[k] = json.RawMessage("null")
			null, err := json.Marshal(object)
			must(t, err)
			if _, err := parseManifest(null); err == nil {
				t.Fatal("accepted null member")
			}
			object[k] = saved
		})
	}
}

func TestSealEmptyCompletedParts(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprint(present), func(t *testing.T) {
			src := fixture(t, false, 0)
			if present {
				s, err := activitystore.Create(filepath.Join(src, activityDirName), archive.Identity{Recording: "fixture", Backend: "fake", Instance: "test"})
				must(t, err)
				must(t, s.Close())
			}
			dst := destination(t)
			result := build(t, src, dst)
			if result.Manifest.Views.Views != 0 || result.Manifest.Activity.Rows != 0 || result.Manifest.Activity.Absent == present {
				t.Fatalf("%+v", result.Manifest)
			}
			rep, err := Verify(context.Background(), dst, options().Limits)
			must(t, err)
			if rep.Status != StatusOK {
				t.Fatalf("%+v", rep)
			}
		})
	}
}

func TestManifestRejectsInvalidUTF8(t *testing.T) {
	src := fixture(t, false, 1)
	result := build(t, src, destination(t))
	result.Manifest.Sealer = "\ufffd"
	raw, _, err := manifestBytes(result.Manifest)
	must(t, err)
	raw = bytes.Replace(raw, []byte("\ufffd"), []byte{0xff}, 1)
	if _, err := parseManifest(raw); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("accepted or misclassified non-UTF-8 JSON: %v", err)
	}
}
