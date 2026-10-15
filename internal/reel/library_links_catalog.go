package reel

// Catalog nesting of linked children (LIBRARY.md "Linked recordings and
// sidecars"): a child whose parent exists is hidden from the top level and
// listed in a collapsed disclosure on its parent's card.

// libraryCatalogChild is one child line under a parent card.
type libraryCatalogChild struct {
	ID, Label, Clock string
	Live             bool
}

// URL opens the child recording.
func (c libraryCatalogChild) URL() string { return "/recordings/" + c.ID }

// libraryCatalogChildren is a parent card's children: those observed on this
// catalog page with their session and duration, the others by short ID.
type libraryCatalogChildren struct {
	Rows []libraryCatalogChild
	// Total is the summed duration of the children with a known duration.
	Total string
}

// Count is the number of linked children.
func (k libraryCatalogChildren) Count() int { return len(k.Rows) }

func (k libraryCatalogChildren) revision() []any {
	var out []any
	for _, r := range k.Rows {
		out = append(out, []any{r.ID, r.Label, r.Clock, r.Live})
	}
	return append(out, k.Total)
}

func libraryCatalogKids(row libraryRecording, onPage map[string]libraryRecording) libraryCatalogChildren {
	var kids libraryCatalogChildren
	total, known := 0.0, false
	for _, id := range row.Children {
		child := libraryCatalogChild{ID: id, Label: shortID(id)}
		if c, ok := onPage[id]; ok {
			v := libraryView(c)
			if s := v.Session(); s != "" {
				child.Label = s
			}
			child.Clock, child.Live = v.Clock(), v.Live()
			if c.DurationSeconds != nil {
				total += *c.DurationSeconds
				known = true
			}
		}
		kids.Rows = append(kids.Rows, child)
	}
	if known {
		kids.Total = libraryClock(total)
	}
	return kids
}
