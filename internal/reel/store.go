package reel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

type Event struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	At      time.Time `json:"at"`
	Segment int       `json:"segment"`
}

// Segment.Path is relative to the recording directory: the literal "visual",
// the one visual store spanning every segment of the recording.
type Segment struct {
	Launched    bool            `json:"launched"` // false: refused attempt; no recorder process existed
	Number      int             `json:"number"`
	Path        string          `json:"path"`
	Started     time.Time       `json:"started"`
	Ended       *time.Time      `json:"ended,omitempty"`
	Observation Observation     `json:"observation"`
	Error       string          `json:"error,omitempty"`
	Live        *LiveCapability `json:"live,omitempty"`
}
type State struct {
	// created supplies visual metadata to name rendering when there is no segment.
	// It is never serialized: callers must supply the store meta.created instant.
	created    time.Time
	ID         string          `json:"id"`
	Target     Target          `json:"target"`
	Status     string          `json:"status"`
	Pending    string          `json:"pending,omitempty"`
	Suppressed bool            `json:"suppressed"`
	Holder     ProcessIdentity `json:"holder"`
	Segments   []Segment       `json:"segments"`
	Events     []Event         `json:"events"`
	Error      string          `json:"error,omitempty"`
	// Context is the explicitly declared start context for library grouping:
	// the starting command's working directory and, when a bounded read-only
	// JJ lookup answers, the main (default) workspace root. Absent for older
	// recordings and never backfilled or discovered from ambient state later.
	Context *RecordingContext `json:"context,omitempty"`
	// Storage is the bounded retention/storage-health projection of
	// RETENTION.md: the warning thresholds in effect for this logical recording,
	// at most one warning per threshold, and the first storage failure. The
	// holder is its only writer; absent for older recordings and for a
	// recording no holder has assigned itself to yet.
	Storage *StorageState `json:"storage,omitempty"`
	// Format is the recording format, always activityFormatIndexed
	// ("visual/1") for recordings created now. "" (absent) marks a recording in
	// the removed legacy raw format: it is listed as unsupported and every
	// reader, control and export refuses it (errLegacyRecording).
	Format string `json:"format,omitempty"`
}

// StorageState never carries paths, raw errors or archive content.
type StorageState struct {
	Thresholds StorageThresholds `json:"thresholds"`
	Warnings   []StorageWarning  `json:"warnings,omitempty"`
	Failure    *StorageFailure   `json:"failure,omitempty"`
}

// StorageWarning records one crossed threshold, once per logical recording.
// Observed is the authoritative archive progress that crossed it: durable
// stored bytes, or validated footage seconds on the recorder's relative clock,
// summed over every segment; never receipt wall time.
type StorageWarning struct {
	Kind      string    `json:"kind"` // stored-bytes | footage-seconds
	Threshold float64   `json:"threshold"`
	Observed  float64   `json:"observed"`
	Segment   int       `json:"segment"` // segment being captured when crossed
	At        time.Time `json:"at"`
}

// StorageFailure classifies an interruption or refusal caused by the archive's
// own storage (a failed durable write, a directory that could not be created,
// or a backlog with no durable progress). Reason is the bounded operating
// system errno text when there is one, never a path. The full diagnostic stays
// in the recording's `error`.
type StorageFailure struct {
	Kind    string    `json:"kind"` // create | publish | backlog
	Reason  string    `json:"reason,omitempty"`
	Segment int       `json:"segment"`
	At      time.Time `json:"at"`
}

const storageWarningsMax = 8 // readers refuse more; the writer records at most one per kind

func (s *StorageState) warned(kind string) bool {
	if s == nil {
		return false
	}
	for _, w := range s.Warnings {
		if w.Kind == kind {
			return true
		}
	}
	return false
}

// RecordingContext holds bounded declared paths for the library UI only. It is
// not authority for anything and is never used to browse a filesystem.
type RecordingContext struct {
	WorkingDirectory string `json:"workingDirectory,omitempty"`
	JJMain           string `json:"jjMain,omitempty"`
	JJMainSource     string `json:"jjMainSource,omitempty"`
}
type launchSpec struct {
	SealName string `json:"sealName,omitempty"`
	Target   Target `json:"target"`
	Token    string `json:"token"`
}

// Store.Context, when set by the CLI before a start, is recorded on any
// recording created by that command. Tests and bound controls leave it nil.
//
// Store.Launch, when set, replaces the built-in detached exec of the holder:
// it must start a holder for recording id in library root (the equivalent of
// `areel _hold root id`) in whatever process scope the caller owns, for
// example a transient per-recording user-systemd unit, and return once the
// process has been handed off. The CLI leaves it nil; the reachability wait,
// the private socket and every holder semantic are unchanged either way.
type Store struct {
	SealName string
	Root     string
	Context  *RecordingContext
	Launch   func(root, id string) error
}

