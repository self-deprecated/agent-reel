package videoexport

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/chromedp/chromedp"
)

// The keys overlay of a composition (EDL.md "Keys overlay"): a KeyCastr-style
// strip of chips, one per recorded stroke, drawn as DOM in the frames page
// (textContent only). The caller supplies each stroke's chip text, client
// colour and the local frame it first shows on; the renderer decides per
// frame which chips are visible: the strokes of the last KeysHold output
// seconds within the overlay's range, at most KeysMaxChips, newest last.

// ComposeKey is one stroke of a keys overlay.
type ComposeKey struct {
	// Frame is the chunk-local frame the chip first shows on.
	Frame int `json:"frame"`
	// Text is the chip text ("git status", "Ctrl+C", "↓ ×5", "•••" for
	// masked input, "⎘ 120 chars" for a paste).
	Text string `json:"text"`
	// Color is the client's #rrggbb colour (the chip's left border).
	Color  string `json:"color"`
	Masked bool   `json:"masked,omitempty"`
	Paste  bool   `json:"paste,omitempty"`
}

const (
	// MaxComposeKeys bounds the strokes of one keys overlay.
	MaxComposeKeys = 65536
	// KeysMaxChips is the most chips one strip shows.
	KeysMaxChips = 6
	// keysHoldNum/keysHoldDen is how long (output seconds) a chip stays: 2.5 s,
	// as in the library viewer's overlay.
	keysHoldNum, keysHoldDen = 5, 2
)

// KeysPositions are the strip positions the page knows.
var KeysPositions = []string{"bottom-right", "bottom-left", "bottom-center", "top-right", "top-left", "top-center"}

// checkComposeKeys validates one keys overlay of a chunk of frames frames.
// Keys overlays are drawn on single terminal clips only (not cards or
// layouts).
func checkComposeKeys(o ComposeOverlay, frames int, notClip bool) error {
	if notClip {
		return errors.New("a keys overlay needs a single terminal clip (not a card or layout)")
	}
	if o.Text != "" || o.Subtitle != "" || o.Cell != nil || o.Px != nil {
		return errors.New("a keys overlay takes no text or anchor")
	}
	if !slices.Contains(KeysPositions, o.Position) {
		return errors.New("unknown keys position")
	}
	if len(o.Keys) > MaxComposeKeys {
		return fmt.Errorf("more than %d keystrokes", MaxComposeKeys)
	}
	prev := -1
	for _, k := range o.Keys {
		if k.Frame < prev || k.Frame < o.StartFrame || k.Frame >= o.EndFrame || k.Frame >= frames {
			return errors.New("keystroke frames must ascend inside the overlay's range")
		}
		prev = k.Frame
		if k.Text == "" || composeText(k.Text) != nil || !composeHex.MatchString(k.Color) || (k.Masked && k.Paste) {
			return errors.New("invalid keystroke chip")
		}
	}
	return nil
}

// keysHoldFrames is the chip lifetime in frames: ceil(2.5 s × rate).
func keysHoldFrames(rate Rate) int {
	n := int64(keysHoldNum) * rate.Num
	d := int64(keysHoldDen) * rate.Den
	return int((n + d - 1) / d)
}

// VisibleKeys lists the chips of a keys overlay shown on local frame k: the
// strokes in the overlay's range that first showed at most hold-1 frames
// before k, the last KeysMaxChips of them.
func VisibleKeys(o ComposeOverlay, k, hold int) []ComposeKey {
	if k < o.StartFrame || k >= o.EndFrame {
		return nil
	}
	lo := max(o.StartFrame, k-hold+1)
	i := sort.Search(len(o.Keys), func(i int) bool { return o.Keys[i].Frame >= lo })
	j := sort.Search(len(o.Keys), func(j int) bool { return o.Keys[j].Frame > k })
	if j-i > KeysMaxChips {
		i = j - KeysMaxChips
	}
	return o.Keys[i:j]
}

// composeChipPage is a chip as the page receives it.
type composeChipPage struct {
	Text   string `json:"t"`
	Color  string `json:"c"`
	Masked bool   `json:"m,omitempty"`
	Paste  bool   `json:"p,omitempty"`
}

