//go:build linux && browser

package reel

import (
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// M4: marks of a sealed .areel recording. The player draws the history
// index's markers (the recorder's marker event plus the you/agent/tool marks)
// as ticks on its timeline, the marks rail lists the sealed activity, and a
// mark opens history at its position under the same token.
func TestBrowserSealedMarksTimelineAndRail(t *testing.T) {
	g := sealedMarksFixture(t)
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)), chromedp.Poll(`document.querySelectorAll('#marks .mark-row').length === 4`, nil))
	dvrSafetyText(t, ctx, "TIMELINE-000")
	token := dvrSafetyToken(t, ctx)
	// The official player's own timeline ticks, tagged by who for styling.
	var who []string
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('#terminal-frame').contentDocument.querySelectorAll('.ap-marker-container[data-who]').length === 4`, nil),
		chromedp.Evaluate(`Array.from(document.querySelector('#terminal-frame').contentDocument.querySelectorAll('.ap-marker-container')).map(m=>m.dataset.who)`, &who))
	if strings.Join(who, ",") != "other,you,agent,tool" {
		t.Fatalf("timeline ticks %v", who)
	}
	// The rail: time, who and label per mark; the beyond-coverage mark has no link.
	var rows []string
	runBrowser(t, ctx, chromedp.Evaluate(`Array.from(document.querySelectorAll('#marks .mark-row')).map(r=>r.tagName+' '+r.className+' '+r.querySelector('.mark-at').textContent+' '+r.querySelector('.mark-label').textContent)`, &rows))
	want := []string{"A mark-row mark-you 00:12 You", "A mark-row mark-agent 00:22 Agent replied", "A mark-row mark-tool 00:23 Tool result", "SPAN mark-row mark-agent mark-unplaced 00:41 Agent replied"}
	if strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Fatalf("rail rows %q", rows)
	}
	var href string
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelectorAll('#marks a.mark-row')[1].getAttribute('href')`, &href))
	if !strings.Contains(href, "segment=1&at=22&cutoff="+token) {
		t.Fatalf("mark link %s", href)
	}
	// A click seeks the player in place, under the same token, and the row
	// at the current position is highlighted.
	var location string
	runBrowser(t, ctx, chromedp.Location(&location), chromedp.Evaluate(`document.querySelectorAll('#marks a.mark-row')[1].click()`, nil))
	dvrSafetyText(t, ctx, "TIMELINE-020")
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('#marks .mark-current .mark-label')?.textContent==='Agent replied'`, nil))
	var after string
	runBrowser(t, ctx, chromedp.Location(&after))
	if after != location || dvrSafetyToken(t, ctx) != token {
		t.Fatal("mark seek navigated or repinned", location, after)
	}
	// Opened at a position, the rail highlights the mark there.
	runBrowser(t, ctx, chromedp.Navigate(base+"/recordings/"+g.id+"?mode=history&segment=1&at=23"), chromedp.Poll(`document.querySelector('#marks .mark-current .mark-label')?.textContent==='Tool result'`, nil))
}

// Labelled manual marks of a sealed recording: the rail names them by their
// labels (a retroactive one at its offset position), the player's timeline
// ticks carry the labels, and the Mark button is disabled with its reason.
func TestBrowserSealedLabelledMarks(t *testing.T) {
	g := labelledMarksFixture(t)
	base := g.serve(nil)
	ctx := libraryBrowser(t)
	runBrowser(t, ctx, chromedp.Navigate(base+viewerURL(g.id, "history", 1, 0)), chromedp.Poll(`document.querySelectorAll('#marks .mark-row').length === 5`, nil))
	var rows []string
	runBrowser(t, ctx, chromedp.Evaluate(`Array.from(document.querySelectorAll('#marks .mark-row')).map(r=>r.className+' '+r.querySelector('.mark-at').textContent+' '+r.querySelector('.mark-label').textContent)`, &rows))
	want := []string{"mark-row mark-you 00:12 You", "mark-row mark-other 00:15 plan approved", "mark-row mark-other 00:18 Mark", "mark-row mark-other 00:22 " + labelledMarksLong, "mark-row mark-other 00:26 earlier"}
	if strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Fatalf("rail rows %q", rows)
	}
	var tips []string
	runBrowser(t, ctx, chromedp.Poll(`document.querySelector('#terminal-frame').contentDocument.querySelectorAll('.ap-marker-container[data-who]').length === 5`, nil),
		chromedp.Evaluate(`Array.from(document.querySelector('#terminal-frame').contentDocument.querySelectorAll('.ap-marker-container')).map(m=>m.dataset.who+' '+m.querySelector('.ap-tooltip').textContent)`, &tips))
	if len(tips) != 5 || tips[0] != "you 00:12 - You" || tips[1] != "other 00:15 - plan approved" || tips[4] != "other 00:26 - earlier" {
		t.Fatalf("timeline ticks %q", tips)
	}
	var disabled bool
	var title string
	runBrowser(t, ctx, chromedp.Evaluate(`document.querySelector('[data-mark-unavailable]').disabled`, &disabled), chromedp.Evaluate(`document.querySelector('[data-mark-unavailable]').title`, &title))
	if !disabled || !strings.Contains(title, "while the recording is live") {
		t.Fatalf("mark button %v %q", disabled, title)
	}
}
