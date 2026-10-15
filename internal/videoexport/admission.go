package videoexport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// Renderer admission is independent of archive verification. Bound both initial
// and later resized terminal allocations before a browser sees the cast.
func admitCast(ctx context.Context, path string) error {
	return scanCast(ctx, path, nil)
}

// Geometry is a terminal size in cells.
type Geometry struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// MaxGeometries bounds the distinct terminal sizes RenderFrames preflights.
const MaxGeometries = 16

// castGeometries applies admitCast's policy and also returns the distinct
// header/resize geometries in first-seen order, refusing more than MaxGeometries.
func castGeometries(ctx context.Context, path string) ([]Geometry, error) {
	var seen []Geometry
	err := scanCast(ctx, path, func(g Geometry) error {
		for _, s := range seen {
			if s == g {
				return nil
			}
		}
		if len(seen) == MaxGeometries {
			return fmt.Errorf("video cast has more than %d distinct terminal geometries", MaxGeometries)
		}
		seen = append(seen, g)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return seen, nil
}

// scanCast is admitCast's policy; geometry, when non-nil, observes the admitted
// header geometry and every admitted resize in order.
func scanCast(ctx context.Context, path string, geometry func(Geometry) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(contextReader{ctx, f})
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	if !scanner.Scan() {
		return errors.New("missing video cast header")
	}
	var header struct {
		Version       int
		Width, Height int
		Term          struct{ Cols, Rows int }
	}
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
		return errors.New("invalid video cast header")
	}
	cols, rows := header.Width, header.Height
	if header.Version == 3 {
		cols, rows = header.Term.Cols, header.Term.Rows
	} else if header.Version != 2 {
		return errors.New("unsupported video cast version")
	}
	if !geometryAllowed(cols, rows) {
		return errors.New("video terminal geometry exceeds renderer policy")
	}
	if geometry != nil {
		if err := geometry(Geometry{cols, rows}); err != nil {
			return err
		}
	}
	count := 0
	for scanner.Scan() {
		count++
		if count > 2_000_000 {
			return errors.New("video input event limit exceeded")
		}
		var event []json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || len(event) != 3 {
			return errors.New("invalid video cast event")
		}
		var clock float64
		var kind string
		if json.Unmarshal(event[0], &clock) != nil || clock < 0 || math.IsNaN(clock) || math.IsInf(clock, 0) || json.Unmarshal(event[1], &kind) != nil {
			return errors.New("invalid video event clock or type")
		}
		if kind == "r" {
			var size string
			if json.Unmarshal(event[2], &size) != nil {
				return errors.New("invalid video resize")
			}
			parts := strings.Split(size, "x")
			if len(parts) != 2 {
				return errors.New("invalid video resize")
			}
			cols, e1 := strconv.Atoi(parts[0])
			rows, e2 := strconv.Atoi(parts[1])
			if e1 != nil || e2 != nil || !geometryAllowed(cols, rows) {
				return errors.New("video resize exceeds renderer policy")
			}
			if geometry != nil {
				if err := geometry(Geometry{cols, rows}); err != nil {
					return err
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if count == 0 {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func geometryAllowed(cols, rows int) bool {
	return cols > 0 && rows > 0 && cols <= 512 && rows <= 512 && cols*rows <= 32768
}
