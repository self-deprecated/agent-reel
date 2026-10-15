package videoexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/self-deprecated/agent-reel/internal/termtheme"
)

// Rate is a reduced output frame rate of Num/Den frames per second.
type Rate struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// NewRate reduces num/den and admits only integer 1..60 and 24000/1001,
// 30000/1001 and 60000/1001 (the editmap rate set).
func NewRate(num, den int64) (Rate, error) {
	if num <= 0 || den <= 0 {
		return Rate{}, fmt.Errorf("video rate %d/%d must be positive", num, den)
	}
	g := new(big.Int).GCD(nil, nil, big.NewInt(num), big.NewInt(den)).Int64()
	n, d := num/g, den/g
	switch {
	case d == 1 && n >= 1 && n <= 60:
	case d == 1001 && (n == 24000 || n == 30000 || n == 60000):
	default:
		return Rate{}, fmt.Errorf("video rate %d/%d is not allowed; use an integer 1..60 or 24000/1001, 30000/1001, 60000/1001", num, den)
	}
	return Rate{n, d}, nil
}

func (r Rate) String() string { return itoa64(r.Num) + "/" + itoa64(r.Den) }

// frameTime is the exact output instant k·Den/Num of frame k.
func (r Rate) frameTime(k int) *big.Rat {
	return new(big.Rat).SetFrac(big.NewInt(int64(k)*r.Den), big.NewInt(r.Num))
}

// seekSeconds formats frame k's exact instant as the player seek argument. The
// value is decided by integers; only the final decimal rendering uses float64.
func (r Rate) seekSeconds(k int) string {
	f, _ := r.frameTime(k).Float64()
	return strconv.FormatFloat(f, 'f', -1, 64)
}

const (
	FramingFit    = "fit"
	FramingFont   = "font"
	CursorSteady  = "steady"
	CursorHidden  = "hidden"
	DefaultMinPx  = 10
	MaxPaddingPx  = 128
	minFontPx     = 6
	maxMinFontPx  = 48
	maxFramingPx  = 96
	advanceTarget = 0.60 // DejaVu Sans Mono advance, em
	advanceSlack  = 0.02
)

// Presentation controls how the official player frames the terminal. The
// renderer page only receives these validated numbers and enumerations.
type Presentation struct {
	// Framing is "fit" (player fit:'both') or "font" (fit:false with FontPx).
	Framing string `json:"framing"`
	FontPx  int    `json:"fontPx,omitempty"`
	// Padding is CSS pixels inside the viewport on every side, 0..128.
	Padding int `json:"paddingPx"`
	// Cursor is "steady" or "hidden"; blinking is never offered.
	Cursor    string `json:"cursor"`
	MinFontPx int    `json:"minFontPx"`
	// Theme, when set, is the terminal colour theme the player paints with
	// (default colours and the 16-colour palette; explicit 256-colour and
	// true-colour cells stay as recorded). Nil keeps the player's default.
	// Every value is re-validated as #rrggbb before it reaches the page.
	Theme *termtheme.Theme `json:"theme,omitempty"`
}

// normalize fills defaults and validates the presentation.
func (p Presentation) normalize() (Presentation, error) {
	if p.Framing == "" {
		p.Framing = FramingFit
	}
	if p.Cursor == "" {
		p.Cursor = CursorSteady
	}
	if p.MinFontPx == 0 {
		p.MinFontPx = DefaultMinPx
	}
	switch p.Framing {
	case FramingFit:
		if p.FontPx != 0 {
			return p, errors.New("video fontPx is only allowed with font framing")
		}
	case FramingFont:
		if p.FontPx < minFontPx || p.FontPx > maxFramingPx {
			return p, fmt.Errorf("video font framing needs fontPx %d..%d", minFontPx, maxFramingPx)
		}
	default:
		return p, errors.New("video framing must be fit or font")
	}
	if p.Cursor != CursorSteady && p.Cursor != CursorHidden {
		return p, errors.New("video cursor must be steady or hidden")
	}
	if p.Padding < 0 || p.Padding > MaxPaddingPx {
		return p, fmt.Errorf("video padding must be 0..%d px", MaxPaddingPx)
	}
	if p.MinFontPx < minFontPx || p.MinFontPx > maxMinFontPx {
		return p, fmt.Errorf("video minimum font must be %d..%d px", minFontPx, maxMinFontPx)
	}
	if p.Theme != nil {
		if err := p.Theme.Validate(); err != nil {
			return p, fmt.Errorf("video %w", err)
		}
		theme := *p.Theme
		p.Theme = &theme
	}
	return p, nil
}

