//go:build linux || darwin

package seal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExclusiveRenamePreservesCollision(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	dest := filepath.Join(root, "dest")
	if e := os.Mkdir(source, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(dest, 0700); e != nil {
		t.Fatal(e)
	}
	if e := renameExclusive(source, dest); e == nil {
		t.Fatal("replaced existing directory")
	}
	if _, e := os.Stat(source); e != nil {
		t.Fatal("lost source", e)
	}
	if e := os.Remove(dest); e != nil {
		t.Fatal(e)
	}
	if e := renameExclusive(source, dest); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(source); !os.IsNotExist(e) {
		t.Fatal("source still present", e)
	}
}
