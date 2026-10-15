//go:build linux && browser

package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/creack/pty"
	"github.com/self-deprecated/agent-reel/internal/archive"
)

// This matrix exercises REAL recorder Init and output messages through the Go
// proxy. The oracle is the recorder's validated original output replayed by
// the same unmodified official player, never a fabricated Init or a second
// emulator.
func TestBrowserLiveContinuationMatrix(t *testing.T) {
	executable := os.Getenv("AREEL_TEST_ASCIINEMA")
	if executable == "" {
		t.Fatal("live fidelity qualification requires AREEL_TEST_ASCIINEMA")
	}
	chromium := os.Getenv("AREEL_TEST_CHROMIUM")
	if chromium == "" {
		t.Fatal("live fidelity qualification requires AREEL_TEST_CHROMIUM")
	}
	if err := checkVersion(context.Background(), chromium, "Chromium 150.0.7871.46"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, prefix, continuation string
		resize                     bool
	}{
		{"erased-screen", "ERASED-SECRET\x1b[2J\x1b[Hvisible", "-selected", false},
		{"unicode-color", "\x1b[31m界 e\u0301\r\nabc", "\x1b[0m日", false},
		{"saved-origin-margins-nowrap", "\x1b[?7labc\x1b[2;4r\x1b[?6h\x1b[3;74H\x1b7\x1b[31m", "\x1b8\tABCDEFGH\nX", false},
		{"pending-wrap", "\x1b[1;80HX", "Y\nZ", false},
		{"tabs-insert", "\x1b[3g\x1b[5G\x1bH\r\x1b[4hABC", "\r\tX\x1b[4lY", false},
		{"alternate-restore-hidden-cursor", "PRIMARY\x1b7\x1b[?1049hALTERNATE\x1b[?25l", "\x1b[?1049l\x1b8X", false},
		{"partial-csi", "before\x1b[38;2;12;", "34;56mCOLOR", false},
		{"partial-osc", "before\x1b]0;ignored", " title\x07AFTER", false},
		{"partial-charset", "before\x1b(", "0qqq\x1b(Bnormal", false},
		// Like production captures, this one runs the recorder without a
		// --window-size override, so a real owned PTY resize produces an actual
		// ALiS/archive resize event.
		{"recorder-dynamic-resize", "before\x1b[31m界\x1b[3;79HXYZ", "\x1b[2J\x1b[Hafter", true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newLiveFidelityFixture(t, executable, []string{"\x18\x1bc" + test.prefix, test.continuation, "\x1b[1m+FOLLOW\x1b[0m"}, test.resize)
			// The durable seed exists before even constructing a library/browser. This
			// proves warmth without a subscriber, including a pending parser sequence.
			raw := f.snapshot(t, 0, "")
			h, server, state, _ := syntheticLiveLibrary(t, f.socket, nil)
			browser := libraryBrowser(t)
			open := func() {
				runBrowser(t, browser, chromedp.Navigate(server.URL+"/recordings/"+state.ID), chromedp.Poll(`document.querySelector('#player-status')?.textContent === 'Live'`, nil))
			}
			open()
			liveFidelityCompare(t, browser, raw, "initial warm state")
			if test.resize {
				if err := pty.Setsize(f.observer.(*capture).master, &pty.Winsize{Cols: 40, Rows: 12}); err != nil {
					t.Fatal(err)
				}
				raw = f.snapshot(t, 0, "40x12")
				liveFidelityCompare(t, browser, raw, "live resize")
			}
			f.advance(t, 1)
			raw = f.snapshot(t, 1, "")
			liveFidelityCompare(t, browser, raw, "post-subscription continuation")
			// Ordinary navigation must release both sockets even while the
			// recorder is idle; no explicit test-side transport cancellation.
			runBrowser(t, browser, chromedp.Navigate(server.URL+"/"))
			liveFidelityWait(t, f.ctx, "proxy renderer disposal", func() bool {
				h.streams.mu.Lock()
				defer h.streams.mu.Unlock()
				return len(h.streams.sessions) == 0
			})
			// A fresh ALiS seed must contain the current restored state, not the initial
			// screen; subsequent real output must continue from that reconstructed state.
			open()
			liveFidelityCompare(t, browser, raw, "reconnected current state")
			f.advance(t, 2)
			raw = f.snapshot(t, 2, "")
			liveFidelityCompare(t, browser, raw, "reconnected continuation")
			libraryBrowserScreenshot(t, browser, "live-fidelity-"+test.name)
			runBrowser(t, browser, chromedp.Navigate("about:blank"))
			stop, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if err := f.observer.Stop(stop); err != nil {
				t.Fatal(err)
			}
			complete := f.sink.cast()
			liveFidelityOutput(t, complete) // Also asserts no captured input.
			if !f.sink.completed() {
				t.Fatal("owned recorder did not complete its stream")
			}
			t.Logf("verified warm init, continuation, reconnect and following output; %d bytes", len(complete))
		})
	}
}

