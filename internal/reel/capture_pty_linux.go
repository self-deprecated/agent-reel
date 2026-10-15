package reel

import (
	"os"
	"syscall"
)

func prepareCapturePTY(master *os.File) (*os.File, error) {
	return master, syscall.SetNonblock(int(master.Fd()), true)
}
