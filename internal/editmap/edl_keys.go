package editmap

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The keys overlay (EDL.md "Keys overlay"): a KeyCastr-style strip of the
// source's recorded keystrokes (areel attach's areel:keys activity) drawn over
// its clip for the overlay's range. It takes no text: the chips come from the
// keystroke track of the clip's own source span. Client, when set, keeps one
// client's keys; Position places the strip.

const (
	// OverlayKeys is the keystroke strip overlay.
	OverlayKeys = "keys"
	// KeysPositionDefault is the strip's position when none is given.
	KeysPositionDefault = "bottom-right"
	// maxKeysClientBytes bounds a client filter (the recorder's own bound).
	maxKeysClientBytes = 128
)

// KeysPositions are the accepted strip positions.
var KeysPositions = []string{"bottom-right", "bottom-left", "bottom-center", "top-right", "top-left", "top-center"}

// validateKeysOverlay checks one keys overlay of segment s.
func validateKeysOverlay(s *EDLSegment, o EDLOverlay, ow string) error {
	if s.Kind != SegmentClip {
		return fmt.Errorf("edl: %s: a keys overlay needs a clip segment (a card has no keystrokes; a layout's slots are not keyed: show the keys on a clip of that source instead)", ow)
	}
	if o.Text != "" || o.Subtitle != "" || o.Anchor != nil {
		return fmt.Errorf("edl: %s: a keys overlay takes no text, subtitle or anchor (its chips are the recorded keystrokes)", ow)
	}
	if o.Client != "" {
		if err := validKeysClient(o.Client); err != nil {
			return fmt.Errorf("edl: %s client: %w", ow, err)
		}
	}
	if o.Position != "" && !slices.Contains(KeysPositions, o.Position) {
		return fmt.Errorf("edl: %s: position must be one of %s", ow, strings.Join(KeysPositions, ", "))
	}
	if o.At != "" {
		if _, err := ParseDuration(o.At); err != nil {
			return fmt.Errorf("edl: %s at: %w", ow, err)
		}
	}
	if o.For != "" {
		if d, err := ParseDuration(o.For); err != nil || d <= 0 {
			return fmt.Errorf("edl: %s: for must be a positive duration", ow)
		}
	}
	return nil
}

func validKeysClient(c string) error {
	switch {
	case len(c) > maxKeysClientBytes:
		return fmt.Errorf("exceeds %d bytes", maxKeysClientBytes)
	case !utf8.ValidString(c):
		return errors.New("is not valid UTF-8")
	case strings.TrimSpace(c) == "":
		return errors.New("is blank")
	case strings.IndexFunc(c, unicode.IsControl) >= 0:
		return errors.New("contains a control character")
	}
	return nil
}

// keysPosition is o's strip position, the default when unset.
func keysPosition(o EDLOverlay) string {
	if o.Type != OverlayKeys {
		return ""
	}
	if o.Position == "" {
		return KeysPositionDefault
	}
	return o.Position
}
