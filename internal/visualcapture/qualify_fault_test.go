//go:build linux && qualify

package visualcapture

// V8 Run 4: Tier A fault injection (A.fault.F1–F10, plan Task 4, C5) and H4
// (Verify beside a live writer). Every target is a Run 2 Tier A worker
// process; faults go through the unexported writer/terminal seams, the helper
// proxy (F5), orchestrator signals to processes this test started, at-rest
// damage inside the artifact directory (F7) and a real ENOSPC on a private
// tmpfs inside an unprivileged user+mount namespace (F8). Victims are judged
// by the production `areel doctor`/`areel recover` (built into OUT/bin, CLI
// default limits) on a visual/1 library layout under the artifact directory;
// survivors by exported visualstore.Verify as in A.matrix.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/qualify"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

const (
	faultPrefix = "A.fault."
	// h4Scenario is not in plan.go's PlannedCells (Run 7 may add it); the
	// report lists it as an unplanned scenario and judges its cell by B17.
	h4Scenario = "A.fault.H4"

	// proxyEnv/proxyFlipEnv switch this test binary into helper-proxy mode
	// (F5) in init, before the testing framework runs.
	proxyEnv     = "AREEL_QUALIFY_HELPER_PROXY"
	proxyFlipEnv = "AREEL_QUALIFY_PROXY_FLIP"
	// nsInitEnv names the F8 namespace spec (TestQualifyWorker's ns mode).
	nsInitEnv = "AREEL_QUALIFY_NSINIT"

	faultDeadline = 10 * time.Minute
	tmpfsMagic    = 0x01021994
)

// faultName and faultOutcome are the plan's Task 4 fault table: the fault and
// its "Required outcome" text, verbatim (criterion 1).
var faultName = map[string]string{
	"F1":  "Crash after a store commit, before ack (self-SIGKILL in OnBatch-adjacent wrapped Commit return)",
	"F2":  "Crash mid-transaction (parent SIGKILL at a random time during a W3 burst) and crash before commit",
	"F3":  "Missing helper at 32 concurrent starts (one target points at a nonexistent path)",
	"F4":  "Corrupt helper binary (a copy truncated to half, chmod 0700)",
	"F5":  "Helper protocol corruption mid-stream: the Helper path is the test binary in \"helper-proxy\" mode, exec'ing the real helper and flipping one byte in the Nth reply",
	"F6":  "Helper killed (SIGKILL) mid-stream",
	"F7":  "At-rest corruption of completed stores: flip a row-payload byte; truncate the WAL tail of an uncheckpointed store; corrupt the ack JSON; delete the ack record",
	"F8":  "Real ENOSPC",
	"F9":  "Slow sink: wrapped Commit delay of 200 ms, 1 s, then 20 s (> StallAge) on one of 10 targets",
	"F10": "Cancellation mid-stream without SetFinal, by closing the capture's input (Capture.Close() refuses after Start), at 1 and 32 targets simultaneously; SIGTERM of a worker",
}

var faultOutcome = map[string]string{
	"F1":  "Survivors complete `ok`. The victim's doctor is `incomplete`, reports the unacknowledged tail, and has the ack `present`. A resume via `visualcapture.New` on it is refused as a `create`-stage error. Recover seals the acknowledged verified prefix as interrupted, reports dropped tail counts, and retains the tail source.",
	"F2":  "Doctor `incomplete`, ack `present`. The committed prefix equals the ack or the ack plus an unacknowledged tail, never less.",
	"F3":  "That `New` refuses (non-storage, nothing created: no store dir, no ack record). The other 31 run.",
	"F4":  "As F3: that `New` refuses (non-storage, nothing created: no store dir, no ack record). The other 31 run.",
	"F5":  "Plain capture failure, not a storage failure. No completion. The committed prefix verifies. Survivors are unaffected.",
	"F6":  "Capture failure within ≤ StallAge + 1 s, or immediately. No completion. The prefix verifies.",
	"F7":  "Doctor `damaged` / `truncated` / error-with-no-verdict / error-with-no-verdict respectively. `visualstore` Reader refuses (`Screen` verification or `ErrSelection`). Never `ok`.",
	"F8":  "a typed `publish` StorageError, and Failed; nothing acknowledged beyond what is durable; after `mount -o remount,size=larger` (inside the namespace), the doctor is `incomplete` and clean with the ack `present`; survivors on the normal disk are unaffected.",
	"F9":  "200 ms/1 s: no loss, no backlog, and the victim's commit latency is reported. 20 s: a typed `backlog` StorageError is visible; the stalled batch is never acknowledged; the prefix is `ok` up to the ack. Survivors meet B19. Labelled \"simulated by injected commit latency; not a slow block device\".",
	"F10": "Every store is `incomplete` (never complete). Time-to-close p99 is measured and reported. The prefix verifies.",
}

// faultSpans are the logical spans of the W3 (even) and W4 (odd) targets. The
// survivors' spans keep them feeding through each fault window at
// accelerated pace; victims fault early so their stores stay small.
type faultSpans struct{ w3, w4 int64 }

var (
	spans10   = faultSpans{4 * qualify.Minute, 45 * qualify.Minute}
	spansF9   = faultSpans{8 * qualify.Minute, 90 * qualify.Minute}
	spans32   = faultSpans{1 * qualify.Minute, 12 * qualify.Minute}
	spansF10  = faultSpans{2 * qualify.Minute, 20 * qualify.Minute}
	spansH4   = faultSpans{10 * qualify.Minute, 0}
	spanF7Vic = 1 * qualify.Minute // F7 victims: W4, complete before the damage
)

// ---------------------------------------------------------------------------
// Helper proxy (F5)

func init() {
	real := os.Getenv(proxyEnv)
	if real == "" {
		return
	}
	flip, _ := strconv.Atoi(os.Getenv(proxyFlipEnv))
	os.Exit(helperProxy(real, flip))
}

