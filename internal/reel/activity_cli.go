package reel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
)

var activityHelp = map[string]cliCommand{
	"send": {Name: "activity send",
		Summary: `Integration: submit one bounded activity event (JSON on stdin) to the holder
of an explicitly bound recording. Prints the receipt JSON. Acceptance is not
durability; retry the same event ID to read its current receipt state.`,
		Examples: []string{"areel activity send --recording ID --generation G < event.json", "areel activity send --recording ID --library LIB < event.json"},
		Docs:     "docs/integrations.md (Activity)"},
	"mark": {Name: "activity mark",
		Summary:  `Integration: like activity send, but the event is a navigation-worthy mark.`,
		Examples: []string{"areel activity mark --recording ID --generation G < event.json", "areel activity mark --recording ID --library LIB < event.json"},
		Docs:     "docs/integrations.md (Activity)"},
	"status": {Name: "activity status",
		Summary:  `Integration: print the activity receiver status of a bound recording as JSON.`,
		Examples: []string{"areel activity status --recording ID", "areel activity status --recording ID --library LIB"},
		Docs:     "docs/integrations.md (Activity)"},
	"register": {Name: "producer register",
		Summary: `Integration: register a producer descriptor (JSON on stdin) with the holder
of a bound recording. Prints the registration with its generation as JSON.`,
		Examples: []string{"areel producer register --recording ID < producer.json", "areel producer register --recording ID --library LIB < producer.json"},
		Docs:     "docs/integrations.md (Activity)"},
	"refresh": {Name: "producer refresh",
		Summary:  `Integration: refresh a producer registration (generation in the stdin JSON).`,
		Examples: []string{"areel producer refresh --recording ID < producer.json", "areel producer refresh --recording ID --library LIB < producer.json"},
		Docs:     "docs/integrations.md (Activity)"},
	"replace": {Name: "producer replace",
		Summary:  `Integration: replace a producer registration with a new generation.`,
		Examples: []string{"areel producer replace --recording ID < producer.json", "areel producer replace --recording ID --library LIB < producer.json"},
		Docs:     "docs/integrations.md (Activity)"},
}

// activityOperations maps a subcommand to its holder wire operation.
var activityOperations = map[string]string{
	"send": "activity", "mark": "marker", "status": "activity-status",
	"register": "producer-register", "refresh": "producer-refresh", "replace": "producer-replace",
}

// activityCLI is routed before backend construction and ignores ambient binding.
// input is injected in tests; CLI supplies stdin only for these explicit commands.
// args[0] is the subcommand: send, mark, status, register, refresh or replace.
func activityCLI(args []string, input io.Reader, out, errOut io.Writer) error {
	operation, ok := activityOperations[args[0]]
	if !ok {
		return usageErr("unknown activity operation %q", args[0])
	}
	f := newCLIFlags(activityHelp[args[0]], out)
	f.library()
	recording := f.String("recording", "ID", "", "explicitly bound recording ID (required)")
	backend := f.String("backend", "NAME", "", "optional backend assertion, not discovery")
	var generation *string
	if operation == "activity" || operation == "marker" {
		generation = f.String("generation", "G", "", "receiver-issued producer generation")
	} else {
		generation = new(string)
	}
	instance := f.String("instance", "ID", "", "optional target-instance assertion")
	_, help, e := f.parse(args[1:], 0, 0)
	if help || e != nil {
		return e
	}
	if !validID.MatchString(*recording) {
		return usageErr("%s: --recording must be a full 32-hex recording ID (got %q)", f.cmd.Name, *recording)
	}
	s, e := f.load(nil)
	if e != nil {
		return e
	}
	library, e := requireLibrary(s)
	if e != nil {
		return e
	}
	root, e := offlineAbsolute(library)
	if e != nil {
		return e
	}
	if e = archive.CheckDirectory(root); e != nil {
		return e
	}
	store := &Store{Root: root, SealName: s.SealName}
	dir, state, spec, e := activityBinding(store, *recording)
	if e != nil {
		return e
	}
	if (*backend != "" && *backend != state.Target.Backend) || (*instance != "" && *instance != state.Target.Instance) {
		return fmt.Errorf("activity explicit binding assertion conflicts")
	}
	req := activityRequest{Token: spec.Token, Recording: state.ID, Backend: state.Target.Backend, Instance: state.Target.Instance, Operation: operation}
	if operation == "activity-status" {
		req.Operation = "status"
	} else if operation == "producer-register" || operation == "producer-refresh" || operation == "producer-replace" {
		data, e := io.ReadAll(io.LimitReader(input, producerDescriptorMax+257))
		if e != nil {
			return e
		}
		var reg producerInput
		if len(data) > producerDescriptorMax+256 || strictActivityJSON(data, &reg) != nil {
			return fmt.Errorf("invalid bounded producer input")
		}
		if e = validateProducer(reg.Descriptor, state.Target); e != nil {
			return e
		}
		req.Descriptor = &reg.Descriptor
		req.Generation = reg.Generation
	} else {
		data, e := io.ReadAll(io.LimitReader(input, activityEnvelopeMax+1))
		if e != nil {
			return e
		}
		if _, e = parseActivity(data, state.Target); e != nil {
			return e
		}
		req.Envelope = data
		if *generation != "" {
			if !validID.MatchString(*generation) {
				return fmt.Errorf("invalid producer generation")
			}
			req.Generation = *generation
		}
	}
	res, e := callActivity(dir, req)
	if e != nil {
		return e
	}
	if e = json.NewEncoder(out).Encode(res); e != nil {
		return e
	}
	if res.Error != "" {
		return fmt.Errorf("activity: %s", res.Error)
	}
	// Pending admission is a successful *acceptance*, explicitly not durability.
	// The caller may retry the same ID/payload to obtain its current receipt state.
	return nil
}

