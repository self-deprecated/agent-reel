package qualify

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func collectHostEnvironment(e *Environment) {
	if f, err := os.Open("/proc/cpuinfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "model name" {
				e.CPUModel = strings.TrimSpace(v)
				break
			}
		}
		f.Close()
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var kib int64
			if n, _ := fmt.Sscanf(sc.Text(), "MemTotal: %d kB", &kib); n == 1 {
				e.MemTotalBytes = kib << 10
				break
			}
		}
		f.Close()
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		e.Kernel = strings.TrimSpace(string(b))
	}
}
