package reel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// areel inspect REC: what an EDL author needs from one recording in one
// read-only call (docs/compose.md, "Discover"): segments with their durable coverage
// ends, terminal sizes, labelled marks with ready-to-paste timerefs, visual
// markers, lifecycle gaps, links (none yet) and the search index status (not
// built yet). Everything comes from one frozen input, like an export.

var inspectHelp = cliCommand{Name: "inspect", Args: "REC",
	Summary: `Describe one recording for composing it (docs/compose.md): each segment's durable
coverage end and terminal sizes, marks with a timeref to paste into an EDL,
visual markers and the pauses between segments. Read-only; sealed or live.`,
	Examples: []string{"areel inspect latest", "areel inspect 3f2a --json"},
	Docs:     "docs/compose.md"}

type inspectSegment struct {
	Segment     int        `json:"segment"`
	State       string     `json:"state"`
	Precision   string     `json:"precision"`
	CoverageEnd string     `json:"coverageEnd"`
	Closed      bool       `json:"closed"`
	HasViews    bool       `json:"hasViews"`
	Sizes       []string   `json:"sizes"`
	WallStart   *time.Time `json:"wallStart,omitempty"`
	WallEnd     *time.Time `json:"wallEnd,omitempty"`
}

type inspectMark struct {
	Seq       int64           `json:"seq"`
	Kind      string          `json:"kind"`
	Label     string          `json:"label,omitempty"`
	Placed    bool            `json:"placed"`
	Segment   int             `json:"segment,omitempty"`
	Seconds   string          `json:"seconds,omitempty"`
	OffsetNs  int64           `json:"offsetNs,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Timeref   json.RawMessage `json:"timeref,omitempty"`
	Precision string          `json:"precision"`
}

type inspectMarker struct {
	Segment int    `json:"segment"`
	Seconds string `json:"seconds"`
	Label   string `json:"label,omitempty"`
	Timeref string `json:"timeref"`
}

type inspectGap struct {
	AfterSegment  int        `json:"afterSegment"`
	BeforeSegment int        `json:"beforeSegment"`
	Kind          string     `json:"kind"`
	WallFrom      *time.Time `json:"wallFrom,omitempty"`
	WallTo        *time.Time `json:"wallTo,omitempty"`
	WallSeconds   *float64   `json:"wallSeconds,omitempty"`
	WallClock     string     `json:"wallClock"`
}

type inspectIndex struct {
	Status string `json:"status"`
	Note   string `json:"note"`
}

type inspectOutput struct {
	Recording       string           `json:"recording"`
	Status          string           `json:"status"`
	Sealed          bool             `json:"sealed"`
	Digest          string           `json:"digest,omitempty"`
	Complete        bool             `json:"complete"`
	FootageSeconds  string           `json:"footageSeconds"`
	Segments        []inspectSegment `json:"segments"`
	Marks           []inspectMark    `json:"marks"`
	VisualMarkers   []inspectMarker  `json:"visualMarkers"`
	Gaps            []inspectGap     `json:"gaps"`
	Links           []struct{}       `json:"links"`
	LinksNote       string           `json:"linksNote"`
	Index           inspectIndex     `json:"index"`
	BeyondCutoff    []int            `json:"segmentsBeyondCutoff,omitempty"`
	WithoutCapture  []int            `json:"attemptsWithoutCapture,omitempty"`
	ActivityPresent bool             `json:"activityPresent"`
}

// inspectRecording reads one recording's frozen input and describes it.
func inspectRecording(ctx context.Context, root, id string) (inspectOutput, error) {
	o := inspectOutput{Recording: id, Segments: []inspectSegment{}, Marks: []inspectMark{}, VisualMarkers: []inspectMarker{}, Gaps: []inspectGap{}, Links: []struct{}{},
		LinksNote: "linked sub-recordings and sidecars arrive with phase 3",
		Index:     inspectIndex{Status: "absent", Note: "the search index is not built yet (phase 2); {\"search\"} timerefs are refused"}}
	g := newLibraryGuard(ctx)
	loc, err := libraryLocate(root, id, g)
	if err != nil {
		return o, err
	}
	if loc.Kind == "sealed" {
		f, err := seal.Open(loc.Path)
		if err != nil {
			return o, err
		}
		o.Digest, err = f.ManifestDigest()
		if err = errors.Join(err, f.Close()); err != nil {
			return o, err
		}
		o.Sealed = true
	}
	if _, _, err := libraryState(root, id, g); err != nil {
		return o, err
	}
	in, err := freezeVisualExport(ctx, root, exportSpec{Recording: id})
	if err != nil {
		return o, err
	}
	defer func() { _ = in.close() }()
	o.Status, o.Complete = in.state.Status, in.sel.Complete
	o.BeyondCutoff, o.WithoutCapture = in.beyond, in.unlaunched
	o.ActivityPresent = !in.activity.absent
	var total visualtime.Time
	for _, b := range in.bounds {
		seg := inspectSegment{Segment: b.Segment, CoverageEnd: editmap.FormatSeconds(b.End), Closed: b.Closed, HasViews: b.HasViews, Sizes: []string{}}
		if b.HasViews {
			total += b.End
		}
		if info, ok := in.infos[b.Segment]; ok {
			seg.State, seg.Precision = info.State, info.Precision
			seg.Sizes = append(seg.Sizes, strconv.Itoa(info.Cols)+"x"+strconv.Itoa(info.Rows))
			// Resize events and visual markers of the frozen selection.
			for from := int64(1); ; {
				page, err := in.reader.Events(ctx, in.sel, b.Segment, from, visualstore.MaxPage)
				if err != nil {
					return o, err
				}
				for _, e := range page {
					from = e.Seq + 1
					switch e.Kind {
					case visualstore.EventResize:
						size := visualEventText(e.Data)
						known := false
						for _, s := range seg.Sizes {
							known = known || s == size
						}
						if !known && len(seg.Sizes) < 64 {
							seg.Sizes = append(seg.Sizes, size)
						}
					case visualstore.EventMarker:
						if len(o.VisualMarkers) < visualExportEventsMax {
							at := editmap.FormatSeconds(e.Point.At)
							o.VisualMarkers = append(o.VisualMarkers, inspectMarker{Segment: b.Segment, Seconds: at, Label: visualEventText(e.Data), Timeref: fmt.Sprintf(`{"segment":%d,"at":%q}`, b.Segment, at)})
						}
					}
				}
				if len(page) < visualstore.MaxPage {
					break
				}
			}
		}
		if s := visualExportSegment(in.state, b.Segment); s != nil {
			if !s.Started.IsZero() {
				t := s.Started.UTC()
				seg.WallStart = &t
			}
			if s.Ended != nil && !s.Ended.IsZero() {
				t := s.Ended.UTC()
				seg.WallEnd = &t
			}
		}
		o.Segments = append(o.Segments, seg)
	}
	o.FootageSeconds = editmap.FormatSeconds(total)
	for i := 0; i+1 < len(o.Segments); i++ {
		a, b := o.Segments[i], o.Segments[i+1]
		gap := inspectGap{AfterSegment: a.Segment, BeforeSegment: b.Segment, Kind: "interruption", WallFrom: a.WallEnd, WallTo: b.WallStart, WallClock: "approximate (state.json)"}
		for _, e := range in.state.Events {
			if e.Segment == a.Segment && (e.Kind == "pause" || e.Kind == "stop") {
				gap.Kind = e.Kind
			}
		}
		if gap.WallFrom != nil && gap.WallTo != nil {
			s := gap.WallTo.Sub(*gap.WallFrom).Seconds()
			gap.WallSeconds = &s
		}
		o.Gaps = append(o.Gaps, gap)
	}
	marks, err := composeReadMarks(ctx, root, in)
	if err != nil {
		return o, err
	}
	labels := map[string]int{}
	for _, m := range marks {
		if editmap.LabelledMark(m.Kind) && m.Label != "" {
			labels[m.Label]++
		}
	}
	for _, m := range marks {
		im := inspectMark{Seq: m.Seq, Kind: m.Kind, Label: visualEventText(m.Label), Placed: m.Placed, Segment: m.Segment, OffsetNs: m.OffsetNs, Reason: m.Reason, Precision: "unknown"}
		if m.Placed {
			im.Precision = "estimated"
			im.Seconds = editmap.FormatSeconds(max(0, m.At-visualtime.Time(m.OffsetNs)))
			ref := fmt.Sprintf(`{"mark":{"seq":%d}}`, m.Seq)
			if editmap.LabelledMark(m.Kind) && labels[m.Label] == 1 {
				b, _ := json.Marshal(map[string]string{"mark": m.Label})
				ref = string(b)
			}
			im.Timeref = json.RawMessage(ref)
		}
		o.Marks = append(o.Marks, im)
	}
	return o, nil
}

func inspectCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(inspectHelp, out)
	f.library()
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
	root, e := requireLibrary(s)
	if e != nil {
		return e
	}
	if root, e = libraryRoot(root); e != nil {
		return fmt.Errorf("library %s: %w", s.Library, e)
	}
	id, e := resolveREC(root, pos[0], currentRecording(root, s), s.SealName)
	if e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(withSealName(context.Background(), s.SealName), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, exportAge)
	defer cancel()
	o, e := inspectRecording(ctx, root, id)
	if e != nil {
		return fmt.Errorf("inspect %s: %w", shortID(id), e)
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(o)
	}
	var b strings.Builder
	kind := "live"
	if o.Sealed {
		kind = "sealed " + o.Digest
	}
	fmt.Fprintf(&b, "%s  %s, %s, %s s of footage\n", o.Recording, o.Status, kind, o.FootageSeconds)
	for _, s := range o.Segments {
		fmt.Fprintf(&b, "  segment %d  0–%s s  %s  %s\n", s.Segment, s.CoverageEnd, strings.Join(s.Sizes, ", "), s.State)
	}
	for _, g := range o.Gaps {
		fmt.Fprintf(&b, "  gap after segment %d: %s\n", g.AfterSegment, g.Kind)
	}
	for _, m := range o.Marks {
		if !m.Placed {
			fmt.Fprintf(&b, "  mark seq %d %s %q: %s\n", m.Seq, m.Kind, m.Label, m.Reason)
			continue
		}
		fmt.Fprintf(&b, "  mark seq %d %s %q  segment %d at %s s  %s\n", m.Seq, m.Kind, m.Label, m.Segment, m.Seconds, m.Timeref)
	}
	for _, m := range o.VisualMarkers {
		fmt.Fprintf(&b, "  marker %q  segment %d at %s s  %s\n", m.Label, m.Segment, m.Seconds, m.Timeref)
	}
	_, e = io.WriteString(out, b.String())
	return e
}
