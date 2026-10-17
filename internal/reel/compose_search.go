package reel

import (
	"errors"
	"fmt"
	"strings"

	"github.com/self-deprecated/agent-reel/internal/editmap"
	"github.com/self-deprecated/agent-reel/internal/redact"
	"github.com/self-deprecated/agent-reel/internal/textindex"
	"github.com/self-deprecated/agent-reel/internal/visualtime"
)

// {"search":"text","nth":N} timerefs (docs/compose.md, "Timerefs"): the n-th plain
// hit of the source's text index, in first-appearance order, exactly as
// `areel search TEXT REC` lists them (screen rows and labelled marks). The
// index is built first when it is missing or stale; only sealed recordings
// are indexed, so a live source is refused with a clear message. --plan
// lists every chosen hit (searchHits) and --freeze embeds the hit's exact
// {segment, at} with the query as `authored`, so a frozen EDL never consults
// the index again.

// composeSearchHit is one resolved {search} timeref in the plan.
type composeSearchHit struct {
	Source    string `json:"source"`
	Query     string `json:"query"`
	Nth       int    `json:"nth"`
	Recording string `json:"recording"`
	Kind      string `json:"kind"`
	Segment   int    `json:"segment"`
	At        string `json:"at"`
	ViewSeq   int64  `json:"viewSeq"`
	Y         int    `json:"y"`
	Cols      [2]int `json:"cols"`
	// Text is the hit's row text, masked by the EDL's redact rules.
	Text  string `json:"text"`
	Index string `json:"index"`
}

// Search resolves a {search} timeref of source name (editmap.Resolver).
func (c *composeSources) Search(name, query string, nth int) (editmap.Instant, error) {
	s, err := c.source(name)
	if err != nil {
		return editmap.Instant{}, err
	}
	text, err := textindex.NormalizeQuery(query)
	if err != nil {
		return editmap.Instant{}, fmt.Errorf("search %q: %w", query, err)
	}
	if nth > textindex.LimitMax {
		return editmap.Instant{}, fmt.Errorf("search %q: nth %d is above %d; choose a more specific query", query, nth, textindex.LimitMax)
	}
	if !s.sealed {
		return editmap.Instant{}, fmt.Errorf("search %q: source %s is a live recording; {\"search\":…} needs the text index, which only sealed recordings have (seal it, or use seconds or a mark)", query, name)
	}
	ix, status, digest, err := openRecordingIndex(c.ctx, c.root, s.id)
	if errors.Is(err, errIndexNotSealed) {
		return editmap.Instant{}, fmt.Errorf("search %q: source %s is not sealed; {\"search\":…} needs a sealed recording's text index", query, name)
	}
	if err != nil {
		return editmap.Instant{}, fmt.Errorf("search %q in source %s: %w", query, name, err)
	}
	defer ix.Close()
	if digest != s.digest {
		return editmap.Instant{}, fmt.Errorf("search %q: source %s's sealed file changed while composing; run again", query, name)
	}
	hits, err := ix.Search(c.ctx, textindex.Query{Text: text, Limit: nth})
	if err != nil {
		return editmap.Instant{}, fmt.Errorf("search %q in source %s: %w", query, name, err)
	}
	if len(hits) < nth {
		if len(hits) == 0 {
			return editmap.Instant{}, fmt.Errorf("search %q: no match in source %s (matching is a case-insensitive substring of one terminal row; try areel search)", query, name)
		}
		var at []string
		for i, h := range hits {
			if i == 5 {
				at = append(at, "…")
				break
			}
			at = append(at, fmt.Sprintf("segment %d at %ss", h.Segment, h.At))
		}
		return editmap.Instant{}, fmt.Errorf("search %q: nth %d but source %s has %d hit(s): %s", query, nth, name, len(hits), strings.Join(at, ", "))
	}
	h := hits[nth-1]
	c.searches = append(c.searches, composeSearchHit{Source: name, Query: query, Nth: nth, Recording: s.id, Kind: h.Kind, Segment: h.Segment, At: h.At,
		ViewSeq: h.ViewSeq, Y: h.Y, Cols: h.Cols, Text: c.maskText(h.Text), Index: status})
	return editmap.Instant{Segment: h.Segment, At: visualtime.Time(h.AtNs)}, nil
}

// maskText applies the EDL's redact rules to a plan string.
func (c *composeSources) maskText(text string) string {
	set, err := redact.Compile(c.doc.Redact)
	if err != nil {
		return ""
	}
	if set.Len() == 0 {
		return text
	}
	spans, _ := set.Find(text)
	return redact.ApplyText(text, spans)
}