// helperProxy runs the real helper as its child and relays the readout
// protocol verbatim, except that the first byte of the reply to the flipAt-th
// reply-bearing request ('V', 'W' or 'D') is inverted. That byte is the most
// significant byte of the reply's column count, so the corruption is a
// contract violation the client can detect. Requests are parsed only to count
// them: the 12-byte "AVT1" start, 'F' + u32 length + data, 'R' + 8 bytes,
// and the payload-free 'P' version handshake, whose reply is never flipped.
func helperProxy(real string, flipAt int) int {
	cmd := exec.Command(real)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, proxyEnv+"=") && !strings.HasPrefix(kv, proxyFlipEnv+"=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	in, err := cmd.StdinPipe()
	if err != nil {
		return 1
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 1
	}
	if err := cmd.Start(); err != nil {
		return 1
	}
	var armed atomic.Bool
	relayed := make(chan struct{})
	go func() {
		defer close(relayed)
		buf := make([]byte, 64<<10)
		flipped := false
		for {
			n, err := out.Read(buf)
			if n > 0 {
				// The client reads each reply fully before its next request,
				// so the first bytes read after request flipAt was armed
				// belong to its reply.
				if !flipped && armed.Load() {
					buf[0] ^= 0xFF
					flipped = true
				}
				if _, werr := os.Stdout.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	r := bufio.NewReaderSize(os.Stdin, 64<<10)
	w := bufio.NewWriterSize(in, 64<<10)
	relay := func(n int64) error {
		_, err := io.CopyN(w, r, n)
		return err
	}
	replies := 0
	err = relay(12)
	for err == nil {
		var op byte
		if op, err = r.ReadByte(); err != nil {
			break
		}
		if op == 'V' || op == 'W' || op == 'D' {
			if replies++; replies == flipAt {
				armed.Store(true)
			}
		}
		if err = w.WriteByte(op); err != nil {
			break
		}
		switch op {
		case 'F':
			var size [4]byte
			if _, err = io.ReadFull(r, size[:]); err == nil {
				if _, err = w.Write(size[:]); err == nil {
					err = relay(int64(binary.BigEndian.Uint32(size[:])))
				}
			}
		case 'R':
			err = relay(8)
		case 'V', 'W', 'D', 'P': // no request payload; 'P' is the uncounted handshake
		default:
			_, err = io.Copy(w, r) // unknown framing: relay the rest verbatim
		}
		if err == nil {
			err = w.Flush()
		}
	}
	_ = w.Flush()
	_ = in.Close()
	<-relayed
	_ = cmd.Wait()
	return 0
}

// ---------------------------------------------------------------------------
// F8 namespace init

// nsSpec is what the orchestrator gives the F8 namespace init.
type nsSpec struct {
	Worker  string   `json:"worker"` // the child worker's spec file
	Rec     string   `json:"rec"`    // the tmpfs mount point: the victim's recording directory
	Size    int64    `json:"size"`
	Larger  int64    `json:"larger"`
	Areel   string   `json:"areel"`
	Library string   `json:"library"`
	ID      string   `json:"id"`
	CLIEnv  []string `json:"cliEnv"`
	Result  string   `json:"result"`
}

// nsResult is what the namespace init observed, written to the normal disk.
type nsResult struct {
	FSType            string         `json:"fsType"`
	Size              int64          `json:"size"`
	Larger            int64          `json:"larger"`
	ChildExit         string         `json:"childExit"`
	Holder            map[string]any `json:"holder"`
	FreeBeforeRemount int64          `json:"freeBeforeRemount"`
	FreeAfterRemount  int64          `json:"freeAfterRemount"`
	Inspection        inspection     `json:"inspection"`
	InspectErr        string         `json:"inspectError,omitempty"`
	DoneUnixNs        int64          `json:"doneUnixNs"`
}

// nsInit is the first process of F8's private user+mount namespace (uid and
// gid mapped to 0 inside, nothing privileged outside). It makes its mount
// tree private, mounts a size-limited tmpfs over the victim's recording
// directory, runs the capture worker as its child (sharing the orchestrator's
// control descriptors), then remounts the tmpfs larger and runs the
// production doctor and recover on the store before the namespace, and with
// it the tmpfs, disappears.
func nsInit(path string) error {
	ctl := os.NewFile(3, "qualify-ctl-out")
	send := func(m workerMsg) {
		b, _ := json.Marshal(m)
		_, _ = ctl.Write(append(b, '\n'))
	}
	fail := func(err error) error {
		send(workerMsg{Msg: "error", Error: err.Error()})
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fail(err)
	}
	var spec nsSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return fail(err)
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fail(fmt.Errorf("F8: making the mount tree private in the new mount namespace failed (needs CAP_SYS_ADMIN in an unprivileged user namespace): %w", err))
	}
	if err := syscall.Mount("tmpfs", spec.Rec, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, fmt.Sprintf("size=%d,mode=0700", spec.Size)); err != nil {
		return fail(fmt.Errorf("F8: mounting a size-limited tmpfs over %s inside the private user+mount namespace failed (needs tmpfs mounts in an unprivileged user namespace): %w", spec.Rec, err))
	}
	res := nsResult{Size: spec.Size, Larger: spec.Larger}
	var st syscall.Statfs_t
	if err := syscall.Statfs(spec.Rec, &st); err != nil || st.Type != tmpfsMagic {
		return fail(fmt.Errorf("F8: %s is not a tmpfs after the mount (type %#x, %v)", spec.Rec, st.Type, err))
	}
	res.FSType = "tmpfs"
	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestQualifyWorker$", "-test.count=1", "-test.timeout=4h")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, nsInitEnv+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "AREEL_QUALIFY_WORKER="+spec.Worker)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{ctl, os.NewFile(4, "qualify-ctl-in")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	holder, idErr := procIdentity(cmd.Process.Pid)
	waitErr := cmd.Wait()
	res.ChildExit = fmt.Sprint(waitErr)
	if idErr != nil {
		return fail(fmt.Errorf("F8 holder identity: %w", idErr))
	}
	res.Holder = holder
	res.FreeBeforeRemount = statfsFree(spec.Rec)
	if err := syscall.Mount("tmpfs", spec.Rec, "tmpfs", syscall.MS_REMOUNT|syscall.MS_NOSUID|syscall.MS_NODEV, fmt.Sprintf("size=%d,mode=0700", spec.Larger)); err != nil {
		return fail(fmt.Errorf("F8: remounting the tmpfs larger inside the namespace: %w", err))
	}
	res.FreeAfterRemount = statfsFree(spec.Rec)
	res.Inspection, err = inspectRecording(spec.Areel, spec.CLIEnv, spec.Library, spec.Rec, spec.ID, holder, "recording")
	if err != nil {
		res.InspectErr = err.Error()
	}
	res.DoneUnixNs = time.Now().UnixNano()
	b, err := json.Marshal(res)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(spec.Result, b, 0o600); err != nil {
		return fail(err)
	}
	send(workerMsg{Msg: "nsdone", UnixNs: res.DoneUnixNs})
	return nil
}

// ---------------------------------------------------------------------------
// Victim inspection: the production doctor and recover on a visual/1 library
// layout, the acknowledged record and the store's completion table.

type cliRecord struct {
	Report    doctorReport `json:"report"`
	Err       string       `json:"error,omitempty"`
	WallNs    int64        `json:"wallNs"`
	EndUnixNs int64        `json:"endUnixNs"`
}

func recordCLI(r cliRun) cliRecord {
	return cliRecord{Report: r.report, Err: r.err, WallNs: r.wall.Nanoseconds(), EndUnixNs: r.end.UnixNano()}
}

type inspection struct {
	Doctor         cliRecord           `json:"doctor"`
	Recovery       cliRecord           `json:"recover"`
	RecoveryVerify *recoverySealReport `json:"recoveryVerify,omitempty"`
	RecoveryErr    string              `json:"recoveryError,omitempty"`
	AckViews       int64               `json:"ackViews"`
	AckComplete    bool                `json:"ackComplete"`
	AckErr         string              `json:"ackError,omitempty"`
	// CompletionRows counts the store's completion table (-1: unknown).
	CompletionRows int64               `json:"completionRows"`
	NoDB           bool                `json:"noDatabase"`
	CompletionErr  string              `json:"completionError,omitempty"`
	Secondary      *visualstore.Report `json:"secondary,omitempty"`
	SecondaryErr   string              `json:"secondaryError,omitempty"`
	WALAtRest      int64               `json:"walAtRest"`
}

// writeTierAState writes the state and matching launch metadata used by
// doctor/recover (as A.recovery): the
// worker is the holder and the one segment was never launched (Tier A has no
// recorder or watcher process).
func writeTierAState(rec, id string, holder map[string]any, status string) error {
	state := map[string]any{
		"id": id, "target": map[string]string{"backend": "qualify", "instance": "tier-a", "name": "qualify", "address": ""},
		"status": status, "suppressed": false, "holder": holder,
		"segments": []map[string]any{{"launched": false, "number": 1, "path": StoreDirName, "started": time.Now().UTC().Format(time.RFC3339Nano)}},
		"events":   []any{}, "format": "visual/1",
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(rec, "state.json"), b, 0o600); err != nil {
		return err
	}
	launch, err := json.Marshal(map[string]any{"target": state["target"], "token": "", "sealName": "{id}"})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(rec, "launch.json"), launch, 0o600)
}

func walBytes(rec string) int64 {
	if info, err := os.Lstat(filepath.Join(rec, StoreDirName, visualstore.DatabaseName+"-wal")); err == nil {
		return info.Size()
	}
	return 0
}

// completionRows counts the completion table on a read-only connection
// (0 with noDB when there is no database at all).
func completionRows(rec string) (int64, bool, string) {
	path := filepath.Join(rec, StoreDirName, visualstore.DatabaseName)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return 0, true, ""
	}
	db, err := readOnlyDB(path)
	if err != nil {
		return -1, false, err.Error()
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow(`SELECT count(*) FROM completion`).Scan(&n); err != nil {
		return -1, false, err.Error()
	}
	return n, false, ""
}

func unverifiedCLI(r cliRecord) bool {
	return strings.Contains(r.Err, visualstore.ErrLimit.Error()) || (r.Report.Visual != nil && r.Report.Visual.Status == visualstore.StatusUnverified)
}

// inspectRecording writes state.json, then runs `areel doctor` and `areel
// recover` in place with CLI default limits. All directory diagnostics
// (acknowledgement, completion rows, WAL and doctor) precede recover. When the doctor gave no
// verdict within its limits, exported visualstore.Verify with
// visualstore.DefaultLimits runs as a labelled secondary diagnostic.
func inspectRecording(bin string, cenv []string, lib, rec, id string, holder map[string]any, status string) (inspection, error) {
	in := inspection{CompletionRows: -1}
	in.WALAtRest = walBytes(rec)
	if err := writeTierAState(rec, id, holder, status); err != nil {
		return in, err
	}
	in.Doctor = recordCLI(runAreel(bin, cenv, "doctor", id, "--library", lib, "--json"))
	ack, ackErr := LoadAcknowledged(filepath.Join(rec, AcknowledgedName))
	if ackErr != nil {
		in.AckErr = ackErr.Error()
	} else {
		in.AckViews, in.AckComplete = ack.Views, ack.Complete
	}
	in.CompletionRows, in.NoDB, in.CompletionErr = completionRows(rec)
	if unverifiedCLI(in.Doctor) && ackErr == nil {
		rep, err := visualstore.Verify(context.Background(), filepath.Join(rec, StoreDirName), ack.Expectation(), visualstore.DefaultLimits())
		in.Secondary = &rep
		if err != nil {
			in.SecondaryErr = err.Error()
		}
	}
	in.Recovery = recordCLI(runAreel(bin, cenv, "recover", id, "--library", lib, "--json"))
	in.verifyRecovery(lib, rec, id)
	return in, nil
}

// verdict is the doctor's visual report when it gave one, else the secondary
// diagnostic's, with how it was obtained.
func (in inspection) verdict() (*visualstore.Report, string) {
	if v := in.Doctor.Report.Visual; v != nil && v.Status != "" && v.Status != visualstore.StatusUnverified {
		return v, "production areel doctor (CLI default limits)"
	}
	if in.Secondary != nil && in.SecondaryErr == "" {
		return in.Secondary, "secondary diagnostic: exported visualstore.Verify with visualstore.DefaultLimits (the doctor gave no verdict: " + in.Doctor.Err + ")"
	}
	return nil, fmt.Sprintf("no verdict: doctor %q; secondary %q", in.Doctor.Err, in.SecondaryErr)
}

// prefixVerifies is "the committed prefix verifies": a clean incomplete
// verdict with the acknowledged prefix present and no fewer views than
// acknowledged. Loss is judged against the acknowledged record only; a screen
// interpreted but not yet flushed was never acknowledged (N7).
func (in inspection) prefixVerifies(who string) []string {
	rep, via := in.verdict()
	switch {
	case in.AckErr != "":
		return []string{who + ": acknowledged record: " + in.AckErr}
	case rep == nil:
		return []string{who + ": " + via}
	case rep.Status != visualstore.StatusIncomplete || rep.FailedItem != "":
		return []string{fmt.Sprintf("%s: verdict %s (failed item %q: %s) via %s", who, rep.Status, rep.FailedItem, rep.Reason, via)}
	case rep.Acknowledged != visualstore.AcknowledgedPresent:
		return []string{fmt.Sprintf("%s: acknowledged prefix %s (%s) via %s", who, rep.Acknowledged, rep.AcknowledgedReason, via)}
	case rep.Counts.Views < in.AckViews:
		return []string{fmt.Sprintf("%s: store holds %d views < %d acknowledged via %s", who, rep.Counts.Views, in.AckViews, via)}
	}
	return nil
}

// falseSuccess lists every "no false success" violation: a completion row, an
// acknowledged complete:true, a doctor reporting ok or completion, a recover
// claiming an ok or complete source. preComplete (F7) skips the completion
// row and record clauses, which exist by construction in a store that
// completed before it was damaged.
func (in inspection) falseSuccess(who string, preComplete bool) []string {
	var out []string
	d := in.Doctor.Report
	if in.Recovery.Report.Sealed && (in.RecoveryErr != "" || in.RecoveryVerify == nil || in.RecoveryVerify.Status != "ok") {
		out = append(out, who+": "+in.recoverySummary())
	}
	if d.ArtifactStatus == visualstore.StatusOK {
		out = append(out, who+": doctor artifactStatus ok")
	}
	// Report.Complete means a completion record exists (verified or not);
	// status ok is the verified completion. A pre-existing record (F7) is
	// evidence, not a false success; its verdict must still never be ok.
	if d.Visual != nil && (d.Visual.Status == visualstore.StatusOK || d.Visual.Complete && !preComplete) {
		out = append(out, fmt.Sprintf("%s: doctor visual status %s, completion record %t", who, d.Visual.Status, d.Visual.Complete))
	}
	if d.Completion != nil && d.Completion.Complete {
		out = append(out, who+": doctor completion complete ("+d.Completion.Reason+")")
	}
	if in.Recovery.Report.Kind == "complete" {
		out = append(out, who+": recover claimed complete")
	}

	if in.Secondary != nil && (in.Secondary.Status == visualstore.StatusOK || in.Secondary.Complete && !preComplete) {
		out = append(out, fmt.Sprintf("%s: secondary Verify status %s, completion record %t", who, in.Secondary.Status, in.Secondary.Complete))
	}
	if !preComplete {
		switch {
		case in.CompletionRows < 0:
			out = append(out, who+": completion table unreadable: "+in.CompletionErr)
		case in.CompletionRows > 0:
			out = append(out, fmt.Sprintf("%s: %d completion row(s)", who, in.CompletionRows))
		}
		if in.AckErr == "" && in.AckComplete {
			out = append(out, who+": acknowledged record complete:true")
		}
	}
	return out
}

func (in inspection) summary() string {
	rep, via := in.verdict()
	status := "none"
	var views int64 = -1
	if rep != nil {
		status, views = rep.Status+"/"+rep.Acknowledged, rep.Counts.Views
	}
	recover := in.recoverySummary()
	short := via
	if i := strings.IndexByte(short, '('); i > 0 {
		short = strings.TrimSpace(short[:i])
	}
	return fmt.Sprintf("doctor artifact %q, verdict %s via %s, store %d views, acknowledged %d (complete %t), unacknowledged %d; recover %s",
		in.Doctor.Report.ArtifactStatus, status, short, views, in.AckViews, in.AckComplete, in.Doctor.Report.UnacknowledgedViews, recover)
}

// ---------------------------------------------------------------------------
// Fault cells

type victim struct {
	i       int
	variant string
	status  string // state.json status written for the doctor
	msg     workerMsg
	killed  bool // the worker produced no result (killed)
	ended   string
	inject  int64 // wall time of the fault (Unix ns)
	in      inspection
	inErr   error
	notes   []string
	ns      *nsResult
	resume  error // F1
	reader  string
	readerN int64
	refused bool
}

type faultCell struct {
	q       qualifyEnv
	env     []string
	cenv    []string
	bin     string
	id      string
	pc      qualify.PlannedCell
	dir     string
	lib     string
	specs   []workerSpec
	holders []map[string]any
	casts   map[string]string
	victims map[int]*victim
	// driveErrs are the orchestration failures per target, apart from the
	// capture errors a victim is expected to report in its result.
	driveErrs []string

	mu     sync.Mutex
	w0, w1 int64 // the fault window (Unix ns)

	// win is the survivors' B19 window evaluation, computed once (f9 states
	// it in its required-outcome evidence before finish uses it).
	win *windowEval
}

// windowEval is windowB19's result; summary states the B19 verdict in words.
type windowEval struct {
	metrics  []qualify.Metric
	check    qualify.Check
	findings []qualify.Finding
	summary  string
}

// window evaluates the survivors' B19 over the fault window once.
func (fc *faultCell) window(run *cellRun) *windowEval {
	if fc.win == nil {
		fc.win = fc.windowB19(run)
	}
	return fc.win
}

func newFaultCell(q qualifyEnv, env, cenv []string, bin, scenarioDir, id string, pc qualify.PlannedCell, sp faultSpans) (*faultCell, error) {
	fc := &faultCell{q: q, env: env, cenv: cenv, bin: bin, id: id, pc: pc, casts: map[string]string{}, victims: map[int]*victim{},
		specs: make([]workerSpec, pc.Targets), holders: make([]map[string]any, pc.Targets), driveErrs: make([]string, pc.Targets)}
	fc.dir = filepath.Join(scenarioDir, "stores", cellDirName(pc.Key()))
	fc.lib = filepath.Join(fc.dir, "lib")
	for _, d := range []string{fc.dir, fc.lib, filepath.Join(fc.lib, "recordings"), filepath.Join(fc.dir, "casts")} {
		if err := privateDir(q.root, d); err != nil {
			return nil, err
		}
	}
	for i := range fc.specs {
		workload, span := qualify.W3, sp.w3
		if i%2 == 1 {
			workload, span = qualify.W4, sp.w4
		}
		if err := fc.target(i, workload, span); err != nil {
			return nil, err
		}
	}
	return fc, nil
}

// cast pre-generates (once per workload and span) a stream every target of
// the cell with that workload reads.
func (fc *faultCell) cast(workload string, span int64) (string, qualify.Expectation, error) {
	stream, x, err := qualify.Generate(qualify.Spec{Workload: workload, SpanNs: span, Seed: matrixSeed("fault:" + workload)})
	if err != nil {
		return "", x, err
	}
	key := fmt.Sprintf("%s-%d", workload, span/qualify.Second)
	if path, ok := fc.casts[key]; ok {
		return path, x, nil
	}
	path := filepath.Join(fc.dir, "casts", key+".cast")
	if err := writeCast(stream, path); err != nil {
		return "", x, err
	}
	fc.casts[key] = path
	return path, x, nil
}

// target (re)defines target i's spec: its own library recording directory,
// named by a 32-hex recording ID that is also the store identity.
func (fc *faultCell) target(i int, workload string, span int64) error {
	cast, x, err := fc.cast(workload, span)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s%s/%s/t%02d", faultPrefix, fc.id, fc.pc.Key(), i)))
	id := hex.EncodeToString(sum[:16])
	rec := filepath.Join(fc.lib, "recordings", id)
	if err := privateDir(fc.q.root, rec); err != nil {
		return err
	}
	fc.specs[i] = workerSpec{
		Cell: fc.pc.Key(), Target: i, Workload: workload, Cols: 80, Rows: 24, SpanNs: span, Seed: matrixSeed("fault:" + workload),
		WarmupNs: x.WarmupNs, Cast: cast, Rec: rec, Helper: fc.q.helper, RecordingID: id,
		SampleEveryNs: max(span/30, Defaults().FlushAge.Nanoseconds()),
		Result:        filepath.Join(fc.dir, fmt.Sprintf("t%02d.result.json", i)),
		Samples:       filepath.Join(fc.dir, fmt.Sprintf("t%02d.samples.jsonl", i)),
	}
	return nil
}

func (fc *faultCell) victim(i int, variant, status string) *victim {
	v := &victim{i: i, variant: variant, status: status}
	fc.victims[i] = v
	return v
}

func (fc *faultCell) survivors() []int {
	var out []int
	for i := range fc.specs {
		if fc.victims[i] == nil {
			out = append(out, i)
		}
	}
	return out
}

// mark widens the fault window to include at.
func (fc *faultCell) mark(at int64) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.w0 == 0 || at < fc.w0 {
		fc.w0 = at
	}
	fc.w1 = max(fc.w1, at)
}

