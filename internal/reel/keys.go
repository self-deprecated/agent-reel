package reel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/keydecode"
)

// Keystroke capture (docs/recording.md, "Keystrokes").
//
// `areel attach SESSION` is a PTY proxy around `zellij attach` that decodes
// the keys its own client sends (internal/keydecode) and submits them, display
// only, as ordinary activity records to the live holder of that session's
// recording: source "areel:attach", kind "areel:keys", data
// {client, agoMs, keys:[{dt,label,mod,n,paste,masked}]}. Nothing new is stored
// or sealed: the records live in the recording's indexed activity store like
// any other activity. Raw input bytes never leave the wrapper.
//
// Capture is opt-in per recording (`areel keys on`, a consent file in the
// recording directory the wrapper re-reads), can be paused per Zellij session
// (`areel keys pause`, a flag file in the private runtime directory) and is
// masked while a focused pane reads a password (ICANON set, ECHO clear).

const (
	keysSource   = "areel:attach"
	keysKind     = "areel:keys"
	keysProducer = "areel-attach"
	// keysConsentName is the per-recording opt-in, in recordings/ID/.
	keysConsentName = "keys-consent.json"
	// Bounds of one stored batch. A batch is split before its envelope
	// reaches keysEnvelopeTarget (the activity ingress cap is 8 KiB).
	keysEntriesMax     = 256
	keysLabelMax       = 256
	keysClientMax      = 128
	keysEnvelopeTarget = 6 << 10
	// keysSpanMax bounds dt and agoMs: a batch never spans more than this.
	keysSpanMax = int64(time.Hour / time.Millisecond)
)

// isKeysItem reports a stored keystroke batch: kept out of the activity and
// marks lists and out of export events (the overlay is their only view).
func isKeysItem(item activityListItem) bool {
	return item.Record != nil && isKeysEnvelope(item.Record.Envelope)
}

func isKeysEnvelope(v activityEnvelope) bool { return isKeysKindSource(v.Kind, v.Source) }

// isKeysMark is the same predicate for a marks-page row.
func isKeysMark(m libraryMark) bool { return isKeysKindSource(m.Kind, m.Source) }

// isKeysKindSource is the one keystroke-batch predicate: kind and source.
// viewer-adapter.mjs applies the same test to marks JSON rows.
func isKeysKindSource(kind, source string) bool { return kind == keysKind && source == keysSource }

// keysPaste is the only thing stored about a paste: its size.
type keysPaste struct {
	Bytes int `json:"bytes"`
	Lines int `json:"lines"`
}

// keysEntry is one stroke of a batch. Dt is milliseconds after the previous
// entry of the batch (the first entry's is 0). Label and Mod are empty for a
// paste or masked entry; N is the repeat count when above 1.
type keysEntry struct {
	Dt     int64      `json:"dt"`
	Label  string     `json:"label,omitempty"`
	Mod    string     `json:"mod,omitempty"`
	N      int        `json:"n,omitempty"`
	Paste  *keysPaste `json:"paste,omitempty"`
	Masked bool       `json:"masked,omitempty"`
}

// keysBatchData is the data of one areel:keys record. AgoMs is how long
// before submission the first entry happened: the holder places the record
// at admission, so the first entry's footage time is the placement minus
// agoMs.
type keysBatchData struct {
	Client string      `json:"client"`
	AgoMs  int64       `json:"agoMs"`
	Keys   []keysEntry `json:"keys"`
}

var keysModNames = map[string]bool{"Ctrl": true, "Alt": true, "Shift": true, "Super": true}

// validKeysText is bounded, control-free UTF-8.
func validKeysText(s string, max int) bool {
	return s != "" && len(s) <= max && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}