type liveFidelityFixture struct {
	ctx      context.Context
	dir      string
	observer Observer
	sink     *streamSink
	socket   recorderSocket
	phases   []string
}

func newLiveFidelityFixture(t *testing.T, executable string, phases []string, dynamic bool) *liveFidelityFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	if err := checkVersion(ctx, executable, "asciinema 3.2.1"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "config", "state"} {
		if err := privateDir(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "config.toml"), []byte(recorderConfig), 0600); err != nil {
		t.Fatal(err)
	}
	for i, phase := range phases {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("phase-%d", i)), []byte(phase), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Private files gate output; no bytes are sent as terminal input. Each wait
	// is bounded even if the browser fails. Stop targets only this artificial PTY.
	script := filepath.Join(dir, "producer.sh")
	body := `#!/bin/sh
trap 'exit 0' INT TERM
wait_for() { i=0; while [ ! -f "$1" ]; do i=$((i+1)); [ "$i" -lt 2500 ] || exit 1; sleep 0.02; done; }
cd ` + shellQuote(dir) + `
cat phase-0
wait_for go-1
cat phase-1
wait_for go-2
cat phase-2
wait_for never
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	fifoPath := filepath.Join(dir, "capture.fifo")
	fifo, err := archive.OpenFIFO(fifoPath)
	if err != nil {
		t.Fatal(err)
	}
	sink := startStreamSink(fifo.Reader, archive.Geometry{Columns: 80, Rows: 24})
	args := []string{"session", "--overwrite", "--return", "--stream-local", "127.0.0.1:0", "--output-file", fifoPath, "-c", "/bin/sh " + shellQuote(script)}
	if !dynamic {
		args = append(args, "--window-size", "80x24")
	}
	cmd := exec.Command(executable, args...)
	cmd.Env = append(cleanCaptureEnv(""), "ASCIINEMA_CONFIG_HOME="+filepath.Join(dir, "config"), "ASCIINEMA_STATE_HOME="+filepath.Join(dir, "state"))
	var observer Observer
	c, err := startOwnedCapture(ctx, cmd, sink, fifo, Observation{Geometry: Geometry{80, 24}}, dir)
	if c != nil {
		observer = c
	}
	if observer != nil {
		t.Cleanup(func() {
			stop, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			_ = observer.Stop(stop)
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	var process ProcessIdentity
	if err := readJSON(filepath.Join(dir, "recorder.json"), &process); err != nil {
		t.Fatal(err)
	}
	socket, err := localSocketProc().discover(process)
	if err != nil {
		t.Fatal(err)
	}
	return &liveFidelityFixture{ctx, dir, observer, sink, socket, phases}
}
func (f *liveFidelityFixture) advance(t *testing.T, phase int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, fmt.Sprintf("go-%d", phase)), nil, 0600); err != nil {
		t.Fatal(err)
	}
}
func liveFidelityWait(t *testing.T, ctx context.Context, what string, ready func() bool) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(15 * time.Millisecond)
	defer tick.Stop()
	for {
		if ready() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(what, ctx.Err())
		case <-timer.C:
			t.Fatal("timed out:", what)
		case <-tick.C:
		}
	}
}
func (f *liveFidelityFixture) snapshot(t *testing.T, phase int, resize string) []byte {
	t.Helper()
	var raw []byte
	want := strings.ReplaceAll(strings.Join(f.phases[:phase+1], ""), "\r", "")
	liveFidelityWait(t, f.ctx, "validated recorder phase", func() bool {
		if err := f.observer.Err(); err != nil {
			t.Fatal(err)
		}
		out := f.sink.cast()
		if len(out) == 0 {
			return false
		}
		text, sizes := liveFidelityOutput(t, out)
		if strings.ReplaceAll(text, "\r", "") != want || (resize != "" && !strings.Contains(sizes, "|"+resize+"|")) {
			return false
		}
		raw = out
		return true
	})
	return raw
}
func liveFidelityOutput(t *testing.T, raw []byte) (string, string) {
	t.Helper()
	var text, sizes strings.Builder
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	for _, line := range lines[1:] {
		var event []json.RawMessage
		if err := json.Unmarshal(line, &event); err != nil || len(event) != 3 {
			t.Fatal("invalid verified cast event", err)
		}
		var kind, data string
		if err := json.Unmarshal(event[1], &kind); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(event[2], &data); err != nil {
			t.Fatal(err)
		}
		if kind == "i" {
			t.Fatal("input capture enabled")
		}
		if kind == "o" {
			text.WriteString(data)
		}
		if kind == "r" {
			sizes.WriteString("|" + data + "|")
		}
	}
	return text.String(), sizes.String()
}

func liveFidelityCompare(t *testing.T, ctx context.Context, raw []byte, phase string) {
	t.Helper()
	encoded, _ := json.Marshal(string(raw))
	// This test-only reference uses public player create/pause/seek APIs. The
	// production Live instance, source and options are never intercepted/changed.
	script := fmt.Sprintf(`(async()=>{
 const win=document.querySelector('#terminal-frame').contentWindow,doc=win.document;
 if(win.fidelityReference)await win.fidelityReference.dispose();
 doc.querySelector('#fidelity-reference')?.remove();
 const host=doc.createElement('div');host.id='fidelity-reference';host.style.cssText='position:fixed;left:-10000px;width:800px;height:600px';doc.body.append(host);
 const p=win.AsciinemaPlayer.create({data:%s},host,{autoPlay:false,preload:true,controls:false,fit:'both',cursorMode:'steady',idleTimeLimit:Infinity,terminalFontFamily:'DejaVu Sans Mono'});
 win.fidelityReference=p;await p.pause();await p.seek('100%%');await doc.fonts.ready;
 await new Promise(r=>win.requestAnimationFrame(()=>win.requestAnimationFrame(r)));
 win.fidelityGrid=root=>JSON.stringify({
  size:[root.querySelector('.ap-term').style.getPropertyValue('--term-cols'),root.querySelector('.ap-term').style.getPropertyValue('--term-rows')],
  lines:Array.from(root.querySelectorAll('.ap-line'),line=>Array.from(line.querySelectorAll('span')).flatMap(span=>Array.from(span.textContent,c=>{const s=win.getComputedStyle(span);return[c,s.color,s.backgroundColor,s.fontWeight,s.fontStyle,s.textDecorationLine]}))),
  canvas:Array.from(root.querySelectorAll('.ap-term canvas'),c=>c.toDataURL()),
  symbols:Array.from(root.querySelectorAll('.ap-term-symbols use'),n=>[n.getAttribute('href'),n.getAttribute('x'),n.getAttribute('y'),win.getComputedStyle(n).color])
 });
})()`, encoded)
	runBrowser(t, ctx, chromedp.Evaluate(script, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
	// Live uses the production blinking cursor; the reference's steady cursor is
	// equal during its visible blink phase. This still checks cursor position,
	// visibility, canvas colors and glyph/style state rather than hiding cursors.
	var matched bool
	err := chromedp.Run(ctx, chromedp.Poll(`(()=>{const w=document.querySelector('#terminal-frame')?.contentWindow;return w?.fidelityGrid && w.fidelityGrid(w.document.querySelector('#terminal'))===w.fidelityGrid(w.document.querySelector('#fidelity-reference'))})()`, &matched, chromedp.WithPollingInterval(30*time.Millisecond), chromedp.WithPollingTimeout(2500*time.Millisecond)))
	if err != nil || !matched {
		var dumps []string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(()=>{const w=document.querySelector('#terminal-frame').contentWindow;return ['#terminal','#fidelity-reference'].map(s=>w.fidelityGrid(w.document.querySelector(s)))})()`, &dumps))
		t.Fatalf("%s: real ALiS differs from the validated recorder output replay: %v\n%v", phase, err, dumps)
	}
}
