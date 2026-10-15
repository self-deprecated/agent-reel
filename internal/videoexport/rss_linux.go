package videoexport

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Parse only a tiny proc statm record; callers pass owned browser/encoder PIDs.
func processRSS(pid int) (int64, error) {
	if pid < 1 {
		return 0, errors.New("invalid owned video PID")
	}
	f, e := os.Open(fmt.Sprintf("/proc/%d/statm", pid))
	if e != nil {
		return 0, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 256))
	if e != nil {
		return 0, e
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return 0, errors.New("unavailable video memory accounting")
	}
	pages, e := strconv.ParseInt(fields[1], 10, 64)
	if e != nil || pages < 0 || pages > 1<<40 {
		return 0, errors.New("invalid video memory accounting")
	}
	return pages * int64(os.Getpagesize()), nil
}
