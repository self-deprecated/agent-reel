package reel

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/self-deprecated/agent-reel/internal/links"
)

// linkResumeHook returns the holder's resume callback: when the parent resumes
// (segment N begins), one areel:link resume record is admitted for every open
// link whose start is an admitted record, re-anchoring the child clock on the
// new segment (docs/integrations.md "Resume re-anchor"). It runs off the lifecycle
// loop; the manifest is only read. A manifest that cannot be read leaves the
// segment un-anchored, which link ls reports.
func linkResumeHook(root string, activity *activityService, state State) func(int, time.Time) {
	return func(segment int, started time.Time) {
		go linkResumeAnchors(root, activity, state, segment, started)
	}
}

func linkResumeAnchors(root string, activity *activityService, state State, segment int, started time.Time) []activityResponse {
	m, exists, err := links.Read(root, state.ID)
	if err != nil {
		holderNotify(fmt.Sprintf("areel: recording %s: links manifest unreadable at resume (segment %d stays un-anchored): %v", state.ID, segment, err))
		return nil
	}
	if !exists {
		return nil
	}
	var out []activityResponse
	for _, l := range m.Links {
		if !l.Open() || l.StartAnchor == nil || l.StartAnchor.Seq == nil {
			continue
		}
		envelope := linkEnvelope(l, links.PhaseResume, links.SourceHolder, map[string]any{"segment": segment, "resumedAt": started.UTC()})
		data, e := json.Marshal(envelope)
		if e == nil {
			envelope, e = parseActivity(data, state.Target)
		}
		if e != nil {
			holderNotify(fmt.Sprintf("areel: recording %s: link %s resume record invalid: %v", state.ID, l.ID, e))
			continue
		}
		out = append(out, activity.submit(envelope, true))
	}
	return out
}