// errActivitySuperseded refuses a binding whose target has a newer recording.
var errActivitySuperseded = errors.New("activity recording superseded")

// activityBinding checks that recording id is a live, current, visual/1
// directory recording with a holder to receive activity: sealed, stopped and
// superseded recordings refuse. It returns the recording directory with its
// state and launch spec.
func activityBinding(store *Store, id string) (dir string, state State, spec launchSpec, e error) {
	loc, e := store.locate(id)
	if e != nil {
		return "", State{}, launchSpec{}, e
	}
	if loc.Kind == "sealed" {
		return "", State{}, launchSpec{}, fmt.Errorf("invalid or stopped recording binding: sealed file has no activity receiver")
	}
	dir, _ = store.dir(id)
	if e = archive.CheckDirectory(dir); e != nil {
		return "", State{}, launchSpec{}, e
	}
	if _, e = offlineJSON(filepath.Join(dir, "state.json"), 4<<20, &state); e != nil {
		return "", State{}, launchSpec{}, e
	}
	if _, e = offlineJSON(filepath.Join(dir, "launch.json"), 16<<10, &spec); e != nil {
		return "", State{}, launchSpec{}, e
	}
	if state.ID != id || !state.Target.Equal(spec.Target) || state.Status == "stopped" || !activityText(state.Target.Backend, 128) || !activityText(state.Target.Instance, 512) {
		return "", State{}, launchSpec{}, fmt.Errorf("invalid or stopped recording binding")
	}
	if e = requireVisualFormat(state.Format); e != nil {
		return "", State{}, launchSpec{}, e
	}
	var current string
	if _, e = offlineJSON(store.targetPath(state.Target), 128, &current); e != nil {
		return "", State{}, launchSpec{}, e
	}
	if current != state.ID {
		return "", State{}, launchSpec{}, errActivitySuperseded
	}
	return dir, state, spec, nil
}

// callActivity sends one request to the recording's activity socket with the
// standard 1 s connect and 3 s exchange deadlines.
func callActivity(dir string, req activityRequest) (activityResponse, error) {
	return callActivityWithin(dir, req, time.Second, 3*time.Second)
}

// callActivityWithin is callActivity with explicit connect and exchange
// deadlines (areel hook uses shorter ones).
func callActivityWithin(dir string, req activityRequest, connect, exchange time.Duration) (activityResponse, error) {
	var res activityResponse
	socket := filepath.Join(dir, "activity.sock")
	st, e := os.Lstat(socket)
	if e != nil {
		return res, fmt.Errorf("activity endpoint unavailable: %w", e)
	}
	if st.Mode()&os.ModeSocket == 0 || st.Mode().Perm()&0077 != 0 || !privateOwner(st) {
		return res, fmt.Errorf("unsafe activity socket")
	}
	data, e := json.Marshal(req)
	if e != nil {
		return res, e
	}
	if len(data) > activityRequestMax {
		return res, fmt.Errorf("activity request exceeds limit")
	}
	conn, e := net.DialTimeout("unix", socket, connect)
	if e != nil {
		return res, fmt.Errorf("activity endpoint unavailable: %w", e)
	}
	defer conn.Close()
	if e = conn.SetDeadline(time.Now().Add(exchange)); e != nil {
		return res, e
	}
	if n, e := conn.Write(data); e != nil {
		return res, e
	} else if n != len(data) {
		return res, io.ErrShortWrite
	}
	if e = conn.(*net.UnixConn).CloseWrite(); e != nil {
		return res, e
	}
	// The reply must be the one bounded response shape: unknown, aliased or
	// duplicate keys refuse rather than decode loosely.
	data, e = io.ReadAll(io.LimitReader(conn, 16<<10+1))
	if e != nil {
		return res, e
	}
	if len(data) > 16<<10 || strictActivityJSON(data, &res) != nil {
		return res, fmt.Errorf("invalid activity response")
	}
	return res, nil
}
