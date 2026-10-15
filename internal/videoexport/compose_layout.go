package videoexport

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

// Layout chunks (EDL.md, "Layouts and links"): up to four slots on the one
// compose page, each in its own absolutely positioned container at its
// output-pixel rectangle. A terminal slot is a fresh official player with its
// own theme (the slot container carries the theme's colour variables) fitted
// to the slot and preflighted against the slot's size; a cells view zooms it
// with a CSS transform (Chromium re-rasterises the text at the scaled size),
// clipped by the slot. An image slot is an <img> (object-fit contain on the
// theme's background) showing one file, or the chunk's frame k of a video
// decoded at the output rate. The capture is the whole output at scale 1.

// MaxComposeSlots bounds the slots of one chunk.
const MaxComposeSlots = 4

// ClipChunk is a clip chunk: one full-output terminal slot showing cast with
// presentation p (also the chunk's) and an optional cells view.
func ClipChunk(cast io.Reader, frames int, p Presentation, view *ViewCells) ComposeChunk {
	return ComposeChunk{Frames: frames, Presentation: p, Slots: []ComposeSlot{{Cast: cast, Presentation: p, View: view}}}
}

// imageTypes are the served image slot types by extension.
var imageTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif"}

// planSlots validates chunk i's slots.
func planSlots(req ComposeRequest, i int) error {
	c := req.Chunks[i]
	if len(c.Slots) > MaxComposeSlots {
		return fmt.Errorf("chunk %d: at most %d slots", i+1, MaxComposeSlots)
	}
	for s, slot := range c.Slots {
		where := fmt.Sprintf("chunk %d slot %d", i+1, s+1)
		if (slot.Cast == nil) == (len(slot.Images) == 0) {
			return fmt.Errorf("%s: a slot is exactly one of a cast or images", where)
		}
		if len(slot.Images) > 0 {
			if len(slot.Images) != 1 && len(slot.Images) != c.Frames {
				return fmt.Errorf("%s: an image slot has one image or one per frame", where)
			}
			if slot.View != nil {
				return fmt.Errorf("%s: an image slot has no cells to view", where)
			}
			for _, f := range slot.Images {
				if !filepath.IsAbs(f) || filepath.Clean(f) != f || imageTypes[strings.ToLower(filepath.Ext(f))] == "" {
					return fmt.Errorf("%s: image files are absolute, clean .png, .jpg, .jpeg, .webp or .gif paths", where)
				}
			}
		}
		p, err := slot.Presentation.normalize()
		if err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if p.Framing != FramingFit || p.Theme == nil {
			return fmt.Errorf("%s: slots use fit framing and a theme", where)
		}
		if r := slot.Rect; r != nil {
			if r.W < 16 || r.H < 16 || r.X < 0 || r.Y < 0 || r.X+r.W > req.Width || r.Y+r.H > req.Height {
				return fmt.Errorf("%s: rect %d,%d %dx%d is not inside the %dx%d output", where, r.X, r.Y, r.W, r.H, req.Width, req.Height)
			}
		} else if len(c.Slots) > 1 {
			return fmt.Errorf("%s: slots of a layout need rectangles", where)
		}
	}
	return nil
}

func (j *composeJob) slotRect(slot ComposeSlot) SlotRect {
	if slot.Rect != nil {
		return *slot.Rect
	}
	return SlotRect{0, 0, j.req.Width, j.req.Height}
}

// imageURL is the page path of image k of chunk i slot s.
func (j *composeJob) imageURL(i, s, k int) string {
	f := j.req.Chunks[i].Slots[s].Images[k]
	return "/img/" + strconv.Itoa(i) + "/" + strconv.Itoa(s) + "/" + strconv.Itoa(k) + strings.ToLower(filepath.Ext(f))
}

