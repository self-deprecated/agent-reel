package reel

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/self-deprecated/agent-reel/internal/links"
)

// Library projections of links (LIBRARY.md "Linked recordings and sidecars").
// A path ref is never projected: no route, JSON field or page names it.

// libraryLinkTarget is what a link opens in the browser.
type libraryLinkTarget struct {
	Type        string `json:"type"` // recording, asset or external
	Recording   string `json:"recording,omitempty"`
	URL         string `json:"url,omitempty"`
	ContentType string `json:"contentType,omitempty"`
}

type libraryLinkView struct {
	ID       string             `json:"id"`
	Kind     string             `json:"kind"`
	Label    string             `json:"label"`
	Origin   string             `json:"origin"`
	OffsetNs int64              `json:"offsetNs"`
	Open     bool               `json:"open"`
	Target   libraryLinkTarget  `json:"target"`
	Digest   *string            `json:"digest"`
	Start    *linkPlacementView `json:"start"`
	Stop     *linkPlacementView `json:"stop"`
	Issues   []string           `json:"issues"`
	// Anchors (the alignment's pairs) and ChildSeconds (a recording child's
	// footage duration, null when unknown) feed the sidecar view.
	Anchors      []linkPairView `json:"anchors"`
	ChildSeconds *float64       `json:"childSeconds"`
}

type libraryLinksPage struct {
	Recording string            `json:"recording"`
	Role      string            `json:"role"`
	Parent    *links.ParentLink `json:"parent"`
	Links     []libraryLinkView `json:"links"`
}

func libraryLinkAssetURL(parent, link string) string {
	return "/api/recordings/" + parent + "/links/" + link + "/asset"
}

// libraryLinks projects one recording's resolved links for the browser.
func libraryLinks(ctx context.Context, root, id string) (libraryLinksPage, error) {
	res, e := resolveLinks(ctx, root, id, false, contextSealName(ctx))
	if e != nil {
		return libraryLinksPage{}, e
	}
	page := libraryLinksPage{Recording: id, Role: res.Role, Parent: res.ParentOf, Links: []libraryLinkView{}}
	for _, r := range res.Links {
		l := r.Link
		v := libraryLinkView{ID: l.ID, Kind: l.Kind, Label: l.Label, Origin: l.Origin, OffsetNs: l.OffsetNs, Open: l.Open(), Digest: l.Digest,
			Start: linkPlacement(r.Start), Stop: linkPlacement(r.Stop), Issues: r.Issues}
		v.Anchors, v.ChildSeconds = sidecarLinkFacts(ctx, root, r)
		switch {
		case l.Ref.Recording != "":
			v.Target = libraryLinkTarget{Type: "recording", Recording: l.Ref.Recording}
		case l.Ref.Asset != "":
			v.Target = libraryLinkTarget{Type: "asset", URL: libraryLinkAssetURL(id, l.ID), ContentType: links.ContentType(l.Ref.Asset)}
		default:
			v.Target = libraryLinkTarget{Type: "external"}
		}
		page.Links = append(page.Links, v)
	}
	return page, nil
}

// serveLinkAsset serves one screenshot asset after re-verifying it (private
// regular file, no symlink, bounded size, manifest digest). Anything that is
// not a copied asset (a video path) is refused.
func (h *libraryHandler) serveLinkAsset(ctx context.Context, w http.ResponseWriter, r *http.Request, route libraryRoute) {
	if _, _, e := libraryState(h.root, route.id, newLibraryGuard(ctx)); e != nil {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	m, _, e := links.Read(h.root, route.id)
	var l *links.Link
	if e == nil {
		l = m.Find(route.link)
	}
	if l == nil {
		libraryError(w, http.StatusNotFound)
		return
	}
	data, contentType, e := links.OpenAsset(h.root, route.id, *l)
	if e != nil || ctx.Err() != nil {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// libraryViewerLinks is the viewer's links lane.
type libraryViewerLinks struct {
	Items []libraryViewerLinkItem
	// From names the parent of a child recording.
	From *libraryViewerLinkItem
}

type libraryViewerLinkItem struct {
	Label, Kind, OpenURL, JumpURL, When string
	// Asset opens in a new tab; External has no URL (CLI only).
	Asset, External, Live bool
	Issues                string
}

// viewerLinks builds the lane; failures hide it rather than fail the page.
func viewerLinks(ctx context.Context, root, id string) *libraryViewerLinks {
	page, e := libraryLinks(ctx, root, id)
	if e != nil || (len(page.Links) == 0 && page.Parent == nil) {
		return nil
	}
	lane := &libraryViewerLinks{}
	if p := page.Parent; p != nil {
		lane.From = &libraryViewerLinkItem{Label: shortID(p.Recording), Kind: "parent", OpenURL: "/recordings/" + p.Recording}
	}
	for _, l := range page.Links {
		item := libraryViewerLinkItem{Label: l.Label, Kind: l.Kind, Live: l.Open}
		switch l.Target.Type {
		case "recording":
			item.OpenURL = "/recordings/" + l.Target.Recording
		case "asset":
			item.OpenURL, item.Asset = l.Target.URL, true
		default:
			item.External = true
		}
		if s := l.Start; s != nil && s.Seconds != nil && s.Segment >= 1 {
			item.When = fmt.Sprintf("%s", libraryClock(*s.Seconds))
			if s.Segment > 1 {
				item.When = fmt.Sprintf("s%d %s", s.Segment, item.When)
			}
			item.JumpURL = viewerURL(id, "history", s.Segment, *s.Seconds)
		}
		if len(l.Issues) > 0 {
			item.Issues = fmt.Sprint(l.Issues)
		}
		lane.Items = append(lane.Items, item)
	}
	return lane
}
