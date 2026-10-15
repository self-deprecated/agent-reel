package seal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/castexport"
	"github.com/self-deprecated/agent-reel/internal/fidelity"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

const run3ID = "0123456789abcdef0123456789abcdef"

func recoveryOptions() Options {
	o := options()
	o.Kind = KindInterrupted
	o.Recovery = RecoveryInput{SourceStatus: "recording", SourcePending: "stop", RecoveredAt: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC), Recoverer: "areel 0.1.0"}
	return o
}

// All paths, stores, seals, mutations and deletion candidates in Run 3 tests
// are synthetic descendants of t.TempDir. No helper or external corpus is used.
func run3Fixture(t *testing.T, n, ackAt int, complete, activity bool) (string, string) {
	t.Helper()
	root := t.TempDir()
	must(t, os.Chmod(root, 0700))
	src := filepath.Join(root, run3ID)
	must(t, os.Mkdir(src, 0700))
	id := archive.Identity{Recording: run3ID, Backend: "fake", Instance: "test"}
	v, err := visualstore.Create(filepath.Join(src, "visual"), id)
	must(t, err)
	t.Cleanup(func() { _ = v.Close() })
	must(t, v.BeginSegment(1, archive.Geometry{Columns: 2, Rows: 1}, "exact"))
	commitEvery := 10
	if n > 1000 {
		commitEvery = 1000
	}
	var ack visualcapture.Acknowledged
	for i := 1; i <= n; i++ {
		screen := castexport.Screen{Cols: 2, Rows: 1, Cursor: castexport.Cursor{Col: i % 2, Visible: true}, Cells: [][]castexport.Cell{{{Char: rune('a' + i%20), Width: 1}, {Char: ' ', Width: 1}}}}
		ok, err := v.Record(visualstore.View{Segment: 1, Point: point(i), Screen: screen})
		must(t, err)
		if !ok {
			t.Fatal("coalesced")
		}
		if i%commitEvery == 0 || i == n {
			must(t, v.Event(visualstore.Event{Segment: 1, Point: point(i), Kind: visualstore.EventMarker, Data: fmt.Sprint(i)}))
			must(t, v.Fidelity(visualstore.FidelityRecord{Segment: 1, Point: point(i), Finding: fidelity.Finding{Class: "fixture", Kind: fidelity.KindPoint, Detail: fmt.Sprint(i)}}))
			must(t, v.Commit(visualtime.Cutoff{Segment: 1, Source: point(i), Presentations: uint64(i), Events: uint64((i + commitEvery - 1) / commitEvery)}))
		}
		if i == ackAt {
			r, err := visualstore.OpenReader(filepath.Join(src, "visual"))
			must(t, err)
			snap, err := r.Snapshot(context.Background())
			must(t, err)
			must(t, r.Close())
			ack = visualcapture.Acknowledged{Format: visualstore.Format, Views: snap.ViewSeq, Chain: snap.Chain, SourceEnds: map[int]visualtime.Time{1: point(i).At}, Segment: 1}
		}
	}
	if complete {
		must(t, v.Complete(visualtime.Cutoff{Segment: 1, Source: point(n + 1), SourceComplete: true, Presentations: uint64(n), Events: uint64((n + commitEvery - 1) / commitEvery)}))
		ack.Complete = true
		ack.SourceEnds[1] = point(n + 1).At
	}
	must(t, v.Close())
	must(t, visualcapture.WriteAcknowledged(filepath.Join(src, visualcapture.AcknowledgedName), ack))
	status := "interrupted"
	if complete {
		status = "stopped"
	}
	must(t, os.WriteFile(filepath.Join(src, "state.json"), []byte(fmt.Sprintf(`{"id":%q,"target":{"backend":"fake","instance":"test"},"format":"visual/1","status":%q,"pending":"","segments":[{"path":"visual"}]}`, run3ID, status)), 0600))
	must(t, os.WriteFile(filepath.Join(src, "launch.json"), []byte(`{"target":{"backend":"fake","instance":"test"}}`), 0600))
	if activity {
		a, err := activitystore.Create(filepath.Join(src, activityDirName), id)
		must(t, err)
		t.Cleanup(func() { _ = a.Close() })
		var saved []byte
		for i := 1; i <= 5; i++ {
			_, err = a.Append(int64(i), activitystore.Record{ID: fmt.Sprint(i), Envelope: []byte(fmt.Sprintf(`{"row":%d}`, i)), ReceivedAt: time.Unix(1700000000, int64(i)), Placement: visualtime.Placement{Precision: activitystore.PrecisionUnknown}})
			must(t, err)
			must(t, a.Commit())
			if i == 3 {
				saved, err = os.ReadFile(filepath.Join(src, activityAckName))
				must(t, err)
			}
		}
		must(t, a.Close())
		if !complete {
			must(t, os.WriteFile(filepath.Join(src, activityAckName), saved, 0600))
		}
	}
	return src, filepath.Join(root, "fixture.areel")
}