// slotFile answers the page paths of layout casts (/slot/I/S.cast) and
// images (/img/I/S/K.ext) with the job's private files.
func (j *composeJob) slotFile(path string) (string, bool) {
	num := func(s string) (int, bool) {
		n, err := strconv.Atoi(s)
		return n, err == nil && n >= 0 && strconv.Itoa(n) == s
	}
	if rest, ok := strings.CutPrefix(path, "/slot/"); ok {
		parts := strings.Split(strings.TrimSuffix(rest, ".cast"), "/")
		if !strings.HasSuffix(rest, ".cast") || len(parts) != 2 {
			return "", false
		}
		i, ok1 := num(parts[0])
		s, ok2 := num(parts[1])
		if !ok1 || !ok2 || i >= len(j.casts) || s >= len(j.casts[i]) || j.casts[i][s] == "" {
			return "", false
		}
		return j.casts[i][s], true
	}
	if rest, ok := strings.CutPrefix(path, "/img/"); ok {
		parts := strings.Split(rest, "/")
		if len(parts) != 3 {
			return "", false
		}
		i, ok1 := num(parts[0])
		s, ok2 := num(parts[1])
		ext := filepath.Ext(parts[2])
		k, ok3 := num(strings.TrimSuffix(parts[2], ext))
		if !ok1 || !ok2 || !ok3 || i >= len(j.req.Chunks) || s >= len(j.req.Chunks[i].Slots) || k >= len(j.req.Chunks[i].Slots[s].Images) {
			return "", false
		}
		if j.imageURL(i, s, k) != path {
			return "", false
		}
		return j.req.Chunks[i].Slots[s].Images[k], true
	}
	return "", false
}

// layoutSpec is a slot as the page receives it.
type layoutSpec struct {
	Rect SlotRect      `json:"rect"`
	Kind string        `json:"kind"`
	Cfg  composeConfig `json:"cfg"`
}

// slotZoom is the transform that shows cells as large as fit in the slot:
// output = zoom·p + (x, y) for a slot point p. The visible area is centred on
// the cells and, like ZoomClip, kept inside the terminal where it fits, so a
// view at the terminal's edge shows more cells rather than the letterbox.
func slotZoom(m composeMeasurement, c ViewCells, rect SlotRect) (zoom, x, y float64, err error) {
	if c.Cols < 1 || c.Rows < 1 || c.X < 0 || c.Y < 0 || c.X+c.Cols > m.Cols || c.Y+c.Rows > m.Rows {
		return 0, 0, 0, fmt.Errorf("view cells %d,%d %dx%d exceed the %dx%d terminal", c.X, c.Y, c.Cols, c.Rows, m.Cols, m.Rows)
	}
	if !(m.AdvancePx > 0 && m.LineHeight > 0) {
		return 0, 0, 0, errors.New("view cells need a measured terminal")
	}
	left, top := m.Left+m.BorderPx, m.Top+m.BorderPx
	rx, ry := left+float64(c.X)*m.AdvancePx, top+float64(c.Y)*m.LineHeight
	rw, rh := float64(c.Cols)*m.AdvancePx, float64(c.Rows)*m.LineHeight
	zoom = math.Min(float64(rect.W)/rw, float64(rect.H)/rh)
	// The visible area in unzoomed slot pixels.
	vw, vh := float64(rect.W)/zoom, float64(rect.H)/zoom
	vx, vy := rx+rw/2-vw/2, ry+rh/2-vh/2
	if tw := float64(m.Cols) * m.AdvancePx; vw <= tw {
		vx = max(left, min(vx, left+tw-vw))
	}
	if th := float64(m.Rows) * m.LineHeight; vh <= th {
		vy = max(top, min(vy, top+th-vh))
	}
	return zoom, math.Round(-vx * zoom), math.Round(-vy * zoom), nil
}

