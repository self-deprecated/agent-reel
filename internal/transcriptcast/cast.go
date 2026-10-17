package transcriptcast

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"
)

// CastHeader is what an asciicast's first line says about it.
type CastHeader struct {
	Version   int
	Cols      int
	Rows      int
	Title     string
	Timestamp int64
}

type rawHeader struct {
	Version int `json:"version"`
	Width   int `json:"width"`  // v2
	Height  int `json:"height"` // v2
	Term    struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	} `json:"term"` // v3
	Title     string `json:"title"`
	Timestamp int64  `json:"timestamp"`
}

// ReadCastHeader reads and checks an asciicast v2 or v3 header.
func ReadCastHeader(r io.Reader) (CastHeader, error) {
	l, e := bufio.NewReaderSize(r, 64<<10).ReadBytes('\n')
	if e != nil && e != io.EOF {
		return CastHeader{}, e
	}
	return parseCastHeader(l)
}

func parseCastHeader(l []byte) (CastHeader, error) {
	var h rawHeader
	if e := json.Unmarshal(l, &h); e != nil {
		return CastHeader{}, fmt.Errorf("asciicast header: %w", e)
	}
	out := CastHeader{Version: h.Version, Title: h.Title, Timestamp: h.Timestamp}
	switch h.Version {
	case 2:
		out.Cols, out.Rows = h.Width, h.Height
	case 3:
		out.Cols, out.Rows = h.Term.Cols, h.Term.Rows
	default:
		return CastHeader{}, fmt.Errorf("asciicast version %d unsupported (want 2 or 3)", h.Version)
	}
	if out.Cols < 1 || out.Rows < 1 || out.Cols > 512 || out.Rows > 512 || out.Cols*out.Rows > 32768 {
		return CastHeader{}, fmt.Errorf("asciicast size %dx%d unsupported (at most 512 per side and 32768 cells)", out.Cols, out.Rows)
	}
	return out, nil
}

// NormalizeCast copies an asciicast v2 or v3 recording as the v3 a capture
// accepts: an unlimited idle limit, exact nanosecond deltas, and only output,
// resize and marker events (input and exit are dropped). The exit event is
// written last; with drained set, after drained is called and ctx ends.
func NormalizeCast(ctx context.Context, r io.Reader, w io.Writer, drained func()) error {
	br := bufio.NewReaderSize(r, 1<<20)
	first, e := br.ReadBytes('\n')
	if e != nil && e != io.EOF {
		return e
	}
	h, e := parseCastHeader(first)
	if e != nil {
		return e
	}
	cw := newCastWriter(w)
	if e := cw.header(h.Cols, h.Rows, h.Title, time.Unix(h.Timestamp, 0)); e != nil {
		return e
	}
	var at time.Duration // footage time of the last event
	output := false
	for n := 2; ctx.Err() == nil; n++ {
		l, e := br.ReadBytes('\n')
		if len(l) > 0 {
			var ev []json.RawMessage
			if json.Unmarshal(l, &ev) != nil || len(ev) != 3 {
				if len(bytesTrim(l)) == 0 {
					continue
				}
				return fmt.Errorf("asciicast line %d: not an event", n)
			}
			t, err := strconv.ParseFloat(string(ev[0]), 64)
			if err != nil || t < 0 || math.IsInf(t, 0) || math.IsNaN(t) {
				return fmt.Errorf("asciicast line %d: bad time", n)
			}
			var code, data string
			if json.Unmarshal(ev[1], &code) != nil || json.Unmarshal(ev[2], &data) != nil {
				return fmt.Errorf("asciicast line %d: bad event", n)
			}
			d := time.Duration(math.Round(t * 1e9))
			if h.Version == 2 {
				at = max(d, at) // v2 times are absolute
			} else {
				at += d
			}
			switch code {
			case "o", "r", "m":
				cw.event(at, code, data)
				output = output || code == "o"
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
	}
	if !output {
		return fmt.Errorf("asciicast has no output")
	}
	if drained != nil {
		if e := cw.flush(); e != nil {
			return e
		}
		drained()
		<-ctx.Done()
	}
	cw.event(cw.last, "x", "0")
	return cw.flush()
}

func bytesTrim(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