// startTarget starts a worker and records its process identity (the holder
// identity the doctor checks for absence).
func (fc *faultCell) startTarget(i int, spec workerSpec, specPath, logPath string) (*workerProc, error) {
	p, err := startWorker(fc.env, spec, specPath, logPath)
	if err != nil {
		return nil, err
	}
	id, err := procIdentity(p.cmd.Process.Pid)
	if err != nil {
		p.kill()
		return nil, fmt.Errorf("holder identity: %w", err)
	}
	fc.holders[i] = id
	return p, nil
}

func (fc *faultCell) run(ctl workerControl) (*cellRun, error) {
	if ctl.start == nil {
		ctl.start = fc.startTarget
	}
	drive := ctl.drive
	ctl.drive = func(i int, p *workerProc, stop time.Time) (bool, string) {
		noResult, errText := drive(i, p, stop)
		fc.driveErrs[i] = errText
		return noResult, errText
	}
	return runWorkersWith(fc.env, fc.specs, fc.dir, faultDeadline, ctl)
}

// inspect runs the doctor/recover inspection of victim v after its worker
// exited.
func (fc *faultCell) inspect(v *victim) {
	spec := fc.specs[v.i]
	v.in, v.inErr = inspectRecording(fc.bin, fc.cenv, fc.lib, spec.Rec, spec.RecordingID, fc.holders[v.i], v.status)
}

func (fc *faultCell) readResult(i int) (*workerResult, error) {
	data, err := os.ReadFile(fc.specs[i].Result)
	if err != nil {
		return nil, err
	}
	var r workerResult
	return &r, json.Unmarshal(data, &r)
}

// ended describes how a worker process ended.
func ended(p *workerProc) string {
	if p.cmd.ProcessState == nil {
		return "not reaped"
	}
	if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return "signal " + ws.Signal().String()
	}
	return p.cmd.ProcessState.String()
}

