package reel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/self-deprecated/agent-reel/internal/textindex"
)

// GET /api/search?q=TEXT[&recording=ID][&fuzzy=1][&limit=N] searches the text
// index of one sealed recording or of the whole library (see
// textindex_library.go). It is read-only for the recordings; a missing or
// stale index is queued for the background indexer and counted in pending,
// never built while the request waits. It is admitted like every route
// (requestAllowed); only its query may be percent-encoded, and every value is
// validated here.

const librarySearchLimitMax = 200

type librarySearchQuery struct {
	text      string
	recording string
	fuzzy     bool
	limit     int
}

func parseLibrarySearch(r *http.Request) (librarySearchQuery, error) {
	var q librarySearchQuery
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || r.URL.ForceQuery {
		return q, fmt.Errorf("invalid search query")
	}
	for key, vs := range values {
		if len(vs) != 1 {
			return q, fmt.Errorf("duplicate search parameter")
		}
		switch key {
		case "q":
			if q.text, err = textindex.NormalizeQuery(vs[0]); err != nil {
				return q, err
			}
		case "recording":
			if !validID.MatchString(vs[0]) {
				return q, fmt.Errorf("invalid recording")
			}
			q.recording = vs[0]
		case "fuzzy":
			if vs[0] != "1" && vs[0] != "0" {
				return q, fmt.Errorf("invalid fuzzy")
			}
			q.fuzzy = vs[0] == "1"
		case "limit":
			n, e := strconv.Atoi(vs[0])
			if e != nil || n < 1 || n > librarySearchLimitMax || strconv.Itoa(n) != vs[0] {
				return q, fmt.Errorf("invalid limit")
			}
			q.limit = n
		default:
			return q, fmt.Errorf("unexpected search parameter")
		}
	}
	if q.text == "" {
		return q, fmt.Errorf("missing search text")
	}
	if q.limit == 0 {
		q.limit = textindex.LimitDefault
	}
	return q, nil
}

// libraryIndexerFor returns the handler's background indexer, started on
// first use; nil once the handler is closed.
func (h *libraryHandler) libraryIndexerFor() *libraryIndexer {
	h.exportMu.Lock()
	defer h.exportMu.Unlock()
	if h.closed {
		return nil
	}
	if h.indexer == nil {
		h.indexer = newLibraryIndexer(h.root)
	}
	return h.indexer
}

// searchReasons replaces diagnostics (which may name paths) with the fixed
// explanations the JSON projection shows.
var searchReasons = map[string]string{
	indexLive:        "not sealed yet: only stopped, sealed recordings are searchable",
	indexUnavailable: "the recording or its text index could not be read",
	indexQueued:      "the text index is being built; search again shortly",
}

func (h *libraryHandler) serveSearch(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	q, err := parseLibrarySearch(r)
	if err != nil {
		libraryError(w, http.StatusBadRequest)
		return
	}
	indexer := h.libraryIndexerFor()
	if indexer == nil {
		libraryError(w, http.StatusServiceUnavailable)
		return
	}
	report, err := searchLibrary(ctx, h.root, q.recording, textindex.Query{Text: q.text, Fuzzy: q.fuzzy, Limit: q.limit}, indexer.policy)
	if err != nil || ctx.Err() != nil {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	for i := range report.Recordings {
		report.Recordings[i].Reason = searchReasons[report.Recordings[i].Status]
	}
	raw, err := json.Marshal(report)
	if err != nil || len(raw)+1 > libraryOutputMax {
		libraryError(w, http.StatusUnprocessableEntity)
		return
	}
	raw = append(raw, '\n')
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	_, _ = w.Write(raw)
}
