//go:build linux && video

package reel

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/seal"
)

// The video matrix compares the derived render input and chapters, never MP4
// container bytes. It is deliberately tagged: the untagged matrix needs no
// Chromium, FFmpeg, helper or corpus. Run 4 holds execution of this variant.
func TestSealedDifferentialVideo(t *testing.T) {
	for _, name := range []string{"AREEL_TEST_CHROMIUM", "AREEL_TEST_FFMPEG", "AREEL_TEST_FFPROBE"} {
		if os.Getenv(name) == "" {
			t.Skipf("%s unset", name)
		}
	}
	exportVideoPrerequisites(t)
	for _, tpl := range []string{"{id}", "{date}-{target}-{id}"} {
		t.Run(tpl, func(t *testing.T) {
			g := newSealedDifferentialFixture(t, tpl)
			ctx, cancel := context.WithTimeout(withSealName(context.Background(), tpl), 10*time.Minute)
			defer cancel()
			spec := exportTestSpec(g.id)
			spec.Format = "h264"
			spec.Recipe = visualTestRecipe(t, sealedDifferentialRecipe)
			capture := func() map[string][]byte {
				t.Helper()
				bundle, err := buildExport(ctx, g.root, spec, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				manifest, entries := visualExportRead(t, bundle)
				if len(entries[visualRenderInputName]) == 0 || len(manifest.Chapters) == 0 {
					t.Fatal("video comparison would be vacuous")
				}
				return map[string][]byte{
					visualRenderInputName:    entries[visualRenderInputName],
					"chapters":               sealedDifferentialJSON(t, manifest.Chapters),
					visualResolveMarkersName: entries[visualResolveMarkersName],
				}
			}
			a := capture()
			name, err := renderSealName(tpl, g.state)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(g.root, name+".areel")
			if _, err := seal.Seal(ctx, seal.Source{RecordingDir: g.dir}, seal.Destination{Path: path}, sealedDifferentialOptions()); err != nil {
				t.Fatal(err)
			}
			if _, err := seal.RemoveSource(ctx, sealedDifferentialRemoval(g.dir, path)); err != nil {
				t.Fatal(err)
			}
			before := sealedDifferentialFile(t, path)
			c := capture()
			after := sealedDifferentialFile(t, path)
			if before.sum != after.sum || !os.SameFile(before.info, after.info) || !before.info.ModTime().Equal(after.info.ModTime()) {
				t.Fatal("video export changed sealed file")
			}
			for key, want := range a {
				if !bytes.Equal(want, c[key]) {
					t.Errorf("%s differs across A/C", key)
				}
			}
		})
	}
}
