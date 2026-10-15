package webembed

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

func testAssets() Assets {
	return Assets{PlayerJS: []byte("var AsciinemaPlayer={};"), PlayerCSS: []byte(".ap-player{}"), Font: []byte("FONT"), BoxFont: []byte("BOX"),
		PlayerLicense: []byte("Apache License"), FontLicense: []byte("Bitstream Vera")}
}

// castPart extracts the inline cast of a page.
func castPart(t *testing.T, page string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<script type="application/x-asciicast" id="agent-reel-cast">(.*?)</script>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no inline cast in %s", page)
	}
	return m[1]
}

func buildPage(t *testing.T, in Input) (*Bundle, string) {
	t.Helper()
	if in.Assets.PlayerJS == nil {
		in.Assets = testAssets()
	}
	b, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var page bytes.Buffer
	if err = b.WriteIndex(&page); err != nil {
		t.Fatal(err)
	}
	return b, page.String()
}

func TestSanitizeCastRewritesHeaderAndDropsInput(t *testing.T) {
	cast := `{"version":3,"term":{"cols":10,"rows":2,"type":"xterm","theme":{"fg":"#ffffff"}},"env":{"HOME":"/home/private"},"command":"ssh host.example","title":"secret title","timestamp":1}
# a comment
[0.25,"o","a</script><!--b"]
[0.5,"i","password"]
[0.25,"x","0"]
[1,"m","kept"]
[0.5,"o","c"]
`
	b, page := buildPage(t, Input{Cast: []byte(cast)})
	got := castPart(t, page)
	want := `{"version":3,"term":{"cols":10,"rows":2}}
[0.25,"o","a\u003c/script>\u003c!--b"]
[1.75,"m","kept"]
[0.5,"o","c"]
`
	if got != want {
		t.Fatalf("cast\n%s\nwant\n%s", got, want)
	}
	for _, leak := range []string{"/home/private", "host.example", "secret title", "password", "timestamp"} {
		if strings.Contains(page, leak) {
			t.Fatalf("page carries %q", leak)
		}
	}
	if b.Duration != 2.5 || b.Markers != 1 || b.Cols != 10 || b.Rows != 2 {
		t.Fatalf("bundle info %+v", b)
	}
}

func TestSanitizeCastWeavesMarkers(t *testing.T) {
	cast := "{\"version\":3,\"term\":{\"cols\":10,\"rows\":2}}\n[0,\"o\",\"a\"]\n[1,\"m\",\"old\"]\n[1,\"o\",\"b\"]\n[2,\"o\",\"\"]\n"
	// Replace: the cast's own marker goes; markers at an event's instant
	// follow it; one beyond the end is appended; labels are cleaned.
	b, page := buildPage(t, Input{Cast: []byte(cast), Markers: []Marker{{Time: 2, Label: "Two\n<b>"}, {Time: 0.5, Label: "Half"}, {Time: 9, Label: "Late"}, {Time: 0, Label: ""}}})
	want := "{\"version\":3,\"term\":{\"cols\":10,\"rows\":2}}\n" +
		"[0,\"o\",\"a\"]\n" +
		"[0,\"m\",\"\"]\n" +
		"[0.5,\"m\",\"Half\"]\n" +
		"[1.5,\"o\",\"b\"]\n" +
		"[0,\"m\",\"Two \\u003cb\\u003e\"]\n" +
		"[2,\"o\",\"\"]\n" +
		"[5,\"m\",\"Late\"]\n"
	if got := castPart(t, page); got != want {
		t.Fatalf("cast\n%s\nwant\n%s", got, want)
	}
	if b.Markers != 4 || b.Duration != 4 {
		t.Fatalf("info %+v", b)
	}
	// An empty non-nil slice removes the cast's markers; nil keeps them.
	_, page = buildPage(t, Input{Cast: []byte(cast), Markers: []Marker{}})
	if strings.Contains(castPart(t, page), `"m"`) {
		t.Fatal("empty markers kept the cast's marker")
	}
	_, page = buildPage(t, Input{Cast: []byte(cast)})
	if !strings.Contains(castPart(t, page), `[1,"m","old"]`) {
		t.Fatal("nil markers dropped the cast's marker")
	}
}