func (d keysBatchData) validate() error {
	if !validKeysText(d.Client, keysClientMax) || d.AgoMs < 0 || d.AgoMs > keysSpanMax || len(d.Keys) == 0 || len(d.Keys) > keysEntriesMax {
		return fmt.Errorf("invalid keys batch")
	}
	for _, k := range d.Keys {
		if k.Dt < 0 || k.Dt > keysSpanMax || k.N < 0 || k.N > 1<<20 {
			return fmt.Errorf("invalid keys entry")
		}
		kinds := 0
		if k.Paste != nil {
			kinds++
			if k.Paste.Bytes < 0 || k.Paste.Lines < 0 || k.Label != "" || k.Mod != "" || k.N != 0 {
				return fmt.Errorf("invalid keys paste")
			}
		}
		if k.Masked {
			kinds++
			if k.Label != "" || k.Mod != "" || k.N != 0 {
				return fmt.Errorf("invalid masked keys entry")
			}
		}
		if kinds == 0 && !validKeysText(k.Label, keysLabelMax) {
			return fmt.Errorf("invalid keys label")
		}
		if kinds > 1 {
			return fmt.Errorf("ambiguous keys entry")
		}
		if k.Mod != "" {
			seen := map[string]bool{}
			for _, m := range strings.Split(k.Mod, "+") {
				if !keysModNames[m] || seen[m] {
					return fmt.Errorf("invalid keys modifier")
				}
				seen[m] = true
			}
		}
	}
	return nil
}

// parseKeysData decodes a stored batch strictly: unknown fields, duplicate
// keys and out-of-bound values refuse.
func parseKeysData(raw json.RawMessage) (keysBatchData, error) {
	var d keysBatchData
	if len(raw) == 0 || len(raw) > activityRecordMax {
		return d, fmt.Errorf("invalid keys batch")
	}
	if e := strictActivityJSON(raw, &d); e != nil {
		return d, fmt.Errorf("invalid keys batch: %w", e)
	}
	return d, d.validate()
}

// keysEntries converts strokes to batch entries, timed relative to the first.
func keysEntries(strokes []keydecode.Stroke) []keysEntry {
	out := make([]keysEntry, 0, len(strokes))
	for i, s := range strokes {
		e := keysEntry{}
		if i > 0 {
			e.Dt = max(0, s.At.Sub(strokes[i-1].At).Milliseconds())
		}
		switch s.Kind {
		case keydecode.KindPaste:
			e.Paste = &keysPaste{Bytes: s.PasteBytes, Lines: s.PasteLines}
		case keydecode.KindMasked:
			e.Masked = true
		default:
			e.Label, e.Mod = s.Label, s.Mod.String()
			if s.Count > 1 {
				e.N = s.Count
			}
		}
		out = append(out, e)
	}
	return out
}

// keysEnvelope builds the declaration-only activity envelope of one batch.
func keysEnvelope(id, client string, ago time.Duration, strokes []keydecode.Stroke) ([]byte, error) {
	data := keysBatchData{Client: client, AgoMs: min(keysSpanMax, max(0, ago.Milliseconds())), Keys: keysEntries(strokes)}
	if e := data.validate(); e != nil {
		return nil, e
	}
	raw, e := json.Marshal(data)
	if e != nil {
		return nil, e
	}
	producer := keysProducer
	return json.Marshal(activityEnvelope{ID: id, Producer: &producer, Source: keysSource, Kind: keysKind, Data: raw})
}

// keysClientLabel names this wrapper's client: the host, and the SSH client
// address when the wrapper runs in an SSH session.
func keysClientLabel(env func(string) string) string {
	host, _ := os.Hostname()
	host = keysSanitize(host, 64)
	if host == "" {
		host = "unknown-host"
	}
	if f := strings.Fields(env("SSH_CONNECTION")); len(f) > 0 {
		return keysSanitize("ssh:"+f[0]+"@"+host, keysClientMax)
	}
	return keysSanitize("local@"+host, keysClientMax)
}

