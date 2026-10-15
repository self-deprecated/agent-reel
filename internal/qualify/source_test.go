package qualify

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestTreeManifestV0Method(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod": "module x\n", "go.sum": "", "cmd/a/main.go": "package main\n", "internal/b/b.go": "package b\n",
		"internal/b/budgets.json": "{}\n",
		// Excluded, as the Nix source filter excludes them:
		"internal/b/README.md": "doc", "internal/b/b.go~": "backup", "internal/b/.b.go.swp": "swap", "tools/x.go": "outside",
	}
	for p, c := range files {
		must(t, os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755))
		must(t, os.WriteFile(filepath.Join(root, p), []byte(c), 0o644))
	}
	sum, n, err := TreeManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	h := func(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }
	manifest := ""
	for _, p := range []string{"cmd/a/main.go", "go.mod", "go.sum", "internal/b/b.go", "internal/b/budgets.json"} {
		manifest += fmt.Sprintf("%s  %s\n", h(files[p]), p)
	}
	if n != 5 || sum != h(manifest) {
		t.Fatalf("manifest %s over %d files, want %s over 5", sum, n, h(manifest))
	}
	src, err := SourceIdentity(root, "kxabc fd6def", "/nix/store/x-source")
	if err != nil || src.ChangeID != "kxabc" || src.CommitID != "fd6def" || src.TreeSHA256 != sum || src.NixSource == "" {
		t.Fatalf("source %+v %v", src, err)
	}
	if _, err := SourceIdentity(root, "only-one-field", ""); err == nil {
		t.Fatal("a malformed jj identity was accepted")
	}
	// The real module tree hashes without error.
	if sum, n, err := TreeManifest(filepath.Join("..", "..")); err != nil || n < 100 {
		t.Fatalf("module manifest %s %d %v", sum, n, err)
	}
}
