// Package archive is the recorder-stream vocabulary the visual/1 pipeline
// shares: recording identity and geometry, the asciicast line Validator, the
// recorder FIFO, stream limits, storage-failure classification and private
// directory checks. The legacy raw archive format (independent zstd frames)
// has been removed; only its read-only Verify remains, for internal/loopoc.
package archive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Geometry struct {
	Columns int `json:"columns"`
	Rows    int `json:"rows"`
}
type Identity struct {
	Recording string `json:"recording"`
	Segment   int    `json:"segment"`
	Backend   string `json:"backend"`
	Instance  string `json:"instance"`
}

// Validator inspects framing without reserializing or rounding source bytes.
// The capture contract forbids input and requires an unlimited idle policy.
type Validator struct {
	Geometry Geometry
	Lines    int64
	// Elapsed sums the validated events' source deltas in seconds. It is the
	// recorder's own relative clock as far as validated lines go: an approximate
	// footage position, never a wall-clock or paint-time claim.
	Elapsed      float64
	Output, Exit bool
}

func (v *Validator) Line(line []byte) error {
	if len(line) == 0 || line[len(line)-1] != '\n' || !utf8.Valid(line) {
		return fmt.Errorf("incomplete/non-UTF8 cast line")
	}
	if v.Exit {
		return fmt.Errorf("event after exit")
	}
	if v.Lines == 0 {
		var h struct {
			Version int `json:"version"`
			Term    struct {
				Cols int `json:"cols"`
				Rows int `json:"rows"`
			} `json:"term"`
			Idle float64 `json:"idle_time_limit"`
		}
		if err := json.Unmarshal(line, &h); err != nil {
			return fmt.Errorf("cast header: %w", err)
		}
		if h.Version != 3 || h.Term.Cols != v.Geometry.Columns || h.Term.Rows != v.Geometry.Rows || h.Term.Cols < 1 || h.Term.Rows < 1 || h.Idle != math.MaxFloat64 {
			return fmt.Errorf("cast header version/geometry/idle policy mismatch")
		}
	} else {
		var event []json.RawMessage
		if err := json.Unmarshal(line, &event); err != nil || len(event) != 3 {
			return fmt.Errorf("invalid event framing")
		}
		// JSON numbers only; reject null, strings, negative and overflow. Original
		// decimal timestamp bytes remain untouched in storage and hashing.
		n := bytes.TrimSpace(event[0])
		if len(n) == 0 || (n[0] != '-' && (n[0] < '0' || n[0] > '9')) {
			return fmt.Errorf("invalid event delta")
		}
		if n[0] == '-' {
			mantissa := n[1:]
			if i := bytes.IndexAny(mantissa, "eE"); i >= 0 {
				mantissa = mantissa[:i]
			}
			// Negative nonzero values can underflow float64 to -0. Permit
			// lexical negative zero, but never accept a negative delta that way.
			if bytes.IndexAny(mantissa, "123456789") >= 0 {
				return fmt.Errorf("negative event delta")
			}
		}
		delta, err := strconv.ParseFloat(string(n), 64)
		if err != nil || delta < 0 || math.IsInf(delta, 0) || math.IsNaN(delta) {
			return fmt.Errorf("invalid event delta")
		}
		v.Elapsed += delta
		var kind, data string
		if json.Unmarshal(event[1], &kind) != nil || json.Unmarshal(event[2], &data) != nil || bytes.Equal(bytes.TrimSpace(event[2]), []byte("null")) {
			return fmt.Errorf("event type/data must be strings")
		}
		switch kind {
		case "o":
			v.Output = true
		case "r":
			size := strings.Split(data, "x")
			if len(size) != 2 {
				return fmt.Errorf("invalid resize")
			}
			for _, s := range size {
				n, e := strconv.Atoi(s)
				if e != nil || n < 1 || n > 65535 || strconv.Itoa(n) != s {
					return fmt.Errorf("invalid resize")
				}
			}
		case "m":
		case "x":
			if data != "0" {
				return fmt.Errorf("unsuccessful exit %q", data)
			}
			v.Exit = true
		default:
			return fmt.Errorf("unsupported/captured-input event %q", kind)
		}
	}
	v.Lines++
	return nil
}
func (v *Validator) Complete() error {
	if v.Lines == 0 || !v.Output || !v.Exit {
		return fmt.Errorf("cast incomplete: expected header, output and final x0")
	}
	return nil
}
