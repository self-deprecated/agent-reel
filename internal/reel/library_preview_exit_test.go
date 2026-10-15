//go:build linux || darwin

package reel

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/self-deprecated/agent-reel/internal/castexport"
)

// zellijExitTail is testdata/zellij-exit-tail.json: the tail of a real sealed
// Zellij recording, ending on Zellij's exit screen.
type zellijExitTail struct {
	Cols, Rows int
	ExitNs     int64 `json:"exitNs"`
	Views      []struct {
		AtNs   int64 `json:"atNs"`
		Cursor struct {
			Col, Row int
			Visible  bool
		} `json:"cursor"`
		Rows []struct {
			Row  int    `json:"row"`
			Text string `json:"text"`
		} `json:"rows"`
	} `json:"views"`
}

func loadZellijExitTail(t *testing.T) zellijExitTail {
	t.Helper()
	raw, e := os.ReadFile("testdata/zellij-exit-tail.json")
	if e != nil {
		t.Fatal(e)
	}
	var tail zellijExitTail
	if e = json.Unmarshal(raw, &tail); e != nil || len(tail.Views) != 3 {
		t.Fatal("fixture", e)
	}
	return tail
}

// screen rebuilds a fixture view: default-pen text rows at their real rows.
func (z zellijExitTail) screen(i int) castexport.Screen {
	v := z.Views[i]
	s := visualGUIScreen(z.Cols, z.Rows)
	for _, row := range v.Rows {
		col := 0
		for _, r := range row.Text {
			if col < z.Cols {
				s.Cells[row.Row][col] = castexport.Cell{Char: r, Width: 1}
			}
			col++
		}
	}
	s.Cursor = castexport.Cursor{Col: v.Cursor.Col, Row: v.Cursor.Row, Visible: v.Cursor.Visible}
	return s
}

// zellijExitFixture records the real exit tail after an earlier view and
// completes the recording at the real exit event.
func zellijExitFixture(t *testing.T) (*visualGUI, zellijExitTail) {
	t.Helper()
	tail := loadZellijExitTail(t)
	g := newVisualGUI(t, "stopped")
	g.begin(1, tail.Cols, tail.Rows)
	g.view(0, visualGUIScreen(tail.Cols, tail.Rows, "EARLIER-VIEW"))
	for i, v := range tail.Views {
		g.view(float64(v.AtNs)/1e9, tail.screen(i))
	}
	g.completeAt(float64(tail.ExitNs) / 1e9)
	g.acknowledge()
	return g, tail
}

// P1 regression: a recording that ends on Zellij's exit screen previews the
// last substantive screen, never "Bye from Zellij!", in both the card still
// and the segment still.
func TestVisualStillSkipsZellijExitScreen(t *testing.T) {
	g, tail := zellijExitFixture(t)
	substantive := visualFull(t, tail.screen(1))
	for _, path := range []string{"/api/recordings/" + g.id + "/preview-durable", "/api/recordings/" + g.id + "/segments/1/preview"} {
		w := libraryRequest(g.h, path, nil)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		cols, rows, data := visualCastScreen(t, w.Body.Bytes())
		if cols != tail.Cols || rows != tail.Rows {
			t.Fatalf("%s: geometry %dx%d", path, cols, rows)
		}
		if strings.Contains(data, "Bye from Zellij!") || !strings.Contains(data, "┌─────────┬────────────┐") {
			t.Fatalf("%s: still is not the last substantive screen", path)
		}
		if data != substantive {
			t.Fatalf("%s: still is not exactly the newest substantive view", path)
		}
	}
	// The facts still name the newest view as the last change.
	w := libraryRequest(g.h, "/api/recordings/"+g.id+"/preview-durable", nil)
	if got := w.Header().Get("AgentReel-Preview-Last-Change"); got != "segment=1;at=12.656" {
		t.Fatalf("last change %q", got)
	}
}