// keysScript is the setKeys(...) call that fills chunk i's keys strips for
// local frame k, or "" when the chunk has no keys overlay.
func (j *composeJob) keysScript(i, k int) (string, error) {
	c := j.req.Chunks[i]
	var strips map[int][]composeChipPage
	hold := keysHoldFrames(j.result.Rate)
	for n, o := range c.Overlays {
		if o.Type != "keys" {
			continue
		}
		if strips == nil {
			strips = map[int][]composeChipPage{}
		}
		chips := []composeChipPage{}
		for _, key := range VisibleKeys(o, k, hold) {
			chips = append(chips, composeChipPage{Text: key.Text, Color: key.Color, Masked: key.Masked, Paste: key.Paste})
		}
		strips[n] = chips
	}
	if strips == nil {
		return "", nil
	}
	arg, err := jsonJS(strips)
	if err != nil {
		return "", err
	}
	return `setKeys(` + arg + `);`, nil
}

// captureKeys records the chip texts the page shows on a captured frame.
func (j *composeJob) captureKeys(frame int) error {
	var texts []string
	if err := chromedp.Run(j.page.browser, chromedp.Evaluate(`keysShown()`, &texts)); err != nil {
		return fmt.Errorf("keys evidence frame %d: %w", frame, err)
	}
	if j.result.CapturedKeys == nil {
		j.result.CapturedKeys = map[int][]string{}
	}
	j.result.CapturedKeys[frame] = texts
	return nil
}

// composeKeysPage extends composePage: keys strips positioned by
// data-position, chips with the client's colour as their left border, text
// set with textContent only. A paste chip's "⎘" (absent from the export font)
// is kept in the text but drawn as a CSS copy icon, so no fallback glyph is
// ever painted.
const composeKeysPage = `<style>
.o.keys{max-width:92%;overflow:hidden;gap:.35em;align-items:flex-end;flex-wrap:nowrap}
.o.keys.on{display:flex}
.o.keys[data-position^="bottom"]{bottom:5%}.o.keys[data-position^="top"]{top:5%}
.o.keys[data-position$="right"]{right:3%;justify-content:flex-end}.o.keys[data-position$="left"]{left:3%}
.o.keys[data-position$="center"]{left:50%;transform:translateX(-50%);justify-content:center}
.chip{flex:none;max-width:24em;overflow:hidden;text-overflow:ellipsis;white-space:pre;padding:.2em .55em;border-left:.3em solid;border-radius:.3em;background:rgba(0,0,0,.72);color:#fff;line-height:1.25}
.chip.masked{letter-spacing:.1em}
.chip .icon{display:inline-block;position:relative;width:.9em;height:1em;margin-right:.1em;vertical-align:-.15em;overflow:hidden;color:transparent}
.chip .icon::before,.chip .icon::after{content:"";position:absolute;box-sizing:border-box;width:.55em;height:.65em;border:.1em solid #fff;border-radius:.08em}
.chip .icon::before{left:0;top:.3em}.chip .icon::after{left:.28em;top:.05em;background:#2a2a2a}
</style><script>
{const base=setOverlays;setOverlays=function(list){base(list);for(const it of items){if(it.o.type==='keys'){it.el.textContent='';it.el.dataset.position=it.o.position||'bottom-right'}}}}
function setKeys(strips){
 for(const [n,chips] of Object.entries(strips)){
  const it=items[Number(n)];if(!it||it.o.type!=='keys')throw Error('no keys overlay '+n);
  it.el.replaceChildren(...chips.map(k=>{
   const el=document.createElement('span');el.className='chip'+(k.m?' masked':'')+(k.p?' paste':'');el.style.borderLeftColor=k.c;
   const text=k.t;
   if(k.p&&text.startsWith('⎘')){const i=document.createElement('span');i.className='icon';i.textContent='⎘';el.append(i,document.createTextNode(text.slice(1)))}else{el.textContent=text}
   return el;
  }));
 }
}
function keysShown(){return [...document.querySelectorAll('#overlay .o.keys.on .chip')].map(c=>c.textContent)}
</script>`
