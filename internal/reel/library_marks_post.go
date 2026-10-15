package reel

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"
)

// POST /api/recordings/ID/marks?label=B64&ago=SECONDS adds one manual mark to
// a live recording, exactly as `areel mark` does (placeMark): the viewer's
// Mark button and m key. It carries no body, so the library's request
// admission (Host/peer/identity, no bodies outside /exports, no percent
// escapes) applies unchanged: the label travels as unpadded base64url of its
// UTF-8 bytes and ago as plain decimal seconds. A page of another origin can
// neither send the required AgentReel-Mark header without a preflight (which
// the library refuses) nor claim a same-origin fetch, so a cross-site form
// cannot place marks. Only a live holder accepts; sealed and stopped
// recordings answer 409 with the reason.

const (
	// libraryMarkHeader must be "1" on a mark POST.
	libraryMarkHeader = "AgentReel-Mark"
	// libraryMarkLabelInput bounds the decoded label before it is cut to the
	// envelope bound (the page sends at most the envelope bound).
	libraryMarkLabelInput = 1024
)

// libraryMarkPostQuery reads a mark POST's optional label and ago.
func libraryMarkPostQuery(q url.Values) (string, time.Duration, error) {
	label := ""
	if values, ok := q["label"]; ok {
		if len(values) != 1 || len(values[0]) > base64.RawURLEncoding.EncodedLen(libraryMarkLabelInput) {
			return "", 0, fmt.Errorf("invalid mark label")
		}
		raw, e := base64.RawURLEncoding.Strict().DecodeString(values[0])
		if e != nil || !utf8.Valid(raw) {
			return "", 0, fmt.Errorf("invalid mark label")
		}
		label = markLabelText(string(raw))
	}
	var ago time.Duration
	if values, ok := q["ago"]; ok {
		if len(values) != 1 {
			return "", 0, fmt.Errorf("invalid mark offset")
		}
		seconds, e := strconv.ParseFloat(values[0], 64)
		if e != nil || seconds < 0 || seconds > markAgoMax.Seconds() || strconv.FormatFloat(seconds, 'f', -1, 64) != values[0] {
			return "", 0, fmt.Errorf("invalid mark offset")
		}
		ago = time.Duration(math.Round(seconds * 1e9))
	}
	return label, ago, nil
}

// libraryMarkReply is a mark POST's JSON answer. Error is a fixed reason
// code: not-live (sealed, stopped or superseded; nothing to receive it) or
// unavailable.
type libraryMarkReply struct {
	Recording string `json:"recording,omitempty"`
	Sequence  uint64 `json:"sequence,omitempty"`
	Label     string `json:"label,omitempty"`
	OffsetNs  int64  `json:"offsetNs,omitempty"`
	Durable   bool   `json:"durable"`
	Error     string `json:"error,omitempty"`
}

func (h *libraryHandler) serveMarkPost(w http.ResponseWriter, r *http.Request, route libraryRoute) {
	reply := func(status int, v libraryMarkReply) {
		data, _ := json.Marshal(v)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)+1))
		w.WriteHeader(status)
		_, _ = w.Write(append(data, '\n'))
	}
	if r.Header.Get(libraryMarkHeader) != "1" || (r.Header.Get("Sec-Fetch-Site") != "" && r.Header.Get("Sec-Fetch-Site") != "same-origin") {
		libraryError(w, http.StatusForbidden)
		return
	}
	store := &Store{Root: h.root, SealName: h.sealName}
	res, state, e := placeMark(store, route.id, route.markLabel, route.markAgo)
	switch {
	case errors.Is(e, errMarkNotLive):
		reply(http.StatusConflict, libraryMarkReply{Error: "not-live"})
	case e != nil && res.Receipt == nil:
		// A missing holder or superseded binding is not live either; the
		// detail (paths, socket errors) stays out of the reply.
		if v, re := store.read(route.id); re == nil && (v.Status == "stopped" || v.Status == "interrupted" || v.Suppressed) {
			reply(http.StatusConflict, libraryMarkReply{Error: "not-live"})
			return
		}
		reply(http.StatusServiceUnavailable, libraryMarkReply{Error: "unavailable"})
	case e != nil:
		reply(http.StatusServiceUnavailable, libraryMarkReply{Error: "unavailable"})
	default:
		reply(http.StatusOK, libraryMarkReply{Recording: state.ID, Sequence: res.Receipt.Sequence, Label: route.markLabel, OffsetNs: route.markAgo.Nanoseconds(), Durable: res.Durable})
	}
}
