//go:build linux || darwin

package reel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExportsBoundedRetentionExpiryAndFailedArtifacts(t *testing.T) {
	base, s := libraryFixture(t)
	h, e := newLibraryExports(base.root, libraryTestHost)
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	h.build = func(_ context.Context, _ string, _ exportSpec, dir string) (string, error) {
		p := filepath.Join(dir, "partial")
		if e := os.WriteFile(p, []byte("unpublished"), 0600); e != nil {
			return "", e
		}
		return "", fmt.Errorf("failed verification")
	}
	var first string
	for i := 0; i < exportJobsMax+2; i++ {
		id, e := h.start(exportTestSpec(s.ID))
		if e != nil {
			t.Fatal(e)
		}
		if first == "" {
			first = id
		}
		exportWait(t, "failed job settlement", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.jobs[id].state == "failed" })
		h.mu.Lock()
		j := h.jobs[id]
		if _, e = os.Stat(j.dir); !os.IsNotExist(e) {
			t.Fatal("failed staging retained")
		}
		if len(h.jobs) > exportJobsMax {
			t.Fatal("retention exceeded")
		}
		h.mu.Unlock()
	}
	h.mu.Lock()
	if _, ok := h.jobs[first]; ok {
		t.Fatal("oldest job not evicted")
	}
	for _, j := range h.jobs {
		j.finished = time.Now().Add(-exportAge - time.Second)
	}
	h.expireLocked(time.Now())
	if len(h.jobs) != 0 {
		t.Fatal("expired jobs retained")
	}
	h.mu.Unlock()
}
func TestExportsRootReplacementAndPublicPermissionsRefused(t *testing.T) {
	base, s := libraryFixture(t)
	h, e := newLibraryExports(base.root, libraryTestHost)
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	if e = os.Chmod(base.root, 0755); e != nil {
		t.Fatal(e)
	}
	defer os.Chmod(base.root, 0700)
	if w := exportsRequest(h, "GET", "/exports/new?recording="+s.ID, "", nil); w.Code != 422 {
		t.Fatal(w.Code)
	}
}
func TestExportsTMPDIRCannotWriteIntoSource(t *testing.T) {
	base, state := libraryFixture(t)
	t.Setenv("TMPDIR", base.root)
	before := activityTree(t, base.root)
	h, e := newLibraryExports(base.root, libraryTestHost)
	if e != nil {
		t.Fatal("ordinary startup depends on export storage", e)
	}
	defer h.Close()
	if _, e = h.start(exportTestSpec(state.ID)); e == nil {
		t.Fatal("temporary directory accepted inside source")
	}
	after := activityTree(t, base.root)
	if len(before) != len(after) {
		t.Fatal("source modified before destination admission")
	}
}