// A segment whose screen really is empty keeps its newest view: the rule
// never reaches past the lookback or invents content.
func TestVisualStillKeepsGenuinelyBlankScreen(t *testing.T) {
	g := newVisualGUI(t, "stopped")
	g.begin(1, 80, 24)
	g.view(0, visualGUIText("$"))
	g.view(1, visualGUIText("$ clear"))
	g.view(2, visualGUIText("$ "))
	g.commit(3)
	g.acknowledge()
	w := libraryRequest(g.h, "/api/recordings/"+g.id+"/preview-durable", nil)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	_, _, data := visualCastScreen(t, w.Body.Bytes())
	if data != visualFull(t, visualGUIText("$ ")) {
		t.Fatal("near-blank tail did not keep the newest view")
	}
	if !screenNearBlank(visualGUIText("Bye from Zellij!")) || screenNearBlank(visualGUIText("one", "two", "three")) || screenNearBlank(visualGUIText(strings.Repeat("x", 41), strings.Repeat("y", 41))) {
		t.Fatal("near-blank classification")
	}
	inverse := visualGUIText()
	for col := 0; col < 80; col++ {
		inverse.Cells[5][col].Attrs = castexport.AttrInverse
		inverse.Cells[6][col].Attrs = castexport.AttrInverse
	}
	if screenNearBlank(inverse) {
		t.Fatal("an inverse-video bar is content")
	}
}

// P2/U3: when Live is unavailable the viewer shows the latest durable still in
// a separate frame with the retry link, without pinning history or replaying
// a cast as Live; a live card carries the durable still as its fallback.
func TestVisualLiveUnavailableShowsDurableStill(t *testing.T) {
	g := visualPreviewFixture(t, "recording")
	w := libraryRequest(g.h, "/recordings/"+g.id, nil)
	body := w.Body.String()
	still := `/player/` + g.id + `?mode=live&amp;segment=2&amp;still=1`
	if w.Code != 200 || !strings.Contains(body, "Live unavailable · showing the last saved screen from ") || !strings.Contains(body, `id="still-frame"`) || !strings.Contains(body, `src="`+still+`"`) || !strings.Contains(body, "data-live-retry") || strings.Contains(body, `id="terminal-frame"`) || strings.Contains(body, "cutoff=") {
		t.Fatalf("live-unavailable viewer: %d\n%s", w.Code, body)
	}
	if len(g.h.history.bindings) != 0 {
		t.Fatal("Live fallback pinned history")
	}
	player := libraryRequest(g.h, "/player/"+g.id+"?mode=live&segment=2&still=1", nil).Body.String()
	if !strings.Contains(player, `data-source="/api/recordings/`+g.id+`/preview-durable" data-mode="still"`) {
		t.Fatal("still frame does not play the durable still", player)
	}
	if plain := libraryRequest(g.h, "/player/"+g.id+"?mode=live&segment=2", nil).Body.String(); strings.Contains(plain, `data-mode="still"`) {
		t.Fatal("the Live frame itself played the still", plain)
	}
	if w := libraryRequest(g.h, "/recordings/"+g.id+"?mode=history&segment=2&still=1", nil); w.Code != 400 {
		t.Fatal("still accepted outside Live", w.Code)
	}
	if w := libraryRequest(g.h, "/api/recordings/"+g.id+"/preview-durable", nil); w.Code != 200 {
		t.Fatal("durable still unavailable for a live recording", w.Code)
	}
	cards := libraryRequest(g.h, "/", nil).Body.String()
	if !strings.Contains(cards, `data-preview-url="/api/recordings/`+g.id+`/preview-current" data-preview-fallback-url="/api/recordings/`+g.id+`/preview-durable"`) {
		t.Fatal("live card has no durable fallback", cards)
	}
	g.setStatus("paused")
	g.h.catalog.Invalidate()
	if cards := libraryRequest(g.h, "/", nil).Body.String(); strings.Contains(cards, "data-preview-fallback-url") {
		t.Fatal("durable card carries a fallback", cards)
	}
}

// A still frame (still=1) always plays the durable still: a recorder that
// validates between the viewer render and the still frame's load must not turn
// the frame titled "Latest durable screen" into a Live stream.
func TestVisualStillFrameNeverStreamsLive(t *testing.T) {
	g := visualPreviewFixture(t, "recording")
	validate := libraryLiveValidate
	t.Cleanup(func() { libraryLiveValidate = validate })
	libraryLiveValidate = func(context.Context, string, string, int) error { return nil }
	still := libraryRequest(g.h, "/player/"+g.id+"?mode=live&segment=2&still=1", nil).Body.String()
	if !strings.Contains(still, `data-source="/api/recordings/`+g.id+`/preview-durable" data-mode="still"`) || strings.Contains(still, "/segments/2/live") {
		t.Fatal("still frame streamed Live after the recorder validated", still)
	}
	live := libraryRequest(g.h, "/player/"+g.id+"?mode=live&segment=2", nil).Body.String()
	if !strings.Contains(live, `data-source="/api/recordings/`+g.id+`/segments/2/live" data-mode="live"`) {
		t.Fatal("validated Live frame did not stream Live", live)
	}
}
