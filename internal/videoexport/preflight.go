package videoexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// PreflightRequest measures terminal geometries in the real renderer page with
// synthetic casts and no encoder (for plans/previews).
type PreflightRequest struct {
	Geometries    []Geometry
	Width, Height int
	Presentation  Presentation
}

// Measurement is one geometry measured in the renderer page, in CSS pixels.
type Measurement struct {
	Geometry
	Left       float64 `json:"left"`
	Top        float64 `json:"top"`
	Width      float64 `json:"width"`
	Height     float64 `json:"height"`
	FontPx     float64 `json:"fontPx"`
	AdvancePx  float64 `json:"advancePx"`
	AdvanceEm  float64 `json:"advanceEm"`
	FontLoaded bool    `json:"fontLoaded"`
	// BorderPx is the player's background-coloured terminal border on each
	// side; Left/Top/Width/Height include it.
	BorderPx float64 `json:"borderPx"`
	// Clip is the painted region: the viewport intersected with the padding
	// box of every overflow-clipping ancestor of the terminal (the player
	// clips at its own box). Cells outside it are never painted.
	Clip Rect `json:"clip"`
	// Refusals lists why this geometry cannot be rendered legibly/uncropped.
	Refusals []string `json:"refusals,omitempty"`
}

// Rect is a layout rectangle in CSS pixels.
type Rect struct {
	Left   float64 `json:"left"`
	Top    float64 `json:"top"`
	Right  float64 `json:"right"`
	Bottom float64 `json:"bottom"`
}

// AssetHashes pin the glyph behaviour the measurements depend on.
type AssetHashes struct {
	PlayerJS  string `json:"playerJS"`
	PlayerCSS string `json:"playerCSS"`
	Font      string `json:"font"`
	BoxFont   string `json:"boxFont,omitempty"`
}

type PreflightResult struct {
	Width        int           `json:"width"`
	Height       int           `json:"height"`
	Presentation Presentation  `json:"presentation"`
	Assets       AssetHashes   `json:"assets"`
	Geometries   []Measurement `json:"geometries"`
}

// PreflightError is a measured refusal; the result still carries measurements.
// Width, Height, MinFontPx and Measurements let a caller explain the refusal
// without parsing Reasons.
type PreflightError struct {
	Reasons       []string
	Width, Height int
	MinFontPx     int
	Measurements  []Measurement
}

func (e *PreflightError) Error() string {
	return "video preflight refused: " + strings.Join(e.Reasons, "; ")
}

// judge applies the refusal rules to one measurement. Measurements are layout
// observations, so float comparison is appropriate here.
func judge(m *Measurement, width, height int, p Presentation) {
	const eps = 0.5 // sub-pixel layout rounding, never a visible crop
	m.Refusals = nil
	if !m.FontLoaded {
		m.Refusals = append(m.Refusals, "export font ExportMono did not load")
	}
	if !(m.AdvanceEm >= advanceTarget-advanceSlack && m.AdvanceEm <= advanceTarget+advanceSlack) {
		m.Refusals = append(m.Refusals, fmt.Sprintf("glyph advance %.4f em is outside DejaVu Sans Mono %.2f±%.2f em (fallback font)", m.AdvanceEm, advanceTarget, advanceSlack))
	}
	if !(m.FontPx >= float64(p.MinFontPx)) {
		m.Refusals = append(m.Refusals, fmt.Sprintf("%dx%d terminal font %.2f px is below the %d px minimum at %dx%d (illegible)", m.Cols, m.Rows, m.FontPx, p.MinFontPx, width, height))
	}
	// The cell area (the box inside the player's background-coloured border)
	// must lie inside the padded viewport and inside the measured painted
	// clip. The border itself may lose a sub-pixel to the player's own fit
	// rounding, never a cell.
	pad := float64(p.Padding)
	left, top := m.Left+m.BorderPx, m.Top+m.BorderPx
	right, bottom := m.Left+m.Width-m.BorderPx, m.Top+m.Height-m.BorderPx
	inViewport := left >= pad-eps && top >= pad-eps && right <= float64(width)-pad+eps && bottom <= float64(height)-pad+eps
	inClip := left >= m.Clip.Left-eps && top >= m.Clip.Top-eps && right <= m.Clip.Right+eps && bottom <= m.Clip.Bottom+eps
	if !(m.BorderPx >= 0 && inViewport && inClip) {
		m.Refusals = append(m.Refusals, fmt.Sprintf("%dx%d terminal cells %.1fx%.1f at %.1f,%.1f with %d px padding exceed the %dx%d viewport (would crop)", m.Cols, m.Rows, right-left, bottom-top, left, top, p.Padding, width, height))
	}
}

func (r *Renderer) assetHashes() AssetHashes {
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	hashes := AssetHashes{PlayerJS: sum(r.config.Assets.PlayerJS), PlayerCSS: sum(r.config.Assets.PlayerCSS), Font: sum(r.config.Assets.Font)}
	if len(r.config.Assets.BoxFont) != 0 {
		hashes.BoxFont = sum(r.config.Assets.BoxFont)
	}
	return hashes
}

