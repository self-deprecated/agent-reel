package videoexport

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

// The legacy Render argv is pinned literally; RenderFrames shares the builder.
func TestLegacyEncoderArgvUnchanged(t *testing.T) {
	common := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-threads", "2", "-filter_threads", "1", "-f", "image2pipe", "-framerate", "30", "-c:v", "png", "-i", "pipe:0", "-an", "-frames:v", "123"}
	h264 := append(append([]string{}, common...), "-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-movflags", "+faststart", "-f", "mp4", "-threads", "2", "-fs", "1073741824", "/job/video")
	prores := append(append([]string{}, common...), "-c:v", "prores_ks", "-profile:v", "3", "-pix_fmt", "yuv422p10le", "-f", "mov", "-threads", "2", "-fs", "1073741824", "/job/video")
	if got := encoderArgs("30", 123, "h264", "", "/job/video"); !reflect.DeepEqual(got, h264) {
		t.Fatalf("h264 argv changed:\n%q\n%q", got, h264)
	}
	if got := encoderArgs("30", 123, "prores", "", "/job/video"); !reflect.DeepEqual(got, prores) {
		t.Fatalf("prores argv changed:\n%q\n%q", got, prores)
	}
}

func TestFramesEncoderArgvRationalRateAndChapters(t *testing.T) {
	want := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-threads", "2", "-filter_threads", "1", "-f", "image2pipe", "-framerate", "30000/1001", "-c:v", "png", "-i", "pipe:0", "-f", "ffmetadata", "-i", "/job/chapters.ffmeta", "-map", "0:v:0", "-map_chapters", "1", "-an", "-frames:v", "45", "-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-movflags", "+faststart", "-f", "mp4", "-threads", "2", "-fs", "1073741824", "/job/video"}
	rate, _ := NewRate(30000, 1001)
	if got := encoderArgs(rate.String(), 45, "h264", "/job/chapters.ffmeta", "/job/video"); !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
}

func TestFramesMaxFramesMatchesEditmap(t *testing.T) {
	if MaxFrames != editmap.MaxFrames {
		t.Fatalf("videoexport.MaxFrames %d != editmap.MaxFrames %d", MaxFrames, editmap.MaxFrames)
	}
}

func TestFramesRateSet(t *testing.T) {
	for _, c := range []struct {
		num, den int64
		want     Rate
	}{{30, 1, Rate{30, 1}}, {30000, 1000, Rate{30, 1}}, {60, 2, Rate{30, 1}}, {1, 1, Rate{1, 1}}, {60, 1, Rate{60, 1}}, {24000, 1001, Rate{24000, 1001}}, {30000, 1001, Rate{30000, 1001}}, {60000, 1001, Rate{60000, 1001}}, {120000, 2002, Rate{60000, 1001}}} {
		got, err := NewRate(c.num, c.den)
		if err != nil || got != c.want {
			t.Fatalf("%d/%d: %v %v", c.num, c.den, got, err)
		}
	}
	for _, c := range [][2]int64{{0, 1}, {61, 1}, {25, 2}, {30000, 1002}, {-30, 1}, {30, -1}, {30, 0}, {48000, 1001}} {
		if _, err := NewRate(c[0], c[1]); err == nil {
			t.Fatalf("%d/%d accepted", c[0], c[1])
		}
	}
}