func keysSanitize(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	for len(s) > limit {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// keysConsentFile is the per-recording opt-in.
type keysConsentFile struct {
	Enabled bool      `json:"enabled"`
	Since   time.Time `json:"since"`
}

// keysConsent reports whether recording directory dir has opted in. Anything
// but a readable, private, well-formed file with enabled:true is "no".
func keysConsent(dir string) bool {
	data, e := offlineJSONBytes(filepath.Join(dir, keysConsentName), 1024)
	if e != nil {
		return false
	}
	var c keysConsentFile
	return strictActivityJSON(data, &c) == nil && c.Enabled
}

func offlineJSONBytes(path string, limit int) ([]byte, error) {
	var raw json.RawMessage
	return offlineJSON(path, limit, &raw)
}

// keysRuntimeDir is the private per-user directory holding pause flags.
func keysRuntimeDir(env func(string) string) (string, error) {
	base := env("XDG_RUNTIME_DIR")
	if !filepath.IsAbs(base) {
		tmp := env("TMPDIR")
		if !filepath.IsAbs(tmp) {
			tmp = "/tmp"
		}
		base = filepath.Join(tmp, fmt.Sprintf("agent-reel-%d", os.Getuid()))
	} else {
		base = filepath.Join(base, "agent-reel")
	}
	return filepath.Join(base, "keys"), nil
}

// keysPausePath is the pause flag of one backend-qualified target.
func keysPausePath(env func(string) string, t Target) (string, error) {
	dir, e := keysRuntimeDir(env)
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256([]byte(t.Backend + "\x00" + t.Instance))
	return filepath.Join(dir, hex.EncodeToString(sum[:16])+".paused"), nil
}

// keysPaused fails closed: only a definitely absent flag means "not paused".
func keysPaused(path string) bool {
	_, e := os.Lstat(path)
	return !errors.Is(e, os.ErrNotExist)
}

var keysHelp = map[string]cliCommand{
	"on": {Name: "keys on", Args: "[REC]",
		Summary: `Opt a live recording in to keystroke capture. Only clients attached with
areel attach are captured, and only while this recording is opted in. REC
defaults to @, the recording of this session.`,
		Examples: []string{"areel keys on", "areel keys on 3f2a"},
		Docs:     "docs/recording.md (Keystrokes)"},
	"off": {Name: "keys off", Args: "[REC]",
		Summary:  `Opt a recording out of keystroke capture again. Wrappers stop within about 2s.`,
		Examples: []string{"areel keys off", "areel keys off 3f2a"},
		Docs:     "docs/recording.md (Keystrokes)"},
	"pause": {Name: "keys pause", Args: "[SESSION]",
		Summary: `Pause keystroke capture for a Zellij session (every areel attach client of
it) until areel keys resume. SESSION defaults to the session you run in,
then the configured target. Nothing typed while paused is kept.`,
		Examples: []string{"areel keys pause", "areel keys pause work"},
		Docs:     "docs/recording.md (Keystrokes)"},
	"resume": {Name: "keys resume", Args: "[SESSION]",
		Summary:  `Resume keystroke capture for a Zellij session after areel keys pause.`,
		Examples: []string{"areel keys resume", "areel keys resume work"},
		Docs:     "docs/recording.md (Keystrokes)"},
	"status": {Name: "keys status", Args: "[REC]",
		Summary:  `Show whether a recording is opted in to keystroke capture and whether its session is paused.`,
		Examples: []string{"areel keys status", "areel keys status 3f2a --json"},
		Docs:     "docs/recording.md (Keystrokes)"},
}

type keysStatusReply struct {
	Recording string `json:"recording"`
	Session   string `json:"session"`
	Consent   bool   `json:"consent"`
	Paused    bool   `json:"paused"`
	Sealed    bool   `json:"sealed"`
}

// keysCLI runs areel keys on|off|pause|resume|status. args[0] is the subcommand.
func keysCLI(args []string, out, errOut io.Writer) error {
	cmd, ok := keysHelp[args[0]]
	if !ok {
		return usageErr("unknown keys subcommand %q; want one of on, off, pause, resume, status", args[0])
	}
	f := newCLIFlags(cmd, out)
	f.library()
	var asJSON *bool
	if args[0] == "status" {
		asJSON = f.json()
	}
	pos, help, e := f.parse(args[1:], 0, 1)
	if help || e != nil {
		return e
	}
	arg := ""
	if len(pos) == 1 {
		arg = pos[0]
	}
	s, e := f.load(nil)
	if e != nil {
		return e
	}
	switch args[0] {
	case "pause", "resume":
		name := arg
		if name == "" {
			name = os.Getenv("ZELLIJ_SESSION_NAME")
		}
		if name == "" {
			name = s.Target
		}
		if name == "" {
			return usageErr("keys %s: no session: run inside Zellij or pass SESSION", args[0])
		}
		t, e := NewZellij().Resolve(context.Background(), name)
		if e != nil {
			return e
		}
		path, e := keysPausePath(os.Getenv, t)
		if e != nil {
			return e
		}
		if args[0] == "pause" {
			if e = privateDir(filepath.Dir(path)); e != nil {
				return e
			}
			if e = os.WriteFile(path, nil, 0600); e != nil {
				return e
			}
			_, e = fmt.Fprintf(out, "keystroke capture paused for session %s\n", name)
			return e
		}
		if e = os.Remove(path); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		_, e = fmt.Fprintf(out, "keystroke capture resumed for session %s\n", name)
		return e
	}
	if arg == "" {
		arg = recCurrent
	}
	root, e := requireLibrary(s)
	if e != nil {
		return e
	}
	if root, e = offlineAbsolute(root); e != nil {
		return e
	}
	if e = archive.CheckDirectory(root); e != nil {
		return e
	}
	id, e := resolveREC(root, arg, currentRecording(root, s), s.SealName)
	if e != nil {
		return e
	}
	store := &Store{Root: root, SealName: s.SealName}
	loc, e := store.locate(id)
	if e != nil {
		return e
	}
	if args[0] == "status" {
		reply := keysStatusReply{Recording: id, Sealed: loc.Kind == "sealed"}
		if !reply.Sealed {
			dir, _ := store.dir(id)
			reply.Consent = keysConsent(dir)
			var state State
			if _, e = offlineJSON(filepath.Join(dir, "state.json"), 4<<20, &state); e == nil {
				reply.Session = state.Target.Name
				if path, pe := keysPausePath(os.Getenv, state.Target); pe == nil {
					reply.Paused = keysPaused(path)
				}
			}
		}
		if *asJSON {
			return writeJSONLine(out, reply)
		}
		state := "off"
		if reply.Consent {
			state = "on"
		}
		if reply.Sealed {
			state = "sealed (no capture)"
		}
		pause := ""
		if reply.Paused {
			pause = ", paused for session " + reply.Session
		}
		_, e = fmt.Fprintf(out, "keystroke capture for %s: %s%s\n", shortID(id), state, pause)
		return e
	}
	if loc.Kind == "sealed" {
		return fmt.Errorf("keys %s: recording %s is sealed; keystroke capture applies to live recordings only", args[0], shortID(id))
	}
	dir, _ := store.dir(id)
	if e = archive.CheckDirectory(dir); e != nil {
		return e
	}
	if args[0] == "off" {
		if e = os.Remove(filepath.Join(dir, keysConsentName)); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		_, e = fmt.Fprintf(out, "keystroke capture off for recording %s\n", shortID(id))
		return e
	}
	var state State
	if _, e = offlineJSON(filepath.Join(dir, "state.json"), 4<<20, &state); e != nil {
		return e
	}
	if state.ID != id || state.Status == "stopped" {
		return fmt.Errorf("keys on: recording %s is stopped", shortID(id))
	}
	if e = requireVisualFormat(state.Format); e != nil {
		return e
	}
	if e = writeJSON(filepath.Join(dir, keysConsentName), keysConsentFile{Enabled: true, Since: time.Now().UTC()}); e != nil {
		return e
	}
	_, e = fmt.Fprintf(out, "keystroke capture on for recording %s (session %s); attach with: areel attach %s\n", shortID(id), state.Target.Name, state.Target.Name)
	return e
}