// expectKilled waits for a self-kill report and the worker's exit by SIGKILL.
func expectKilled(p *workerProc, stop time.Time) (workerMsg, error) {
	m, err := p.expect("killing", time.Until(stop))
	if err != nil {
		p.kill()
		return m, err
	}
	<-p.exited
	if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		return m, fmt.Errorf("target %d was not SIGKILLed: %v", p.target, p.waitErr)
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// Common checks and metrics

// survivorsCheck verifies every survivor's store (exported Verify with
// DefaultLimits, eight at a time, as A.matrix).
func (fc *faultCell) survivorsCheck(run *cellRun) qualify.Check {
	surv := fc.survivors()
	if len(surv) == 0 {
		return check("survivors-complete-ok", "survivors (every non-faulted target) complete ok", nil, "no survivors: every target of this cell is faulted by design")
	}
	type res struct {
		rep visualstore.Report
		ack Acknowledged
		err error
	}
	out := make([]res, len(fc.specs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, i := range surv {
		if run.errs[i] != "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i].rep, out[i].ack, _, out[i].err = verifyTarget(fc.specs[i].Rec)
		}()
	}
	wg.Wait()
	var fails []string
	for _, i := range surv {
		r := out[i]
		switch {
		case run.errs[i] != "":
			fails = append(fails, fmt.Sprintf("t%d: %s", i, run.errs[i]))
		case r.err != nil:
			fails = append(fails, fmt.Sprintf("t%d: verify: %v", i, r.err))
		case !r.ack.Complete || r.rep.Status != visualstore.StatusOK || r.rep.Acknowledged != visualstore.AcknowledgedPresent || !r.rep.Complete || r.rep.Counts.Views != r.ack.Views:
			fails = append(fails, fmt.Sprintf("t%d: verify %s acknowledged %s complete %t/%t views %d/%d %s", i, r.rep.Status, r.rep.Acknowledged, r.rep.Complete, r.ack.Complete, r.rep.Counts.Views, r.ack.Views, r.rep.Reason))
		}
	}
	return check("survivors-complete-ok", "survivors (every non-faulted target) are unaffected: each capture completed without error after SetFinal(true), the worker exited cleanly, and the store verifies ok against its acknowledged record (complete:true, acknowledged present, committed views = acknowledged views; exported visualstore.Verify with DefaultLimits, as A.matrix)",
		fails, fmt.Sprintf("%d/%d survivors completed and verified ok", len(surv), len(surv)))
}

// b19Limits reads B19's limits from the frozen budgets (ms and count).
func b19Limits() (p99, p999, pmax, backlog float64, err error) {
	bf, err := qualify.LoadBudgets()
	if err != nil {
		return 0, 0, 0, 0, err
	}
	b, ok := bf.Budget("B19")
	if !ok {
		return 0, 0, 0, 0, errors.New("B19 missing from budgets.json")
	}
	for _, l := range b.Limits {
		v := l.Value
		if l.Unit != "count" {
			if v, err = qualify.ConvertUnit(l.Value, l.Unit, "ms"); err != nil {
				return 0, 0, 0, 0, err
			}
		}
		switch l.Metric {
		case "commit.p99":
			p99 = v
		case "commit.p999":
			p999 = v
		case "commit.max":
			pmax = v
		case "commit.backlog_failures":
			backlog = v
		}
	}
	return p99, p999, pmax, backlog, nil
}

// windowB19 evaluates B19 on the survivors over the fault window: every
// survivor batch whose [Start, Acknowledged] overlaps [w0, w1]. B19 is a
// resource verdict (decision 8), so it is recorded in fault_window.* metrics
// and, when not met, as a finding; the check fails only when the evaluation
// could not be made (no window, or a survivor with no batch in it).
func (fc *faultCell) windowB19(run *cellRun) *windowEval {
	surv := fc.survivors()
	n := len(fc.specs)
	var m metricSet
	lp99, lp999, lmax, lbacklog, err := b19Limits()
	desc := fmt.Sprintf("survivors' B19 (p99 ≤ %g ms, p99.9 ≤ %g ms, max ≤ %g ms, backlog failures == %g; limits read from the frozen budgets.json) is evaluated over the fault window on every survivor batch whose [Start, Acknowledged] overlaps it. B19 is a resource verdict (decision 8): it is recorded in fault_window.* and, when not met, as a finding; this check fails only when it could not be evaluated (no window, or a survivor without a batch in the window)",
		lp99, lp999, lmax, lbacklog)
	if err != nil {
		return &windowEval{check: check("survivors-b19-window", "survivors' B19 is evaluated over the fault window", []string{err.Error()}, ""), summary: "Survivors meet B19: NOT EVALUATED — " + err.Error()}
	}
	if len(surv) == 0 {
		return &windowEval{check: check("survivors-b19-window", desc, nil, "not applicable: no survivors (every target is faulted by design)"), summary: "Survivors meet B19: not applicable (no survivors)"}
	}
	fc.mu.Lock()
	w0, w1 := fc.w0, fc.w1
	fc.mu.Unlock()
	if w0 == 0 || w1 < w0 {
		return &windowEval{check: check("survivors-b19-window", desc, []string{"no fault window was recorded"}, ""), summary: "Survivors meet B19: NOT EVALUATED — no fault window was recorded"}
	}
	batches, p99, p999, pmax, backlog, cover := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	var pooled []int32
	var fails, missed []string
	for _, i := range surv {
		r := run.results[i]
		if r == nil {
			fails = append(fails, fmt.Sprintf("t%d: no result", i))
			continue
		}
		var lat []int32
		var last int64
		for k, start := range r.CommitStartNs {
			end := start + int64(r.CommitUs[k])*1000
			last = max(last, end)
			if start <= w1 && end >= w0 {
				lat = append(lat, r.CommitUs[k])
			}
		}
		batches[i] = float64(len(lat))
		cover[i] = min(1, max(0, float64(min(last, w1)-w0)/float64(max(w1-w0, 1))))
		if r.Backlog {
			backlog[i] = 1
		}
		if len(lat) == 0 {
			fails = append(fails, fmt.Sprintf("t%d: no batch in the fault window", i))
			continue
		}
		s := qualify.Summarize(lat)
		p99[i], p999[i], pmax[i] = float64(s.P99)/1e3, float64(s.P999)/1e3, float64(s.Max)/1e3
		pooled = append(pooled, lat...)
		if p99[i] > lp99 || p999[i] > lp999 || pmax[i] > lmax || backlog[i] > lbacklog {
			missed = append(missed, fmt.Sprintf("t%d p99 %.1f / p99.9 %.1f / max %.1f ms, backlog %g", i, p99[i], p999[i], pmax[i], backlog[i]))
		}
	}
	all := qualify.Summarize(pooled)
	windowS := float64(w1-w0) / 1e9
	in := map[string]float64{"window_s": windowS, "survivors": float64(len(surv)), "batches": float64(len(pooled))}
	lat := "survivor batches (BatchSample.Acknowledged − Start) overlapping the fault window, accelerated commit concurrency; victim entries are 0 and excluded from Value; Value pooled over survivors"
	m.measured("fault_window.wall", "s", "fault window: from the fault's injection to the end of its handling (doctor, recover, damage or resume attempt) while the survivors fed", windowS, nil)
	m.measured("fault_window.batches", "count", "survivor batches overlapping the fault window; Value = Σ", sum(batches), batches)
	m.derived("fault_window.coverage", "ratio", "fraction of the fault window during which each survivor was still committing (last batch end − window start) ÷ window; victims 0; Value = min over survivors", "(min(last_ack, w1) - w0) / (w1 - w0)", in,
		func() float64 {
			v := 1.0
			for _, i := range surv {
				v = min(v, cover[i])
			}
			return v
		}(), cover)
	m.derived("fault_window.commit_p99", "ms", "nearest-rank p99 of "+lat, "nearest_rank(window_latency, 99/100)", in, float64(all.P99)/1e3, p99)
	m.derived("fault_window.commit_p999", "ms", "nearest-rank p99.9 of "+lat, "nearest_rank(window_latency, 999/1000)", in, float64(all.P999)/1e3, p999)
	m.derived("fault_window.commit_max", "ms", "maximum of "+lat, "max(window_latency)", in, float64(all.Max)/1e3, pmax)
	m.measured("fault_window.backlog_failures", "count", "survivors whose capture failed with a backlog storage error; Value = Σ", sum(backlog), backlog)
	met := 1.0
	if len(missed) > 0 || len(fails) > 0 {
		met = 0
	}
	m.derived("fault_window.b19_met", "bool", "1 when every survivor met every B19 limit over the fault window (resource evidence, decision 8; B19's own scope excludes fault scenarios)", "all(survivor p99, p99.9, max, backlog within B19)", in, met, nil)
	for i := range m.ms {
		m.ms[i].ReportOnly = true
	}
	var findings []qualify.Finding
	findingID := "B19-fault-window/" + fc.pc.Key()
	if len(missed) > 0 {
		findings = append(findings, qualify.Finding{
			ID: findingID,
			Evidence: fmt.Sprintf("scenarios/%s%s/result.json#%s fault_window.*: pooled p99 %.1f ms, p99.9 %.1f ms, max %.1f ms over %d survivor batches in a %.1f s window; over B19: %s",
				faultPrefix, fc.id, fc.pc.Key(), float64(all.P99)/1e3, float64(all.P999)/1e3, float64(all.Max)/1e3, len(pooled), windowS, strings.Join(missed, "; ")),
			SuspectedCause: "accelerated Tier A commit concurrency: every survivor commits far above real-time pace and all fsync on one filesystem (A.matrix shows the same latencies without any fault, Run 2 F-a); the fault itself is the alternative cause",
			ProposedRemedy: "compare with Tier B real-time B19 (Run 5); a fault-window miss that persists at real-time pace needs a product decision (decision 12)",
		})
	}
	evidence := fmt.Sprintf("evaluated over a %.1f s window on %d survivor batches: B19 met %t (pooled p99 %.1f ms, p99.9 %.1f ms, max %.1f ms)", windowS, len(pooled), met == 1, float64(all.P99)/1e3, float64(all.P999)/1e3, float64(all.Max)/1e3)
	verdict := "MET"
	switch {
	case len(fails) > 0:
		verdict = "NOT EVALUATED for every survivor (" + strings.Join(fails, "; ") + ")"
	case len(missed) > 0:
		verdict = "NOT MET"
	}
	summary := fmt.Sprintf("Survivors meet B19: %s — pooled p99 %.1f ms / p99.9 %.1f ms / max %.1f ms over %d survivor batches in a %.1f s fault window (limits p99 %g ms, p99.9 %g ms, max %g ms, backlog %g); resource verdict (B19's scope excludes fault scenarios, decision 8), see fault_window.b19_met",
		verdict, float64(all.P99)/1e3, float64(all.P999)/1e3, float64(all.Max)/1e3, len(pooled), windowS, lp99, lp999, lmax, lbacklog)
	if len(missed) > 0 {
		summary += " and finding " + findingID + " (" + strings.Join(missed, "; ") + ")"
	}
	return &windowEval{metrics: m.ms, check: check("survivors-b19-window", desc, fails, evidence), findings: findings, summary: summary}
}

// walPeakMetric is B17's wal.peak for every target: the worker's peak after
// every acknowledged batch, and for a target killed without a result the
// larger of its last reported peak and the WAL size at rest after the kill.
func (fc *faultCell) walPeakMetric(run *cellRun) qualify.Metric {
	per := make([]float64, len(fc.specs))
	for i := range per {
		var peak int64
		if r := run.results[i]; r != nil {
			peak = max(r.WALPeak, r.ActivityWALPeak)
		}
		if v := fc.victims[i]; v != nil {
			peak = max(peak, v.msg.WALPeak, v.in.WALAtRest)
		}
		per[i] = float64(peak) / mib
	}
	return qualify.Metric{ID: "wal.peak", Unit: "MiB", Kind: qualify.KindMeasured, Value: maxOf(per), PerTarget: per,
		Method: "maximum Lstat size of visual.sqlite-wal after every acknowledged batch; for a target killed without a result, the larger of its last reported peak and the WAL size at rest after the kill (a lower bound); a refused target has none (0); Value = max"}
}

// noFalseSuccess is criterion 2 over every victim.
func (fc *faultCell) noFalseSuccess(preComplete bool) qualify.Check {
	desc := "no false success for every faulted target: no completion row in its store, no acknowledged record with complete:true, the production `areel doctor` (CLI default limits) never reports ok and never reports completion, and `areel recover` never claims an ok or complete source (an unverified doctor is never ok)"
	if preComplete {
		desc += ". F7 damages stores that completed before the damage: their completion row and complete:true record exist by construction, so for F7 this check asserts the doctor and recover clauses and reports the pre-existing completion as evidence"
	}
	var fails, notes []string
	for _, i := range slices.Sorted(mapKeysInt(fc.victims)) {
		v := fc.victims[i]
		who := fmt.Sprintf("t%d (%s)", i, v.variant)
		if v.inErr != nil {
			fails = append(fails, who+": inspection: "+v.inErr.Error())
			continue
		}
		fails = append(fails, v.in.falseSuccess(who, preComplete)...)
		notes = append(notes, fmt.Sprintf("%s: completion rows %d (no database %t), acknowledged complete %t, doctor %q", who, v.in.CompletionRows, v.in.NoDB, v.in.AckComplete, v.in.Doctor.Report.ArtifactStatus))
	}
	return check("no-false-success", desc, fails, strings.Join(notes, "; "))
}

func mapKeysInt[V any](m map[int]V) func(func(int) bool) {
	return func(yield func(int) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// unverifiedFindings reports every victim whose doctor ran out of its default
// work budget (F-d), without raising any limit (decision 12).
func (fc *faultCell) unverifiedFindings() []qualify.Finding {
	var out []qualify.Finding
	for _, i := range slices.Sorted(mapKeysInt(fc.victims)) {
		v := fc.victims[i]
		if !unverifiedCLI(v.in.Doctor) {
			continue
		}
		out = append(out, qualify.Finding{
			ID:             fmt.Sprintf("F-d/%s%s/%s/t%02d", faultPrefix, fc.id, fc.pc.Key(), i),
			Evidence:       fmt.Sprintf("scenarios/%s%s/result.json#%s t%02d (%s): areel doctor with default limits: %s; recover: %s", faultPrefix, fc.id, fc.pc.Key(), i, v.variant, v.in.Doctor.Err, v.in.Recovery.Err),
			SuspectedCause: "the doctor's default work budget (archive.RecoveryDefaults().Work = " + strconv.Itoa(archive.RecoveryDefaults().Work) + ", also used for the visual store) is far below what Verify needs even for a short store (Run 3 F-d)",
			ProposedRemedy: "size the visual doctor's default budget from the store (or use visualstore.DefaultLimits); a product decision (decision 12)",
		})
	}
	return out
}

// finish assembles the cell: the fault's required-outcome check first, then
// no false success, the survivors, their B19 window, B17's wal.peak and the
// fault's own metrics.
func (fc *faultCell) finish(run *cellRun, outcome qualify.Check, preComplete bool, ms []qualify.Metric, findings []qualify.Finding) (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
	cell := qualify.Cell{Workload: fc.pc.Workload, Targets: fc.pc.Targets, Timeline: fc.pc.Timeline, Geometry: "80x24"}
	win := fc.window(run)
	wm, wc, wf := win.metrics, win.check, win.findings
	cell.Checks = append(cell.Checks, outcome)
	if len(fc.victims) > 0 {
		cell.Checks = append(cell.Checks, fc.noFalseSuccess(preComplete))
	}
	cell.Checks = append(cell.Checks, fc.survivorsCheck(run), wc)
	for i := range ms {
		ms[i].ReportOnly = true
	}
	cell.Metrics = append(append([]qualify.Metric{fc.walPeakMetric(run)}, ms...), wm...)
	findings = append(append(findings, wf...), fc.unverifiedFindings()...)
	if !keepStores() {
		if err := os.RemoveAll(fc.dir); err != nil {
			return cell, run.samples, findings, err
		}
	}
	return cell, run.samples, findings, nil
}

func outcomeCheck(id string, fails []string, evidence []string) qualify.Check {
	return check("required-outcome-"+strings.ToLower(id), faultOutcome[id], fails, strings.Join(evidence, "; "))
}

// ---------------------------------------------------------------------------
// TestQualifyFault

func TestQualifyFault(t *testing.T) {
	q := openQualify(t)
	began := time.Now()
	env, err := childEnv(q.root)
	if err != nil {
		t.Fatal(err)
	}
	cenv, err := cliEnv(q.root)
	if err != nil {
		t.Fatal(err)
	}
	bin, err := buildAreel(q)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range qualify.FaultIDs {
		t.Run(id, func(t *testing.T) {
			scenario := faultPrefix + id
			cells := plannedOf(q.profile, scenario)
			label := qualify.LabelAccelerated + "; fault " + id + ": " + faultName[id] + "; victims judged by the production areel doctor/recover (CLI default limits) on a visual/1 library layout under the artifact directory"
			switch id {
			case "F8":
				label += "; real ENOSPC on tmpfs, not on the production filesystem (unprivileged user+mount namespace, uid/gid mapped to 0 inside only)"
			case "F9":
				label += "; simulated by injected commit latency; not a slow block device"
			}
			logical := spans10.w4
			switch id {
			case "F3", "F4":
				logical = spans32.w4
			case "F9":
				logical = spansF9.w4
			}
			w := beginScenario(t, q, scenario, label, logical, len(cells))
			w.eachCell(cells, func(pc qualify.PlannedCell) (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
				return runFaultCell(q, env, cenv, bin, w.dir, id, pc)
			})
			w.finish()
		})
	}
	t.Run("H4", func(t *testing.T) {
		// H4 is report-only (plan decision 6, E-metrics have no verdict):
		// scenario-level metrics and no cell, so no budget scope (B17's "*"
		// judges cells) turns it into a verdict.
		pc := qualify.PlannedCell{Scenario: h4Scenario, Workload: qualify.W3, Targets: 1, Timeline: qualify.TimelineFault}
		w := beginScenario(t, q, h4Scenario, qualify.LabelAccelerated+"; H4: exported visualstore.Verify (DefaultLimits) run back to back at package level beside a live W3 writer; its WAL peak is an E-metric (a finding above B17, not a budget exemption); unplanned in plan.go", spansH4.w3, 1)
		ms, checks, findings, samples, err := runH4(q, env, cenv, bin, w.dir, pc)
		if err != nil {
			w.fail(err)
		}
		w.s.Cells, w.s.Metrics, w.s.Checks, w.s.Findings = []qualify.Cell{}, ms, checks, findings
		for _, s := range samples {
			if err := w.samples.Write(s); err != nil {
				w.fail(err)
			}
		}
		w.cellsRun = 1 // written although it has no cell
		w.finish()
	})
	t.Logf("TestQualifyFault: %s wall", time.Since(began).Round(time.Second))
}

func runFaultCell(q qualifyEnv, env, cenv []string, bin, dir, id string, pc qualify.PlannedCell) (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
	sp := spans10
	switch {
	case id == "F9":
		sp = spansF9
	case id == "F10" && pc.Targets > 1:
		sp = spansF10
	case pc.Targets > 10:
		sp = spans32
	}
	fc, err := newFaultCell(q, env, cenv, bin, dir, id, pc, sp)
	if err != nil {
		return qualify.Cell{}, nil, nil, err
	}
	switch id {
	case "F1":
		return fc.f1()
	case "F2":
		return fc.f2()
	case "F3", "F4":
		return fc.f3f4()
	case "F5":
		return fc.f5()
	case "F6":
		return fc.f6()
	case "F7":
		return fc.f7()
	case "F8":
		return fc.f8()
	case "F9":
		return fc.f9()
	case "F10":
		return fc.f10()
	}
	return qualify.Cell{}, nil, nil, fmt.Errorf("fault %s", id)
}

// ---------------------------------------------------------------------------
// F1: SIGKILL after a store commit, before its acknowledgement.

func (fc *faultCell) f1() (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
	const vi = 0 // W3
	fc.specs[vi].KillAtNs = 15 * qualify.Second
	v := fc.victim(vi, "self-SIGKILL after Store.Commit returned, before the acknowledged write (Run 3 killWriter)", "recording")
	run, err := fc.run(workerControl{drive: func(i int, p *workerProc, stop time.Time) (bool, string) {
		if i != vi {
			return false, driveToDone(p, stop)
		}
		m, err := expectKilled(p, stop)
		if err != nil {
			return false, err.Error()
		}
		v.msg, v.inject, v.killed, v.ended = m, m.UnixNs, true, ended(p)
		fc.mark(m.UnixNs)
		v.resume = fc.resume(vi)
		fc.inspect(v)
		fc.mark(time.Now().UnixNano())
		return true, ""
	}})
	if err != nil {
		return qualify.Cell{}, nil, nil, err
	}
	var fails, ev []string
	if fc.driveErrs[vi] != "" {
		fails = append(fails, "victim: "+fc.driveErrs[vi])
	}
	rep, via := v.in.verdict()
	switch {
	case v.inErr != nil:
		fails = append(fails, "inspection: "+v.inErr.Error())
	case rep == nil:
		fails = append(fails, via)
	default:
		unack := rep.Counts.Views - v.in.AckViews
		if rep.Status != visualstore.StatusIncomplete || rep.FailedItem != "" {
			fails = append(fails, fmt.Sprintf("verdict %s (%s) via %s", rep.Status, rep.Reason, via))
		}
		if rep.Acknowledged != visualstore.AcknowledgedPresent {
			fails = append(fails, "acknowledged prefix "+rep.Acknowledged)
		}
		if unack <= 0 || rep.Counts.Views != v.msg.Views {
			fails = append(fails, fmt.Sprintf("no unacknowledged tail: store %d views (the killed batch left %d), acknowledged %d", rep.Counts.Views, v.msg.Views, v.in.AckViews))
		}
		if !unverifiedCLI(v.in.Doctor) && v.in.Doctor.Report.UnacknowledgedViews != unack {
			fails = append(fails, fmt.Sprintf("doctor reports %d unacknowledged views, store − acknowledged = %d", v.in.Doctor.Report.UnacknowledgedViews, unack))
		}

		ev = append(ev, fmt.Sprintf("victim %s: %s (via %s)", v.ended, v.in.summary(), via))
	}
	fails = append(fails, v.in.recoveredPrefix(v.msg.Views, 0)...)
	ev = append(ev, v.in.recoverySummary())
	var se *archive.StorageError
	if v.resume == nil || !errors.As(v.resume, &se) || se.Stage != "create" {
		fails = append(fails, fmt.Sprintf("resume via visualcapture.New (segment 2) was not refused as a create-stage StorageError: %v", v.resume))
	} else {
		ev = append(ev, "resume refused: "+v.resume.Error())
	}
	var m metricSet
	unack := float64(0)
	if rep != nil {
		unack = float64(max(rep.Counts.Views-v.in.AckViews, 0))
	}
	m.measured("victim.unacknowledged_views", "count", "committed views beyond the acknowledged record after the kill (store views − acknowledged views; via "+via+")", unack, nil)
	m.measured("victim.doctor_run", "s", "wall time of the victim's `areel doctor` process", float64(v.in.Doctor.WallNs)/1e9, nil)
	return fc.finish(run, outcomeCheck("F1", fails, ev), false, m.ms, nil)
}

// resume attempts visualcapture.New for the next segment on victim i's store.
func (fc *faultCell) resume(i int) error {
	spec := fc.specs[i]
	c, err := New(Config{
		Store: filepath.Join(spec.Rec, StoreDirName), Acknowledged: filepath.Join(spec.Rec, AcknowledgedName),
		Identity: archive.Identity{Recording: spec.RecordingID, Backend: "qualify", Instance: "tier-a"}, Segment: 2,
		Geometry: archive.Geometry{Columns: spec.Cols, Rows: spec.Rows}, Helper: fc.q.helper, Limits: Defaults(),
	})
	if err == nil {
		_ = c.Close()
	}
	return err
}

// ---------------------------------------------------------------------------
// F2: parent SIGKILL at a random time during a W3 burst; SIGKILL before commit.

func (fc *faultCell) f2() (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
	const parentKill, beforeCommit = 0, 2 // both W3
	fc.specs[parentKill].ProgressAtBatch = 5
	fc.specs[beforeCommit].Fault, fc.specs[beforeCommit].KillAtNs = faultBeforeCommit, 12*qualify.Second
	vp := fc.victim(parentKill, "parent SIGKILL at a random time mid-W3", "recording")
	vb := fc.victim(beforeCommit, "self-SIGKILL at Store.Commit entry, transaction open", "recording")
	rng := rand.New(rand.NewPCG(matrixSeed("fault:F2:kill"), 2))
	delay := time.Duration(rng.Int64N(int64(200 * time.Millisecond)))
	run, err := fc.run(workerControl{drive: func(i int, p *workerProc, stop time.Time) (bool, string) {
		switch i {
		case parentKill:
			if _, err := p.expect("progress", time.Until(stop)); err != nil {
				p.kill()
				return false, err.Error()
			}
			time.Sleep(delay) // the random kill time is the fault itself
			vp.inject = time.Now().UnixNano()
			p.kill()
			vp.killed, vp.ended = true, ended(p)
			fc.mark(vp.inject)
			fc.inspect(vp)
			fc.mark(time.Now().UnixNano())
			return true, ""
		case beforeCommit:
			m, err := expectKilled(p, stop)
			if err != nil {
				return false, err.Error()
			}
			vb.msg, vb.inject, vb.killed, vb.ended = m, m.UnixNs, true, ended(p)
			fc.mark(m.UnixNs)
			fc.inspect(vb)
			fc.mark(time.Now().UnixNano())
			return true, ""
		}
		return false, driveToDone(p, stop)
	}})
	if err != nil {
		return qualify.Cell{}, nil, nil, err
	}
	var fails, ev []string
	for _, v := range []*victim{vp, vb} {
		who := fmt.Sprintf("t%d (%s)", v.i, v.variant)
		if fc.driveErrs[v.i] != "" {
			fails = append(fails, who+": "+fc.driveErrs[v.i])
			continue
		}
		if v.inErr != nil {
			fails = append(fails, who+": inspection: "+v.inErr.Error())
			continue
		}
		fails = append(fails, v.in.prefixVerifies(who)...)
		rep, via := v.in.verdict()
		if v == vb && rep != nil && (rep.Counts.Views != v.in.AckViews || v.msg.Views != v.in.AckViews) {
			fails = append(fails, fmt.Sprintf("%s: the open transaction was not rolled back to the acknowledged prefix: store %d, committed at the kill %d, acknowledged %d", who, rep.Counts.Views, v.msg.Views, v.in.AckViews))
		}
		ev = append(ev, fmt.Sprintf("%s ended by %s: %s (via %s)", who, v.ended, v.in.summary(), via))
	}
	ev = append(ev, fmt.Sprintf("parent kill %s after the victim's 5th acknowledged batch (seeded)", delay.Round(time.Microsecond)))
	var m metricSet
	m.measured("victim.parent_kill_delay", "ms", "seeded random delay between the W3 victim's 5th acknowledged batch and the parent's SIGKILL", float64(delay.Microseconds())/1e3, nil)
	return fc.finish(run, outcomeCheck("F2", fails, ev), false, m.ms, nil)
}

// ---------------------------------------------------------------------------
// F3/F4: a helper that cannot start, at 32 concurrent starts.

func (fc *faultCell) f3f4() (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
	const vi = 0
	variant := "Helper points at a nonexistent path"
	fc.specs[vi].Helper = filepath.Join(fc.dir, "no-such-areel-avt-checkpoint")
	if fc.id == "F4" {
		variant = "Helper is a copy of the pinned helper truncated to half, chmod 0700"
		data, err := os.ReadFile(fc.q.helper)
		if err != nil {
			return qualify.Cell{}, nil, nil, err
		}
		bad := filepath.Join(fc.dir, "areel-avt-checkpoint-truncated")
		if err := os.WriteFile(bad, data[:len(data)/2], 0o700); err != nil {
			return qualify.Cell{}, nil, nil, err
		}
		if err := os.Chmod(bad, 0o700); err != nil {
			return qualify.Cell{}, nil, nil, err
		}
		fc.specs[vi].Helper = bad
	}
	fc.specs[vi].ExpectRefusal = true
	v := fc.victim(vi, variant, "failed")
	run, err := fc.run(workerControl{
		ready: func(i int) []string {
			if i == vi {
				return []string{"ready", "refused"}
			}
			return []string{"ready"}
		},
		drive: func(i int, p *workerProc, stop time.Time) (bool, string) {
			errText := driveToDone(p, stop)
			if i == vi {
				v.ended = ended(p)
				if r, err := fc.readResult(vi); err == nil && r.Refusal != nil {
					v.refused, v.inject = true, r.Refusal.UnixNs
					fc.mark(r.Refusal.UnixNs)
				}
				fc.inspect(v) // after the refusal evidence was taken: state.json lands in the directory
			}
			return false, errText
		},
	})
	if err != nil {
		return qualify.Cell{}, nil, nil, err
	}
	// The window runs from the refusal to the survivors' last acknowledgement.
	for _, i := range fc.survivors() {
		if r := run.results[i]; r != nil && len(r.CommitStartNs) > 0 {
			k := len(r.CommitStartNs) - 1
			fc.mark(r.CommitStartNs[k] + int64(r.CommitUs[k])*1000)
		}
	}
	var fails, ev []string
	r := run.results[vi]
	switch {
	case r == nil:
		fails = append(fails, "victim wrote no result: "+run.errs[vi])
	case r.Refusal == nil:
		fails = append(fails, "New did not refuse the unusable helper (the victim ran)")
	default:
		if r.Refusal.Storage {
			fails = append(fails, "the refusal is a storage failure: "+r.Refusal.Err)
		}
		if len(r.Refusal.Entries) != 0 {
			fails = append(fails, fmt.Sprintf("the refusal created %v in the recording directory", r.Refusal.Entries))
		}
		ev = append(ev, fmt.Sprintf("New refused (storage %t): %s; recording directory empty afterwards", r.Refusal.Storage, r.Refusal.Err))
	}
	surv := fc.survivors()
	ran := 0
	for _, i := range surv {
		if run.errs[i] == "" && run.results[i] != nil {
			ran++
		}
	}
	if ran != len(surv) {
		fails = append(fails, fmt.Sprintf("%d of the other %d targets ran", ran, len(surv)))
	}
	ev = append(ev, fmt.Sprintf("the other %d targets ran (survivors-complete-ok verifies their stores)", ran), "victim doctor: "+v.in.summary())
	var m metricSet
	hs := make([]float64, len(fc.specs))
	for _, i := range surv {
		if r := run.results[i]; r != nil {
			hs[i] = float64(r.HandshakeNs) / 1e6
		}
	}
	m.measured("survivors.helper_handshake", "ms", fmt.Sprintf("helper spawn → first readout, %d concurrent starts beside the refused one; the refused target 0; Value = max", len(fc.specs)), maxOf(hs), hs)
	return fc.finish(run, outcomeCheck(fc.id, fails, ev), false, m.ms, nil)
}

// ---------------------------------------------------------------------------
// F5: helper protocol corruption through the helper proxy.

func (fc *faultCell) f5() (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
	const vi, flipAt = 0, 250 // W3: one 'W' readout per output line
	exe, err := os.Executable()
	if err != nil {
		return qualify.Cell{}, nil, nil, err
	}
	fc.specs[vi].Helper, fc.specs[vi].ProxyHelper, fc.specs[vi].ProxyFlipAt = exe, fc.q.helper, flipAt
	v := fc.victim(vi, fmt.Sprintf("helper proxy inverts the first byte (columns MSB) of readout reply %d", flipAt), "recording")
	run, err := fc.run(workerControl{drive: func(i int, p *workerProc, stop time.Time) (bool, string) {
		errText := driveToDone(p, stop)
		if i == vi {
			v.ended = ended(p)
			if r, err := fc.readResult(vi); err == nil && r.FailedUnixNs > 0 {
				v.inject = r.FailedUnixNs
				fc.mark(r.FailedUnixNs)
			}
			fc.inspect(v)
			fc.mark(time.Now().UnixNano())
		}
		return false, errText
	}})
	if err != nil {
		return qualify.Cell{}, nil, nil, err
	}
	var fails, ev []string
	r := run.results[vi]
	switch {
	case r == nil:
		fails = append(fails, "victim wrote no result: "+run.errs[vi])
	case r.Err == "" || r.FailedUnixNs == 0:
		fails = append(fails, "the corrupted reply did not fail the capture")
	case r.ErrStorage || r.ErrStage != "":
		fails = append(fails, fmt.Sprintf("a storage failure (stage %q): %s", r.ErrStage, r.Err))
	case !strings.Contains(r.Err, "visual capture helper"):
		fails = append(fails, "not a helper failure: "+r.Err)
	default:
		ev = append(ev, "plain capture failure: "+r.Err)
	}
	if v.inErr != nil {
		fails = append(fails, "inspection: "+v.inErr.Error())
	} else {
		fails = append(fails, v.in.prefixVerifies("victim")...)
		ev = append(ev, "victim: "+v.in.summary())
	}
	ev = append(ev, "no completion: see no-false-success; survivors unaffected: see survivors-complete-ok")
	return fc.finish(run, outcomeCheck("F5", fails, ev), false, nil, nil)
}

// ---------------------------------------------------------------------------
// F6: the helper SIGKILLed mid-stream.

func (fc *faultCell) f6() (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
	const vi = 0 // W3
	fc.specs[vi].ProgressAtBatch = 8
	v := fc.victim(vi, "helper process SIGKILLed after the 8th acknowledged batch", "recording")
	run, err := fc.run(workerControl{drive: func(i int, p *workerProc, stop time.Time) (bool, string) {
		if i != vi {
			return false, driveToDone(p, stop)
		}
		if _, err := p.expect("progress", time.Until(stop)); err != nil {
			p.kill()
			return false, err.Error()
		}
		v.inject = time.Now().UnixNano()
		if err := syscall.Kill(p.helper, syscall.SIGKILL); err != nil {
			v.notes = append(v.notes, "helper kill: "+err.Error())
		}
		fc.mark(v.inject)
		errText := driveToDone(p, stop)
		v.ended = ended(p)
		fc.inspect(v)
		fc.mark(time.Now().UnixNano())
		return false, errText
	}})
	if err != nil {
		return qualify.Cell{}, nil, nil, err
	}
	var fails, ev []string
	var m metricSet
	limit := Defaults().StallAge + time.Second
	r := run.results[vi]
	switch {
	case r == nil:
		fails = append(fails, "victim wrote no result: "+run.errs[vi])
	case r.Err == "" || r.FailedUnixNs == 0:
		fails = append(fails, "killing the helper did not fail the capture"+strings.Join(v.notes, "; "))
	default:
		d := time.Duration(r.FailedUnixNs - v.inject)
		m.measured("helper_kill.to_failure", "s", "helper SIGKILL (orchestrator wall time) → Capture.Failed closed in the worker (wall, same host clock)", d.Seconds(), nil)
		if d > limit {
			fails = append(fails, fmt.Sprintf("capture failure %s after the helper kill > StallAge + 1 s (%s)", d, limit))
		}
		ev = append(ev, fmt.Sprintf("capture failure %s after the helper SIGKILL (limit %s): %s (storage %t)", d.Round(time.Microsecond), limit, r.Err, r.ErrStorage))
	}
	if v.inErr != nil {
		fails = append(fails, "inspection: "+v.inErr.Error())
	} else {
		fails = append(fails, v.in.prefixVerifies("victim")...)
		ev = append(ev, "victim: "+v.in.summary())
	}
	return fc.finish(run, outcomeCheck("F6", fails, ev), false, m.ms, nil)
}

// ---------------------------------------------------------------------------
// F7: at-rest corruption of completed stores.

func (fc *faultCell) f7() (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
	const flip, trunc, badAck, noAck = 0, 1, 2, 3
	for _, i := range []int{flip, trunc, badAck, noAck} {
		if err := fc.target(i, qualify.W4, spanF7Vic); err != nil {
			return qualify.Cell{}, nil, nil, err
		}
	}
	fc.specs[trunc].Fault = faultKillOnClose
	vs := map[int]*victim{
		flip:   fc.victim(flip, "one row-payload byte flipped", "stopped"),
		trunc:  fc.victim(trunc, "WAL tail truncated on a store killed before its closing checkpoint", "stopped"),
		badAck: fc.victim(badAck, "acknowledged-record JSON corrupted", "stopped"),
		noAck:  fc.victim(noAck, "acknowledged record deleted", "stopped"),
	}
	run, err := fc.run(workerControl{drive: func(i int, p *workerProc, stop time.Time) (bool, string) {
		v := vs[i]
		if v == nil {
			return false, driveToDone(p, stop)
		}
		noResult, errText := false, ""
		if i == trunc {
			m, err := expectKilled(p, stop)
			if err != nil {
				return false, err.Error()
			}
			v.msg, v.killed, noResult = m, true, true
		} else if errText = driveToDone(p, stop); errText != "" {
			return false, errText
		}
		v.ended = ended(p)
		v.inject = time.Now().UnixNano()
		fc.mark(v.inject)
		rec := fc.specs[i].Rec
		var derr error
		switch i {
		case flip:
			derr = flipRowPayload(rec, v)
		case trunc:
			derr = truncateWALTail(rec, v)
		case badAck:
			derr = corruptAck(rec, v)
		case noAck:
			derr = os.Remove(filepath.Join(rec, AcknowledgedName))
			v.notes = append(v.notes, "acknowledged record deleted")
		}
		if derr != nil {
			return noResult, "damage: " + derr.Error()
		}
		v.reader, v.readerN = readerProbe(rec)
		fc.inspect(v)
		fc.mark(time.Now().UnixNano())
		return noResult, errText
	}})
	if err != nil {
		return qualify.Cell{}, nil, nil, err
	}
	var fails, ev []string
	for _, i := range []int{flip, trunc, badAck, noAck} {
		v := vs[i]
		who := fmt.Sprintf("t%d (%s)", i, v.variant)
		if fc.driveErrs[i] != "" {
			fails = append(fails, who+": "+fc.driveErrs[i])
			continue
		}
		if v.inErr != nil {
			fails = append(fails, who+": inspection: "+v.inErr.Error())
			continue
		}
		d := v.in.Doctor
		rep, via := v.in.verdict()
		switch i {
		case flip, trunc:
			want := visualstore.StatusDamaged
			if i == trunc {
				want = visualstore.StatusTruncated
			}
			if rep == nil || rep.Status != want {
				got := "none"
				if rep != nil {
					got = rep.Status + ": " + rep.Reason
				}
				fails = append(fails, fmt.Sprintf("%s: verdict %s, want %s (via %s)", who, got, want, via))
			}
			if !v.in.AckComplete {
				fails = append(fails, who+": the store had not completed before the damage")
			}
		default:
			if d.Err == "" || d.Report.Visual != nil {
				fails = append(fails, fmt.Sprintf("%s: doctor gave a verdict (%v) or no error (%q); want an error with no verdict", who, d.Report.Visual != nil, d.Err))
			}
		}
		switch i {
		case flip:
			if !strings.HasPrefix(v.reader, "refused") {
				fails = append(fails, who+": the Reader did not refuse: "+v.reader)
			}
		case trunc:
			if v.readerN < 0 || v.readerN >= v.in.AckViews {
				fails = append(fails, fmt.Sprintf("%s: the Reader presents %d views, not fewer than the %d acknowledged (%s)", who, v.readerN, v.in.AckViews, v.reader))
			}
		}
		ev = append(ev, fmt.Sprintf("%s: %s; doctor error %q; reader %s; %s", who, v.in.summary(), d.Err, v.reader, strings.Join(v.notes, ", ")))
	}
	var findings []qualify.Finding
	if v := vs[trunc]; fc.driveErrs[trunc] == "" && v.inErr == nil && !strings.HasPrefix(v.reader, "refused") {
		findings = append(findings, qualify.Finding{
			ID: "F7-reader-truncated/" + fc.pc.Key(),
			Evidence: fmt.Sprintf("scenarios/%sF7/result.json#%s t%02d (%s): the plan's \"Reader refuses\" did not happen: the visualstore Reader served %d of %d acknowledged views (reader: %s; %s); the production doctor, which holds the acknowledged record, reports %q",
				faultPrefix, fc.pc.Key(), trunc, v.variant, v.readerN, v.in.AckViews, v.reader, strings.Join(v.notes, ", "), v.in.Doctor.Report.ArtifactStatus),
			SuspectedCause: "the Reader holds no owner expectation (acknowledged record): SQLite recovery of a WAL whose tail was cut yields a consistent, shorter store the Reader cannot tell from a short one",
			ProposedRemedy: "a product decision (decision 12): e.g. give readers the acknowledged record as an expectation, or accept doctor-only detection and document it",
		})
	}
	ev = append(ev, "the Reader has no owner expectation: a WAL-truncated store is a consistent shorter store it cannot refuse, so for that store the check asserts the Reader presents fewer views than acknowledged; the acknowledged-record variants leave the store itself intact (the Reader result is informational)")
	return fc.finish(run, outcomeCheck("F7", fails, ev), true, nil, findings)
}

// flipRowPayload inverts one byte in the middle of the first row's payload
// through an ordinary read-write SQL connection (the store is closed).
func flipRowPayload(rec string, v *victim) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(rec, StoreDirName, visualstore.DatabaseName))
	if err != nil {
		return err
	}
	defer db.Close()
	var id int64
	var payload []byte
	if err := db.QueryRow(`SELECT id, payload FROM "rows" ORDER BY id LIMIT 1`).Scan(&id, &payload); err != nil {
		return err
	}
	if len(payload) == 0 {
		return errors.New("empty row payload")
	}
	payload[len(payload)/2] ^= 0xFF
	if _, err := db.Exec(`UPDATE "rows" SET payload = ? WHERE id = ?`, payload, id); err != nil {
		return err
	}
	v.notes = append(v.notes, fmt.Sprintf("row %d payload byte %d of %d inverted", id, len(payload)/2, len(payload)))
	return nil
}

// truncateWALTail cuts the uncheckpointed WAL to its first half of frames, at
// a frame boundary; the dropped tail holds acknowledged commits.
func truncateWALTail(rec string, v *victim) error {
	path := filepath.Join(rec, StoreDirName, visualstore.DatabaseName+"-wal")
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	var hdr [32]byte
	_, err = io.ReadFull(f, hdr[:])
	info, serr := f.Stat()
	_ = f.Close()
	if err != nil || serr != nil {
		return errors.Join(err, serr)
	}
	page := int64(binary.BigEndian.Uint32(hdr[8:12]))
	frame := 24 + page
	frames := (info.Size() - 32) / frame
	if page < 512 || frames < 2 {
		return fmt.Errorf("WAL of %d bytes, page %d, %d frames: nothing to truncate", info.Size(), page, frames)
	}
	keep := frames / 2
	if err := os.Truncate(path, 32+keep*frame); err != nil {
		return err
	}
	v.notes = append(v.notes, fmt.Sprintf("WAL cut from %d to %d frames (page %d B)", frames, keep, page))
	return nil
}

// corruptAck inverts the acknowledged record's first byte.
func corruptAck(rec string, v *victim) error {
	path := filepath.Join(rec, AcknowledgedName)
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return errors.Join(err, errors.New("empty acknowledged record"))
	}
	data[0] ^= 0xFF
	v.notes = append(v.notes, "acknowledged record byte 0 inverted")
	return os.WriteFile(path, data, 0o600)
}

// readerProbe opens the store through visualstore's Reader, takes a snapshot
// and reads every distinct screen of it. It returns "refused: …" on the first
// refusal, else "served …", and the snapshot's view count (-1 without one).
func readerProbe(rec string) (string, int64) {
	store := filepath.Join(rec, StoreDirName)
	r, err := visualstore.OpenReader(store)
	if err != nil {
		return "refused: OpenReader: " + err.Error(), -1
	}
	defer r.Close()
	ctx := context.Background()
	sel, err := r.Snapshot(ctx)
	if err != nil {
		return fmt.Sprintf("refused: Snapshot (ErrSelection %t): %v", errors.Is(err, visualstore.ErrSelection), err), -1
	}
	var ids []int64
	seen := make(map[int64]bool)
	for _, segment := range sel.Segments {
		for from := int64(1); ; {
			views, err := r.Views(ctx, sel, segment.Segment, from, visualstore.MaxPage)
			if err != nil {
				return "refused: Views: " + err.Error(), sel.ViewSeq
			}
			if len(views) == 0 {
				break
			}
			for _, view := range views {
				if !seen[view.ScreenID] {
					seen[view.ScreenID] = true
					ids = append(ids, view.ScreenID)
				}
			}
			from = views[len(views)-1].Seq + 1
		}
	}
	for _, id := range ids {
		if _, err := r.Screen(ctx, sel, id); err != nil {
			return fmt.Sprintf("refused: Screen(%d) (ErrSelection %t): %v", id, errors.Is(err, visualstore.ErrSelection), err), sel.ViewSeq
		}
	}
	return fmt.Sprintf("served %d views and all %d screens", sel.ViewSeq, len(ids)), sel.ViewSeq
}

// ---------------------------------------------------------------------------
// F8: real ENOSPC on a private tmpfs.

func (fc *faultCell) f8() (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
	modes := map[int]string{0: faultENOSPCWAL, 2: faultENOSPCCheckpoint, 4: faultENOSPCAck} // W3 targets
	variants := map[string]string{
		faultENOSPCWAL:        "ENOSPC while the WAL grows during commit 3",
		faultENOSPCCheckpoint: "ENOSPC in the database file during the checkpoint of the first commit after the WAL passed 4 MiB",
		faultENOSPCAck:        "ENOSPC writing the acknowledged record after commit 3",
	}
	for i, mode := range modes {
		fc.specs[i].Fault, fc.specs[i].FaultAt = mode, 3
		fc.victim(i, variants[mode], "recording")
	}
	nsResults := make(map[int]*nsResult)
	var nsMu sync.Mutex
	start := func(i int, spec workerSpec, specPath, logPath string) (*workerProc, error) {
		if _, ok := modes[i]; !ok {
			return fc.startTarget(i, spec, specPath, logPath)
		}
		ns := nsSpec{Worker: specPath, Rec: spec.Rec, Size: 64 << 20, Larger: 256 << 20, Areel: fc.bin, Library: fc.lib, ID: spec.RecordingID,
			CLIEnv: fc.cenv, Result: filepath.Join(fc.dir, fmt.Sprintf("t%02d.ns.json", i))}
		b, err := json.Marshal(ns)
		if err != nil {
			return nil, err
		}
		nsPath := filepath.Join(fc.dir, fmt.Sprintf("t%02d.ns-spec.json", i))
		if err := os.WriteFile(nsPath, b, 0o600); err != nil {
			return nil, err
		}
		sys := &syscall.SysProcAttr{
			Pdeathsig:                  syscall.SIGKILL,
			Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
			UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
			GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
			GidMappingsEnableSetgroups: false,
		}
		p, err := startWorkerProc(fc.env, spec, specPath, logPath, nsInitEnv+"="+nsPath, sys)
		if err != nil {
			return nil, fmt.Errorf("F8 needs unprivileged user namespaces (clone with CLONE_NEWUSER|CLONE_NEWNS and a uid/gid map) to mount a private tmpfs; that capability is missing: %w", err)
		}
		return p, nil
	}
	run, err := fc.run(workerControl{start: start, drive: func(i int, p *workerProc, stop time.Time) (bool, string) {
		if _, ok := modes[i]; !ok {
			return false, driveToDone(p, stop)
		}
		v := fc.victims[i]
		if _, err := p.expect("done", time.Until(stop)); err != nil {
			p.kill()
			return false, err.Error()
		}
		m, err := p.expect("nsdone", time.Until(stop))
		if err != nil {
			p.kill()
			return false, err.Error()
		}
		<-p.exited
		v.ended = ended(p)
		if p.waitErr != nil {
			return false, fmt.Sprintf("namespace init exit: %v", p.waitErr)
		}
		data, err := os.ReadFile(filepath.Join(fc.dir, fmt.Sprintf("t%02d.ns.json", i)))
		if err != nil {
			return false, err.Error()
		}
		var res nsResult
		if err := json.Unmarshal(data, &res); err != nil {
			return false, err.Error()
		}
		nsMu.Lock()
		nsResults[i] = &res
		nsMu.Unlock()
		v.ns, v.in = &res, res.Inspection
		if res.InspectErr != "" {
			v.inErr = errors.New(res.InspectErr)
		}
		if r, err := fc.readResult(i); err == nil && r.FailedUnixNs > 0 {
			v.inject = r.FailedUnixNs
			fc.mark(r.FailedUnixNs)
		}
		fc.mark(m.UnixNs)
		return false, ""
	}})
	if err != nil {
		return qualify.Cell{}, nil, nil, err
	}
	var fails, ev []string
	filled := make([]float64, len(fc.specs))
	for _, i := range slices.Sorted(mapKeysInt(modes)) {
		v, mode := fc.victims[i], modes[i]
		who := fmt.Sprintf("t%d (%s)", i, v.variant)
		r := run.results[i]
		if fc.driveErrs[i] != "" || r == nil {
			fails = append(fails, who+": "+fc.driveErrs[i])
			continue
		}
		f := r.Fill
		switch {
		case f == nil:
			fails = append(fails, who+": the filler never ran (the strike point was not reached)")
			continue
		case f.FillErr != "" || f.FilledBytes == 0:
			fails = append(fails, fmt.Sprintf("%s: filler wrote %d bytes: %s", who, f.FilledBytes, f.FillErr))
		}
		filled[i] = float64(f.FilledBytes) / mib
		lower := strings.ToLower(r.Err)
		if r.ErrStage != "publish" || !r.ErrStorage || r.FailedUnixNs == 0 || !(strings.Contains(lower, "no space left") || strings.Contains(lower, "disk is full") || strings.Contains(lower, "full")) {
			fails = append(fails, fmt.Sprintf("%s: not a typed publish ENOSPC StorageError with Failed: stage %q storage %t failed %t: %s", who, r.ErrStage, r.ErrStorage, r.FailedUnixNs != 0, r.Err))
		}
		if v.inErr != nil {
			fails = append(fails, who+": inspection after the remount: "+v.inErr.Error())
			continue
		}
		fails = append(fails, v.in.prefixVerifies(who)...)
		if r.AckViews != v.in.AckViews {
			fails = append(fails, fmt.Sprintf("%s: the acknowledged record holds %d views, the last acknowledged batch %d", who, v.in.AckViews, r.AckViews))
		}
		rep, via := v.in.verdict()
		unack := int64(-1)
		if rep != nil {
			unack = rep.Counts.Views - v.in.AckViews
		}
		struck := ""
		switch mode {
		case faultENOSPCWAL:
			if f.CommitErr == "" || unack != 0 {
				struck = "the WAL commit did not fail with nothing durable beyond the acknowledged prefix"
			}
		case faultENOSPCCheckpoint:
			if f.CommitErr == "" || unack <= 0 || f.WALBefore <= storeCheckpointWALBytes {
				struck = "the commit did not become durable and then fail in its checkpoint (committed views beyond the acknowledged record expected)"
			}
		case faultENOSPCAck:
			if f.CommitErr != "" || unack <= 0 {
				struck = "the acknowledged write did not fail after a durable commit"
			}
		}
		if struck != "" {
			fails = append(fails, fmt.Sprintf("%s: strike point: %s (commit error %q, %d unacknowledged views, WAL %d→%d B, db %d→%d B)", who, struck, f.CommitErr, unack, f.WALBefore, f.WALAfter, f.DBBefore, f.DBAfter))
		}
		ns := v.ns
		ev = append(ev, fmt.Sprintf("%s: %s on %s (%d MiB, %d B free before the remount, %d B after the %d MiB remount); commit call %d, filler %d B, WAL %d→%d B, db %d→%d B, commit error %q; capture %q; after the remount %s (via %s)",
			who, mode, ns.FSType, ns.Size>>20, ns.FreeBeforeRemount, ns.FreeAfterRemount, ns.Larger>>20, f.CommitCall, f.FilledBytes, f.WALBefore, f.WALAfter, f.DBBefore, f.DBAfter, f.CommitErr, r.Err, v.in.summary(), via))
	}
	ev = append(ev, "survivors on the normal disk: see survivors-complete-ok")
	var m metricSet
	m.measured("enospc.filled", "MiB", "filler bytes written on each victim's private tmpfs until ENOSPC (at the strike point); survivors 0; Value = Σ", sum(filled), filled)
	return fc.finish(run, outcomeCheck("F8", fails, ev), false, m.ms, nil)
}

// ---------------------------------------------------------------------------
// F9: slow sink by injected commit latency.

func (fc *faultCell) f9() (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
	const vi = 1 // W4
	delays := []commitDelay{
		{From: 6, To: 10, DelayNs: int64(200 * time.Millisecond), Phase: "200ms"},
		{From: 11, To: 15, DelayNs: int64(time.Second), Phase: "1s"},
		{From: 16, To: 16, DelayNs: int64(20 * time.Second), Phase: "20s"},
	}
	fc.specs[vi].Fault, fc.specs[vi].Delays = faultDelay, delays
	v := fc.victim(vi, "Store.Commit delayed 200 ms ×5, 1 s ×5, then 20 s", "recording")
	run, err := fc.run(workerControl{drive: func(i int, p *workerProc, stop time.Time) (bool, string) {
		errText := driveToDone(p, stop)
		if i == vi {
			v.ended = ended(p)
			if r, err := fc.readResult(vi); err == nil {
				if at := r.PhaseStartNs["200ms"]; at > 0 {
					v.inject = at
					fc.mark(at)
				}
			}
			fc.inspect(v)
			fc.mark(time.Now().UnixNano())
		}
		return false, errText
	}})
	if err != nil {
		return qualify.Cell{}, nil, nil, err
	}
	var fails, ev []string
	var m metricSet
	r := run.results[vi]
	if r == nil {
		fails = append(fails, "victim wrote no result: "+run.errs[vi])
	} else {
		for _, d := range delays[:2] {
			lat := r.PhaseUs[d.Phase]
			if want := d.To - d.From + 1; len(lat) != want {
				fails = append(fails, fmt.Sprintf("%s: %d of %d delayed batches acknowledged (loss or backlog)", d.Phase, len(lat), want))
				continue
			}
			s := qualify.Summarize(lat)
			in := map[string]float64{"batches": float64(len(lat)), "injected_ms": float64(d.DelayNs) / 1e6}
			m.derived("victim.commit_"+d.Phase+"_p50", "ms", "the victim's acknowledged latency (BatchSample.Acknowledged − Start) of the commits delayed by "+d.Phase+" (injected before the inner Store.Commit)", "nearest_rank(phase_latency, 50/100)", in, float64(s.P50)/1e3, nil)
			m.derived("victim.commit_"+d.Phase+"_max", "ms", "maximum of the same", "max(phase_latency)", in, float64(s.Max)/1e3, nil)
			ev = append(ev, fmt.Sprintf("%s ×%d: all acknowledged, latency p50 %.1f ms max %.1f ms", d.Phase, len(lat), float64(s.P50)/1e3, float64(s.Max)/1e3))
		}
		stall := r.PhaseStartNs["20s"]
		switch {
		case stall == 0:
			fails = append(fails, "the 20 s delay was never reached")
		case r.ErrStage != "backlog" || !r.ErrStorage:
			fails = append(fails, fmt.Sprintf("not a typed backlog StorageError: stage %q: %s", r.ErrStage, r.Err))
		case r.FailedUnixNs < stall:
			fails = append(fails, "the capture failed before the 20 s delay: "+r.Err)
		default:
			d := time.Duration(r.FailedUnixNs - stall)
			m.measured("victim.backlog_detect", "s", "start of the 20 s delay → Capture.Failed (the StallAge watchdog)", d.Seconds(), nil)
			ev = append(ev, fmt.Sprintf("20 s: typed backlog StorageError %s after the stall began: %s", d.Round(time.Millisecond), r.Err))
		}
		if len(r.PhaseUs["20s"]) != 0 {
			fails = append(fails, "the stalled batch was acknowledged")
		}
		if v.inErr == nil && r.AckViews != v.in.AckViews {
			fails = append(fails, fmt.Sprintf("the acknowledged record holds %d views, the last acknowledged batch %d", v.in.AckViews, r.AckViews))
		}
	}
	if v.inErr != nil {
		fails = append(fails, "inspection: "+v.inErr.Error())
	} else {
		fails = append(fails, v.in.prefixVerifies("victim")...)
		ev = append(ev, "victim (stalled batch committed late, never acknowledged): "+v.in.summary())
	}
	ev = append(ev, fc.window(run).summary+"; it does not fail this check")
	return fc.finish(run, outcomeCheck("F9", fails, ev), false, m.ms, nil)
}

// ---------------------------------------------------------------------------
// F10: cancellation mid-stream; SIGTERM of a worker.

func (fc *faultCell) f10() (qualify.Cell, []qualify.Sample, []qualify.Finding, error) {
	n := len(fc.specs)
	sigterm := -1
	progressAt := 10
	if n > 1 {
		sigterm, progressAt = 0, 5
	}
	for i := range fc.specs {
		fc.specs[i].Fault, fc.specs[i].ProgressAtBatch = faultClose, progressAt
		variant := "input closed mid-stream without SetFinal"
		if i == sigterm {
			variant = "worker SIGTERMed mid-stream (no handler: the default action)"
		}
		fc.victim(i, variant, "recording")
	}
	var arrived sync.WaitGroup
	arrived.Add(n)
	release := make(chan struct{})
	var cmdAt atomic.Int64
	var termExit atomic.Int64
	run, err := fc.run(workerControl{
		afterGo: func(procs []*workerProc) {
			go func() {
				arrived.Wait()
				// Simultaneously: every closer gets "close", the SIGTERM
				// target its signal.
				cmdAt.Store(time.Now().UnixNano())
				for i, p := range procs {
					if i == sigterm {
						_ = p.cmd.Process.Signal(syscall.SIGTERM)
						continue
					}
					_ = p.send("close")
				}
				close(release)
			}()
		},
		drive: func(i int, p *workerProc, stop time.Time) (bool, string) {
			_, err := p.expect("progress", time.Until(stop))
			arrived.Done()
			<-release
			if err != nil {
				p.kill()
				return false, err.Error()
			}
			v := fc.victims[i]
			if i == sigterm {
				<-p.exited
				termExit.Store(time.Now().UnixNano())
				v.killed, v.ended = true, ended(p)
				return true, ""
			}
			errText := driveToDone(p, stop)
			v.ended = ended(p)
			return false, errText
		},
	})
	if err != nil {
		return qualify.Cell{}, nil, nil, err
	}
	// Inspection after every target ended (no survivors to overlap).
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range fc.specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fc.inspect(fc.victims[i])
		}()
	}
	wg.Wait()
	var fails, ev []string
	closeS := make([]float64, n)
	var closers []float64
	for i := range fc.specs {
		v := fc.victims[i]
		who := fmt.Sprintf("t%d", i)
		if fc.driveErrs[i] != "" {
			fails = append(fails, who+": "+fc.driveErrs[i])
			continue
		}
		if i != sigterm {
			r := run.results[i]
			switch {
			case r == nil:
				fails = append(fails, who+": no result")
				continue
			case r.CloseCmdUnixNs == 0 || r.ClosedAtViews == 0:
				fails = append(fails, fmt.Sprintf("%s: the input was not closed mid-stream (command %d, views at close %d): %s", who, r.CloseCmdUnixNs, r.ClosedAtViews, r.Err))
			case r.CloseRefusal == "":
				fails = append(fails, who+": Capture.Close() after Start did not refuse")
			default:
				closeS[i] = float64(r.CloseToDoneNs) / 1e9
				closers = append(closers, closeS[i])
			}
		}
		if v.inErr != nil {
			fails = append(fails, who+": inspection: "+v.inErr.Error())
			continue
		}
		fails = append(fails, v.in.prefixVerifies(who)...)
	}
	sorted := slices.Sorted(slices.Values(closers))
	var m metricSet
	if len(sorted) > 0 {
		p99 := qualify.NearestRank(sorted, 99, 100)
		m.measured("close.to_done", "s", "per closed target: receipt of the \"close\" command in the worker → Capture.Done (input closed at a line boundary without x0 or SetFinal); the SIGTERM target 0; Value = max", maxOf(closeS), closeS)
		m.derived("close.p99", "s", fmt.Sprintf("nearest-rank p99 of close.to_done over the %d closed targets (closed simultaneously)", len(sorted)), "nearest_rank(close.to_done, 99/100)",
			map[string]float64{"targets": float64(len(sorted)), "max_s": sorted[len(sorted)-1]}, p99, nil)
		ev = append(ev, fmt.Sprintf("time-to-close p99 %.3f s, max %.3f s over %d simultaneous closes", p99, sorted[len(sorted)-1], len(sorted)))
	} else {
		fails = append(fails, "no time-to-close was measured")
	}
	if sigterm >= 0 {
		d := time.Duration(termExit.Load() - cmdAt.Load())
		m.measured("sigterm.to_exit", "s", "SIGTERM (orchestrator wall time) → the worker's exit observed by the orchestrator", d.Seconds(), nil)
		ev = append(ev, fmt.Sprintf("t%d SIGTERMed: exited by %s after %s; %s", sigterm, fc.victims[sigterm].ended, d.Round(time.Millisecond), fc.victims[sigterm].in.summary()))
	}
	if r := run.results[min(1, n-1)]; r != nil {
		ev = append(ev, fmt.Sprintf("closer t%d: capture error %q; Capture.Close() after Start: %q; %s", min(1, n-1), r.Err, r.CloseRefusal, fc.victims[min(1, n-1)].in.summary()))
	}
	// Known limit: a started Capture has no cancellation API; closing its
	// input is the only way to cancel it.
	var findings []qualify.Finding
	if r := run.results[min(1, n-1)]; r != nil && r.CloseRefusal != "" {
		findings = append(findings, qualify.Finding{
			ID: "F10-no-cancel-after-start/" + fc.pc.Key(),
			Evidence: fmt.Sprintf("scenarios/%sF10/result.json#%s: Capture.Close() after Start refuses (%q); the plan's \"Close() mid-stream\" was exercised as closing the capture's input at a line boundary without x0 or SetFinal (capture error %q)",
				faultPrefix, fc.pc.Key(), r.CloseRefusal, r.Err),
			SuspectedCause: "known limit by design: after Start the pipeline owns its input, store and helper, and Capture has no cancellation method; the holder cancels by ending the recorder, which closes the FIFO",
			ProposedRemedy: "none required unless a direct cancellation API is wanted; a product decision (decision 12)",
		})
	}
	return fc.finish(run, outcomeCheck("F10", fails, ev), false, m.ms, findings)
}