func TestFramesSeekInstantsFromIntegers(t *testing.T) {
	rate := Rate{30000, 1001}
	for k, want := range map[int]string{0: "0", 1: "0.03336666666666667", 7: "0.23356666666666667", 30000: "1001", 107999: "3603.5666333333334"} {
		if got := rate.seekSeconds(k); got != want {
			t.Fatalf("frame %d seeks %s, want %s", k, got, want)
		}
		if exact := rate.frameTime(k); exact.Cmp(big.NewRat(int64(k)*1001, 30000)) != 0 {
			t.Fatalf("frame %d exact time %s", k, exact)
		}
	}
	if got := (Rate{24, 1}).seekSeconds(7); got != "0.2916666666666667" {
		t.Fatal(got)
	}
	// The float64 seek argument never crosses a half-frame event placed at
	// (k-½)·Den/Num or (k+½)·Den/Num, for every frame of every rational rate.
	for _, rate := range []Rate{{24000, 1001}, {30000, 1001}, {60000, 1001}, {60, 1}} {
		for k := 1; k < MaxFrames; k += 97 {
			seek, _ := new(big.Rat).SetString(rate.seekSeconds(k))
			lo := new(big.Rat).SetFrac64(int64(2*k-1)*rate.Den, 2*rate.Num)
			hi := new(big.Rat).SetFrac64(int64(2*k+1)*rate.Den, 2*rate.Num)
			if seek.Cmp(lo) <= 0 || seek.Cmp(hi) >= 0 {
				t.Fatalf("%s frame %d seek %s outside its half-frame window", rate, k, rate.seekSeconds(k))
			}
		}
	}
}

func frameRequest(t *testing.T) FrameRequest {
	t.Helper()
	return FrameRequest{Cast: strings.NewReader("cast"), Output: filepath.Join(t.TempDir(), "out.mp4"), Rate: Rate{30000, 1001}, Frames: 45, Width: 640, Height: 480, Codec: "h264"}
}

