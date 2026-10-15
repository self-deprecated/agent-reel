package qualify

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

const CPUMethodRusage = "getrusage(RUSAGE_SELF)"

type Rusage struct{ UserNs, SystemNs, MaxRSSBytes int64 }

func (r Rusage) CPUNs() int64 { return r.UserNs + r.SystemNs }
func SelfRusage() (Rusage, error) {
	var ru syscall.Rusage
	if e := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); e != nil {
		return Rusage{}, e
	}
	// Darwin reports ru_maxrss in bytes (Linux uses KiB).
	return Rusage{ru.Utime.Nano(), ru.Stime.Nano(), ru.Maxrss}, nil
}
func FSType(path string) (string, error) {
	var st unix.Statfs_t
	if e := unix.Statfs(path, &st); e != nil {
		return "", e
	}
	return unix.ByteSliceToString(st.Fstypename[:]), nil
}

type LoadAverage struct{ Raw string }

func LoadAvg() (LoadAverage, error) {
	data, e := unix.SysctlRaw("vm.loadavg")
	if e != nil {
		return LoadAverage{}, e
	}
	// Three uint32 fixed-point values followed by a 64-bit scale on Darwin.
	if len(data) != 24 {
		return LoadAverage{}, fmt.Errorf("invalid load average structure")
	}
	scale := binary.NativeEndian.Uint64(data[16:24])
	if scale == 0 {
		return LoadAverage{}, fmt.Errorf("invalid load average scale")
	}
	value := func(i int) float64 { return float64(binary.NativeEndian.Uint32(data[i:i+4])) / float64(scale) }
	return LoadAverage{Raw: fmt.Sprintf("%.2f %.2f %.2f", value(0), value(4), value(8))}, nil
}
func FileSizes(dir string) (map[string]int64, error) {
	entries, e := os.ReadDir(dir)
	if e != nil {
		return nil, e
	}
	out := map[string]int64{}
	for _, entry := range entries {
		st, e := os.Lstat(filepath.Join(dir, entry.Name()))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if st.Mode().IsRegular() {
			out[entry.Name()] = st.Size()
		}
	}
	return out, nil
}

// The Linux qualification CPU/RSS budgets have no equivalent measurement
// contract on Darwin yet. Refuse metrics rather than substitute zeros.
type Status struct {
	VmRSS, VmHWM int64
	Threads      int
}
type CPU struct {
	Ns     int64
	Method string
}

func ProcStatus(int) (Status, error) {
	return Status{}, fmt.Errorf("per-process qualification metrics require Linux")
}
func ProcCPU(int) (CPU, error) {
	return CPU{}, fmt.Errorf("per-process qualification metrics require Linux")
}
