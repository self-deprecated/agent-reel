//go:build linux

package reel

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func platformLowerSealPriority() error {
	var errs []error
	if err := unix.Setpriority(unix.PRIO_PROCESS, 0, 10); err != nil {
		errs = append(errs, fmt.Errorf("nice 10: %w", err))
	}
	// IOPRIO_WHO_PROCESS, current thread, best-effort class, level 7.
	if _, _, err := unix.Syscall(unix.SYS_IOPRIO_SET, 1, 0, (2<<13)|7); err != 0 {
		errs = append(errs, fmt.Errorf("I/O best-effort 7: %w", err))
	}
	return errors.Join(errs...)
}