// FrameRequest renders exactly Frames frames of a caller-verified cast whose
// playback clock is the output clock: frame k shows the cast at k·Den/Num.
type FrameRequest struct {
	// Cast is a caller-verified immutable local input positioned at its header.
	Cast io.Reader
	// Output must be absolute, clean and not yet exist; its parent is trusted.
	Output        string
	Rate          Rate
	Frames        int
	Width, Height int
	Codec         string // h264 (.mp4) or prores (.mov)
	Presentation  Presentation
	// Chapters, when present, are contiguous over [0, Frames).
	Chapters []Chapter
	// Progress, when set, is called after each frame is handed to the encoder
	// with the frames done so far and the total. It must not block.
	Progress func(done, total int)
	// MaxAge is the render deadline; zero means MaxJobAge. It is capped at
	// MaxJobAgeLimit.
	MaxAge time.Duration
}

// ProbedStream is the ffprobe evidence for the encoded video stream.
type ProbedStream struct {
	Codec        string `json:"codec"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	RFrameRate   string `json:"rFrameRate"`
	DecodedFrame int    `json:"decodedFrames"`
	Duration     string `json:"duration"`
}

type FrameResult struct {
	Codec        string       `json:"codec"`
	Rate         Rate         `json:"rate"`
	Width        int          `json:"width"`
	Height       int          `json:"height"`
	Frames       int          `json:"frames"`
	Presentation Presentation `json:"presentation"`
	// VideoDuration is the exact Frames·Den/Num seconds as "n" or "n/d".
	VideoDuration string          `json:"videoDuration"`
	VideoSeconds  float64         `json:"videoSeconds"`
	SourceSHA256  string          `json:"sourceSHA256"`
	SourceBytes   int64           `json:"sourceBytes"`
	Chapters      []Chapter       `json:"chapters,omitempty"`
	Probe         ProbedStream    `json:"probe"`
	ProbeChapters []ProbedChapter `json:"probedChapters,omitempty"`
	Preflight     PreflightResult `json:"preflight"`
	// Timing is observational only.
	ElapsedSeconds  float64 `json:"elapsedSeconds"`
	FramesPerSecond float64 `json:"renderedFramesPerSecond"`
	PeakRSSBytes    int64   `json:"observedPeakRSSBytes"`
}

// planFrames validates everything that needs no file or process.
func planFrames(req FrameRequest) (FrameResult, error) {
	r := FrameResult{Codec: req.Codec, Width: req.Width, Height: req.Height, Frames: req.Frames}
	if req.Codec != "h264" && req.Codec != "prores" {
		return r, errors.New("unsupported video codec")
	}
	if !outputAllowed(req.Width, req.Height) {
		return r, errors.New("unsupported video geometry")
	}
	rate, err := NewRate(req.Rate.Num, req.Rate.Den)
	if err != nil {
		return r, err
	}
	r.Rate = rate
	if req.Frames < 1 || req.Frames > MaxFrames {
		return r, fmt.Errorf("video needs 1..%d frames", MaxFrames)
	}
	if r.Presentation, err = req.Presentation.normalize(); err != nil {
		return r, err
	}
	if r.Chapters, err = checkChapters(req.Chapters, req.Frames); err != nil {
		return r, err
	}
	d := rate.frameTime(req.Frames)
	r.VideoDuration = formatRat(d)
	r.VideoSeconds, _ = d.Float64()
	return r, nil
}

func outputAllowed(width, height int) bool {
	return width >= 160 && width <= 1920 && height >= 120 && height <= 1080 && width%2 == 0 && height%2 == 0
}

// Test hooks; nil in production.
var (
	afterFrameHook func(frame int)
	processHook    func(pid int)
)

// RenderFrames copies the cast privately, preflights every distinct geometry
// in the real renderer page, then paints frame k at exactly k·Den/Num through
// the seek/paint barrier, encodes at -framerate Num/Den with chapters muxed in
// the same encode, verifies rate/frames/duration/codec/geometry/chapters with
// ffprobe and links the artifact into Output. Failures leave no output/staging.
func (r *Renderer) RenderFrames(parent context.Context, req FrameRequest) (result FrameResult, err error) {
	result, err = planFrames(req)
	if err != nil {
		return FrameResult{}, err
	}
	if req.Cast == nil || !filepath.IsAbs(req.Output) || filepath.Clean(req.Output) != req.Output {
		return FrameResult{}, errors.New("explicit immutable cast and absolute clean output path required")
	}
	if _, err := os.Lstat(req.Output); !errors.Is(err, os.ErrNotExist) {
		return FrameResult{}, errors.New("output already exists or is inaccessible")
	}
	select {
	case permit <- struct{}{}:
		defer func() { <-permit }()
	default:
		return FrameResult{}, ErrBusy
	}
	age := req.MaxAge
	if age <= 0 {
		age = MaxJobAge
	}
	ctx, cancel := context.WithTimeout(parent, min(age, MaxJobAgeLimit))
	defer cancel()
	start := time.Now()
	dir, err := os.MkdirTemp(filepath.Dir(req.Output), ".areel-video-")
	if err != nil {
		return FrameResult{}, err
	}
	defer os.RemoveAll(dir)
	castPath := filepath.Join(dir, "source.cast")
	f, err := os.OpenFile(castPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return FrameResult{}, err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, hash), io.LimitReader(contextReader{ctx, req.Cast}, MaxCastBytes+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		return FrameResult{}, errors.Join(copyErr, closeErr)
	}
	if n == 0 || n > MaxCastBytes {
		return FrameResult{}, errors.New("video cast input exceeds policy or is empty")
	}
	geometries, err := castGeometries(ctx, castPath)
	if err != nil {
		return FrameResult{}, err
	}
	result.SourceBytes = n
	result.SourceSHA256 = hex.EncodeToString(hash.Sum(nil))
	metadata := ""
	if len(result.Chapters) > 0 {
		metadata = filepath.Join(dir, "chapters.ffmeta")
		if err = writeMetadata(metadata, result.Rate, result.Chapters); err != nil {
			return FrameResult{}, err
		}
	}

	p, err := r.openPage(ctx, cancel, dir, req.Width, req.Height, castPath, result.Presentation, geometries)
	if err != nil {
		return FrameResult{}, err
	}
	defer func() {
		if monitorErr := p.close(); monitorErr != nil {
			result, err = FrameResult{}, monitorErr
		}
	}()
	result.Preflight, err = p.preflight(geometries)
	if err != nil {
		return FrameResult{}, err
	}
	if err = p.load("/source.cast"); err != nil {
		return FrameResult{}, err
	}
	var duration float64
	if err = chromedp.Run(p.browser, chromedp.Evaluate(`player.getDuration()`, &duration)); err != nil {
		return FrameResult{}, err
	}
	// The cast must cover the whole output clock; the final frame interval
	// ends at Frames·Den/Num.
	if result.VideoSeconds > duration+0.000001 {
		return FrameResult{}, errors.New("requested video extends beyond supplied cast")
	}

	staged := filepath.Join(dir, "video")
	encoder := exec.CommandContext(ctx, r.config.FFmpeg, encoderArgs(result.Rate.String(), result.Frames, req.Codec, metadata, staged)...)
	ownCommand(encoder)
	input, err := encoder.StdinPipe()
	if err != nil {
		return FrameResult{}, err
	}
	stderr := &boundedLog{}
	encoder.Stderr = stderr
	if err = encoder.Start(); err != nil {
		return FrameResult{}, err
	}
	waited := false
	defer func() {
		_ = input.Close()
		if !waited {
			cancel()
			_ = encoder.Wait()
		}
	}()
	p.encoderPID.Store(int64(encoder.Process.Pid))
	if processHook != nil {
		processHook(encoder.Process.Pid)
	}
	for k := 0; k < result.Frames; k++ {
		var png []byte
		err = chromedp.Run(p.browser, chromedp.Evaluate(`paintAt(`+result.Rate.seekSeconds(k)+`)`, nil, func(q *runtime.EvaluateParams) *runtime.EvaluateParams { return q.WithAwaitPromise(true) }), chromedp.ActionFunc(func(ctx context.Context) error {
			var e error
			png, e = page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).WithClip(&page.Viewport{X: 0, Y: 0, Width: float64(req.Width), Height: float64(req.Height), Scale: 1}).Do(ctx)
			return e
		}))
		if err != nil {
			return FrameResult{}, fmt.Errorf("video painted frame %d: %w", k, err)
		}
		if len(png) > 16<<20 {
			return FrameResult{}, errors.New("video screenshot exceeds frame bound")
		}
		if _, err = input.Write(png); err != nil {
			return FrameResult{}, fmt.Errorf("video encoder pipe: %w", err)
		}
		if afterFrameHook != nil {
			afterFrameHook(k)
		}
		if req.Progress != nil {
			req.Progress(k+1, result.Frames)
		}
	}
	_ = input.Close()
	err = encoder.Wait()
	waited = true
	monitorErr := p.finish()
	result.PeakRSSBytes = p.peak
	if monitorErr != nil {
		return FrameResult{}, monitorErr
	}
	if err != nil {
		return FrameResult{}, fmt.Errorf("video encoder: %w (%s)", err, stderr.String())
	}
	info, err := os.Stat(staged)
	if err != nil {
		return FrameResult{}, err
	}
	if info.Size() == 0 || info.Size() >= MaxOutputBytes {
		return FrameResult{}, errors.New("video output limit reached")
	}
	if result.Probe, err = r.verifyFrames(ctx, staged, result); err != nil {
		return FrameResult{}, err
	}
	if result.ProbeChapters, err = r.verifyChapters(ctx, staged, result.Rate, result.Chapters); err != nil {
		return FrameResult{}, err
	}
	if err = ctx.Err(); err != nil {
		return FrameResult{}, err
	}
	file, err := os.OpenFile(staged, os.O_RDWR, 0600)
	if err != nil {
		return FrameResult{}, err
	}
	if err = file.Chmod(0600); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return FrameResult{}, err
	}
	result.ElapsedSeconds = time.Since(start).Seconds()
	result.FramesPerSecond = float64(result.Frames) / result.ElapsedSeconds
	if err = os.Link(staged, req.Output); err != nil {
		return FrameResult{}, fmt.Errorf("publish video: %w", err)
	}
	return result, nil
}

type probeStreams struct {
	Streams []struct {
		Codec      string `json:"codec_name"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		RFrameRate string `json:"r_frame_rate"`
		Frames     string `json:"nb_read_frames"`
		Duration   string `json:"duration"`
	} `json:"streams"`
}

