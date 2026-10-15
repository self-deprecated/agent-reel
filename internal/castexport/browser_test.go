//go:build linux && browser

package castexport

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
)

// Explicit optional renderer qualification: requires both pinned helper and
// Chromium. This compares cells/styles, terminal geometry and canvas layers via
// public create/seek APIs and DOM inspection, not a second emulator or vendor
// modification. It is not a whole-compositor screenshot/pixel-equivalence claim.
func TestBrowserRangeCheckpoints(t *testing.T) {
	if os.Getenv("AREEL_TEST_AVT") == "" {
		t.Fatal("browser range qualification requires AREEL_TEST_AVT")
	}
	helperCtx, factory := helperFactory(t)
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
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
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
	t.Run("rejects-failed-seeks-even-for-identical-sources", func(t *testing.T) {
		_, raw := sourceCase("", "", false)
		if err := compare(raw, raw, 1.5, 1.5, true); err == nil || !strings.Contains(err.Error(), "seek failed") {
			t.Fatalf("two failed seeks must not qualify equal blank terminals: %v", err)
		}
	})
	for _, test := range continuationCases {
		t.Run(test.name, func(t *testing.T) {
			s, raw := sourceCase(test.prefix, test.suffix, test.resize)
			var out bytes.Buffer
			if _, err := Export(helperCtx, &out, []Source{s}, &Range{1, 2}, factory); err != nil {
				t.Fatal(err)
			}
			if err := compare(raw, out.Bytes(), 1.5, .5, false); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("combined-segment-reset", func(t *testing.T) {
		a, b, reference := combinedStateCase()
		var out bytes.Buffer
		if _, err := Export(helperCtx, &out, []Source{a, b}, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := compare(reference, out.Bytes(), 1.5, 2.5, false); err != nil {
			t.Fatal(err)
		}
	})
}
