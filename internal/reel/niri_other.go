//go:build !linux

package reel

import (
	"fmt"
	"io"
)

func niriHighlightCLI(args []string, out, errOut io.Writer) error {
	f := newCLIFlags(niriHighlightHelp, out)
	_, help, e := f.parse(args[1:], 0, 0)
	if help || e != nil {
		return e
	}
	return fmt.Errorf("niri highlighting requires Linux and the niri compositor")
}
