package t3replay

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"io"
	"strings"
)

//go:embed player.html
var playerHTML string

// The page's fonts, so it looks the same on every machine and in the video:
// subsets of Inter and JetBrains Mono (fonts/NOTICE.txt).
//
//go:embed fonts/Inter.woff2
var interFont []byte

//go:embed fonts/JetBrainsMono.woff2
var monoFont []byte

const (
	timelineSlot = "/*TIMELINE*/null"
	titleSlot    = "<title>Replay</title>"
	fontsSlot    = "/*FONTS*/"
)

func fontFaces() string {
	face := func(family, weight string, b []byte) string {
		return "@font-face { font-family: \"" + family + "\"; font-weight: " + weight + "; font-display: block; src: url(data:font/woff2;base64," +
			base64.StdEncoding.EncodeToString(b) + ") format(\"woff2\"); }\n"
	}
	return face("Inter", "400 700", interFont) + face("JetBrains Mono", "400", monoFont)
}

// WriteHTML writes a self-contained page that plays t. The page loads nothing
// from the network. For filming, it exposes window.replay with duration and
// seek(seconds), and ?frame=1 draws one still frame without playing.
func WriteHTML(w io.Writer, t *Timeline) error {
	if e := t.Check(); e != nil {
		return e
	}
	// json.Marshal escapes <, > and & (and U+2028/9), so the timeline cannot
	// end the script element it sits in.
	b, e := json.Marshal(t)
	if e != nil {
		return e
	}
	if strings.Count(playerHTML, timelineSlot) != 1 || strings.Count(playerHTML, titleSlot) != 1 || strings.Count(playerHTML, fontsSlot) != 1 {
		return errors.New("t3replay: the player page lost its slots")
	}
	page := strings.Replace(playerHTML, titleSlot, "<title>"+html.EscapeString(t.Title)+"</title>", 1)
	page = strings.Replace(page, fontsSlot, fontFaces(), 1)
	page = strings.Replace(page, timelineSlot, string(b), 1)
	_, e = io.WriteString(w, page)
	return e
}