// ---------------------------------------------------------------------------
// H4: Verify beside a live W3 writer.

func runH4(q qualifyEnv, env, cenv []string, bin, dir string, pc qualify.PlannedCell) ([]qualify.Metric, []qualify.Check, []qualify.Finding, []qualify.Sample, error) {
	fc, err := newFaultCell(q, env, cenv, bin, dir, "H4", pc, spansH4)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	fc.specs[0].ProgressAtBatch = 20
	store := filepath.Join(fc.specs[0].Rec, StoreDirName)
	var (
		runs, errs             int
		verifyMax              time.Duration
		walPeak, walStart      int64
		runGrowth, firstGrowth int64
		statuses               = map[string]int{}
		lastErr                string
		liveRuns               int
		verifyBegan, doneAt    time.Time
		viewsBegan, viewsDone  int64
	)
	ackViews := func() int64 {
		if ack, err := LoadAcknowledged(filepath.Join(fc.specs[0].Rec, AcknowledgedName)); err == nil {
			return ack.Views
		}
		return 0
	}
	run, err := fc.run(workerControl{drive: func(i int, p *workerProc, stop time.Time) (bool, string) {
		if _, err := p.expect("progress", time.Until(stop)); err != nil {
			p.kill()
			return false, err.Error()
		}
		walStart = walBytes(fc.specs[0].Rec)
		stopV := make(chan struct{})
		var live atomic.Bool
		live.Store(true)
		verified := make(chan struct{})
		var peak, runPeak atomic.Int64
		observe := func() {
			s := walBytes(fc.specs[0].Rec)
			if s > peak.Load() {
				peak.Store(s)
			}
			if s > runPeak.Load() {
				runPeak.Store(s)
			}
		}
		sampled := make(chan struct{})
		go func() { // WAL sampler, every 20 ms while Verify runs back to back
			defer close(sampled)
			tk := time.NewTicker(20 * time.Millisecond)
			defer tk.Stop()
			for {
				observe()
				select {
				case <-stopV:
					return
				case <-tk.C:
				}
			}
		}()
		verifyBegan, viewsBegan = time.Now(), ackViews()
		go func() {
			defer close(verified)
			for {
				select {
				case <-stopV:
					return
				default:
				}
				var expect *visualstore.Expectation
				if ack, err := LoadAcknowledged(filepath.Join(fc.specs[0].Rec, AcknowledgedName)); err == nil {
					expect = ack.Expectation()
				}
				start := walBytes(fc.specs[0].Rec)
				runPeak.Store(start)
				t0 := time.Now()
				rep, err := visualstore.Verify(context.Background(), store, expect, visualstore.DefaultLimits())
				d := time.Since(t0)
				observe()
				growth := runPeak.Load() - start
				if runs == 0 {
					firstGrowth = growth
				}
				runGrowth = max(runGrowth, growth)
				runs++
				if live.Load() {
					liveRuns++
				}
				verifyMax = max(verifyMax, d)
				statuses[rep.Status]++
				if err != nil {
					errs++
					lastErr = err.Error()
				}
			}
		}()
		errText := driveToDone(p, stop)
		live.Store(false)
		doneAt, viewsDone = time.Now(), ackViews()
		close(stopV)
		<-verified
		<-sampled
		walPeak = peak.Load()
		return false, errText
	}})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	b17 := 20.0
	if bf, err := qualify.LoadBudgets(); err == nil {
		if b, ok := bf.Budget("B17"); ok && len(b.Limits) > 0 {
			b17 = b.Limits[0].Value
		}
	}
	var m metricSet
	writerPeak := 0.0
	if r := run.results[0]; r != nil {
		writerPeak = float64(r.WALPeak) / mib
	}
	m.measured("h4.verify_live_wal_peak", "MiB", "peak Lstat size of visual.sqlite-wal sampled every 20 ms while exported visualstore.Verify (DefaultLimits, the owner's current acknowledged expectation) ran back to back beside the live W3 writer (E-metric; not a budget)", float64(walPeak)/mib, nil)
	m.measured("h4.wal_at_verify_start", "MiB", "WAL size when the first Verify began (after the writer's 20th acknowledged batch)", float64(walStart)/mib, nil)
	m.measured("h4.single_verify_wal_growth", "MiB", "largest WAL growth during one Verify run (peak during the run − size at its start), over the back-to-back runs", float64(runGrowth)/mib, nil)
	m.measured("h4.first_verify_wal_growth", "MiB", "WAL growth during the first Verify run alone (the WAL was not pinned by an earlier run)", float64(firstGrowth)/mib, nil)
	window := doneAt.Sub(verifyBegan).Seconds()
	m.derived("h4.writer_views_per_s", "count/s", "acknowledged views per wall second of the writer during the Verify window: the accelerated writer (W3 at real time is 20 views/s) grows the pinned WAL correspondingly faster than production would", "(ack_views_end - ack_views_start) / window_s",
		map[string]float64{"views": float64(viewsDone - viewsBegan), "window_s": window}, float64(viewsDone-viewsBegan)/max(window, 1e-9), nil)
	m.measured("h4.verify_runs", "count", "Verify runs while the writer was live (the worker had not reported done)", float64(liveRuns), nil)
	m.measured("h4.verify_wall_max", "s", "longest single Verify beside the live writer", verifyMax.Seconds(), nil)
	m.measured("h4.window", "s", "wall time from the first Verify to the writer's done", window, nil)
	var fails []string
	if liveRuns == 0 {
		fails = append(fails, "no Verify ran beside the live writer")
	}
	evidence := fmt.Sprintf("%d back-to-back Verify runs (%d while live, %d errors%s, statuses %v), longest %s; WAL peak %.2f MiB during Verify (writer's after-batch peak %.2f MiB; one run grew the WAL by at most %.2f MiB, the first by %.2f MiB; the accelerated writer committed %.0f views/s; B17 %g MiB)",
		runs, liveRuns, errs, func() string {
			if lastErr != "" {
				return ": " + lastErr
			}
			return ""
		}(), statuses, verifyMax.Round(time.Millisecond), float64(walPeak)/mib, writerPeak, float64(runGrowth)/mib, float64(firstGrowth)/mib, float64(viewsDone-viewsBegan)/max(window, 1e-9), b17)
	var findings []qualify.Finding
	if peakMiB := max(float64(walPeak)/mib, writerPeak); peakMiB > b17 {
		findings = append(findings, qualify.Finding{
			ID:             "H4/" + pc.Key(),
			Evidence:       "scenarios/" + h4Scenario + "/result.json#" + pc.Key() + ": " + evidence,
			SuspectedCause: "Verify runs every check in one read transaction and pins the WAL beside a live writer",
			ProposedRemedy: "freeze a selection first and verify each pass in its own short read transaction, as Reader does (decision 12: not fixed in V8)",
		})
	}
	outcome := check("h4-verify-beside-live-writer", fmt.Sprintf("exported visualstore.Verify (DefaultLimits) ran at package level beside a live W3 writer and its WAL peak is recorded as an E-metric; a peak above B17 (%g MiB) is a finding, not a budget exemption", b17), fails, evidence)
	m.measured("h4.writer_wal_peak_after_batch", "MiB", "the writer's WAL peak after every acknowledged batch (the wal.peak B17 would judge in a cell; report-only here)", writerPeak, nil)
	for i := range m.ms {
		m.ms[i].ReportOnly = true
	}
	checks := []qualify.Check{outcome, fc.survivorsCheck(run)}
	if !keepStores() {
		if err := os.RemoveAll(fc.dir); err != nil {
			return m.ms, checks, findings, run.samples, err
		}
	}
	return m.ms, checks, findings, run.samples, nil
}

