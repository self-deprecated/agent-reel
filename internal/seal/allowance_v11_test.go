package seal

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

func TestAllowanceV11WholeFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
	}{{"few-screens", 20000}, {"new-screens", 2000}, {"spread-rows", 6*visualstore.SealChunkRecords/4 + 600}, {"resize-scroll", 4001}, {"scroll", 4000}} {
		t.Run(tc.name, func(t *testing.T) {
			src := fixtureShape(t, true, tc.n, tc.name)
			dst := destination(t)
			start := time.Now()
			build(t, src, dst)
			t.Logf("seal wall=%s", time.Since(start))
			start = time.Now()
			rep, err := Verify(context.Background(), dst, visualstore.DefaultLimits())
			must(t, err)
			t.Logf("verify wall=%s work=%d decoded=%d visual work=%d decoded=%d allowance=%+v activity work=%d allowance=%+v", time.Since(start), rep.Work, rep.DecodedBytes, rep.Visual.Work, rep.Visual.DecodedBytes, rep.Visual.Allowance, rep.Activity.Work, rep.Activity.Allowance)
			if rep.Status != StatusOK || rep.Visual.Work > rep.Visual.Allowance.Work/2 || rep.Visual.DecodedBytes > rep.Visual.Allowance.DecodedBytes/2 || rep.Activity.Allowance.Work != 1024+8*int(rep.Activity.Count) {
				t.Fatalf("%+v", rep)
			}
		})
	}
}

func TestAllowanceV11Grounding(t *testing.T) {
	data, err := os.ReadFile("testdata/results/v11-run6.json")
	must(t, err)
	var measurements struct {
		Stores []struct {
			Name    string             `json:"name"`
			Minimum visualstore.Limits `json:"minimum_limits"`
			Source  visualstore.Limits `json:"source_verification_counters"`
		} `json:"stores"`
	}
	must(t, json.Unmarshal(data, &measurements))
	ceiling := visualstore.DefaultLimits()
	check := func(name string, demand visualstore.Limits) {
		t.Helper()
		if demand.Work > 0 && ceiling.Work < 4*demand.Work {
			t.Errorf("%s work ceiling %d < 4 * %d", name, ceiling.Work, demand.Work)
		}
		if demand.DecodedBytes > 0 && ceiling.DecodedBytes < 4*demand.DecodedBytes {
			t.Errorf("%s decoded ceiling %d < 4 * %d", name, ceiling.DecodedBytes, demand.DecodedBytes)
		}
	}
	found := false
	if len(measurements.Stores) == 0 {
		t.Fatal("missing Run 6 stores")
	}
	for _, s := range measurements.Stores {
		check(s.Name, s.Minimum)
		if s.Name == "mixed-3d" {
			if s.Source.Work <= 0 || s.Source.DecodedBytes <= 0 {
				t.Fatal("missing mixed-3d source counters")
			}
			found = true
			check(s.Name+" source", s.Source)
		}
	}
	if !found {
		t.Fatal("missing mixed-3d")
	}
	// W3-10m ×48 extrapolation for W3-8h, before Run A's decode reuse.
	check("W3-8h extrapolation", visualstore.Limits{Work: 42_000_000, DecodedBytes: 38 << 30})
}
