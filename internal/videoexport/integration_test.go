//go:build linux && video

package videoexport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func integrationRenderer(t *testing.T) *Renderer {
	t.Helper()
	config := Config{Chromium: os.Getenv("AREEL_TEST_CHROMIUM"), FFmpeg: os.Getenv("AREEL_TEST_FFMPEG"), FFprobe: os.Getenv("AREEL_TEST_FFPROBE"), NoSandbox: os.Getenv("AREEL_TEST_CHROMIUM_NO_SANDBOX") == "1"}
	for name, dst := range map[string]*[]byte{"player.js": &config.Assets.PlayerJS, "player.css": &config.Assets.PlayerCSS, "DejaVuSansMono.ttf": &config.Assets.Font, "AgentReelBox.ttf": &config.Assets.BoxFont} {
		var err error
		*dst, err = os.ReadFile(filepath.Join("..", "reel", "web", "vendor", name))
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func colorCast(seconds int) string {
	var b strings.Builder
	b.WriteString("{\"version\":3,\"term\":{\"cols\":80,\"rows\":24}}\n")
	for n := 0; n <= seconds; n++ {
		color := 41
		if n%3 == 1 {
			color = 42
		}
		if n%3 == 2 {
			color = 44
		}
		delta := 1
		if n == 0 {
			delta = 0
		}
		event, _ := json.Marshal([]any{delta, "o", fmt.Sprintf("\x1b[%dm\x1b[2J\x1b[HFRAME %03d", color, n)})
		b.Write(event)
		b.WriteByte('\n')
	}
	return b.String()
}
func TestVideoRealCodecsPaintedFramesAndGapTiming(t *testing.T) {
	r := integrationRenderer(t)
	for _, codec := range []string{"h264", "prores"} {
		t.Run(codec, func(t *testing.T) {
			q := request(t)
			q.Codec = codec
			q.Cast = strings.NewReader(colorCast(3))
			q.Spans = []Span{{Start: 0, End: 1}, {Start: 2, End: 3}}
			result, err := r.Render(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			if result.Frames != 4 || result.Timing[1].Start != 2 || result.VideoSeconds != 2 {
				t.Fatalf("timing %+v", result)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, r.config.FFmpeg, "-v", "error", "-i", q.Output, "-vf", "scale=1:1", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1")
			raw, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) != 12 {
				t.Fatalf("decoded %d bytes, wanted four RGB frames", len(raw))
			}
			for i := 0; i < 4; i++ {
				p := raw[i*3 : i*3+3]
				if i < 2 && int(p[0]) < int(p[2])+40 {
					t.Fatalf("frame %d not red after seek: %v", i, p)
				}
				if i >= 2 && int(p[2]) < int(p[0])+40 {
					t.Fatalf("frame %d not blue after seek: %v", i, p)
				}
			}
			// Overlay/UI controls must not be baked into a paused/seeked frame.
			pixels, err := exec.CommandContext(ctx, r.config.FFmpeg, "-v", "error", "-i", q.Output, "-frames:v", "1", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1").Output()
			if err != nil || len(pixels) != q.Width*q.Height*3 {
				t.Fatal("cannot inspect actual painted output", err)
			}
			for y := 180; y < 300; y++ {
				for x := 240; x < 400; x++ {
					p := pixels[(y*q.Width+x)*3:][:3]
					if p[0] > 180 && p[1] > 180 && p[2] > 180 {
						t.Fatal("player play/paused overlay leaked into video")
					}
				}
			}
			entries, _ := os.ReadDir(filepath.Dir(q.Output))
			if len(entries) != 1 {
				t.Fatal("private job retained")
			}
			st, _ := os.Stat(q.Output)
			if st.Mode().Perm() != 0600 {
				t.Fatal("artifact not private")
			}
			logEvidence(t, codec, result)
			saveVideoEvidence(t, codec, q.Output, r.config.FFmpeg)
		})
	}
}
func TestVideoLongerSyntheticThroughputAndCancellation(t *testing.T) {
	r := integrationRenderer(t)
	q := request(t)
	q.FPS = 5
	q.Spans = []Span{{Start: 0, End: 30}}
	q.Cast = strings.NewReader(colorCast(30))
	result, err := r.Render(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if result.Frames != 150 {
		t.Fatal(result.Frames)
	}
	logEvidence(t, "longer", result)
	if result.FramesPerSecond < 10 {
		t.Fatal("qualification target: at least 10 painted 640x480 frames/second")
	}
	q = request(t)
	q.FPS = 30
	q.Spans = []Span{{Start: 0, End: 300}}
	q.Cast = strings.NewReader(colorCast(300))
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, err := r.Render(ctx, q); err == nil {
		t.Fatal("canceled real render succeeded")
	}
	entries, _ := os.ReadDir(filepath.Dir(q.Output))
	if len(entries) != 0 {
		t.Fatal("canceled render retained output/profile")
	}
}
func TestVideoDenseTwoHourSourceTail(t *testing.T) {
	r := integrationRenderer(t)
	var source strings.Builder
	source.WriteString("{\"version\":3,\"term\":{\"cols\":80,\"rows\":24}}\n")
	for i := 0; i < 72000; i++ {
		event, _ := json.Marshal([]any{0.1, "o", fmt.Sprintf("\x1b[H%06d %s", i, strings.Repeat("synthetic terminal workload ", 8))})
		source.Write(event)
		source.WriteByte('\n')
	}
	q := request(t)
	q.Cast = strings.NewReader(source.String())
	q.FPS = 5
	q.Spans = []Span{{Start: 7190, End: 7192}}
	result, err := r.Render(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if result.Frames != 10 || result.SourceBytes < 10<<20 {
		t.Fatalf("dense fixture not exercised: %+v", result)
	}
	logEvidence(t, "dense-two-hour-tail", result)
}

func TestVideoEightHourSourceAboveLegacyLimit(t *testing.T) {
	r := integrationRenderer(t)
	source, err := os.CreateTemp(t.TempDir(), "long-source-*.cast")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	w := bufio.NewWriter(source)
	fmt.Fprintln(w, `{"version":3,"term":{"cols":80,"rows":24}}`)
	for i := 0; i < 144000; i++ {
		event, _ := json.Marshal([]any{0.2, "o", fmt.Sprintf("\x1b[H%06d %s", i, strings.Repeat("synthetic terminal workload ", 25))})
		w.Write(event)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	q := request(t)
	q.Cast = source
	q.FPS = 5
	q.Spans = []Span{{Start: 28790, End: 28792}}
	result, err := r.Render(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceBytes <= 80<<20 {
		t.Fatal("fixture did not exceed legacy browser cast cap")
	}
	logEvidence(t, "eight-hour-above-legacy-cap", result)
}

func TestVideoRealInvalidCastAndEncoderFailureCleanUp(t *testing.T) {
	r := integrationRenderer(t)
	q := request(t)
	q.Cast = bytes.NewBufferString("not a cast")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.Render(ctx, q); err == nil {
		t.Fatal("invalid cast published")
	}
	entries, _ := os.ReadDir(filepath.Dir(q.Output))
	if len(entries) != 0 {
		t.Fatal("failed render retained files")
	}
	script := filepath.Join(t.TempDir(), "encoder-fails")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 42\n"), 0700); err != nil {
		t.Fatal(err)
	}
	r.config.FFmpeg = script
	q.Cast = strings.NewReader(colorCast(12))
	q.Spans = []Span{{End: 10}}
	if _, err := r.Render(context.Background(), q); err == nil {
		t.Fatal("failed encoder published")
	}
	entries, _ = os.ReadDir(filepath.Dir(q.Output))
	if len(entries) != 0 {
		t.Fatal("encoder failure retained files")
	}
}
func saveVideoEvidence(t *testing.T, name, path, ffmpeg string) {
	t.Helper()
	dir := os.Getenv("AREEL_VIDEO_EVIDENCE")
	if dir == "" {
		return
	}
	ext := ".mp4"
	if name == "prores" {
		ext = ".mov"
	}
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	out, err := os.OpenFile(filepath.Join(dir, name+ext), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(out, source)
	closeErr := out.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	png, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", path, "-frames:v", "1", "-f", "image2pipe", "-c:v", "png", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".png"), png, 0600); err != nil {
		t.Fatal(err)
	}
}

func logEvidence(t *testing.T, name string, result Result) {
	t.Helper()
	b, _ := json.MarshalIndent(result, "", "  ")
	t.Logf("%s: %s", name, b)
	if dir := os.Getenv("AREEL_VIDEO_EVIDENCE"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, name+".json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
