//go:build linux || darwin

package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

func TestAllowanceV11DoctorRecover(t *testing.T) {
	f := newVisualFixture(t, 1, offlineIdentity)
	s := f.create(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.BeginSegment(1, archive.Geometry{Columns: 200, Rows: 50}, "exact"))
	for i := 1; i <= 2000; i++ {
		scr := castexport.Screen{Cols: 200, Rows: 50, Cells: make([][]castexport.Cell, 50)}
		for r := range scr.Cells {
			scr.Cells[r] = make([]castexport.Cell, 200)
			text := fmt.Sprintf("%0200d", (i-1)*50+r)
			for c := range scr.Cells[r] {
				scr.Cells[r][c] = castexport.Cell{Char: rune(text[c]), Width: 1}
			}
		}
		ok, err := s.Record(visualstore.View{Segment: 1, Point: visualAt(int64(i), visualtime.Time(i*10)), Screen: scr})
		must(err)
		if !ok {
			t.Fatal("view coalesced")
		}
		if i%1000 == 0 {
			must(s.Commit(visualtime.Cutoff{Segment: 1, Source: visualAt(int64(i), visualtime.Time(i*10)), Presentations: uint64(i)}))
		}
	}
	end := visualAt(2001, 20010)
	must(s.Event(visualstore.Event{Segment: 1, Point: end, Kind: visualstore.EventExit, Data: "0"}))
	must(s.Complete(visualtime.Cutoff{Segment: 1, Source: end, Presentations: 2000, Events: 1, SourceComplete: true}))
	must(s.Close())
	f.acknowledge(t, true)
	f.state.Status = "stopped"
	f.save(t)
	// Give the activity doctor something to verify, including explicit ceilings.
	a := newVisualActivity(t, f.dir, f.state)
	a.close()
	for _, flags := range [][]string{nil, {"--work", "5000000"}, {"--decoded-bytes", strconv.FormatInt(256<<30, 10)}} {
		var out bytes.Buffer
		args := append([]string{"doctor", f.state.ID, "--library", f.root, "--json"}, flags...)
		start := time.Now()
		must(offlineCLIWithObservation(args, &out, io.Discard, absentObservation()))
		var rep offlineVisualReport
		must(json.Unmarshal(out.Bytes(), &rep))
		t.Logf("doctor %v wall=%s work=%d decoded=%d allowance=%+v", flags, time.Since(start), rep.Visual.Work, rep.Visual.DecodedBytes, rep.Visual.Allowance)
		if rep.ArtifactStatus != "ok" || rep.Visual.Status != "ok" || rep.Visual.Work <= 32768 || rep.Activity.Report == nil || rep.Activity.Report.Status != "ok" {
			t.Fatalf("%s", out.String())
		}
		want := visualstore.DefaultLimits()
		if len(flags) > 0 && flags[0] == "--work" {
			want.Work = 5_000_000
		}
		if rep.Visual.Limits != want || rep.Activity.Report.Limits.Work != want.Work {
			t.Fatalf("ceilings: %+v %+v", rep.Visual, rep.Activity.Report)
		}
	}
	var out bytes.Buffer
	err := offlineCLIWithObservation([]string{"doctor", f.state.ID, "--library", f.root, "--work", "32768", "--json"}, &out, io.Discard, absentObservation())
	if !errors.Is(err, visualstore.ErrLimit) || !strings.Contains(err.Error(), "caller ceiling") {
		t.Fatalf("%v %s", err, out.String())
	}
	var limited offlineVisualReport
	must(json.Unmarshal(out.Bytes(), &limited))
	if limited.Visual.Status != visualstore.StatusUnverified || limited.Activity.Report.Limits.Work != 32768 {
		t.Fatalf("%s", out.String())
	}
	out.Reset()
	start := time.Now()
	must(recoverCLIWithObservation([]string{"recover", f.state.ID, "--library", f.root, "--json"}, &out, io.Discard, absentObservation()))
	var recovered recoverReport
	must(json.Unmarshal(out.Bytes(), &recovered))
	t.Logf("recover default wall=%s", time.Since(start))
	if !recovered.Sealed || !recovered.RemovedDirectory {
		t.Fatalf("%s", out.String())
	}
	if _, err = os.Lstat(f.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source retained: %v", err)
	}
	for _, flags := range [][]string{nil, {"--work", "5000000"}} {
		out.Reset()
		args := append([]string{"doctor", recovered.SealedPath, "--json"}, flags...)
		must(offlineCLIWithObservation(args, &out, io.Discard, absentObservation()))
		var rep seal.Report
		must(json.Unmarshal(out.Bytes(), &rep))
		if rep.Status != seal.StatusOK || rep.Visual.Allowance.Work == 0 || rep.Activity.Allowance.Work == 0 {
			t.Fatalf("%s", out.String())
		}
	}
	// REC dispatch to an existing seal must use the same ceilings as a file path.
	rep, err := offlineRecording(context.Background(), f.root, f.state.ID, "", visualstore.DefaultLimits(), absentObservation())
	must(err)
	if rep.ArtifactStatus != "ok" {
		t.Fatalf("%+v", rep)
	}
}