// verifyRecovery validates the publication and the reported removal/retention
// against the filesystem, while all inputs to the pre-recovery checks are saved.
func (in *inspection) verifyRecovery(lib, rec, id string) {
	r := in.Recovery.Report
	if !r.Sealed {
		return
	}
	path := filepath.Join(lib, id+".areel")
	if r.SealedPath != path {
		in.RecoveryErr = "unexpected sealed path: " + r.SealedPath
		return
	}
	rep, err := visualstore.VerifySealed(context.Background(), path, nil, visualstore.DefaultLimits())
	in.RecoveryVerify = r.Verify
	if err == nil && (rep.Counts.Views != r.Views || rep.Status != visualstore.StatusIncomplete && r.Kind == "interrupted" || rep.Status != visualstore.StatusOK && r.Kind == "complete") {
		err = fmt.Errorf("sealed visual verify: %s, views %d (report %d)", rep.Status, rep.Counts.Views, r.Views)
	}
	if err != nil {
		in.RecoveryErr = err.Error()
		return
	}
	_, err = os.Lstat(rec)
	if r.RemovedDirectory {
		if !errors.Is(err, os.ErrNotExist) || r.Retained != "" {
			in.RecoveryErr = fmt.Sprintf("reported removal disagrees with source/retained: %v / %q", err, r.Retained)
		}
	} else if err != nil || r.Retained == "" {
		in.RecoveryErr = fmt.Sprintf("reported retention lacks source/reason: %v / %q", err, r.Retained)
	}
}

