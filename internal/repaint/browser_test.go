//go:build linux && browser

package repaint

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/self-deprecated/agent-reel/internal/castexport"
)

// castEvent is one asciicast v3 event at an absolute time; the cast format
// stores intervals, so the writer differences them.
type castEvent struct {
	at   float64
	kind string
	text string
}

func castBytes(cols, rows int, events []castEvent) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "{\"version\":3,\"term\":{\"cols\":%d,\"rows\":%d}}\n", cols, rows)
	encoder := json.NewEncoder(&b)
	previous := float64(0)
	for _, e := range events {
		_ = encoder.Encode([]any{e.at - previous, e.kind, e.text})
		previous = e.at
	}
	return b.Bytes()
}

// referenceCast replays a fixture's raw steps, one event per step at whole
// seconds. This is the recording the player would have been given.
func referenceCast(f fixture) []byte {
	events := make([]castEvent, 0, len(f.steps)+1)
	for i, s := range f.steps {
		if s.resize() {
			events = append(events, castEvent{float64(i), "r", fmt.Sprintf("%dx%d", s.Cols, s.Rows)})
		} else {
			events = append(events, castEvent{float64(i), "o", s.Text})
		}
	}
	return castBytes(f.cols, f.rows, append(events, castEvent{float64(len(f.steps)), "m", "fixture end"}))
}

// fullCast is the whole-screen projection of the fixture's last screen, at the
// fixture's final geometry.
func fullCast(f fixture, screens []castexport.Screen) ([]byte, error) {
	cols, rows := geometryAt(f, len(f.steps)-1)
	text, err := Full(screens[len(screens)-1])
	if err != nil {
		return nil, err
	}
	var events []castEvent
	if cols != f.cols || rows != f.rows {
		events = append(events, castEvent{0, "r", fmt.Sprintf("%dx%d", cols, rows)})
	}
	events = append(events, castEvent{0, "o", text}, castEvent{1, "m", "projection end"})
	return castBytes(cols, rows, events), nil
}

// streamCast is what a live consumer would publish: one projection per step, a
// resize event where the source resized, and nothing at all where the screen
// did not visibly change.
func streamCast(f fixture, screens []castexport.Screen) ([]byte, error) {
	var events []castEvent
	for i := range f.steps {
		if f.steps[i].resize() {
			events = append(events, castEvent{float64(i), "r", fmt.Sprintf("%dx%d", f.steps[i].Cols, f.steps[i].Rows)})
		}
		text, err := projection(f, screens, i)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i, err)
		}
		if text != "" {
			events = append(events, castEvent{float64(i), "o", text})
		}
	}
	events = append(events, castEvent{float64(len(f.steps)), "m", "projection end"})
	return castBytes(f.cols, f.rows, events), nil
}

