package reel

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// These are display projections of persisted context, never a filesystem lookup.
// Palette math matches pi/project-footer, including JavaScript's UTF-16 hashing.
type libraryIdentityPart struct {
	Label, Path            string
	Background, Foreground string
	// Tint and Ink are the chip's light-scheme background and same-hue dark
	// text; TintDark and InkDark are the dark-scheme pair. They share the
	// footer palette's hue, so a repository keeps its colour.
	Tint, Ink, TintDark, InkDark string
}
type libraryIdentity struct {
	Repository libraryIdentityPart
	Workspace  *libraryIdentityPart
}

func libraryPathPart(path, label string) libraryIdentityPart {
	hash := uint32(0x811c9dc5)
	for _, unit := range utf16.Encode([]rune(path)) {
		hash = (hash ^ uint32(unit)) * 0x01000193
	}
	h := float64(hash%360) / 60
	s := float64(68+((hash>>8)%16)) / 100
	l := float64(36+((hash>>16)%10)) / 100
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(h, 2)-1))
	m := l - c/2
	var r, g, b float64
	switch {
	case h < 1:
		r, g = c, x
	case h < 2:
		r, g = x, c
	case h < 3:
		g, b = c, x
	case h < 4:
		g, b = x, c
	case h < 5:
		r, b = x, c
	default:
		r, b = c, x
	}
	red, green, blue := int(math.Round((r+m)*255)), int(math.Round((g+m)*255)), int(math.Round((b+m)*255))
	linear := func(channel int) float64 {
		value := float64(channel) / 255
		if value <= .03928 {
			return value / 12.92
		}
		return math.Pow((value+.055)/1.055, 2.4)
	}
	foreground := "#ffffff"
	if .2126*linear(red)+.7152*linear(green)+.0722*linear(blue) > .42 {
		foreground = "#000000"
	}
	hue := float64(hash % 360)
	return libraryIdentityPart{Label: label, Path: path, Background: fmt.Sprintf("#%02x%02x%02x", red, green, blue), Foreground: foreground,
		Tint: libraryHSL(hue, .62, .9), Ink: libraryHSL(hue, .7, .22), TintDark: libraryHSL(hue, .38, .24), InkDark: libraryHSL(hue, .75, .82)}
}

// libraryHSL formats one HSL colour (hue in degrees, s and l in 0..1) as #rrggbb.
func libraryHSL(hue, s, l float64) string {
	h := math.Mod(hue, 360) / 60
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(h, 2)-1))
	m := l - c/2
	var r, g, b float64
	switch {
	case h < 1:
		r, g = c, x
	case h < 2:
		r, g = x, c
	case h < 3:
		g, b = c, x
	case h < 4:
		g, b = x, c
	case h < 5:
		r, b = x, c
	default:
		r, b = c, x
	}
	return fmt.Sprintf("#%02x%02x%02x", int(math.Round((r+m)*255)), int(math.Round((g+m)*255)), int(math.Round((b+m)*255)))
}
func libraryPathLabel(path string) string {
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' })
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == "Development" {
			if i+1 < len(parts) {
				return strings.Join(parts[i+1:], "/")
			}
			break
		}
	}
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return path
}
func libraryRecordingIdentity(c *libraryContext) libraryIdentity {
	if c == nil {
		return libraryIdentity{Repository: libraryIdentityPart{Label: "Unknown repository"}}
	}
	cwd, main := contextPath(c.WorkingDirectory), contextPath(c.JJMain)
	if cwd == "" {
		if main == "" {
			return libraryRecordingIdentity(nil)
		}
		return libraryIdentity{Repository: libraryPathPart(main, libraryPathLabel(main))}
	}
	parts := strings.FieldsFunc(cwd, func(r rune) bool { return r == '/' || r == '\\' })
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "workspaces" {
			continue
		}
		if i+2 >= len(parts) {
			break
		}
		workspace := "/" + strings.Join(parts[:i+3], "/")
		repo, label := main, libraryPathLabel(main)
		if repo == "" {
			repo, label = filepath.Dir(workspace), parts[i+1]
		}
		part := libraryPathPart(workspace, parts[i+2])
		return libraryIdentity{libraryPathPart(repo, label), &part}
	}
	return libraryIdentity{Repository: libraryPathPart(cwd, libraryPathLabel(cwd))}
}
