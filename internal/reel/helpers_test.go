// Shared test helpers: procfs observation fakes and read-only tree snapshots.

package reel

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var offlineView = processView{Boot: "11111111-1111-1111-1111-111111111111", Namespace: "pid:[123]"}

func offlineIdentity(pid int) ProcessIdentity {
	return ProcessIdentity{PID: pid, Start: fmt.Sprint(pid), Boot: offlineView.Boot, PIDNamespace: offlineView.Namespace}
}
func absentObservation() offlineObservation {
	return offlineObservation{view: func() (processView, error) { return offlineView, nil }, identity: func(int) (ProcessIdentity, error) { return ProcessIdentity{}, errProcessAbsent }}
}
func treeHashes(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	hashes := map[string][32]byte{}
	e := filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		hashes[path] = sha256.Sum256(data)
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return hashes
}

func offlineDestination(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	if e := os.Chmod(parent, 0700); e != nil {
		t.Fatal(e)
	}
	return filepath.Join(parent, "new")
}

// activityTree maps every path under root to its mode, size, content hash
// and link target, to prove a read-only operation changed nothing. SQLite's
// shared-memory index (-shm) and an empty write-ahead log (-wal), which a
// read-only store reader may create, are not recording data and are skipped.
func activityTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	e := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if strings.HasSuffix(path, "-shm") || (strings.HasSuffix(path, "-wal") && st.Size() == 0) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		// A directory's size only reflects its entries, which are compared
		// themselves (and a skipped -shm entry would otherwise show here).
		value := fmt.Sprintf("%v", st.Mode())
		if !st.IsDir() {
			value += fmt.Sprintf(":%d", st.Size())
		}
		if st.Mode().IsRegular() {
			data, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			sum := sha256.Sum256(data)
			value += ":" + hex.EncodeToString(sum[:])
		}
		if st.Mode()&os.ModeSymlink != 0 {
			target, e := os.Readlink(path)
			if e != nil {
				return e
			}
			value += ":" + target
		}
		result[rel] = value
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return result
}