// prepareLayout loads chunk i's slots: every terminal size of every terminal
// slot is preflighted in its slot (export font, glyph advance, nothing
// cropped, text at least MinFontPx unless a draft), then the slot's render
// input is loaded, measured and zoomed; image slots show their first image.
func (j *composeJob) prepareLayout(i int) (ComposeChunkResult, error) {
	c := j.req.Chunks[i]
	chunkPres, _ := c.Presentation.normalize()
	res := ComposeChunkResult{Layout: true, Clip: ClipRect{Width: j.req.Width, Height: j.req.Height, Scale: 1}, Dissolve: c.Dissolve, Slots: make([]ComposeSlotResult, len(c.Slots))}
	cfg, err := jsonJS(composeConfig{Theme: chunkPres.Theme, Cursor: chunkPres.Cursor, Padding: chunkPres.Padding})
	if err != nil {
		return res, err
	}
	if err := j.eval(`loadLayout(`+cfg+`)`, nil); err != nil {
		return res, fmt.Errorf("chunk %d layout: %w", i+1, err)
	}
	var reasons []string
	var measured []Measurement
	for s, slot := range c.Slots {
		rect := j.slotRect(slot)
		pres, _ := slot.Presentation.normalize()
		sr := &res.Slots[s]
		sr.Rect = &rect
		spec := layoutSpec{Rect: rect, Kind: "terminal", Cfg: composeConfig{Theme: pres.Theme, Cursor: pres.Cursor, Padding: pres.Padding}}
		load := func(url string) error {
			specJS, err := jsonJS(spec)
			if err != nil {
				return err
			}
			return j.eval(`loadSlot(`+strconv.Itoa(s)+`,`+specJS+`,`+strconv.Quote(url)+`)`, nil)
		}
		if slot.Cast == nil {
			sr.Kind, sr.Images = "image", len(slot.Images)
			spec.Kind = "image"
			if err := load(j.imageURL(i, s, 0)); err != nil {
				return res, fmt.Errorf("chunk %d slot %d: the image could not be loaded", i+1, s+1)
			}
			continue
		}
		sr.Kind = "terminal"
		judged := pres
		if j.req.Draft || slot.View != nil {
			judged.MinFontPx = 0
		}
		for _, g := range j.geoms[i][s] {
			if err := load(`/geometry/` + strconv.Itoa(g.Cols) + `x` + strconv.Itoa(g.Rows) + `.cast`); err != nil {
				return res, fmt.Errorf("chunk %d slot %d preflight %dx%d: official player could not load it", i+1, s+1, g.Cols, g.Rows)
			}
			var m Measurement
			if err := j.eval(`measureSlot(`+strconv.Itoa(s)+`)`, &m); err != nil {
				return res, fmt.Errorf("chunk %d slot %d preflight %dx%d: %w", i+1, s+1, g.Cols, g.Rows, err)
			}
			got := m.Geometry
			m.Geometry = g
			judge(&m, rect.W, rect.H, judged)
			if got != g {
				m.Refusals = append([]string{fmt.Sprintf("renderer measured %dx%d instead of %dx%d", got.Cols, got.Rows, g.Cols, g.Rows)}, m.Refusals...)
			}
			for _, reason := range m.Refusals {
				reasons = append(reasons, fmt.Sprintf("chunk %d slot %d (%dx%d px): %s", i+1, s+1, rect.W, rect.H, reason))
			}
			sr.Geometries = append(sr.Geometries, m)
			measured = append(measured, m)
		}
		if len(reasons) > 0 {
			continue
		}
		if slot.View != nil && len(j.geoms[i][s]) != 1 {
			return res, fmt.Errorf("chunk %d slot %d: a cells view needs one terminal size in the segment; it has %d", i+1, s+1, len(j.geoms[i][s]))
		}
		if err := load(`/slot/` + strconv.Itoa(i) + `/` + strconv.Itoa(s) + `.cast`); err != nil {
			return res, fmt.Errorf("chunk %d slot %d: official player could not load its render input", i+1, s+1)
		}
		var duration float64
		if err := j.eval(`slots[`+strconv.Itoa(s)+`].player.getDuration()`, &duration); err != nil {
			return res, err
		}
		if f, _ := j.result.Rate.frameTime(c.Frames).Float64(); f > duration+0.000001 {
			return res, fmt.Errorf("chunk %d slot %d: requested frames extend beyond its render input", i+1, s+1)
		}
		var m composeMeasurement
		if err := j.eval(`measureSlot(`+strconv.Itoa(s)+`)`, &m); err != nil {
			return res, fmt.Errorf("chunk %d slot %d: %w", i+1, s+1, err)
		}
		m.LineHeight = (m.Height - 2*m.BorderPx) / float64(m.Rows)
		sr.FontPx, sr.Zoom = m.FontPx, 1
		if slot.View != nil {
			zoom, x, y, err := slotZoom(m, *slot.View, rect)
			if err != nil {
				return res, fmt.Errorf("chunk %d slot %d: %w", i+1, s+1, err)
			}
			font := m.FontPx * zoom
			if !j.req.Draft && font < float64(pres.MinFontPx) {
				return res, &PreflightError{Reasons: []string{fmt.Sprintf("chunk %d slot %d: cells view text %.2f px is below the %d px minimum in its %dx%d slot (illegible); view more cells", i+1, s+1, font, pres.MinFontPx, rect.W, rect.H)}, Width: rect.W, Height: rect.H, MinFontPx: pres.MinFontPx, Measurements: sr.Geometries}
			}
			if font > 96 {
				return res, fmt.Errorf("chunk %d slot %d: cells view text %.1f px exceeds 96 px; view more cells", i+1, s+1, font)
			}
			v, _ := jsonJS(map[string]float64{"x": x, "y": y, "scale": zoom})
			if err := j.eval(`setSlotView(`+strconv.Itoa(s)+`,`+v+`)`, nil); err != nil {
				return res, err
			}
			sr.FontPx, sr.Zoom = font, zoom
		}
	}
	if len(reasons) > 0 {
		return res, &PreflightError{Reasons: reasons, Width: j.req.Width, Height: j.req.Height, MinFontPx: chunkPres.MinFontPx, Measurements: measured}
	}
	return res, j.setOverlays(i, res.Clip, ZoomMetrics{})
}

