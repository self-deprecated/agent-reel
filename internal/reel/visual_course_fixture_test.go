//go:build linux || darwin

package reel

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// The synthetic course fixture (V7 Task 4, 2506 O5): a two-segment visual/1
// recording with manual, activity and lifecycle markers whose expected output
// frames are computed by hand in library_exports_visual_events_test.go.
//
// Segment 1, 80x24, closed (paused) at 42.1 s:
//
//	line 1  view   0    red
//	line 2  view  10    blue
//	line 3  marker 12.5 "Checkpoint A"
//	line 4  marker 24   "Cut marker"          (inside the 20-30 s cut)
//	line 5  view  25    red
//	line 6  view  35    blue
//	line 7  close 42.1
//
// Segment 2, 100x30, resumed an hour later (state.json wall clocks):
//
//	line 1  view   0    green
//	line 2  marker 3    "Manual note"
//	line 3  exit   6.006, completed (stopped) there; 6.006 s is exactly 180
//	        frames at 30000/1001, so the stop maps to the export end.
//
// state.json lifecycle: start (seg 1), pause (seg 1), resume (seg 2), stop
// (seg 2). The indexed activity store holds, from two agents:
//
//	a1  agent A  estimated seg1 basis line 1 (0 s)  at  5 s     within
//	b1  agent B  estimated seg1 basis line 2 (10 s) at 12.5 s   within (same instant as "Checkpoint A")
//	a2  agent A  estimated seg1 basis line 6 (35 s) at 36 s     within
//	a3  agent A  estimated seg1 basis line 2 (10 s) at 22 s     within, in the cut
//	a4  agent B  estimated seg2 basis line 1 (0 s)  at  1 s     within
//	x1  agent A  estimated seg1 basis line 99       at 40 s     beyond durable coverage
//	u1  agent B  unknown placement
//
// Every envelope also carries body-like and argument fields holding
// visualCourseSentinel, which must never appear in any export output.
type visualCourse struct {
	g *visualGUI
	a *visualActivity
	// keys maps a fixture name ("checkpoint", "cutMarker", "manual", "exit",
	// "a1"..., "start", "pause", "resume", "stop") to its member key.
	keys map[string]string
	// colours: segment → view source time → colour, for frame expectations.
	colours map[int]map[visualtime.Time]string
}

// visualCourseSentinel stands for prompt/response bodies and tool arguments.
const visualCourseSentinel = "AREEL-COURSE-SENTINEL-7d41"

var visualCourseColour = map[string]uint8{"red": 1, "green": 2, "blue": 4}

// visualCourseScreen is cols x rows on an indexed background colour. Its top
// row alternates bright white and the colour per column, so a decoded frame
// shows the geometry (cols/2 white runs) as well as the colour.
func visualCourseScreen(cols, rows int, colour string) castexport.Screen {
	s := visualGUIScreen(cols, rows)
	for r := range s.Cells {
		for c := range s.Cells[r] {
			bg := visualCourseColour[colour]
			if r == 0 && c%2 == 0 {
				bg = 15
			}
			s.Cells[r][c].BG = castexport.Color{Kind: castexport.ColorIndexed, Index: bg}
		}
	}
	return s
}

func visualCourseEnvelope(id, producer, agent, label, role, phase, tool string) activityEnvelope {
	v := activityFixture(id)
	v.Producer, v.AgentInstance, v.Label = strptr(producer), strptr(agent), strptr(label)
	v.Conversation = strptr(visualCourseSentinel + "-conversation")
	v.Data = json.RawMessage(fmt.Sprintf(`{"phase":%q,"role":%q,"tool":%q,"body":%q,"arguments":{"command":%q},"keys":%q}`,
		phase, role, tool, visualCourseSentinel+" prompt and response body", visualCourseSentinel+" tool argument", visualCourseSentinel+" keystrokes"))
	return v
}