var validID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func newID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func privateDir(path string) error {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		parent := filepath.Dir(path)
		if _, e := os.Stat(parent); os.IsNotExist(e) {
			if err := privateDir(parent); err != nil {
				return err
			}
		}
		if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		if err := syncDirectory(parent); err != nil {
			return err
		}
	}
	s, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !s.IsDir() || s.Mode().Perm()&0077 != 0 || !privateOwner(s) {
		return fmt.Errorf("directory must be owned, non-symlink and private (0700): %s", path)
	}
	return nil
}
func syncDirectory(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	return errors.Join(f.Sync(), f.Close())
}
func OpenStore(root string) (*Store, error) {
	if err := platformCheck(); err != nil {
		return nil, err
	}
	if root == "" {
		return nil, fmt.Errorf("--library or AREEL_LIBRARY required; no default persistent recording location")
	}
	root, e := filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	if e = privateDir(root); e != nil {
		return nil, e
	}
	// A private root is the filesystem trust boundary, not protection from the same UID.
	for _, d := range []string{"recordings", "targets"} {
		if e = privateDir(filepath.Join(root, d)); e != nil {
			return nil, e
		}
	}
	return &Store{Root: root, SealName: "{id}"}, nil
}
func (s *Store) dir(id string) (string, error) {
	if !validID.MatchString(id) {
		return "", fmt.Errorf("invalid recording ID")
	}
	return filepath.Join(s.Root, "recordings", id), nil
}
func (s *Store) read(id string) (State, error) {
	g := newLibraryGuard(withSealName(context.Background(), s.SealName))
	loc, e := libraryLocate(s.Root, id, g)
	if e != nil {
		return State{}, e
	}
	if loc.Kind == "sealed" {
		v, _, e := libraryState(s.Root, id, g)
		if e == nil {
			e = g.check()
		}
		return v, e
	}
	var v State
	e = readJSON(filepath.Join(loc.Dir, "state.json"), &v)
	if e == nil {
		e = g.check()
	}
	return v, e
}
func readJSON(path string, v any) error {
	st, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || !privateOwner(st) {
		return fmt.Errorf("unsafe private file: %s", path)
	}
	if st.Size() > 4<<20 {
		return fmt.Errorf("metadata too large")
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func writeJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	b = append(b, '\n')
	f, e := os.CreateTemp(filepath.Dir(path), ".state-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	return syncDirectory(filepath.Dir(path))
}
func (s *Store) save(v State) error {
	d, e := s.dir(v.ID)
	if e != nil {
		return e
	}
	return writeJSON(filepath.Join(d, "state.json"), v)
}
func (s *Store) withLock(fn func() error) error {
	path := filepath.Join(s.Root, "lock")
	if st, e := os.Lstat(path); e == nil && (!st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || !privateOwner(st)) {
		return fmt.Errorf("unsafe lock file")
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	if e = lockFile(f); e != nil {
		return e
	}
	defer unlockFile(f)
	return fn()
}

// targetKey is the target index name: a digest of the backend-qualified
// selection key only, never of the address or the display name.
func targetKey(t Target) string {
	sum := sha256.Sum256([]byte(t.Backend + "\x00" + t.Instance))
	return hex.EncodeToString(sum[:])
}
func (s *Store) targetPath(t Target) string {
	return filepath.Join(s.Root, "targets", targetKey(t)+".json")
}
func (s *Store) current(t Target) (State, error) {
	var id string
	if e := readJSON(s.targetPath(t), &id); e != nil {
		return State{}, e
	}
	return s.read(id)
}
func (s *Store) create(t Target) (State, error) {
	template := s.SealName
	if template == "" {
		template = "{id}"
	}
	if _, err := parseSealName(template); err != nil {
		return State{}, err
	}
	// Control and the supervisor both create through here. A refused library
	// configuration refuses creation; every recording is visual/1.
	if _, e := readLibraryConfig(s.Root); e != nil {
		return State{}, e
	}
	v := State{ID: newID(), Target: t, Status: "starting", Segments: []Segment{}, Events: []Event{}, Context: s.Context, Format: activityFormatIndexed}
	d, _ := s.dir(v.ID)
	if len(filepath.Join(d, "control.sock")) >= 104 {
		return v, fmt.Errorf("library path too long for private Unix socket; use a shorter explicit library path")
	}
	if e := privateDir(d); e != nil {
		return v, e
	}
	if e := writeJSON(filepath.Join(d, "launch.json"), launchSpec{Target: t, Token: newID(), SealName: template}); e != nil {
		return v, e
	}
	if e := s.save(v); e != nil {
		return v, e
	}
	return v, writeJSON(s.targetPath(t), v.ID)
}
func (v *State) event(kind string) {
	v.Events = append(v.Events, Event{ID: fmt.Sprintf("%s:%d", v.ID, len(v.Events)+1), Kind: kind, At: time.Now().UTC(), Segment: len(v.Segments)})
}
