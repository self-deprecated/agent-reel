//go:build linux || darwin

package reel

import (
	"bufio"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"os"
)

func validateCast(path string, g Geometry) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	r := bufio.NewReader(f)
	v := archive.Validator{Geometry: archive.Geometry{Columns: g.Columns, Rows: g.Rows}}
	for {
		line, e := r.ReadBytes('\n')
		if len(line) > 0 {
			if x := v.Line(line); x != nil {
				return x
			}
		}
		if e != nil {
			break
		}
	}
	return v.Complete()
}