func checkVerified(t *testing.T, dst string, kind Kind) Report {
	t.Helper()
	r, err := Verify(context.Background(), dst, options().Limits)
	must(t, err)
	if r.Status != StatusOK || r.Kind != kind {
		t.Fatalf("verify: %s %s %s %s", r.Status, r.Kind, r.FailedItem, r.Reason)
	}
	return r
}
func TestInterruptedPrefixAndReaderOracle(t *testing.T) {
	src, dst := run3Fixture(t, 30, 20, false, true)
	r, err := Seal(context.Background(), Source{src}, Destination{dst}, recoveryOptions())
	must(t, err)
	rep := checkVerified(t, dst, KindInterrupted)
	ack, err := visualcapture.LoadAcknowledged(filepath.Join(src, visualcapture.AcknowledgedName))
	must(t, err)
	aa, err := activitystore.LoadAcknowledged(filepath.Join(src, activityDirName))
	must(t, err)
	m := r.Manifest
	if m.Views.Views != 20 || !bytes.Equal(m.Views.Chain, ack.Chain) || m.Activity.Rows != 3 || !bytes.Equal(m.Activity.Chain, aa.Chain) || m.Recovery.DroppedViews != 10 || m.Recovery.DroppedActivityRows != 2 || m.Recovery.SourceVisualStatus != "incomplete" {
		t.Fatalf("%+v recovery=%+v", m, m.Recovery)
	}
	if rep.Visual.Prefix.Views != rep.Visual.Counts.Views {
		t.Fatal(rep.Visual)
	}
	salvage := filepath.Join(t.TempDir(), "oracle")
	sr, err := visualstore.SalvageWith(context.Background(), filepath.Join(src, "visual"), salvage, options().Limits, visualstore.SalvageOptions{MaxViews: 20})
	must(t, err)
	if sr.Copied.Views != 20 {
		t.Fatal(sr)
	}
	plain, err := visualstore.OpenReader(salvage)
	must(t, err)
	defer plain.Close()
	sealed, err := visualstore.OpenSealed(dst)
	must(t, err)
	defer sealed.Close()
	assertReaderEqual(t, plain, sealed)
	ar, err := activitystore.OpenSealed(dst)
	must(t, err)
	defer ar.Close()
	as, err := ar.Snapshot(context.Background())
	must(t, err)
	rows, err := ar.Events(context.Background(), as, 1, 100)
	must(t, err)
	if len(rows) != 3 {
		t.Fatal(rows)
	}
	entries, err := os.ReadDir(filepath.Dir(dst))
	must(t, err)
	if len(entries) != 2 {
		t.Fatalf("scratch remains: %v", entries)
	}
	_, err = RemoveSource(context.Background(), Removal{RecordingDir: src, SealedPath: dst, Limits: options().Limits, ActivityLimits: options().ActivityLimits})
	if !errors.Is(err, ErrTail) {
		t.Fatalf("interrupted tail removed: %v", err)
	}
}
func assertReaderEqual(t *testing.T, a, b *visualstore.Reader) {
	t.Helper()
	ctx := context.Background()
	sa, err := a.Snapshot(ctx)
	must(t, err)
	sb, err := b.Snapshot(ctx)
	must(t, err)
	if !reflect.DeepEqual(sa, sb) {
		t.Fatalf("Snapshot\n%+v\n%+v", sa, sb)
	}
	for from := int64(1); from <= sa.ViewSeq; from += 7 {
		av, err := a.Views(ctx, sa, 1, from, 7)
		must(t, err)
		bv, err := b.Views(ctx, sb, 1, from, 7)
		must(t, err)
		if !reflect.DeepEqual(av, bv) {
			t.Fatal("Views")
		}
		for _, view := range av {
			x, err := a.ViewAt(ctx, sa, 1, view.Point.At)
			must(t, err)
			y, err := b.ViewAt(ctx, sb, 1, view.Point.At)
			must(t, err)
			if !reflect.DeepEqual(x, y) {
				t.Fatal("ViewAt")
			}
			xs, err := a.Screen(ctx, sa, view.ScreenID)
			must(t, err)
			ys, err := b.Screen(ctx, sb, view.ScreenID)
			must(t, err)
			if !reflect.DeepEqual(xs, ys) {
				t.Fatal("Screen")
			}
		}
	}
	for from := int64(1); from <= 10; from += 2 {
		ae, err := a.Events(ctx, sa, 1, from, 2)
		must(t, err)
		be, err := b.Events(ctx, sb, 1, from, 2)
		must(t, err)
		af, err := a.Fidelity(ctx, sa, 1, from, 2)
		must(t, err)
		bf, err := b.Fidelity(ctx, sb, 1, from, 2)
		must(t, err)
		if !reflect.DeepEqual(ae, be) || !reflect.DeepEqual(af, bf) {
			t.Fatal("Events/Fidelity")
		}
	}
}

