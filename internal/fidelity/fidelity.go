// Package fidelity reports terminal output that the pinned avt 0.18.0 emulator
// silently ignores, so a capture owner can open an explicit
// "fidelity-incomplete" interval instead of implying the recording is faithful.
//
// It is not an emulator: it keeps no grid, renders nothing, modifies no data
// and answers no query. Detection covers only the classes below, all derived
// from the pinned parser; upstream omissions outside them are undetectable, so
// an empty finding set is never a fidelity certificate. A finding never
// justifies a raw-output fallback and never excuses a parser or storage
// failure.
package fidelity

// Class identifies one detected divergence between the emulator and a real
// terminal. Identifiers are stable and belong in persisted records.
type Class string

// The detected classes. Interval classes bracket a span of the stream that the
// emulator cannot reproduce; point classes mark a single ignored sequence.
const (
	ClassColumnsMode     Class = "columns-mode"
	ClassCursorBlinkMode Class = "cursor-blink-mode"
	ClassCursorShape     Class = "cursor-shape"
	ClassGraphics        Class = "graphics"
	ClassPalette         Class = "palette"
	ClassReverseVideo    Class = "reverse-video"
	ClassSGRUnsupported  Class = "sgr-unsupported"

	ClassHyperlink       Class = "hyperlink"
	ClassSyncOutput      Class = "sync-output"
	ClassUnknownSequence Class = "unknown-sequence"
)

// Kind distinguishes interval boundaries from standalone observations.
type Kind string

const (
	KindOpen  Kind = "open"
	KindClose Kind = "close"
	KindPoint Kind = "point"
)

// Finding is one reported observation. Detail is a short human-readable cause
// of at most DetailMax bytes; it is diagnostic text, never a reconstruction of
// the sequence and never sufficient to replay it.
type Finding struct {
	Class  Class
	Kind   Kind
	Detail string
}

// DetailMax bounds Finding.Detail so a hostile stream cannot inflate records.
const DetailMax = 64

// interval indexes the interval classes in Class sort order, which is also the
// order Open reports and the order a reset emits closes in.
type interval uint8

const (
	ivColumnsMode interval = iota
	ivCursorBlinkMode
	ivCursorShape
	ivGraphics
	ivPalette
	ivReverseVideo
	ivSGRUnsupported
	intervalCount
)

var intervalClass = [intervalCount]Class{
	ClassColumnsMode,
	ClassCursorBlinkMode,
	ClassCursorShape,
	ClassGraphics,
	ClassPalette,
	ClassReverseVideo,
	ClassSGRUnsupported,
}

// Bounds. Nothing here grows with input length: string payloads stop being
// retained past maxString, parameter storage mirrors the emulator's fixed
// arrays, and the suppression table is capped at maxDetails entries.
const (
	maxString  = 256
	paramsLen  = 32 // avt parser.rs:8 PARAMS_LEN
	maxPartLen = 6  // avt parser.rs:1498 MAX_PARAM_LEN

	maxDetails        = 256
	unknownStride     = 1024
	maxUnknownPerFeed = 64
)

// Scanner consumes output-event text and reports findings. It is a value with
// no external state; a zero Scanner is usable but New avoids the first
// allocations. It is not safe for concurrent use.
type Scanner struct {
	st           state
	params       [paramsLen]param
	curParam     int
	intermediate rune

	kind     strKind
	str      []byte
	strTrunc bool
	dcsInter rune
	dcsFinal rune
	pending  bool // a string payload is held awaiting ESC \

	open     [intervalCount]bool
	penDirty bool

	details        map[string]uint16
	otherDetail    uint32
	emitted        int
	suppressedSeqs uint64
}

// New returns a Scanner in its initial state.
func New() *Scanner {
	return &Scanner{str: make([]byte, 0, maxString), details: make(map[string]uint16, 16)}
}

// Feed consumes one complete UTF-8 output event and returns the findings it
// produced. Tokenizer state persists across calls, so a sequence may be split
// at any rune boundary between calls without changing the outcome, with the
// single documented exception of the per-call unknown-sequence emission cap.
func (s *Scanner) Feed(data string) []Finding {
	var out []Finding
	s.emitted = 0
	for _, r := range data {
		s.step(r, &out)
	}
	return out
}

// Open reports the classes whose interval is currently open, in Class order.
func (s *Scanner) Open() []Class {
	var open []Class
	for i := interval(0); i < intervalCount; i++ {
		if s.open[i] {
			open = append(open, intervalClass[i])
		}
	}
	return open
}

// Suppressed counts unknown-sequence observations that were deliberately not
// reported, either by the per-detail stride or by the per-call cap.
func (s *Scanner) Suppressed() uint64 { return s.suppressedSeqs }

// Reset is an owner-initiated hard reset for a new segment. It returns the
// Scanner to its initial state and closes nothing: any open interval simply
// ceases to exist here, and recording that fact is the caller's decision.
func (s *Scanner) Reset() {
	str, details := s.str[:0], s.details
	*s = Scanner{}
	s.str, s.details = str, details
	clear(details)
}

func (s *Scanner) openInterval(out *[]Finding, iv interval, detail string) {
	if s.open[iv] {
		return
	}
	s.open[iv] = true
	*out = append(*out, Finding{Class: intervalClass[iv], Kind: KindOpen, Detail: clip(detail)})
}

func (s *Scanner) closeInterval(out *[]Finding, iv interval, detail string) {
	if !s.open[iv] {
		return
	}
	s.open[iv] = false
	*out = append(*out, Finding{Class: intervalClass[iv], Kind: KindClose, Detail: clip(detail)})
}

func (s *Scanner) point(out *[]Finding, c Class, detail string) {
	*out = append(*out, Finding{Class: c, Kind: KindPoint, Detail: clip(detail)})
}

// unknown reports an ignored sequence sparsely: at most one finding per detail
// per unknownStride occurrences, and at most maxUnknownPerFeed per Feed call.
// Details beyond maxDetails share one counter so the table stays bounded.
func (s *Scanner) unknown(out *[]Finding, detail string) {
	detail = clip(detail)
	var n uint16
	if s.details == nil {
		s.details = make(map[string]uint16, 16)
	}
	if c, ok := s.details[detail]; ok {
		n = (c + 1) % unknownStride
		s.details[detail] = n
	} else if len(s.details) < maxDetails {
		n = 1
		s.details[detail] = n
	} else {
		s.otherDetail = (s.otherDetail + 1) % unknownStride
		n = uint16(s.otherDetail)
	}
	if n != 1 || s.emitted >= maxUnknownPerFeed {
		s.suppressedSeqs++
		return
	}
	s.emitted++
	*out = append(*out, Finding{Class: ClassUnknownSequence, Kind: KindPoint, Detail: detail})
}

// reset handles RIS, which restores every real terminal setting this package
// tracks and therefore closes every open interval.
func (s *Scanner) reset(out *[]Finding) {
	for i := interval(0); i < intervalCount; i++ {
		s.closeInterval(out, i, "RIS")
	}
	s.penDirty = false
}

func clip(detail string) string {
	if len(detail) <= DetailMax {
		return detail
	}
	end := DetailMax
	for end > 0 && detail[end]&0xc0 == 0x80 {
		end--
	}
	return detail[:end]
}