func checkGeometries(geometries []Geometry) error {
	if len(geometries) == 0 || len(geometries) > MaxGeometries {
		return fmt.Errorf("video preflight needs 1..%d geometries", MaxGeometries)
	}
	for _, g := range geometries {
		if !geometryAllowed(g.Cols, g.Rows) {
			return errors.New("video terminal geometry exceeds renderer policy")
		}
	}
	return nil
}

// Preflight measures each geometry in the renderer page without an encoder. A
// refusal returns the measurements together with a *PreflightError.
func (r *Renderer) Preflight(parent context.Context, req PreflightRequest) (result PreflightResult, err error) {
	pres, err := req.Presentation.normalize()
	if err != nil {
		return PreflightResult{}, err
	}
	if !outputAllowed(req.Width, req.Height) {
		return PreflightResult{}, errors.New("unsupported video geometry")
	}
	if err = checkGeometries(req.Geometries); err != nil {
		return PreflightResult{}, err
	}
	select {
	case permit <- struct{}{}:
		defer func() { <-permit }()
	default:
		return PreflightResult{}, ErrBusy
	}
	ctx, cancel := context.WithTimeout(parent, MaxJobAge)
	defer cancel()
	dir, err := os.MkdirTemp("", ".areel-preflight-")
	if err != nil {
		return PreflightResult{}, err
	}
	defer os.RemoveAll(dir)
	p, err := r.openPage(ctx, cancel, dir, req.Width, req.Height, "", pres, req.Geometries)
	if err != nil {
		return PreflightResult{}, err
	}
	defer func() {
		if monitorErr := p.close(); monitorErr != nil {
			result, err = PreflightResult{}, monitorErr
		}
	}()
	return p.preflight(req.Geometries)
}

// rendererPage owns one private browser/profile/server with resource watchers.
type rendererPage struct {
	browser    context.Context
	width      int
	height     int
	pres       Presentation
	assets     AssetHashes
	encoderPID atomic.Int64
	peak       int64

	stopMonitor context.CancelFunc
	monitored   chan struct{}
	monitorErr  error
	finishOnce  sync.Once
	closers     []func()
}

// finish stops the resource watcher once and returns its error.
func (p *rendererPage) finish() error {
	p.finishOnce.Do(func() {
		if p.stopMonitor != nil {
			p.stopMonitor()
			<-p.monitored
		}
	})
	return p.monitorErr
}

// close stops the watcher, then the browser, allocator and server.
func (p *rendererPage) close() error {
	err := p.finish()
	for i := len(p.closers) - 1; i >= 0; i-- {
		p.closers[i]()
	}
	p.closers = nil
	return err
}

func preflightCast(g Geometry) string {
	return `{"version":3,"term":{"cols":` + strconv.Itoa(g.Cols) + `,"rows":` + strconv.Itoa(g.Rows) + `}}` + "\n" + `[0,"o","M"]` + "\n"
}

// openPage starts the loopback server and private browser, starts the
// resource watcher (which cancels ctx on breach) and loads the renderer page.
// On error everything it started is stopped.
func (r *Renderer) openPage(ctx context.Context, cancel context.CancelFunc, dir string, width, height int, castPath string, pres Presentation, geometries []Geometry) (p *rendererPage, err error) {
	html, err := framesHTML(pres)
	if err != nil {
		return nil, err
	}
	synthetic := map[string]string{}
	for i, g := range geometries {
		synthetic["/preflight/"+strconv.Itoa(i)+".cast"] = preflightCast(g)
	}
	return r.startPage(ctx, cancel, dir, width, height, castPath, pres, html, `typeof load === 'function' && typeof AsciinemaPlayer === 'object'`, func(path string) (string, string, bool) {
		body, ok := synthetic[path]
		return body, "", ok
	})
}