// layoutPaint is the page call that paints local frame k of layout chunk i.
func (j *composeJob) layoutPaint(i, k int, active, fade string) string {
	c := j.req.Chunks[i]
	var seeks, imgs []string
	for s, slot := range c.Slots {
		switch {
		case slot.Cast != nil:
			seeks, imgs = append(seeks, j.result.Rate.seekSeconds(k)), append(imgs, "null")
		case len(slot.Images) == 1:
			seeks, imgs = append(seeks, "null"), append(imgs, strconv.Quote(j.imageURL(i, s, 0)))
		default:
			seeks, imgs = append(seeks, "null"), append(imgs, strconv.Quote(j.imageURL(i, s, k)))
		}
	}
	return "paintLayout([" + strings.Join(seeks, ",") + "],[" + strings.Join(imgs, ",") + "]," + active + "," + fade + ")"
}

// blendPNG is the dissolve: outgoing·(den−num)/den + incoming·num/den per
// channel, rounded, as a PNG of the incoming frame's size.
func blendPNG(outgoing, incoming []byte, num, den int) ([]byte, error) {
	a, err := png.Decode(bytes.NewReader(outgoing))
	if err != nil {
		return nil, err
	}
	b, err := png.Decode(bytes.NewReader(incoming))
	if err != nil {
		return nil, err
	}
	if a.Bounds() != b.Bounds() {
		return nil, fmt.Errorf("frames of %v and %v", a.Bounds(), b.Bounds())
	}
	rgba := func(img image.Image) *image.RGBA {
		if r, ok := img.(*image.RGBA); ok && r.Rect.Min == (image.Point{}) {
			return r
		}
		r := image.NewRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
		draw.Draw(r, r.Rect, img, img.Bounds().Min, draw.Src)
		return r
	}
	pa, pb := rgba(a), rgba(b)
	out := image.NewRGBA(pb.Rect)
	wa, wb, half := den-num, num, den/2
	for n := range out.Pix {
		out.Pix[n] = uint8((int(pa.Pix[n])*wa + int(pb.Pix[n])*wb + half) / den)
	}
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// composeLayoutPage extends composePage: loadLayout(cfg) replaces the
// chunk's players with the slot container on the chunk theme's background,
// loadSlot(i, spec, url) (re)creates slot i as a fresh player for a cast url
// or an <img>, measureSlot(i) measures slot i's terminal relative to the
// slot, setSlotView(i, v) zooms it, and paintLayout seeks every terminal slot,
// shows every image slot's frame, then paints like paintFrame.
const composeLayoutPage = `<style>
#slots{position:absolute;left:0;top:0;overflow:hidden;display:none}
.slot{position:absolute;overflow:hidden;box-sizing:border-box}
.slot img{position:absolute;left:0;top:0;width:100%;height:100%;object-fit:contain}
.slot-term{position:absolute;left:0;top:0;width:100%;height:100%;box-sizing:border-box;transform-origin:0 0}
</style><script>
const slotsEl=document.createElement('div');slotsEl.id='slots';
slotsEl.style.width=config.width+'px';slotsEl.style.height=config.height+'px';
document.body.insertBefore(slotsEl,layer);
let slots=[];
function disposeSlots(){for(const s of slots){if(s&&s.player)s.player.dispose()}slots=[];slotsEl.replaceChildren();slotsEl.style.display='none';container.style.display=''}
function slotColours(el,t){
 el.style.setProperty('--reel-term-fg',t.fg);el.style.setProperty('--reel-term-bg',t.bg);
 t.palette.forEach((c,i)=>el.style.setProperty('--reel-term-'+i,c));el.style.background=t.bg;
}
async function loadLayout(cfg){
 dispose();chunk=cfg;applyTheme(cfg.theme);
 container.style.display='none';slotsEl.style.display='block';
 await document.fonts.load('15px ExportMono');
 await document.fonts.load('15px ExportBox','│').catch(()=>{});
 await document.fonts.ready;
}
async function setImage(s,url){
 if(s.url===url)return;
 s.img.src=url;await s.img.decode();s.url=url;
}
async function loadSlot(i,spec,url){
 let s=slots[i];
 if(!s){
  const el=document.createElement('div');el.className='slot';
  const r=spec.rect;el.style.left=r.x+'px';el.style.top=r.y+'px';el.style.width=r.w+'px';el.style.height=r.h+'px';
  slotsEl.appendChild(el);s=slots[i]={el};
 }
 if(s.player){s.player.dispose();s.player=undefined}
 s.el.replaceChildren();s.img=undefined;s.url=undefined;s.inner=undefined;
 slotColours(s.el,spec.cfg.theme);
 if(spec.kind==='image'){
  const img=document.createElement('img');img.alt='';s.el.appendChild(img);s.img=img;
  await setImage(s,url);await frames();return;
 }
 const inner=document.createElement('div');inner.className='slot-term';inner.style.padding=spec.cfg.padding+'px';
 s.el.appendChild(inner);s.inner=inner;
 const p=AsciinemaPlayer.create(url,inner,{autoPlay:false,preload:true,controls:false,cursorMode:spec.cfg.cursor,terminalFontFamily:'ExportBox, ExportMono',idleTimeLimit:Infinity,theme:'agentreel',fit:'both'});
 await new Promise((resolve,reject)=>{p.addEventListener('ready',resolve);p.addEventListener('error',()=>{window.exportError='load failed';reject(Error('load failed'))})});
 await document.fonts.ready;
 s.player=p;
 await p.pause();await p.seek(0);
 await new Promise(resolve=>setTimeout(resolve,50));
 await frames();
}
function measureSlot(i){
 const s=slots[i];
 const term=s.inner&&s.inner.querySelector('.ap-term');
 if(!term)throw Error('terminal not mounted');
 const base=s.el.getBoundingClientRect();
 const style=getComputedStyle(term);
 const box=term.getBoundingClientRect();
 const cols=Number(style.getPropertyValue('--term-cols'));
 const rows=Number(style.getPropertyValue('--term-rows'));
 const fontPx=parseFloat(style.fontSize);
 const border=parseFloat(style.borderLeftWidth);
 if(style.borderRightWidth!==style.borderLeftWidth||style.borderTopWidth!==style.borderLeftWidth||style.borderBottomWidth!==style.borderLeftWidth)throw Error('asymmetric terminal border');
 const advancePx=(box.width-2*border)/cols;
 const clip={left:base.left,top:base.top,right:base.right,bottom:base.bottom};
 for(let el=term.parentElement;el&&el!==s.el;el=el.parentElement){
  const cs=getComputedStyle(el);
  if(cs.overflowX==='visible'&&cs.overflowY==='visible')continue;
  const r=el.getBoundingClientRect();
  clip.left=Math.max(clip.left,r.left+parseFloat(cs.borderLeftWidth));
  clip.top=Math.max(clip.top,r.top+parseFloat(cs.borderTopWidth));
  clip.right=Math.min(clip.right,r.right-parseFloat(cs.borderRightWidth));
  clip.bottom=Math.min(clip.bottom,r.bottom-parseFloat(cs.borderBottomWidth));
 }
 const face=[...document.fonts].some(f=>f.family.replace(/["']/g,'')==='ExportMono'&&f.status==='loaded');
 return {cols,rows,left:box.left-base.left,top:box.top-base.top,width:box.width,height:box.height,fontPx,advancePx,advanceEm:advancePx/fontPx,borderPx:border,
  clip:{left:clip.left-base.left,top:clip.top-base.top,right:clip.right-base.left,bottom:clip.bottom-base.top},
  fontLoaded:face&&document.fonts.check('12px ExportMono'),viewportWidth:base.width,viewportHeight:base.height};
}
function setSlotView(i,v){slots[i].inner.style.transform='translate('+v.x+'px,'+v.y+'px) scale('+v.scale+')'}
async function paintLayout(seeks,imgs,active,f){
 for(let i=0;i<slots.length;i++){
  const s=slots[i];
  if(seeks[i]!==null){await s.player.pause();await s.player.seek(seeks[i])}
  else if(imgs[i]!==null){await setImage(s,imgs[i])}
 }
 await paintFrame(null,active,f);
 for(let i=0;i<slots.length;i++){
  if(seeks[i]!==null&&Math.abs(slots[i].player.getCurrentTime()-seeks[i])>0.002)throw Error('seek did not settle');
 }
}
</script>`