func TestInterruptedVariants(t *testing.T) {
	for _, variant := range []string{"truncated", "damaged", "complete_ack", "no_prefix", "stopped", "recording", "paused", "complete_kind", "interior_ack", "activity_damaged"} {
		t.Run(variant, func(t *testing.T) {
			complete := variant == "complete_ack"
			src, dst := run3Fixture(t, 30, 30, complete, true)
			if complete {
				editState(t, src, "status", "interrupted")
			}
			opts := recoveryOptions()
			wantViews := int64(30)
			status := "incomplete"
			refused := false
			switch variant {
			case "truncated":
				editAck(t, src, func(a *visualcapture.Acknowledged) { a.Views = 40; a.Chain = bytes.Repeat([]byte{1}, 32) })
				status = "truncated"
			case "damaged":
				path := filepath.Join(src, "visual", visualstore.DatabaseName)
				copy := filepath.Join(t.TempDir(), visualstore.DatabaseName)
				copyFixtureDB(t, path, copy)
				flipPayload(t, copy, "SELECT payload FROM view_blocks WHERE first_seq=21", "UPDATE view_blocks SET payload=? WHERE first_seq=21")
				copyFixtureDB(t, copy, path)
				status = "damaged"
				wantViews = 20
			case "complete_ack":
				status = "ok"
			case "no_prefix":
				mutate(t, filepath.Join(src, "visual", visualstore.DatabaseName), "UPDATE view_blocks SET payload=x'80' WHERE first_seq=1")
				refused = true
			case "stopped", "recording", "paused":
				editState(t, src, "status", variant)
				refused = true
			case "complete_kind":
				opts = options()
				refused = true
			case "interior_ack":
				editAck(t, src, func(a *visualcapture.Acknowledged) { a.Views = 15 })
				refused = true
			case "activity_damaged":
				mutate(t, filepath.Join(src, activityDirName, activitystore.DatabaseName), "UPDATE activity SET envelope='bad' WHERE seq=3")
			}
			// SQLite read-only doctors may establish SHM, as in Runs 1–2.
			// Eligibility must leave the destination's entries unchanged.
			before, err := os.ReadDir(filepath.Dir(dst))
			must(t, err)
			result, err := Seal(context.Background(), Source{src}, Destination{dst}, opts)
			if refused {
				if !errors.Is(err, ErrRefused) {
					t.Fatalf("expected refusal: %v", err)
				}
				after, e := os.ReadDir(filepath.Dir(dst))
				must(t, e)
				if len(before) != len(after) {
					t.Fatal("refusal created destination artifacts")
				}
				for i := range before {
					if before[i].Name() != after[i].Name() {
						t.Fatal("refusal changed destination entries")
					}
				}
				return
			}
			must(t, err)
			checkVerified(t, dst, KindInterrupted)
			if result.Manifest.Views.Views != wantViews || result.Manifest.Recovery.SourceVisualStatus != status {
				t.Fatalf("%+v", result.Manifest)
			}
			if variant == "damaged" && result.Manifest.Recovery.SourceFailedItem == "" {
				t.Fatal("missing failed item")
			}
			if variant == "activity_damaged" && result.Manifest.Activity.Rows != 2 {
				t.Fatal("activity did not stop before damage")
			}
			assertInterruptedReaderOracle(t, src, dst, wantViews)
		})
	}
}

