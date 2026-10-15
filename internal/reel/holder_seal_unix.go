//go:build linux || darwin

package reel

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openHolderSealLock(path string) (*os.File, error) {
	// NOFOLLOW rejects symlinks; NONBLOCK avoids hanging on an unsafe FIFO
	// before fstat can reject it. Neither affects flock's explicit retries.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err == nil && (!st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || !privateOwner(st)) {
		err = fmt.Errorf("seal.lock must be an owned 0600 regular file")
	}
	// A restrictive umask can create it 0400; later O_RDWR opens would then
	// fail forever because the file is never unlinked.
	if err == nil && st.Mode().Perm() != 0600 {
		err = f.Chmod(0600)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func tryHolderSealLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EINTR) {
		return false, nil
	}
	return err == nil, err
}
