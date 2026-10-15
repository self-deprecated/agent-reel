//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSealNameSpawn(t *testing.T) {
	root := shortHolderRoot(t)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.json")
	writeUserConfig(t, config, map[string]any{"seal.name": "{date}-{target}-{id}"})
	t.Setenv("AREEL_CONFIG", config)
	t.Setenv("AREEL_SEAL_NAME", "")
	allow := writeAllowlist(t, t.TempDir(), "allow.json", `{"entries":[{"backend":"fake","target":"work"}]}`)
	fake := newSupervisedFake("fake")
	fake.set("work", true)
	var store *Store
	deps := supervisorDeps{backends: []SupervisedBackend{fake}, self: neverSelf(t, root), bind: func(s *Store) {
		store = s
		launchInProcess(t, s)
	}}
	var out, errOut bytes.Buffer
	if err := supervisorCommand(context.Background(), []string{"supervisor", "--library", root, "--allowlist", allow, "--once"}, &out, &errOut, deps); err != nil {
		t.Fatal(err)
	}
	ids, err := os.ReadDir(filepath.Join(root, "recordings"))
	if err != nil || len(ids) != 1 {
		t.Fatalf("spawned recordings: %v %v; %s", ids, err, out.String())
	}
	var spec launchSpec
	if err := readJSON(filepath.Join(root, "recordings", ids[0].Name(), "launch.json"), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.SealName != "{date}-{target}-{id}" || store.SealName != spec.SealName {
		t.Fatalf("template not pinned by CLI before holder spawn: %+v", spec)
	}
	// The holder receives the pinned launch metadata even after this setting
	// changes. libraryState also has to accept the new field in strict decode.
	t.Setenv("AREEL_SEAL_NAME", "{id}")
	if _, _, err := libraryState(root, ids[0].Name(), newLibraryGuard(context.Background())); err != nil {
		t.Fatalf("strict launch decode: %v", err)
	}
}