func TestKindManifestValidation(t *testing.T) {
	src, dst := run3Fixture(t, 10, 10, true, false)
	result := build(t, src, dst)
	checkVerified(t, dst, KindComplete)
	if result.Manifest.Kind != KindComplete || result.Manifest.Recovery != nil {
		t.Fatal(result.Manifest)
	}
	for _, variant := range []string{"missing", "unknown", "complete_recovery"} {
		t.Run(variant, func(t *testing.T) {
			path := copySeal(t, dst)
			data, _, err := manifestBytes(result.Manifest)
			must(t, err)
			var m map[string]any
			must(t, json.Unmarshal(data, &m))
			switch variant {
			case "missing":
				delete(m, "kind")
			case "unknown":
				m["kind"] = "other"
			case "complete_recovery":
				m["recovery"] = RecoveryPart{}
			}
			data, err = json.Marshal(m)
			must(t, err)
			mutate(t, path, "UPDATE seal SET manifest=?,digest=?", string(data), digest(data))
			r, err := Verify(context.Background(), path, options().Limits)
			must(t, err)
			if r.Status != StatusDamaged || r.FailedItem != "seal" {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestInterruptedCancellationAndPanicCleanup(t *testing.T) {
	src, _ := run3Fixture(t, 30, 20, false, true)
	for _, part := range []string{"salvage", "view_blocks"} {
		t.Run(part, func(t *testing.T) {
			dst := destination(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fired := false
			probe := visualProbeContext{Context: ctx, probe: func(name string, n int64) error {
				if name == part && n > 0 {
					fired = true
					cancel()
				}
				return ctx.Err()
			}}
			_, err := Seal(probe, Source{src}, Destination{dst}, recoveryOptions())
			if !fired || !errors.Is(err, context.Canceled) {
				t.Fatalf("fired=%v err=%v", fired, err)
			}
			assertEmpty(t, dst)
			if _, err = os.Stat(src); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, stage := range []string{"salvage", "visual", "source_recheck"} {
		t.Run("panic_"+stage, func(t *testing.T) {
			dst := destination(t)
			fired := false
			func() {
				defer func() {
					if p := recover(); p != nil {
						fired = true
					}
				}()
				ctx := context.WithValue(context.Background(), hookKey{}, func(part string, n int64) error {
					if part == stage {
						panic("test")
					}
					return nil
				})
				_, _ = Seal(ctx, Source{src}, Destination{dst}, recoveryOptions())
			}()
			if !fired {
				t.Fatal("panic hook did not fire")
			}
			assertEmpty(t, dst)
		})
	}
}

func TestOpenEnvelope(t *testing.T) {
	src, dst := run3Fixture(t, 30, 30, true, true)
	build(t, src, dst)
	f, err := Open(dst)
	must(t, err)
	if f.Path != dst || f.Kind() != KindComplete {
		t.Fatal(f)
	}
	for name, data := range f.SourceFiles {
		want, err := os.ReadFile(filepath.Join(src, name))
		must(t, err)
		if !bytes.Equal(data, want) {
			t.Fatal(name)
		}
	}
	must(t, f.Close())
	for _, variant := range []string{"wal", "mode", "temp", "manifest", "source_digest"} {
		t.Run(variant, func(t *testing.T) {
			path := copySeal(t, dst)
			switch variant {
			case "wal":
				must(t, os.WriteFile(path+"-wal", nil, 0600))
			case "mode":
				must(t, os.Chmod(path, 0644))
			case "temp":
				candidate := filepath.Join(filepath.Dir(path), ".fixture.areel.tmp-0123456789abcdef")
				must(t, os.Rename(path, candidate))
				path = candidate
			case "manifest":
				mutate(t, path, "UPDATE seal SET digest=zeroblob(32)")
			case "source_digest":
				mutate(t, path, "UPDATE source_files SET data=x'20' WHERE name='state.json'")
			}
			f, err := Open(path)
			if err == nil {
				f.Close()
				t.Fatal("Open accepted", variant)
			}
		})
	}
}

// Report only: the fixture is cheap (20 distinct tiny screens), with 200k
// authenticated presentations. Open must never scan presentation payloads.
func TestOpen200kTiming(t *testing.T) {
	src, dst := run3Fixture(t, 200000, 200000, true, false)
	build(t, src, dst)
	started := time.Now()
	f, err := Open(dst)
	elapsed := time.Since(started)
	must(t, err)
	defer f.Close()
	if f.Manifest.Views.Views != 200000 {
		t.Fatal(f.Manifest.Views)
	}
	t.Logf("Open 200000 views: %s (target <50ms, report-only)", elapsed)
}

func TestInterruptedRecoveryStrictness(t *testing.T) {
	src, dst := run3Fixture(t, 30, 20, false, false)
	r, err := Seal(context.Background(), Source{src}, Destination{dst}, recoveryOptions())
	must(t, err)
	data, _, err := manifestBytes(r.Manifest)
	must(t, err)
	for _, bad := range []string{
		strings.Replace(string(data), `"recovery":{`, `"recovery":{"extra":0,`, 1),
		strings.Replace(string(data), `"sourcePending":"stop"`, `"sourcePending":null`, 1),
		strings.Replace(string(data), `"droppedViews":10`, `"droppedViews":9`, 1),
		strings.Replace(string(data), `"recoverer":"areel 0.1.0"`, `"recoverer":""`, 1),
	} {
		if _, err := parseManifest([]byte(bad)); err == nil {
			t.Fatal("accepted bad recovery", bad)
		}
	}
}

func assertInterruptedReaderOracle(t *testing.T, src, dst string, n int64) {
	t.Helper()
	oracle := filepath.Join(t.TempDir(), "oracle")
	report, err := visualstore.SalvageWith(context.Background(), filepath.Join(src, "visual"), oracle, options().Limits, visualstore.SalvageOptions{MaxViews: n})
	must(t, err)
	if report.Copied.Views != n {
		t.Fatalf("oracle copied %d, want %d", report.Copied.Views, n)
	}
	a, err := visualstore.OpenReader(oracle)
	must(t, err)
	defer a.Close()
	b, err := visualstore.OpenSealed(dst)
	must(t, err)
	defer b.Close()
	assertReaderEqual(t, a, b)
	f, err := Open(dst)
	must(t, err)
	defer f.Close()
	if f.Kind() != KindInterrupted {
		t.Fatal(f.Kind())
	}
	activity, err := activitystore.OpenSealed(dst)
	must(t, err)
	defer activity.Close()
	sel, err := activity.Snapshot(context.Background())
	must(t, err)
	if sel.Seq != f.Manifest.Activity.Rows || !bytes.Equal(sel.Chain, f.Manifest.Activity.Chain) {
		t.Fatal("activity prefix selection")
	}
	rows, err := activity.Events(context.Background(), sel, 1, 100)
	must(t, err)
	if int64(len(rows)) != sel.Seq {
		t.Fatal("activity prefix rows")
	}
	sourceActivity, err := activitystore.OpenReader(filepath.Join(src, activityDirName))
	must(t, err)
	defer sourceActivity.Close()
	wantRows, err := sourceActivity.Events(context.Background(), sel, 1, 100)
	must(t, err)
	if !reflect.DeepEqual(wantRows, rows) {
		t.Fatal("activity reader prefix differs")
	}
}