// newVisualCourse builds the course fixture. With complete false, segment 2
// is left live (committed at 6.006 s, no stop and no exit) so a test can keep
// appending after an export froze its input.
func newVisualCourse(t *testing.T, complete bool) *visualCourse {
	t.Helper()
	status := "stopped"
	if !complete {
		status = "recording"
	}
	c := &visualCourse{g: newVisualGUI(t, status), keys: map[string]string{}, colours: map[int]map[visualtime.Time]string{1: {}, 2: {}}}
	g := c.g
	view := func(segment int, at float64, cols, rows int, colour string) {
		g.view(at, visualCourseScreen(cols, rows, colour))
		c.colours[segment][visualGUISeconds(at)] = colour
	}
	g.begin(1, 80, 24)
	view(1, 0, 80, 24, "red")
	view(1, 10, 80, 24, "blue")
	g.marker(12.5, "Checkpoint A")
	g.marker(24, "Cut marker")
	view(1, 25, 80, 24, "red")
	view(1, 35, 80, 24, "blue")
	g.closeSegment(42.1)
	g.begin(2, 100, 30)
	view(2, 0, 100, 30, "green")
	g.marker(3, "Manual note")
	id := g.state.ID
	g.state.Events = append(g.state.Events,
		Event{ID: id + ":1", Kind: "start", At: visualGUIStart, Segment: 1},
		Event{ID: id + ":2", Kind: "pause", At: visualGUIStart.Add(42100 * time.Millisecond), Segment: 1},
		Event{ID: id + ":3", Kind: "resume", At: visualGUIStart.Add(time.Hour), Segment: 2})
	c.keys["start"], c.keys["pause"], c.keys["resume"] = "lifecycle:"+id+":1", "lifecycle:"+id+":2", "lifecycle:"+id+":3"
	if complete {
		g.completeAt(6.006)
		ended := visualGUIStart.Add(time.Hour + 6006*time.Millisecond)
		g.state.Segments[1].Ended = &ended
		g.state.Events = append(g.state.Events, Event{ID: id + ":4", Kind: "stop", At: ended, Segment: 2})
		c.keys["stop"] = "lifecycle:" + id + ":4"
	} else {
		g.commit(6.006)
	}
	g.saveState()
	g.acknowledge()
	// Visual event keys come from the store's own sequence numbers.
	r, sel := visualStoreReader(t, g)
	for segment := 1; segment <= 2; segment++ {
		events, e := r.Events(context.Background(), sel, segment, 1, 100)
		if e != nil {
			t.Fatal(e)
		}
		for _, ev := range events {
			name := map[string]string{"Checkpoint A": "checkpoint", "Cut marker": "cutMarker", "Manual note": "manual"}[ev.Data]
			if ev.Kind == "exit" {
				name = "exit"
			}
			c.keys[name] = fmt.Sprintf("visual-event:%d", ev.Seq)
		}
	}
	c.a = newVisualActivity(t, g.dir, g.state)
	add := func(name string, v activityEnvelope, p visualtime.Placement) {
		c.a.add(v, false, nil, p)
		c.keys[name] = "activity:" + v.ID
	}
	add("a1", visualCourseEnvelope("course-a1", "agent-a", "agent-a-1", "Plan the lesson", "assistant", "start", "bash"), estimatedAt(1, 1, 0, 5))
	add("b1", visualCourseEnvelope("course-b1", "agent-b", "agent-b-1", "Review checkpoint", "reviewer", "check", "read"), estimatedAt(1, 2, 10, 12.5))
	add("a2", visualCourseEnvelope("course-a2", "agent-a", "agent-a-1", "Run the tests", "assistant", "run", "bash"), estimatedAt(1, 6, 35, 36))
	add("a3", visualCourseEnvelope("course-a3", "agent-a", "agent-a-1", "Cut aside", "assistant", "aside", "edit"), estimatedAt(1, 2, 10, 22))
	add("a4", visualCourseEnvelope("course-a4", "agent-b", "agent-b-1", "Resume notes", "reviewer", "resume", "read"), estimatedAt(2, 1, 0, 1))
	add("x1", visualCourseEnvelope("course-x1", "agent-a", "agent-a-1", "Beyond coverage", "assistant", "late", "bash"), estimatedAt(1, 99, 40, 40))
	add("u1", visualCourseEnvelope("course-u1", "agent-b", "agent-b-1", "Unplaced", "reviewer", "none", "read"), unknownPlacement())
	c.a.commit()
	return c
}

// Recipes from the Task 4 acceptance criteria, at 30000/1001 and 640x480.
const (
	// Main: clips [seg1 2-20 s x1, seg1 30-42.1 s (end) x2, seg2 0-end x1].
	visualCourseRecipe = `{"schema":"agent-reel/recipe/1","rate":{"num":30000,"den":1001},"output":{"width":640,"height":480},"clips":[{"segment":1,"start":"2","end":"20"},{"segment":1,"start":"30","speed":{"num":2,"den":1}},{"segment":2}]}`
	// Variant: clip 1 starts at 30.088 s, so D = 6.006 s and D·Rate = 180
	// exactly: the pause maps to the next clip's first frame, with the resume.
	visualCourseRecipeSameFrame = `{"schema":"agent-reel/recipe/1","rate":{"num":30000,"den":1001},"output":{"width":640,"height":480},"clips":[{"segment":1,"start":"2","end":"20"},{"segment":1,"start":"30.088","speed":{"num":2,"den":1}},{"segment":2}]}`
)
