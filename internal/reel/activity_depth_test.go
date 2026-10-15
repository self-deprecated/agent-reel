package reel

import (
	"encoding/json"
	"strings"
	"testing"
)

// Go's existing ingress policy counts values from root depth zero. An empty
// container at depth32 is valid; a child value at depth33 is not.
func TestActivityListIngressDepthBoundary(t *testing.T) {
	s, dir := activityServiceFixture(t, nil)
	v := activityFixture("depth-boundary")
	v.Data = json.RawMessage(strings.Repeat("[", 31) + strings.Repeat("]", 31))
	req := activityRequestFor(t, s, v)
	wire, e := json.Marshal(req)
	if e != nil {
		t.Fatal(e)
	}
	var parsed activityRequest
	if e = strictActivityJSON(wire, &parsed); e != nil {
		t.Fatal("valid request boundary rejected", e)
	}
	response, e := callActivity(dir, req)
	if e != nil {
		t.Fatal(e)
	}
	requireActivityDurable(t, s, response)
	report, e := listActivity(s.store.Root, s.state.ID, 0, 32, activityListOutputDefault, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(report.Items) != 1 || report.Items[0].Status != "stored" || report.Items[0].Record == nil {
		t.Fatalf("writer-accepted empty depth32 container not listed: %+v", report)
	}
	if string(report.Items[0].Record.Envelope.Data) != string(v.Data) {
		t.Fatal("boundary payload changed")
	}
	for _, data := range []string{strings.Repeat("[", 32) + strings.Repeat("]", 32), strings.Repeat("[", 31) + "0" + strings.Repeat("]", 31)} {
		invalid := activityFixture("next-invalid")
		invalid.Data = json.RawMessage(data)
		request := activityRequestFor(t, s, invalid)
		wire, e = json.Marshal(request)
		if e != nil {
			t.Fatal(e)
		}
		if e = strictActivityJSON(wire, &parsed); e == nil {
			t.Fatal("depth33 request unexpectedly valid")
		}
		rejected, e := callActivity(dir, request)
		if e != nil || rejected.Accepted || rejected.Error == "" {
			t.Fatal("invalid boundary passed IPC", rejected, e)
		}
		record := activityRecordFixture(s.state, 2)
		record.Envelope = invalid
		record.Receipt.ID = invalid.ID
		stored, e := json.Marshal(record)
		if e != nil {
			t.Fatal(e)
		}
		if e = activityListJSON(stored, &activityRecord{}); e == nil {
			t.Fatal("reader accepted next invalid depth")
		}
	}
	if s.snapshot().Accounting.Accepted != 1 {
		t.Fatal("invalid requests admitted")
	}
}
