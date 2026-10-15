package reel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func minimalActivityEnvelope(id, text string) []byte {
	return []byte(fmt.Sprintf(`{"id":%q,"source":"a:b","kind":"a:b","data":{"text":%q,"precise":9007199254740993,"decimal":0.0012300}}`, id, text))
}
func activityUnescapedWire(t *testing.T, req activityRequest) []byte {
	t.Helper()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if e := encoder.Encode(req); e != nil {
		t.Fatal(e)
	}
	return buffer.Bytes()
}

// Only the existing owned activity metadata endpoint is used. Disable the
// client's optional HTML escaping to exercise valid unescaped incoming JSON;
// the unchanged writer still canonicalizes/escapes when publishing the record.
func callRawActivityWire(t *testing.T, dir string, wire []byte) activityResponse {
	t.Helper()
	conn, e := net.DialTimeout("unix", filepath.Join(dir, "activity.sock"), time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	if e = conn.SetDeadline(time.Now().Add(3 * time.Second)); e != nil {
		t.Fatal(e)
	}
	n, e := conn.Write(wire)
	if e != nil || n != len(wire) {
		t.Fatal("metadata write", n, e)
	}
	if e = conn.(*net.UnixConn).CloseWrite(); e != nil {
		t.Fatal(e)
	}
	var response activityResponse
	if e = json.NewDecoder(io.LimitReader(conn, activityRequestMax+1)).Decode(&response); e != nil {
		t.Fatal(e)
	}
	return response
}
func TestActivityListCanonicalEnvelopeSize(t *testing.T) {
	for _, html := range []bool{false, true} {
		t.Run(fmt.Sprintf("html-%t", html), func(t *testing.T) {
			s, dir := activityServiceFixture(t, nil)
			id := "canonical-band"
			text := strings.Repeat("x", 8100-len(minimalActivityEnvelope(id, "")))
			if html {
				text = strings.Repeat("<>&", 450)
			}
			raw := minimalActivityEnvelope(id, text)
			req := activityRequestFor(t, s, activityFixture(id))
			req.Envelope = raw
			wire, e := json.Marshal(req)
			if e != nil {
				t.Fatal(e)
			}
			if html {
				wire = activityUnescapedWire(t, req)
			}
			var decoded activityRequest
			if e = strictActivityJSON(wire, &decoded); e != nil {
				t.Fatal("wire rejected before envelope", e)
			}
			v, e := parseActivity(decoded.Envelope, s.state.Target)
			if e != nil {
				t.Fatal("raw wire envelope rejected", e)
			}
			canonical, e := json.Marshal(v)
			if e != nil {
				t.Fatal(e)
			}
			preview := activityRecordFixture(s.state, 1)
			preview.Envelope = v
			preview.Receipt.ID = v.ID
			recordBytes, e := json.Marshal(preview)
			if e != nil {
				t.Fatal(e)
			}
			t.Logf("raw=%d wire-envelope=%d wire=%d canonical-envelope=%d canonical-record-with-newline=%d", len(raw), len(decoded.Envelope), len(wire), len(canonical), len(recordBytes)+1)
			if len(raw) > activityEnvelopeMax || len(decoded.Envelope) > activityEnvelopeMax || len(wire) > activityRequestMax || len(canonical) <= activityEnvelopeMax || len(recordBytes)+1 > activityRecordMax {
				t.Fatal("fixture missed the raw/canonical size band")
			}
			var response activityResponse
			if html {
				response = callRawActivityWire(t, dir, wire)
			} else {
				response, e = callActivity(dir, req)
				if e != nil {
					t.Fatal(e)
				}
			}
			requireActivityDurable(t, s, response)
			stored := activityReadRecord(t, s, response)
			report, e := listActivity(s.store.Root, s.state.ID, 0, 32, activityListOutputDefault, nil)
			if e != nil {
				t.Fatal(e)
			}
			if len(report.Items) != 1 || report.Items[0].Status != "stored" || report.Items[0].Record == nil {
				t.Fatalf("writer-durable canonical envelope misclassified: %+v", report)
			}
			data := report.Items[0].Record.Envelope.Data
			if !bytes.Equal(data, stored.Envelope.Data) || !bytes.Contains(data, []byte(`"precise":9007199254740993`)) || !bytes.Contains(data, []byte(`"decimal":0.0012300`)) {
				t.Fatal("existing stored data/number lexemes changed")
			}
			var meaning struct {
				Text string `json:"text"`
			}
			if e = json.Unmarshal(data, &meaning); e != nil || meaning.Text != text {
				t.Fatal("HTML string meaning changed", e)
			}
		})
	}
}
func TestActivityCanonicalAdmissionBoundsUnchanged(t *testing.T) {
	for _, recordCap := range []bool{false, true} {
		t.Run(fmt.Sprintf("record-cap-%t", recordCap), func(t *testing.T) {
			s, dir := activityServiceFixture(t, nil)
			id := "unchanged-caps"
			text := strings.Repeat("x", activityEnvelopeMax+1-len(minimalActivityEnvelope(id, "")))
			if recordCap {
				text = strings.Repeat("<", 2800)
			}
			raw := minimalActivityEnvelope(id, text)
			req := activityRequestFor(t, s, activityFixture(id))
			req.Envelope = raw
			wire := activityUnescapedWire(t, req)
			var decoded activityRequest
			if e := strictActivityJSON(wire, &decoded); e != nil || len(wire) > activityRequestMax {
				t.Fatal("fixture hit wire guard instead", e)
			}
			v, e := parseActivity(decoded.Envelope, s.state.Target)
			if recordCap {
				if e != nil || len(raw) > activityEnvelopeMax || len(decoded.Envelope) > activityEnvelopeMax {
					t.Fatal("fixture hit raw envelope guard instead", e)
				}
				preview := activityRecordFixture(s.state, 1)
				preview.Envelope = v
				preview.Receipt.ID = v.ID
				canonical, e := json.Marshal(preview)
				if e != nil {
					t.Fatal(e)
				}
				t.Logf("raw=%d wire-envelope=%d wire=%d canonical-record-with-newline=%d", len(raw), len(decoded.Envelope), len(wire), len(canonical)+1)
				if len(canonical)+1 <= activityRecordMax {
					t.Fatal("fixture did not cross canonical record cap")
				}
			} else if e == nil || len(decoded.Envelope) != activityEnvelopeMax+1 {
				t.Fatal("raw ingress cap changed", e)
			}
			rejected := callRawActivityWire(t, dir, wire)
			expected := "activity envelope exceeds 8192 bytes"
			if recordCap {
				expected = "activity record/storage limit reached"
			}
			if rejected.Accepted || rejected.Durable || rejected.Error != expected || s.snapshot().Accounting.Accepted != 0 {
				t.Fatal("wrong admission gate or accepted oversize data", rejected)
			}
		})
	}
}