// matchStream checks the probed video stream against want using exact
// rationals: r_frame_rate reduces to Rate, decoded frames equal Frames and the
// duration is within one frame period of Frames·Den/Num.
func matchStream(output []byte, want FrameResult) (ProbedStream, error) {
	var probe probeStreams
	if err := json.Unmarshal(output, &probe); err != nil {
		return ProbedStream{}, fmt.Errorf("video verification: %w", err)
	}
	if len(probe.Streams) != 1 {
		return ProbedStream{}, errors.New("video missing verified stream")
	}
	s := probe.Streams[0]
	got := ProbedStream{Codec: s.Codec, Width: s.Width, Height: s.Height, RFrameRate: s.RFrameRate, Duration: s.Duration}
	frames, err := strconv.Atoi(s.Frames)
	if err != nil {
		return got, errors.New("video decoded frame count unavailable")
	}
	got.DecodedFrame = frames
	rate, ok := new(big.Rat).SetString(s.RFrameRate)
	if !ok || rate.Cmp(new(big.Rat).SetFrac64(want.Rate.Num, want.Rate.Den)) != 0 {
		return got, fmt.Errorf("video frame rate %s, want %s", s.RFrameRate, want.Rate)
	}
	duration, ok := new(big.Rat).SetString(s.Duration)
	if !ok {
		return got, errors.New("video duration unavailable")
	}
	diff := new(big.Rat).Sub(duration, want.Rate.frameTime(want.Frames))
	if diff.Sign() < 0 {
		diff.Neg(diff)
	}
	if s.Codec != want.Codec || frames != want.Frames || s.Width != want.Width || s.Height != want.Height || diff.Cmp(want.Rate.frameTime(1)) > 0 {
		return got, fmt.Errorf("video frame count, duration, geometry or codec mismatch: %+v", got)
	}
	return got, nil
}