func TestBuildRefusals(t *testing.T) {
	good := []byte("{\"version\":3,\"term\":{\"cols\":10,\"rows\":2}}\n[0,\"o\",\"a\"]\n")
	for name, in := range map[string]Input{
		"v2":        {Cast: []byte("{\"version\":2,\"width\":10,\"height\":2}\n[0,\"o\",\"a\"]\n")},
		"empty":     {Cast: nil},
		"bad event": {Cast: []byte("{\"version\":3,\"term\":{\"cols\":10,\"rows\":2}}\n[0,\"o\"]\n")},
		"negative":  {Cast: []byte("{\"version\":3,\"term\":{\"cols\":10,\"rows\":2}}\n[-1,\"o\",\"a\"]\n")},
		"size":      {Cast: []byte("{\"version\":3,\"term\":{\"cols\":0,\"rows\":2}}\n")},
		"poster":    {Cast: good, Options: Options{Poster: "data:text/plain,x"}},
		"controls":  {Cast: good, Options: Options{Controls: "yes"}},
		"fit":       {Cast: good, Options: Options{Fit: "cover"}},
		"speed":     {Cast: good, Options: Options{Speed: 100}},
		"idle":      {Cast: good, Options: Options{IdleTimeLimit: -1}},
		"marker":    {Cast: good, Markers: []Marker{{Time: -1}}},
	} {
		in.Assets = testAssets()
		if _, err := Build(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	unsafe := testAssets()
	unsafe.PlayerJS = []byte("x='</SCRIPT>'")
	if _, err := Build(Input{Cast: good, Assets: unsafe, SingleFile: true}); err == nil {
		t.Error("an uninlinable script was inlined")
	}
	if _, err := Build(Input{Cast: good, Assets: unsafe}); err != nil {
		t.Errorf("sibling assets need no inlining: %v", err)
	}
}

func TestOptionsAndThemeReachThePage(t *testing.T) {
	cast := []byte("{\"version\":3,\"term\":{\"cols\":10,\"rows\":2}}\n[0,\"o\",\"a\"]\n")
	spec, _ := termtheme.Parse("gruvbox-dark,bg=#010203")
	_, page := buildPage(t, Input{Cast: cast, Theme: spec, Options: Options{Title: "A <b>&\"lesson\"", Poster: "npt:1:02.5", IdleTimeLimit: 2, Speed: 1.5, Controls: "never", AutoPlay: true, Loop: true, Fit: "width"}})
	m := regexp.MustCompile(`<script type="application/json" id="agent-reel-config">(.*?)</script>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no config")
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(m[1]), &config); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"fit": "width", "controls": false, "autoPlay": true, "loop": true, "speed": 1.5, "idleTimeLimit": 2.0, "poster": "npt:1:02.5"}
	for k, v := range want {
		if config[k] != v {
			t.Fatalf("config %s = %v, want %v (%s)", k, config[k], v, m[1])
		}
	}
	if !strings.Contains(page, "<title>A &lt;b&gt;&amp;&#34;lesson&#34;</title>") {
		t.Fatal("title not escaped")
	}
	theme := spec.Theme()
	if !strings.Contains(page, "--reel-term-bg:#010203") || !strings.Contains(page, "--reel-term-fg:"+theme.Foreground) || !strings.Contains(page, "--reel-term-15:"+theme.Palette[15]) {
		t.Fatal("theme colours missing")
	}
	// Defaults: first-frame poster, auto controls, fit both, speed 1.
	_, page = buildPage(t, Input{Cast: cast})
	if !strings.Contains(page, `{"fit":"both","controls":"auto","autoPlay":false,"loop":false,"speed":1,"poster":"npt:0.001"}`) || !strings.Contains(page, "<title>"+DefaultTitle+"</title>") {
		t.Fatalf("defaults: %s", page[:400])
	}
}

func TestZipIsDeflatedAndSelfContained(t *testing.T) {
	cast := []byte("{\"version\":3,\"term\":{\"cols\":10,\"rows\":2}}\n[0,\"o\",\"" + strings.Repeat("hello world ", 2000) + "\"]\n")
	b, err := Build(Input{Cast: cast, Assets: testAssets()})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err = b.WriteZip(&buf); err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range z.File {
		names = append(names, f.Name)
		if f.Method != zip.Deflate {
			t.Fatalf("%s is not deflated", f.Name)
		}
		if f.Name == IndexName && f.CompressedSize64*10 > f.UncompressedSize64 {
			t.Fatalf("index.html barely compressed: %d of %d", f.CompressedSize64, f.UncompressedSize64)
		}
	}
	if strings.Join(names, " ") != "index.html assets/player.js assets/player.css assets/DejaVuSansMono.ttf assets/AgentReelBox.ttf LICENSE-player.txt LICENSE-font.txt NOTICE.txt" {
		t.Fatalf("names %q", names)
	}
	// The page references only its own relative files.
	f, _ := z.Open(IndexName)
	page, _ := io.ReadAll(f)
	for _, ref := range regexp.MustCompile(`(?:src|href)="([^"]*)"|url\("([^"]*)"\)`).FindAllStringSubmatch(string(page), -1) {
		u := ref[1] + ref[2]
		if u != "data:," && !strings.HasPrefix(u, "assets/") {
			t.Fatalf("external reference %q", u)
		}
	}
	if bytes.Contains(page, []byte("http://")) || bytes.Contains(page, []byte("https://")) || bytes.Contains(page, []byte("//cdn")) {
		t.Fatal("page names a network URL")
	}
}

func TestSingleFileInlinesEverything(t *testing.T) {
	cast := []byte("{\"version\":3,\"term\":{\"cols\":10,\"rows\":2}}\n[0,\"o\",\"a\"]\n")
	b, page := buildPage(t, Input{Cast: cast, SingleFile: true})
	if names := b.Names(); len(names) != 1 || names[0] != IndexName {
		t.Fatalf("names %q", names)
	}
	for _, want := range []string{"var AsciinemaPlayer={};", ".ap-player{}", "data:font/ttf;base64,Rk9OVA==", "data:font/ttf;base64,Qk9Y", "Apache License", "Bitstream Vera"} {
		if !strings.Contains(page, want) {
			t.Fatalf("single file lacks %q", want)
		}
	}
	if strings.Contains(page, "assets/") {
		t.Fatal("single file references sibling assets")
	}
	dir := filepath.Join(t.TempDir(), "out")
	if err := b.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dir entries %v", entries)
	}
	if err := b.WriteDir(dir); err == nil {
		t.Fatal("WriteDir replaced an existing directory")
	}
}