func (in inspection) recoverySummary() string {
	r := in.Recovery.Report
	return fmt.Sprintf("recover kind %s, sealed %t, views %d, dropped views/activity %d/%d, removed %t, retained %q, error %q; seal verify error %q",
		r.Kind, r.Sealed, r.Views, r.DroppedViews, r.DroppedActivityRows, r.RemovedDirectory, r.Retained, in.Recovery.Err, in.RecoveryErr)
}

// F1/recovery never promote the unacknowledged tail to the recovered artifact.
// The whole-file verifier also checks the interrupted manifest and absence of
// a completion record. The dropped tail remains in the retained directory.
func (in inspection) recoveredPrefix(sourceViews, droppedActivity int64) []string {
	r := in.Recovery.Report
	v := in.RecoveryVerify
	if in.Recovery.Err != "" || in.RecoveryErr != "" || !r.Sealed ||
		r.Kind != "interrupted" || r.Views != in.AckViews ||
		r.DroppedViews != sourceViews-in.AckViews || r.DroppedActivityRows != droppedActivity ||
		v == nil || v.Status != "ok" || v.Kind != "interrupted" ||
		v.Manifest.Views.Views != r.Views || v.Visual.Complete {
		return []string{in.recoverySummary()}
	}
	if r.DroppedViews > 0 || r.DroppedActivityRows > 0 {
		if r.RemovedDirectory || r.Retained == "" {
			return []string{in.recoverySummary()}
		}
	} else if !r.RemovedDirectory || r.Retained != "" {
		return []string{in.recoverySummary()}
	}
	return nil
}
