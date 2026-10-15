package castexport

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// helperRSS reads one helper's resident set, mirroring the videoexport resource
// watcher's /proc accounting without importing it.
func helperRSS(pid int) (int64, error) {
	if pid < 1 {
		return 0, fmt.Errorf("invalid helper pid %d", pid)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("unreadable VmRSS for pid %d", pid)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kb < 0 {
			return 0, fmt.Errorf("invalid VmRSS for pid %d", pid)
		}
		return kb << 10, nil
	}
	return 0, fmt.Errorf("no VmRSS for pid %d", pid)
}