// TestBrowserRepaintParity is the renderer half of the proof. The Go tests show
// the projection round-trips through avt 0.18, the emulator that produced the
// screen; this shows the unmodified official player (which embeds avt 0.16)
// draws the projection exactly as it draws the original recording, comparing
// cells, computed styles, terminal geometry and canvas layers through the
// player's own public create/seek API. It is not a screenshot or pixel
// equivalence claim about the whole compositor.
func TestBrowserRepaintParity(t *testing.T) {
	if os.Getenv("AREEL_TEST_AVT") == "" {
		t.Fatal("browser repaint qualification requires AREEL_TEST_AVT")
	}
	chromium := os.Getenv("AREEL_TEST_CHROMIUM")
	if chromium == "" {
		t.Fatal("browser qualification requires AREEL_TEST_CHROMIUM")
	}
	js, err := os.ReadFile(filepath.Join("..", "reel", "web", "vendor", "player.js"))
	if err != nil {
		t.Fatal(err)
	}
	css, err := os.ReadFile(filepath.Join("..", "reel", "web", "vendor", "player.css"))
	if err != nil {
		t.Fatal(err)
	}
	var page atomic.Value
	page.Store([]byte{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/player.js":
			w.Header().Set("Content-Type", "text/javascript")
			w.Write(js)
		case "/player.css":
			w.Header().Set("Content-Type", "text/css")
			w.Write(css)
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(page.Load().([]byte))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chromium), chromedp.WindowSize(1600, 1200), chromedp.Flag("host-resolver-rules", "MAP * ~NOTFOUND, EXCLUDE 127.0.0.1"))
	if os.Getenv("AREEL_TEST_CHROMIUM_NO_SANDBOX") == "1" {
		opts = append(opts, chromedp.NoSandbox)
	}
	allocator, stopAllocator := chromedp.NewExecAllocator(context.Background(), opts...)
	defer stopAllocator()
	ctx, stopBrowser := chromedp.NewContext(allocator)
	defer stopBrowser()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	// compare is castexport's browser harness comparison, unchanged: two players
	// over the same public API, seeked, then compared on every cell's text and
	// computed colour/weight/style/decoration, on the terminal's declared and
	// measured geometry, and on every canvas layer's bitmap.
	compare := func(reference, exported []byte, referenceAt, exportedAt float64, rejectSeeks bool) error {
		data, _ := json.Marshal([]string{string(reference), string(exported)})
		page.Store([]byte(fmt.Sprintf(`<!doctype html><link rel="stylesheet" href="/player.css"><div id="a"></div><div id="b"></div><script src="/player.js"></script><script>
const data=%s;
const options={autoPlay:false,preload:true,controls:false,fit:'none',cursorMode:'steady',idleTimeLimit:Infinity};
const players=[AsciinemaPlayer.create({data:data[0]},document.querySelector('#a'),options),AsciinemaPlayer.create({data:data[1]},document.querySelector('#b'),options)];
const seek=(player,at)=>%t?Promise.reject(new Error('forced seek rejection')):player.seek(at);
Promise.all([seek(players[0],%g),seek(players[1],%g)]).then(()=>requestAnimationFrame(()=>requestAnimationFrame(()=>{document.body.dataset.ready='yes'}))).catch(()=>{document.body.dataset.ready='error'});
</script>`, data, rejectSeeks, referenceAt, exportedAt)))
		var ready, dump string
		err := chromedp.Run(ctx, chromedp.Navigate(server.URL), chromedp.Poll(`document.body.dataset.ready === 'yes' || document.body.dataset.ready === 'error'`, nil), chromedp.Evaluate(`document.body.dataset.ready`, &ready))
		if err != nil {
			return err
		}
		if ready != "yes" {
			return fmt.Errorf("official-player seek failed: %s", ready)
		}
		var equal bool
		err = chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
const grid=id=>{
 const term=document.querySelector(id+' .ap-term'); if(!term)return null;
 const cells=Array.from(term.querySelectorAll('.ap-line'),line=>Array.from(line.querySelectorAll('span')).flatMap(span=>Array.from(span.textContent,c=>{const s=getComputedStyle(span);return[c,s.color,s.backgroundColor,s.fontWeight,s.fontStyle,s.textDecorationLine]})));
 if(!cells.length)return null;
 const box=term.getBoundingClientRect(),style=getComputedStyle(term);
 const geometry=[style.getPropertyValue('--term-cols'),style.getPropertyValue('--term-rows'),box.width,box.height];
 const canvases=Array.from(term.querySelectorAll('canvas'),c=>[c.width,c.height,c.toDataURL()]);
 return {cells,geometry,canvases};
}; const a=grid('#a'),b=grid('#b');return a!==null&&b!==null&&JSON.stringify(a)===JSON.stringify(b);
})()`, &equal), chromedp.Evaluate(`document.body.innerText`, &dump))
		if err != nil {
			return err
		}
		if !equal {
			return fmt.Errorf("official-player cells/styles/geometry/canvas differ: %s", dump)
		}
		return nil
	}
	// The guard the comparison is worthless without: a failed seek must never be
	// mistaken for agreement, even when both sources are byte-identical.
	t.Run("rejects-failed-seeks-even-for-identical-sources", func(t *testing.T) {
		raw := referenceCast(fixtures()[0])
		if err := compare(raw, raw, 1.5, 1.5, true); err == nil || !strings.Contains(err.Error(), "seek failed") {
			t.Fatalf("two failed seeks must not qualify equal sources: %v", err)
		}
	})
	for _, f := range fixtures() {
		t.Run(f.name, func(t *testing.T) {
			screens := drive(t, f, false)
			reference := referenceCast(f)
			last := float64(len(f.steps)-1) + .5

			t.Run("full", func(t *testing.T) {
				candidate, err := fullCast(f, screens)
				if err != nil {
					t.Fatal(err)
				}
				err = compare(reference, candidate, last, .5, false)
				switch {
				case f.gapFull != "" && err == nil:
					t.Fatalf("recorded known gap (%s) no longer diverges; re-qualify it and remove the recorded gap", f.gapFull)
				case f.gapFull != "":
					t.Logf("known gap holds (%s): %v", f.gapFull, err)
				case err != nil:
					t.Fatal(err)
				}
			})
			t.Run("stream", func(t *testing.T) {
				candidate, err := streamCast(f, screens)
				if err != nil {
					t.Fatal(err)
				}
				for i := range f.steps {
					err := compare(reference, candidate, float64(i)+.5, float64(i)+.5, false)
					reason, known := f.gapSteps[i]
					switch {
					case known && err == nil:
						t.Errorf("step %d: recorded known gap (%s) no longer diverges; re-qualify it and remove the recorded gap", i, reason)
					case known:
						t.Logf("step %d: known gap holds (%s): %v", i, reason, err)
					case err != nil:
						t.Errorf("step %d: %v", i, err)
					}
				}
			})
		})
	}
}
