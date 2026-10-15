package reel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

var errProcessAbsent = errors.New("numeric process stat absent")

// Only absence of this exact numeric proc stat path establishes absence. A
// missing boot/mount/provenance file must remain unknown, never dead.
func observeIdentity(pid int) (ProcessIdentity, error) {
	id, e := identify(pid)
	var path *os.PathError
	if errors.As(e, &path) && path.Path == fmt.Sprintf("/proc/%d/stat", pid) && errors.Is(path.Err, os.ErrNotExist) {
		return id, errProcessAbsent
	}
	return id, e
}

var namespacePattern = regexp.MustCompile(`^pid:\[[1-9][0-9]{0,19}\]$`)
var bootPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type processView struct{ Boot, Namespace string }
type processEvidence struct {
	Role     string          `json:"role"`
	Identity ProcessIdentity `json:"identity"`
	Status   string          `json:"status"`
	Reason   string          `json:"reason"`
}
type offlineObservation struct {
	view     func() (processView, error)
	identity func(int) (ProcessIdentity, error)
}

func shortDiagnostic(s string) string {
	if len(s) > 256 {
		return s[:256] + " [truncated diagnostic]"
	}
	return s
}
func evidence(role string, id ProcessIdentity, view processView, viewErr error, lookup func(int) (ProcessIdentity, error)) processEvidence {
	r := processEvidence{Role: role, Identity: id, Status: "unknown", Reason: "missing, unreadable or mismatched provenance"}
	start, e := strconv.ParseUint(id.Start, 10, 64)
	if id.PID < 2 || id.PID > 2147483647 || e != nil || start == 0 || strconv.FormatUint(start, 10) != id.Start || !bootPattern.MatchString(id.Boot) || !(namespacePattern.MatchString(id.PIDNamespace) || id.PIDNamespace == "darwin:host") || viewErr != nil || id.Boot != view.Boot || id.PIDNamespace != view.Namespace {
		return r
	}
	now, e := lookup(id.PID)
	if errors.Is(e, errProcessAbsent) {
		r.Status, r.Reason = "absent", "not present in matching unrestricted proc view"
		return r
	}
	if e != nil || now.Boot != view.Boot || now.PIDNamespace != view.Namespace {
		return r
	}
	currentStart, parseErr := strconv.ParseUint(now.Start, 10, 64)
	if now.PID != id.PID || parseErr != nil || currentStart == 0 || strconv.FormatUint(currentStart, 10) != now.Start {
		return r
	}
	if now.Start != id.Start {
		r.Status, r.Reason = "absent", "numeric PID belongs to a different generation"
		return r
	}
	r.Status, r.Reason = "present", "matching identity observed; liveness does not prove health"
	return r
}
func existingPrivateDirectory(path string) error {
	if e := archive.CheckDirectory(path); e != nil {
		return e
	}
	st, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if !privateOwner(st) {
		return fmt.Errorf("directory must be owned by current user")
	}
	return nil
}
func offlineJSON(path string, max int, target any) ([]byte, error) {
	st, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || !privateOwner(st) || st.Size() > int64(max) {
		return nil, fmt.Errorf("unsafe/oversized private metadata")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	data, e := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if e != nil {
		return nil, e
	}
	if len(data) > max {
		return nil, fmt.Errorf("metadata exceeds bound")
	}
	decoded := data
	if _, ok := target.(*State); ok {
		// Persisted state from older writers may carry an inert obsolete
		// `version: 1` member. Returned bytes stay the original file contents.
		decoded = archive.WithoutLegacyVersion(data)
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(target); e != nil {
		return nil, e
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("trailing metadata")
	}
	return data, f.Close()
}

// offlineRecording is the doctor (and, with a destination, the store salvage)
// of one recording: a sealed file or a visual/1 recording directory. A legacy
// raw recording is refused (errLegacyRecording); nothing is inspected.
func offlineRecording(ctx context.Context, library, recording, destination string, limits visualstore.Limits, observe offlineObservation, templates ...string) (report offlineVisualReport, err error) {
	if library == "" || !validID.MatchString(recording) {
		return report, fmt.Errorf("explicit library and recording ID required")
	}
	if e := platformCheck(); e != nil {
		return report, e
	}
	library, err = offlineAbsolute(library)
	if err != nil {
		return report, err
	}
	g := newLibraryGuard(withSealName(ctx, firstSealName(templates)))
	loc, e := libraryLocate(library, recording, g)
	if e != nil {
		return report, e
	}
	if loc.Kind == "sealed" {
		if destination != "" {
			return report, fmt.Errorf("salvage requires a recording directory")
		}
		report, e = offlineSealed(ctx, loc, recording, limits)
		var launch launchSpec
		if raw, le := locationSource(loc, "launch.json", g); le == nil {
			_ = json.Unmarshal(raw, &launch)
		}
		if leftovers, le := recoverLeftovers(library, recording, firstSealName(templates), launch.SealName, "{id}"); le == nil && len(leftovers) > 0 {
			report.RecoverHint = true
		}
		if e == nil {
			e = g.check()
		}
		return report, e
	}
	recordingDir := loc.Dir
	for _, p := range []string{library, filepath.Join(library, "recordings"), recordingDir} {
		if e := existingPrivateDirectory(p); e != nil {
			return report, e
		}
	}
	var state State
	stateBytes, e := offlineJSON(filepath.Join(recordingDir, "state.json"), 4<<20, &state)
	if e != nil {
		return report, e
	}
	if state.ID != recording || state.Target.Backend == "" || state.Target.Instance == "" || len(state.Target.Backend) > 256 || len(state.Target.Instance) > 256 {
		return report, fmt.Errorf("invalid recording identity")
	}
	switch state.Status {
	case "starting", "recording", "pausing", "stopping", "paused", "stopped", "failed", "interrupted":
	default:
		return report, fmt.Errorf("invalid persisted status")
	}
	if state.Pending != "" && state.Pending != "pause" && state.Pending != "stop" {
		return report, fmt.Errorf("invalid persisted intent")
	}
	for i, s := range state.Segments {
		if s.Number != i+1 {
			return report, fmt.Errorf("ambiguous segment numbering")
		}
	}
	if e = requireVisualFormat(state.Format); e != nil {
		return report, e
	}
	return offlineVisual(ctx, library, recordingDir, state, stateBytes, destination, limits, observe, templates...)
}

const verificationBudgetHelp = `
Visual verification derives work = 65536 + 8*(G+B+C) + 4*(R+E+F) +
2*S*(rowsMax+4) + 4*V and decoded bytes = 8 MiB + 3*R*rowBytes +
S*(10*rowsMax+16): G/B/C/R/S/E/F/V count segments/blocks/sealed chunks
(including heads; zero for directories)/rows/screens/events/findings/views;
rowsMax and colsMax are valid screen maxima (screens or sealed screen_heads),
unioned with segment geometry to cover resizes. rowBytes=min(MaxRowBytes,
14*colsMax+16); canonical cells, including RGB colors, use at most 14 bytes.
The screen term has 2x headroom for references and geometry/page overhead.
When the sealed row-hash table exceeds its cap or is unavailable after damage,
add S*rowsMax work and S*rowsMax*MaxChunkRawBytes decoded bytes. The damaged
view walk has a separate allowance: V*rowsMax work and V*rowsMax*rowBytes
decoded bytes; sealed files also add V*rowsMax work for row-chunk inflation
and V*(rowsMax+1)*MaxChunkRawBytes decoded bytes for row and screen chunks.
Activity allows 1024+8*rows.
Each allowance is capped by caller ceilings: 256 GiB decoded bytes and
2000000000 work steps by default. --decoded-bytes and --work override ceilings
(up to 1 TiB and 10000000000 steps); they cannot enlarge store allowances.
Exhaustion is unverified and names the store allowance or caller ceiling.`

var doctorHelp = cliCommand{Name: "doctor", Args: "[REC]",
	Summary: `Without REC: check this machine — platform, the Zellij version (any usable
one; reported against the validated versions), the tested asciinema baseline,
the avt readout helper, Chromium/FFmpeg for video export, the library and the
config file. Exits 1 when capture prerequisites are missing.
With REC: verify one recording offline (no backend discovery or control IPC):
process ownership, visual store integrity against the acknowledged prefix, and
the activity store; or a sealed file. Legacy raw recordings are refused.` + verificationBudgetHelp,
	Examples: []string{"areel doctor", "areel doctor latest", "areel doctor 3f2a --json"},
	Docs:     "ARCHIVE.md, docs/visual-recording-capture.md"}

func doctorCLI(args []string, out, errOut io.Writer) error {
	return offlineCLI(args, out, errOut)
}
func offlineCLI(args []string, out, errOut io.Writer) error {
	return offlineCLIWithObservation(args, out, errOut, offlineObservation{currentProcView, observeIdentity})
}

func offlineCLIWithObservation(args []string, out, errOut io.Writer, observe offlineObservation) error {
	if args[0] != "doctor" {
		return unknownCommand(args[0])
	}
	cmd, min, max := doctorHelp, 0, 1
	f := newCLIFlags(cmd, out)
	f.library()
	visualLimits := visualstore.DefaultLimits()
	f.Int64Var(&visualLimits.DecodedBytes, "decoded-bytes", "N", visualLimits.DecodedBytes, "caller decoded-byte ceiling")
	f.IntVar(&visualLimits.Work, "work", "N", visualLimits.Work, "caller verification work ceiling (visual store and activity)")
	asJSON := f.json()
	pos, help, e := f.parse(args[1:], min, max)
	if help || e != nil {
		return e
	}
	if len(pos) == 0 {
		for _, name := range []string{"decoded-bytes", "work"} {
			if f.isSet(name) {
				return usageErr("doctor: --%s needs a recording (areel doctor REC --%s ...)", name, name)
			}
		}
		return doctorEnvironmentCLI(f, out, *asJSON)
	}
	if strings.HasSuffix(pos[0], ".areel") {
		return standaloneSealedDoctor(context.Background(), pos[0], visualLimits, *asJSON, out)
	}
	if e = checkREC(pos[0]); e != nil {
		return e
	}
	s, e := f.load(nil)
	if e != nil {
		return e
	}
	library, e := requireLibrary(s)
	if e != nil {
		return e
	}
	id, e := resolveREC(library, pos[0], currentRecording(library, s), s.SealName)
	if e != nil {
		return e
	}
	report, e := offlineRecording(context.Background(), library, id, "", visualLimits, observe, s.SealName)
	if report.Recording == "" {
		// Nothing was inspected: never print an empty report.
		return e
	}
	if !*asJSON {
		return errors.Join(e, formatOffline(out, report))
	}
	data, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		return marshalErr
	}
	if len(data) > offlineVisualReportBytes {
		return fmt.Errorf("inspection report exceeds bound")
	}
	data = append(data, '\n')
	n, writeErr := out.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	return errors.Join(e, writeErr)
}

func offlineAbsolute(path string) (string, error) {
	for _, part := range bytes.Split([]byte(path), []byte{os.PathSeparator}) {
		if bytes.Equal(part, []byte("..")) {
			return "", fmt.Errorf("parent traversal forbidden")
		}
	}
	return filepath.Abs(path)
}