// startPage is openPage for any renderer page: html is served at "/",
// extra(path) may answer further GET paths with an asciicast body or a
// private file of the job (after the same Host/loopback/method checks), and
// ready is polled after navigation.
func (r *Renderer) startPage(ctx context.Context, cancel context.CancelFunc, dir string, width, height int, castPath string, pres Presentation, html, ready string, extra func(path string) (body, file string, ok bool)) (p *rendererPage, err error) {
	p = &rendererPage{width: width, height: height, pres: pres, assets: r.assetHashes()}
	defer func() {
		if err != nil {
			p.close()
			p = nil
		}
	}()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return p, err
	}
	host := listener.Addr().String()
	origin := "http://" + host
	legacy := r.handler(castPath, host)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		body, file, isSynthetic := extra(q.URL.Path)
		if q.URL.Path != "/" && !isSynthetic {
			legacy.ServeHTTP(w, q)
			return
		}
		peer, _, _ := net.SplitHostPort(q.RemoteAddr)
		if q.Host != host || !net.ParseIP(peer).IsLoopback() || q.Method != "GET" || q.URL.RawQuery != "" || q.ContentLength != 0 {
			http.Error(w, "refused", 403)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self' 'unsafe-inline' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; font-src 'self'; connect-src 'self'; img-src 'self'")
		if isSynthetic {
			w.Header().Set("Content-Type", "application/x-asciicast")
			if t := imageTypes[strings.ToLower(filepath.Ext(q.URL.Path))]; t != "" && file != "" {
				// A composition's image slot (a private job file).
				w.Header().Set("Content-Type", t)
			}
			if file == "" {
				io.WriteString(w, body)
				return
			}
			f, err := os.Open(file)
			if err != nil {
				http.Error(w, "unavailable", 500)
				return
			}
			defer f.Close()
			io.Copy(w, f)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, html)
	}), ReadHeaderTimeout: 3 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	served := make(chan struct{})
	go func() { defer close(served); _ = server.Serve(listener) }()
	p.closers = append(p.closers, func() { _ = server.Close(); <-served })
	profile := filepath.Join(dir, "profile")
	options := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(r.config.Chromium), chromedp.UserDataDir(profile), chromedp.WindowSize(width, height), chromedp.Flag("host-resolver-rules", "MAP * ~NOTFOUND, EXCLUDE 127.0.0.1"), chromedp.Flag("proxy-server", origin), chromedp.Flag("proxy-bypass-list", "127.0.0.1"), chromedp.Flag("js-flags", "--max-old-space-size=512"), chromedp.Flag("disable-crash-reporter", true), chromedp.Flag("disable-breakpad", true), crashDumpEnv(dir), chromedp.ModifyCmdFunc(ownCommand))
	if r.config.NoSandbox {
		options = append(options, chromedp.NoSandbox)
	}
	allocated, stopAllocator := chromedp.NewExecAllocator(ctx, options...)
	p.closers = append(p.closers, stopAllocator)
	browser, stopBrowser := chromedp.NewContext(allocated)
	p.closers = append(p.closers, stopBrowser)
	if err = chromedp.Run(browser); err != nil {
		return p, err
	}
	p.browser = browser
	if processHook != nil {
		if proc := chromedp.FromContext(browser).Browser.Process(); proc != nil {
			processHook(proc.Pid)
		}
	}
	monitorCtx, stopMonitor := context.WithCancel(browser)
	p.stopMonitor = stopMonitor
	p.monitored = make(chan struct{})
	go func() {
		defer close(p.monitored)
		p.monitorErr = watchResources(monitorCtx, browser, dir, &p.encoderPID, &p.peak)
		if p.monitorErr != nil {
			cancel()
		}
	}()
	if err = chromedp.Run(browser, emulation.SetDeviceMetricsOverride(int64(width), int64(height), 1, false), chromedp.Navigate(origin+"/"), chromedp.Poll(ready, nil)); err != nil {
		return p, fmt.Errorf("video renderer initialization: %w", err)
	}
	return p, nil
}

func awaitPromise(q *runtime.EvaluateParams) *runtime.EvaluateParams { return q.WithAwaitPromise(true) }

// load replaces the page's player with one for url (a fixed local path).
func (p *rendererPage) load(url string) error {
	if err := chromedp.Run(p.browser, chromedp.Evaluate(`load(`+strconv.Quote(url)+`)`, nil, awaitPromise)); err != nil {
		return errors.New("official player could not load supplied cast")
	}
	return nil
}

// preflight measures every geometry with a synthetic cast and refuses before
// any encoder exists when one is illegible, cropped or not in the export font.
func (p *rendererPage) preflight(geometries []Geometry) (PreflightResult, error) {
	result := PreflightResult{Width: p.width, Height: p.height, Presentation: p.pres, Assets: p.assets}
	var reasons []string
	for i, g := range geometries {
		if err := p.load("/preflight/" + strconv.Itoa(i) + ".cast"); err != nil {
			return PreflightResult{}, fmt.Errorf("video preflight %dx%d: %w", g.Cols, g.Rows, err)
		}
		var m Measurement
		if err := chromedp.Run(p.browser, chromedp.Evaluate(`measure()`, &m)); err != nil {
			return PreflightResult{}, fmt.Errorf("video preflight %dx%d: %w", g.Cols, g.Rows, err)
		}
		measured := m.Geometry
		m.Geometry = g
		judge(&m, p.width, p.height, p.pres)
		if measured != g {
			m.Refusals = append([]string{fmt.Sprintf("renderer measured %dx%d instead of %dx%d", measured.Cols, measured.Rows, g.Cols, g.Rows)}, m.Refusals...)
		}
		reasons = append(reasons, m.Refusals...)
		result.Geometries = append(result.Geometries, m)
	}
	if len(reasons) > 0 {
		return result, &PreflightError{Reasons: reasons, Width: p.width, Height: p.height, MinFontPx: p.pres.MinFontPx, Measurements: result.Geometries}
	}
	return result, nil
}
