//go:build linux && video

package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/editmap"
)

const (
	visualCourseWidth  = 640
	visualCourseHeight = 480
)

// visualCourseDecode decodes exactly frame index k (not a time) as rgb24.
func visualCourseDecode(t *testing.T, path string, k int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, e := exec.CommandContext(ctx, os.Getenv("AREEL_TEST_FFMPEG"), "-v", "error", "-i", path, "-map", "0:v:0", "-vf", fmt.Sprintf(`select=eq(n\,%d)`, k), "-fps_mode", "passthrough", "-frames:v", "1", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1").Output()
	if e != nil || len(raw) != visualCourseWidth*visualCourseHeight*3 {
		t.Fatalf("decode frame %d: %v (%d bytes)", k, e, len(raw))
	}
	return raw
}

func visualCoursePixel(frame []byte, x, y int) []byte {
	i := (y*visualCourseWidth + x) * 3
	return frame[i : i+3]
}

// visualCourseColumns counts the white runs across the terminal's top row:
// the course screens paint every even column of row 0 white, so the count is
// cols/2 — the frame's terminal geometry.
func visualCourseColumns(frame []byte) int {
	bright := func(p []byte) bool { return p[0] > 170 && p[1] > 170 && p[2] > 170 }
	top := -1
	for y := 0; y < visualCourseHeight && top < 0; y++ {
		p := visualCoursePixel(frame, visualCourseWidth/2, y)
		if max(p[0], p[1], p[2]) > 90 {
			top = y
		}
	}
	if top < 0 {
		return -1
	}
	y := top + 4
	runs, in := 0, false
	for x := 0; x < visualCourseWidth; x++ {
		b := bright(visualCoursePixel(frame, x, y))
		if b && !in {
			runs++
		}
		in = b
	}
	return runs
}

// visualCourseProbeChapters reads the embedded chapters independently.
func visualCourseProbeChapters(t *testing.T, path string, rate editmap.Rate) []editmap.Chapter {
	t.Helper()
	out, e := exec.Command(os.Getenv("AREEL_TEST_FFPROBE"), "-v", "error", "-show_chapters", "-of", "json", path).Output()
	if e != nil {
		t.Fatal(e)
	}
	var probe struct {
		Chapters []struct {
			TimeBase string `json:"time_base"`
			Start    int64  `json:"start"`
			End      int64  `json:"end"`
			Tags     struct {
				Title string `json:"title"`
			} `json:"tags"`
		} `json:"chapters"`
	}
	if e = json.Unmarshal(out, &probe); e != nil {
		t.Fatal(e)
	}
	frame := func(ticks int64, base string) int {
		tb, ok := new(big.Rat).SetString(base)
		if !ok {
			t.Fatalf("time base %q", base)
		}
		x := new(big.Rat).Mul(new(big.Rat).SetInt64(ticks), tb)
		x.Mul(x, big.NewRat(rate.Num, rate.Den))
		x.Add(x, big.NewRat(1, 2))
		return int(new(big.Int).Quo(x.Num(), x.Denom()).Int64())
	}
	var got []editmap.Chapter
	for _, c := range probe.Chapters {
		got = append(got, editmap.Chapter{Title: c.Tags.Title, StartFrame: frame(c.Start, c.TimeBase), EndFrame: frame(c.End, c.TimeBase)})
	}
	return got
}

func TestExportsVideoVisualCourseMarkersAligned(t *testing.T) {
	exportVideoPrerequisites(t)
	for _, codec := range []string{"h264", "prores"} {
		t.Run(codec, func(t *testing.T) {
			c := newVisualCourse(t, true)
			spec := visualCourseSpec(t, c, codec, visualCourseRecipeSameFrame)
			_, p := visualPrepared(t, c.g, spec)
			r, sel := visualStoreReader(t, c.g)
			c.g.closeWriter()
			c.a.close()
			before := visualLibraryHashes(t, c.g.root)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			bundle, e := buildExport(ctx, c.g.root, spec, t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			m, files := visualExportRead(t, bundle)
			for name, data := range files {
				if bytes.Contains(data, []byte(visualCourseSentinel)) {
					t.Fatalf("%s carries the sentinel", name)
				}
			}
			raw, ok := files[visualResolveMarkersName]
			if !ok || m.ResolveMarkers == nil || m.ResolveMarkers.Markers != 7 || m.ResolveMarkers.VerifiedInResolve {
				t.Fatalf("resolve markers %+v", m.ResolveMarkers)
			}
			// The published document is exactly the planned, deterministic one.
			want, e := p.resolve.Encode()
			if e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(raw, want) {
				t.Fatalf("resolve-markers.json differs from the plan:\n%s", raw)
			}
			var doc editmap.Document
			if e = json.Unmarshal(raw, &doc); e != nil {
				t.Fatal(e)
			}
			video := visualWriteVideo(t, m, files)
			out, e := exec.Command(os.Getenv("AREEL_TEST_FFPROBE"), "-v", "error", "-count_frames", "-select_streams", "v:0", "-show_entries", "stream=r_frame_rate,nb_read_frames", "-of", "csv=p=0", video).Output()
			if e != nil || strings.TrimSpace(string(out)) != fmt.Sprintf("30000/1001,%d", doc.Clip.Frames) || doc.Clip.Frames != 900 || doc.Clip.File != m.VideoFile {
				t.Fatalf("probe %q %v, clip %+v", out, e, doc.Clip)
			}
			// Embedded chapters equal metadata.json chapters.
			probed := visualCourseProbeChapters(t, video, p.plan.Rate)
			if len(probed) != len(m.Chapters) || len(m.Chapters) != 3 {
				t.Fatalf("chapters %+v vs %+v", probed, m.Chapters)
			}
			for i, ch := range m.Chapters {
				if probed[i].Title != ch.Title || probed[i].StartFrame != ch.StartFrame || probed[i].EndFrame != ch.EndFrame {
					t.Fatalf("chapter %d probed %+v, metadata %+v", i, probed[i], ch)
				}
			}
			// Each marker frame and the frame before it show the expected
			// segment state: colour from editmap's sample plus ViewAt, geometry
			// from the sampled segment.
			expect := func(k int) (string, int) {
				_, seg, at, ok := p.plan.Sample(k)
				if !ok {
					t.Fatal("sample", k)
				}
				v, e := r.ViewAt(context.Background(), sel, seg, at)
				if e != nil {
					t.Fatal(e)
				}
				return c.colours[seg][v.Point.At], map[int]int{1: 80, 2: 100}[seg]
			}
			frames := map[int]bool{}
			for _, mk := range doc.Markers {
				frames[mk.Frame] = true
				if mk.Frame > 0 {
					frames[mk.Frame-1] = true
				}
			}
			for k := range frames {
				colour, cols := expect(k)
				decoded := visualCourseDecode(t, video, k)
				if got := visualClassify(visualCoursePixel(decoded, visualCourseWidth/2, visualCourseHeight/2)); got != colour {
					t.Fatalf("frame %d is %s, want %s", k, got, colour)
				}
				if got := visualCourseColumns(decoded); got != cols/2 {
					t.Fatalf("frame %d shows %d top-row runs, want %d (%d columns)", k, got, cols/2, cols)
				}
			}
			// The pause/resume group: frame 720 is segment 2's resume repaint
			// (green, 100x30); frame 719 is segment 1's last state (blue, 80x24).
			var group *editmap.Marker
			for i := range doc.Markers {
				if doc.Markers[i].Frame == 720 {
					group = &doc.Markers[i]
				}
			}
			if group == nil || len(group.Members) != 2 || group.Members[0].Kind != "pause" || group.Members[1].Kind != "resume" {
				t.Fatalf("group %+v", group)
			}
			if c1, _ := expect(720); c1 != "green" {
				t.Fatal("fixture expectation moved: 720")
			}
			if c0, _ := expect(719); c0 != "blue" {
				t.Fatal("fixture expectation moved: 719")
			}
			if after := visualLibraryHashes(t, c.g.root); fmt.Sprint(after) != fmt.Sprint(before) {
				t.Fatal("source library changed")
			}
		})
	}
}