func (r *Renderer) verifyFrames(ctx context.Context, path string, want FrameResult) (ProbedStream, error) {
	cmd := exec.CommandContext(ctx, r.config.FFprobe, "-v", "error", "-count_frames", "-select_streams", "v:0", "-show_entries", "stream=codec_name,width,height,r_frame_rate,nb_read_frames,duration", "-of", "json", path)
	ownCommand(cmd)
	output := &cappedBuffer{limit: 64 << 10}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return ProbedStream{}, fmt.Errorf("video verification: %w", err)
	}
	if output.overflow {
		return ProbedStream{}, errors.New("video verification output exceeds bound")
	}
	return matchStream(output.data, want)
}

// pageConfig is the only data injected into the renderer page: validated
// enumerations and integers, JSON-encoded.
type pageConfig struct {
	Framing string           `json:"framing"`
	FontPx  int              `json:"fontPx"`
	Padding int              `json:"padding"`
	Cursor  string           `json:"cursor"`
	Theme   *termtheme.Theme `json:"theme,omitempty"`
}

func framesHTML(p Presentation) (string, error) {
	p, err := p.normalize()
	if err != nil {
		return "", err
	}
	// json.Marshal escapes <, > and & too; the theme holds validated hex only.
	config, err := json.Marshal(pageConfig{p.Framing, p.FontPx, p.Padding, p.Cursor, p.Theme})
	if err != nil {
		return "", err
	}
	return strings.Replace(framesPage, "/*CONFIG*/null", string(config), 1), nil
}

