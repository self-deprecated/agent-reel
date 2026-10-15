package reel

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/self-deprecated/agent-reel/internal/archive"
)

var marksHelp = cliCommand{Name: "marks", Args: "REC",
	Summary: `List a recording's marks (agent marks, areel mark and manual marks such
as /reel mark) with their labels and estimated positions: segment and seconds
on that segment's clock, or unknown. Works on live, stopped and sealed
recordings; read-only.`,
	Examples: []string{"areel marks latest", "areel marks 3f2a --json"},
	Docs:     "README.md, ACTIVITY.md"}

// marksCLIMark is one mark of areel marks: its activity sequence, label,
// kind, who, placement (segment and seconds, precision estimated or unknown,
// coverage) and declared offset.
type marksCLIMark struct {
	Seq       int      `json:"seq"`
	Label     string   `json:"label,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	Who       string   `json:"who,omitempty"`
	Segment   int      `json:"segment,omitempty"`
	Seconds   *float64 `json:"seconds"`
	Precision string   `json:"precision"`
	Coverage  string   `json:"coverage,omitempty"`
	OffsetNs  int64    `json:"offsetNs,omitempty"`
}

type marksCLIOutput struct {
	Recording string         `json:"recording"`
	Marks     []marksCLIMark `json:"marks"`
}

// marksCLITimeout bounds one listing.
const marksCLITimeout = 60 * time.Second

func marksCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(marksHelp, out)
	f.library()
	f.setting("backend", "backend", "NAME", "backend used to resolve @ (default: config, then library config, then caller context)")
	asJSON := f.json()
	pos, help, e := f.parse(args[1:], 1, 1)
	if help || e != nil {
		return e
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
	root, e := offlineAbsolute(library)
	if e != nil {
		return e
	}
	if e = archive.CheckDirectory(root); e != nil {
		return e
	}
	id, e := resolveREC(root, pos[0], currentRecording(root, s), s.SealName)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(withSealName(context.Background(), s.SealName), marksCLITimeout)
	defer cancel()
	result := marksCLIOutput{Recording: id, Marks: []marksCLIMark{}}
	for after := 0; ; {
		page, e := libraryMarks(ctx, root, id, after, 0, libraryMarksPageMax, libraryOutputMax)
		if e != nil {
			return fmt.Errorf("marks of %s: %w", shortID(id), e)
		}
		for _, m := range page.Items {
			if !m.Marker || m.Status != "stored" || isKeysMark(m) {
				continue
			}
			row := marksCLIMark{Seq: m.Sequence, Label: m.Label, Kind: m.Kind, Who: m.Who, Seconds: m.Seconds, Precision: "unknown", Coverage: m.Coverage, OffsetNs: m.OffsetNs}
			if m.Seconds != nil {
				row.Segment, row.Precision = m.Segment, "estimated"
			}
			result.Marks = append(result.Marks, row)
		}
		if !page.MoreAfter || page.NextAfter <= after {
			break
		}
		after = page.NextAfter
	}
	if *asJSON {
		return writeJSONLine(out, result)
	}
	if len(result.Marks) == 0 {
		_, e = fmt.Fprintf(out, "no marks in %s\n", shortID(id))
		return e
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SEQ\tAT\tMARK")
	for _, m := range result.Marks {
		at := "unplaced"
		if m.Seconds != nil {
			at = fmt.Sprintf("%d · %s", m.Segment, marksClock(*m.Seconds))
		}
		name := m.Label
		if name == "" {
			name = markLabel(m.Who, nil)
		}
		if m.OffsetNs > 0 {
			name += fmt.Sprintf(" (%s earlier)", time.Duration(m.OffsetNs))
		}
		fmt.Fprintf(w, "%d\t%s\t%s\n", m.Seq, at, strings.Map(func(r rune) rune {
			if r == '\t' {
				return ' '
			}
			return r
		}, name))
	}
	return w.Flush()
}

// marksClock formats seconds like the player (mm:ss or h:mm:ss).
func marksClock(seconds float64) string {
	s := int64(max(seconds, 0))
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}
