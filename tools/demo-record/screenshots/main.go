// Command screenshots renders docs/images from the demo recordings that
// tools/demo-record/record.py made: the library, the viewer and a web embed.
//
//	go run ./tools/demo-record/screenshots -chromium "$AREEL_TEST_CHROMIUM"
//
// It serves the demo library with the installed `areel open` on a private
// port, so only demo recordings are shown, and waits for each page to paint
// before capturing it.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/chromedp/chromedp"
)

func main() {
	chromium := flag.String("chromium", os.Getenv("AREEL_TEST_CHROMIUM"), "Chromium executable")
	library := flag.String("library", "/tmp/areel-rec/lib", "demo library")
	out := flag.String("out", "docs/images", "output directory")
	agent := flag.String("agent", "claude", "whose recording the viewer and embed show")
	port := flag.Int("port", 18768, "loopback port for the library UI")
	flag.Parse()
	if *chromium == "" {
		log.Fatal("-chromium is required (run inside nix develop)")
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", *port)
	ui := exec.Command("areel", "open", "--library", *library, "--listen", fmt.Sprintf("127.0.0.1:%d", *port))
	if err := ui.Start(); err != nil {
		log.Fatal(err)
	}
	defer ui.Process.Kill()

	var list struct {
		Items []struct {
			ID     string `json:"id"`
			Target struct {
				Name string `json:"name"`
			} `json:"target"`
		} `json:"items"`
	}
	for i := 0; ; i++ {
		if err := getJSON(base+"/api/recordings", &list); err == nil {
			break
		} else if i == 50 {
			log.Fatalf("library UI: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	rec := ""
	for _, r := range list.Items {
		if r.Target.Name == "demo-"+*agent {
			rec = r.ID
		}
	}
	if rec == "" {
		log.Fatalf("no demo-%s recording in %s", *agent, *library)
	}
	var marks struct {
		Items []struct {
			Kind    string  `json:"kind"`
			Label   string  `json:"label"`
			Seconds float64 `json:"seconds"`
		} `json:"items"`
	}
	if err := getJSON(base+"/api/recordings/"+rec+"/marks?before=9007199254740992&limit=64", &marks); err != nil {
		log.Fatal(err)
	}
	at := map[string]float64{}
	for _, m := range marks.Items {
		if m.Kind == "areel:mark" {
			at[m.Label] = m.Seconds
		}
	}

	embed := filepath.Join(os.TempDir(), fmt.Sprintf("areel-docs-embed-%d.html", os.Getpid()))
	defer os.Remove(embed)
	export := exec.Command("areel", "export", rec, embed, "--library", *library, "--format", "web", "--single-file",
		"--idle-limit", "2", "--redact", "secrets,emails,homepaths", "--controls", "always",
		"--poster", fmt.Sprintf("npt:%.1f", at["Export this recording"]+12))
	if b, err := export.CombinedOutput(); err != nil {
		log.Fatalf("areel export: %v: %s", err, b)
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(*chromium), chromedp.Flag("hide-scrollbars", true))
	actx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(actx)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	frame := `document.querySelector('#terminal-frame')?.contentDocument`
	shots := []struct {
		name          string
		url           string
		width, height int64
		ready         string
		then          string
	}{
		{"library.png", base + "/", 1440, 560,
			`[...document.querySelectorAll('[data-preview-url]')].every(c => c.dataset.previewState === 'ready')`, ""},
		// Mid-way through the docs edit, with the marks rail beside it.
		{"viewer.png", fmt.Sprintf("%s/recordings/%s?mode=history&segment=1&at=%.1f", base, rec, (at["Update the docs"]+at["Export this recording"])/2), 1440, 860,
			frame + `?.querySelector('.ap-term')?.textContent.trim().length > 0`,
			`(() => { const b = ` + frame + `.querySelector('.ap-playback-button'); if (b.getAttribute('aria-label') === 'Pause') b.click(); return true })()`},
		{"web-embed.png", "file://" + embed, 1280, 800,
			`document.querySelector('.ap-term')?.textContent.trim().length > 0`, ""},
	}
	for _, s := range shots {
		var png []byte
		tasks := chromedp.Tasks{
			chromedp.EmulateViewport(s.width, s.height, chromedp.EmulateScale(2)),
			chromedp.Navigate(s.url),
			chromedp.Poll(s.ready, nil, chromedp.WithPollingTimeout(60*time.Second)),
		}
		if s.then != "" {
			tasks = append(tasks, chromedp.Evaluate(s.then, nil))
		}
		tasks = append(tasks, chromedp.Sleep(1500*time.Millisecond), chromedp.CaptureScreenshot(&png))
		if err := chromedp.Run(ctx, tasks); err != nil {
			log.Fatalf("%s: %v", s.name, err)
		}
		path := filepath.Join(*out, s.name)
		if err := os.WriteFile(path, png, 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote", path)
	}
}

func getJSON(url string, v any) error {
	res, err := http.Get(url)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, res.Status)
	}
	return json.NewDecoder(res.Body).Decode(v)
}