// framesPage mirrors rendererHTML's paint barrier. It loads the export font
// before the player measures cell metrics, can load several synthetic
// preflight casts and then the supplied cast, and reports layout measurements.
//
// text-rendering:geometricPrecision makes Chromium lay glyphs out unhinted with
// subpixel positioning whatever the host fontconfig hinting says. The player's
// fit:'both' measures an 80-column terminal once at 15px and scales that size
// linearly; under full hinting (fontconfig's built-in default, e.g. in the
// Nix build sandbox) advances snap to whole pixels per size (9px at 15px, 8px
// at ~12.9px), so the scaled terminal no longer fits and its last column is
// cropped, while a hintslight host (typical desktop) scales linearly. Pinning
// linear advances makes fit exact and renders the same everywhere.
const framesPage = `<!doctype html><meta charset="utf-8"><link rel="stylesheet" href="/player.css"><style>
@font-face{font-family:ExportMono;src:url('/DejaVuSansMono.ttf')}@font-face{font-family:ExportBox;src:url('/AgentReelBox.ttf');unicode-range:U+2500-257F}html,body{width:100%;height:100%;margin:0;overflow:hidden;background:#121314;text-rendering:geometricPrecision}#terminal{box-sizing:border-box;width:100%;height:100%;margin:0;overflow:hidden}*{animation:none!important;transition:none!important}
.ap-overlay,.ap-control-bar{display:none!important}
.ap-player.asciinema-player-theme-agentreel{--term-color-foreground:var(--reel-term-fg);--term-color-background:var(--reel-term-bg);--term-color-0:var(--reel-term-0);--term-color-1:var(--reel-term-1);--term-color-2:var(--reel-term-2);--term-color-3:var(--reel-term-3);--term-color-4:var(--reel-term-4);--term-color-5:var(--reel-term-5);--term-color-6:var(--reel-term-6);--term-color-7:var(--reel-term-7);--term-color-8:var(--reel-term-8);--term-color-9:var(--reel-term-9);--term-color-10:var(--reel-term-10);--term-color-11:var(--reel-term-11);--term-color-12:var(--reel-term-12);--term-color-13:var(--reel-term-13);--term-color-14:var(--reel-term-14);--term-color-15:var(--reel-term-15)}
</style><div id="terminal"></div><script src="/player.js"></script><script>
const config=/*CONFIG*/null;
const container=document.querySelector('#terminal');
container.style.padding=config.padding+'px';
// A theme paints the default colours and 16-colour palette (and the frame
// around the terminal); values are validated #rrggbb set through the CSSOM.
if(config.theme){
 const t=config.theme;
 document.documentElement.style.setProperty('--reel-term-fg',t.fg);
 document.documentElement.style.setProperty('--reel-term-bg',t.bg);
 t.palette.forEach((c,i)=>document.documentElement.style.setProperty('--reel-term-'+i,c));
 document.body.style.background=t.bg;
 document.documentElement.style.background=t.bg;
}
let player;
function options(){
 const o={autoPlay:false,preload:true,controls:false,cursorMode:config.cursor,terminalFontFamily:'ExportBox, ExportMono',idleTimeLimit:Infinity};
 if(config.theme)o.theme='agentreel';
 if(config.framing==='font'){o.fit=false;o.terminalFontSize=config.fontPx+'px'}else{o.fit='both'}
 return o;
}
const frames=()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)));
async function load(url){
 if(player){player.dispose();player=undefined}
 container.replaceChildren();
 window.exportError=undefined;
 await document.fonts.load('15px ExportMono');
 // The box-drawing face is optional (absent when Assets.BoxFont is empty).
 await document.fonts.load('15px ExportBox','\u2502').catch(()=>{});
 const p=AsciinemaPlayer.create(url,container,options());
 await new Promise((resolve,reject)=>{p.addEventListener('ready',resolve);p.addEventListener('error',()=>{window.exportError='load failed';reject(Error('load failed'))})});
 await document.fonts.ready;
 player=p;
 await player.pause();
 await player.seek(0);
 // The player observes its container with a 10 ms debounce.
 await new Promise(resolve=>setTimeout(resolve,50));
 await frames();
}
function measure(){
 const term=container.querySelector('.ap-term');
 if(!term)throw Error('terminal not mounted');
 const style=getComputedStyle(term);
 const box=term.getBoundingClientRect();
 const cols=Number(style.getPropertyValue('--term-cols'));
 const rows=Number(style.getPropertyValue('--term-rows'));
 const fontPx=parseFloat(style.fontSize);
 const border=parseFloat(style.borderLeftWidth);
 if(style.borderRightWidth!==style.borderLeftWidth||style.borderTopWidth!==style.borderLeftWidth||style.borderBottomWidth!==style.borderLeftWidth)throw Error('asymmetric terminal border');
 const advancePx=(box.width-2*border)/cols;
 // Painted region: the viewport clipped by every overflow-clipping ancestor's
 // padding box (the player itself clips at its box).
 const clip={left:0,top:0,right:window.innerWidth,bottom:window.innerHeight};
 for(let el=term.parentElement;el;el=el.parentElement){
  const s=getComputedStyle(el);
  if(s.overflowX==='visible'&&s.overflowY==='visible')continue;
  const r=el.getBoundingClientRect();
  clip.left=Math.max(clip.left,r.left+parseFloat(s.borderLeftWidth));
  clip.top=Math.max(clip.top,r.top+parseFloat(s.borderTopWidth));
  clip.right=Math.min(clip.right,r.right-parseFloat(s.borderRightWidth));
  clip.bottom=Math.min(clip.bottom,r.bottom-parseFloat(s.borderBottomWidth));
 }
 const face=[...document.fonts].some(f=>f.family.replace(/["']/g,'')==='ExportMono'&&f.status==='loaded');
 return {cols,rows,left:box.left,top:box.top,width:box.width,height:box.height,fontPx,advancePx,advanceEm:advancePx/fontPx,borderPx:border,clip,fontLoaded:face&&document.fonts.check('12px ExportMono'),viewportWidth:window.innerWidth,viewportHeight:window.innerHeight};
}
async function paintAt(seconds){
 await player.pause();
 await player.seek(seconds);
 await document.fonts.ready;
 // seek resolves after terminal feed; two animation frames let reactive DOM
 // updates reach layout and a rendering opportunity before CDP's compositor capture.
 await frames();
 if(window.exportError)throw Error('player failed');
 if(Math.abs(player.getCurrentTime()-seconds)>0.002)throw Error('seek did not settle');
}
</script>`