func TestFramesPlanRefusals(t *testing.T) {
	for name, mutate := range map[string]func(*FrameRequest){
		"codec":           func(q *FrameRequest) { q.Codec = "vp9" },
		"odd width":       func(q *FrameRequest) { q.Width = 641 },
		"small height":    func(q *FrameRequest) { q.Height = 100 },
		"rate":            func(q *FrameRequest) { q.Rate = Rate{25, 2} },
		"zero rate":       func(q *FrameRequest) { q.Rate = Rate{} },
		"no frames":       func(q *FrameRequest) { q.Frames = 0 },
		"too many frames": func(q *FrameRequest) { q.Frames = MaxFrames + 1 },
		"framing":         func(q *FrameRequest) { q.Presentation.Framing = "stretch" },
		"fit fontPx":      func(q *FrameRequest) { q.Presentation.FontPx = 12 },
		"font no px":      func(q *FrameRequest) { q.Presentation.Framing = "font" },
		"font huge":       func(q *FrameRequest) { q.Presentation = Presentation{Framing: "font", FontPx: 97} },
		"blinking":        func(q *FrameRequest) { q.Presentation.Cursor = "blinking" },
		"cursor script":   func(q *FrameRequest) { q.Presentation.Cursor = "steady'}</script>" },
		"padding":         func(q *FrameRequest) { q.Presentation.Padding = 129 },
		"negative pad":    func(q *FrameRequest) { q.Presentation.Padding = -1 },
		"min font":        func(q *FrameRequest) { q.Presentation.MinFontPx = 49 },
		"chapter gap":     func(q *FrameRequest) { q.Chapters = []Chapter{{"a", 0, 10}, {"b", 11, 45}} },
		"chapter overlap": func(q *FrameRequest) { q.Chapters = []Chapter{{"a", 0, 10}, {"b", 9, 45}} },
		"chapter short":   func(q *FrameRequest) { q.Chapters = []Chapter{{"a", 0, 44}} },
		"chapter long":    func(q *FrameRequest) { q.Chapters = []Chapter{{"a", 0, 46}} },
		"chapter late":    func(q *FrameRequest) { q.Chapters = []Chapter{{"a", 1, 45}} },
		"chapter empty":   func(q *FrameRequest) { q.Chapters = []Chapter{{"a", 0, 0}, {"b", 0, 45}} },
		"title blank":     func(q *FrameRequest) { q.Chapters = []Chapter{{" \n", 0, 45}} },
		"title long":      func(q *FrameRequest) { q.Chapters = []Chapter{{strings.Repeat("x", 129), 0, 45}} },
		"title invalid":   func(q *FrameRequest) { q.Chapters = []Chapter{{"\xff", 0, 45}} },
	} {
		t.Run(name, func(t *testing.T) {
			q := frameRequest(t)
			mutate(&q)
			if _, err := planFrames(q); err == nil {
				t.Fatal("accepted")
			}
			// Refused before any staging, permit or process.
			if _, err := (&Renderer{}).RenderFrames(context.Background(), q); err == nil {
				t.Fatal("RenderFrames accepted")
			}
			if entries, _ := os.ReadDir(filepath.Dir(q.Output)); len(entries) != 0 {
				t.Fatalf("staging retained: %v", entries)
			}
		})
	}
	q := frameRequest(t)
	q.Chapters = []Chapter{{"Setup", 0, 10}, {"Run", 10, 45}}
	got, err := planFrames(q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rate != (Rate{30000, 1001}) || got.VideoDuration != "3003/2000" || got.Presentation != (Presentation{Framing: "fit", Cursor: "steady", MinFontPx: 10}) || len(got.Chapters) != 2 {
		t.Fatalf("%+v", got)
	}
	q.Rate = Rate{24, 1}
	q.Frames = 48
	q.Chapters = nil
	if got, err = planFrames(q); err != nil || got.VideoDuration != "2" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestFramesAdmissionCancellationAndInputFailurePublishNothing(t *testing.T) {
	r := &Renderer{}
	q := frameRequest(t)
	permit <- struct{}{}
	if _, err := r.RenderFrames(context.Background(), q); !errors.Is(err, ErrBusy) {
		t.Fatalf("%v", err)
	}
	if _, err := r.Preflight(context.Background(), PreflightRequest{Geometries: []Geometry{{80, 24}}, Width: 640, Height: 480}); !errors.Is(err, ErrBusy) {
		t.Fatalf("preflight shares the permit: %v", err)
	}
	<-permit
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.RenderFrames(ctx, q); err == nil {
		t.Fatal("canceled job accepted")
	}
	q.Cast = failedReader{}
	if _, err := r.RenderFrames(context.Background(), q); err == nil {
		t.Fatal("source failure accepted")
	}
	for _, text := range []string{"not a cast", `{"version":3,"term":{"cols":80,"rows":24}}` + "\n[0,\"r\",\"100000x24\"]\n"} {
		q.Cast = strings.NewReader(text)
		if _, err := r.RenderFrames(context.Background(), q); err == nil {
			t.Fatal("inadmissible cast accepted")
		}
	}
	if err := os.WriteFile(q.Output, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	q.Cast = strings.NewReader("cast")
	if _, err := r.RenderFrames(context.Background(), q); err == nil {
		t.Fatal("existing output accepted")
	}
	if got, _ := os.ReadFile(q.Output); string(got) != "existing" {
		t.Fatal("existing output changed")
	}
	os.Remove(q.Output)
	if entries, err := os.ReadDir(filepath.Dir(q.Output)); err != nil || len(entries) != 0 {
		t.Fatalf("partial output/staging retained: %v %v", entries, err)
	}
	if len(permit) != 0 {
		t.Fatal("permit leaked")
	}
}

func TestFramesPreflightRequestRefusals(t *testing.T) {
	r := &Renderer{}
	for name, q := range map[string]PreflightRequest{
		"none":         {Width: 640, Height: 480},
		"too many":     {Geometries: make([]Geometry, MaxGeometries+1), Width: 640, Height: 480},
		"cells":        {Geometries: []Geometry{{512, 512}}, Width: 640, Height: 480},
		"output":       {Geometries: []Geometry{{80, 24}}, Width: 100, Height: 480},
		"presentation": {Geometries: []Geometry{{80, 24}}, Width: 640, Height: 480, Presentation: Presentation{Cursor: "blinking"}},
	} {
		if _, err := r.Preflight(context.Background(), q); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestFramesCastGeometries(t *testing.T) {
	write := func(text string) string {
		path := filepath.Join(t.TempDir(), "source.cast")
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := write(`{"version":3,"term":{"cols":80,"rows":24}}` + "\n[0.5,\"o\",\"x\"]\n[0.5,\"r\",\"100x30\"]\n[0.5,\"r\",\"80x24\"]\n[0.5,\"r\",\"100x30\"]\n")
	got, err := castGeometries(context.Background(), path)
	if err != nil || !reflect.DeepEqual(got, []Geometry{{80, 24}, {100, 30}}) {
		t.Fatalf("%v %v", got, err)
	}
	var b strings.Builder
	b.WriteString(`{"version":3,"term":{"cols":80,"rows":24}}` + "\n")
	for i := 1; i <= MaxGeometries; i++ {
		fmt.Fprintf(&b, "[0.1,\"r\",\"%dx%d\"]\n", 80+i, 24)
	}
	path = write(b.String())
	if _, err := castGeometries(context.Background(), path); err == nil {
		t.Fatal("17 geometries accepted")
	}
	// Legacy admission has no geometry-count policy.
	if err := admitCast(context.Background(), path); err != nil {
		t.Fatal(err)
	}
}

func TestFramesChapterMetadataEscaping(t *testing.T) {
	chapters, err := checkChapters([]Chapter{{"Setup = a;b #c \\d\nnext\r", 0, 7}, {"Plain", 7, 31}}, 31)
	if err != nil {
		t.Fatal(err)
	}
	if chapters[0].Title != "Setup = a;b #c \\d next" {
		t.Fatalf("%q", chapters[0].Title)
	}
	want := ";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1001/30000\nSTART=0\nEND=7\ntitle=Setup \\= a\\;b \\#c \\\\d next\n[CHAPTER]\nTIMEBASE=1001/30000\nSTART=7\nEND=31\ntitle=Plain\n"
	if got := ffmetadata(Rate{30000, 1001}, chapters); got != want {
		t.Fatalf("%q", got)
	}
	if got := escapeMetadata("a\nb"); got != "a\\\nb" {
		t.Fatalf("%q", got)
	}
	if SanitizeChapterTitle("a\x00b\u0085c\td") != "a b c d" {
		t.Fatal("control characters survive")
	}
}

func chapterJSON(tb string, spans ...[2]int64) string {
	var parts []string
	for i, s := range spans {
		parts = append(parts, fmt.Sprintf(`{"id":%d,"time_base":%q,"start":%d,"end":%d,"tags":{"title":"c%d"}}`, i, tb, s[0], s[1], i))
	}
	return `{"chapters":[` + strings.Join(parts, ",") + `]}`
}

func TestFramesChapterVerification(t *testing.T) {
	rate := Rate{30000, 1001}
	want := []Chapter{{"c0", 0, 7}, {"c1", 7, 31}, {"c2", 31, 45}}
	// Observed FFmpeg 8.1.1 MP4/MOV output: millisecond chapter time base.
	got, err := matchChapters([]byte(chapterJSON("1/1000", [2]int64{0, 234}, [2]int64{234, 1034}, [2]int64{1034, 1502})), rate, want)
	if err != nil || len(got) != 3 || got[1].StartFrame != 7 || got[2].EndFrame != 45 {
		t.Fatalf("%+v %v", got, err)
	}
	// Exact frame time base also matches.
	if _, err := matchChapters([]byte(chapterJSON("1001/30000", [2]int64{0, 7}, [2]int64{7, 31}, [2]int64{31, 45})), rate, want); err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string]string{
		"off by a frame": chapterJSON("1/1000", [2]int64{0, 267}, [2]int64{267, 1034}, [2]int64{1034, 1502}),
		"count":          chapterJSON("1/1000", [2]int64{0, 234}, [2]int64{234, 1502}),
		"title":          strings.Replace(chapterJSON("1/1000", [2]int64{0, 234}, [2]int64{234, 1034}, [2]int64{1034, 1502}), `"c1"`, `"c9"`, 1),
		"end":            chapterJSON("1/1000", [2]int64{0, 234}, [2]int64{234, 1034}, [2]int64{1034, 1600}),
		"time base":      chapterJSON("0/1", [2]int64{0, 234}, [2]int64{234, 1034}, [2]int64{1034, 1502}),
		"garbage":        "{",
	} {
		if _, err := matchChapters([]byte(doc), rate, want); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err := matchChapters([]byte(`{"chapters":[]}`), rate, nil); err != nil {
		t.Fatal("chapter-less output rejected")
	}
	if _, err := matchChapters([]byte(chapterJSON("1/1000", [2]int64{0, 1502})), rate, nil); err == nil {
		t.Fatal("unexpected chapters accepted")
	}
}

// Risk check: MP4/MOV store chapter times in milliseconds. Every frame index up
// to MaxFrames at each allowed x/1001 rate survives ms quantization (round or
// truncate) under round(t·Rate), since the error (<1 ms) is far below half a
// frame (>8.3 ms). Exact times do not survive; frame indices do.
func TestFramesMillisecondChapterQuantizationKeepsFrameIndices(t *testing.T) {
	for _, rate := range []Rate{{24000, 1001}, {30000, 1001}, {60000, 1001}, {60, 1}, {1, 1}} {
		exactLost := false
		for k := 0; k <= MaxFrames; k++ {
			ms := new(big.Rat).Mul(rate.frameTime(k), big.NewRat(1000, 1))
			floor := new(big.Int).Quo(ms.Num(), ms.Denom()).Int64()
			nearest := new(big.Int).Quo(new(big.Rat).Add(ms, big.NewRat(1, 2)).Num(), new(big.Rat).Add(ms, big.NewRat(1, 2)).Denom()).Int64()
			if !ms.IsInt() {
				exactLost = true
			}
			for _, ticks := range []int64{floor, nearest} {
				got, err := roundFrame(ticks, "1/1000", rate)
				if err != nil || got != k {
					t.Fatalf("%s frame %d: %d ms reads back as frame %d (%v)", rate, k, ticks, got, err)
				}
			}
		}
		if rate.Den == 1001 && !exactLost {
			t.Fatalf("%s unexpectedly exact in milliseconds", rate)
		}
	}
}

func TestFramesStreamVerification(t *testing.T) {
	want := FrameResult{Codec: "h264", Rate: Rate{30000, 1001}, Width: 640, Height: 480, Frames: 45}
	doc := func(rate, frames, duration string) []byte {
		return []byte(fmt.Sprintf(`{"streams":[{"codec_name":"h264","width":640,"height":480,"r_frame_rate":%q,"nb_read_frames":%q,"duration":%q}]}`, rate, frames, duration))
	}
	if got, err := matchStream(doc("30000/1001", "45", "1.501500"), want); err != nil || got.DecodedFrame != 45 {
		t.Fatalf("%+v %v", got, err)
	}
	// Within one frame period (1001/30000 s) either side.
	for _, d := range []string{"1.468134", "1.534866"} {
		if _, err := matchStream(doc("30000/1001", "45", d), want); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
	}
	for name, b := range map[string][]byte{
		"rate":       doc("2997/100", "45", "1.501500"),
		"integer":    doc("30/1", "45", "1.501500"),
		"frames":     doc("30000/1001", "44", "1.501500"),
		"long":       doc("30000/1001", "45", "1.534900"),
		"short":      doc("30000/1001", "45", "1.468100"),
		"no frames":  doc("30000/1001", "", "1.501500"),
		"no streams": []byte(`{"streams":[]}`),
	} {
		if _, err := matchStream(b, want); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	codec := want
	codec.Codec = "prores"
	if _, err := matchStream(doc("30000/1001", "45", "1.501500"), codec); err == nil {
		t.Fatal("codec mismatch accepted")
	}
}

func TestFramesPreflightJudgement(t *testing.T) {
	pres, _ := Presentation{}.normalize()
	ok := Measurement{Geometry: Geometry{80, 24}, Left: 106.45, Top: 0, Width: 1067.5, Height: 720.44, FontPx: 21.5, AdvancePx: 12.94, AdvanceEm: 0.602, FontLoaded: true, BorderPx: 16.1, Clip: Rect{106.45, 0, 1173.95, 720}}
	m := ok
	judge(&m, 1280, 720, pres)
	if len(m.Refusals) != 0 {
		t.Fatal(m.Refusals)
	}
	for name, c := range map[string]struct {
		mutate func(*Measurement, *Presentation)
		reason string
	}{
		"illegible": {func(m *Measurement, p *Presentation) { m.FontPx = 9.9 }, "illegible"},
		"fallback":  {func(m *Measurement, p *Presentation) { m.AdvanceEm = 0.55 }, "fallback font"},
		"unloaded":  {func(m *Measurement, p *Presentation) { m.FontLoaded = false }, "did not load"},
		"wide":      {func(m *Measurement, p *Presentation) { m.Width = 1200 }, "would crop"},
		"offscreen": {func(m *Measurement, p *Presentation) { m.Left = -17 }, "would crop"},
		"tall":      {func(m *Measurement, p *Presentation) { m.Height = 740 }, "would crop"},
		"border":    {func(m *Measurement, p *Presentation) { m.BorderPx = -1 }, "would crop"},
		"padding":   {func(m *Measurement, p *Presentation) { p.Padding = 17 }, "would crop"},
		"clipped":   {func(m *Measurement, p *Presentation) { m.Clip.Right = 1150 }, "would crop"},
		"unclipped": {func(m *Measurement, p *Presentation) { m.Clip = Rect{} }, "would crop"},
		"nan":       {func(m *Measurement, p *Presentation) { m.FontPx = 0 / zero }, "illegible"},
	} {
		m, p := ok, pres
		c.mutate(&m, &p)
		judge(&m, 1280, 720, p)
		if len(m.Refusals) != 1 || !strings.Contains(m.Refusals[0], c.reason) {
			t.Fatalf("%s: %v", name, m.Refusals)
		}
	}
}

var zero float64

func TestFramesPageInjectsOnlyValidatedConfig(t *testing.T) {
	html, err := framesHTML(Presentation{Framing: "font", FontPx: 24, Padding: 8, Cursor: "hidden", MinFontPx: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, `const config={"framing":"font","fontPx":24,"padding":8,"cursor":"hidden"};`) || strings.Contains(html, "/*CONFIG*/") {
		t.Fatal("config not injected")
	}
	if _, err := framesHTML(Presentation{Framing: "fit", Cursor: `"};alert(1);//`, MinFontPx: 10}); err == nil {
		t.Fatal("unvalidated presentation injected")
	}
}

func TestFramesPageThemeIsValidatedHexOnly(t *testing.T) {
	purple := termtheme.Spec{Preset: "asciinema", Bg: "#5b2a86"}.Theme()
	html, err := framesHTML(Presentation{Framing: "fit", Cursor: "steady", MinFontPx: 10, Theme: &purple})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, `"theme":{"fg":"#cccccc","bg":"#5b2a86","palette":["#000000",`) || !strings.Contains(html, "o.theme='agentreel'") {
		t.Fatal("theme not injected")
	}
	for _, bad := range []string{`#000000;}</style><script>alert(1)</script>`, "red", "#12345", "#1234567", "#ABCDEF", `#00000"`, "var(--x)", ""} {
		theme := purple
		theme.Background = bad
		if _, err := framesHTML(Presentation{Framing: "fit", Cursor: "steady", MinFontPx: 10, Theme: &theme}); err == nil {
			t.Fatalf("unvalidated theme background %q injected", bad)
		}
		theme = purple
		theme.Palette[15] = bad
		if _, err := (Presentation{Theme: &theme}).normalize(); err == nil {
			t.Fatalf("unvalidated palette colour %q accepted", bad)
		}
	}
}
