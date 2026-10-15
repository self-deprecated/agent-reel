package qualify

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// CPU method names, recorded beside every CPU metric.
const (
	CPUMethodSchedstat = "schedstat"      // Σ /proc/<pid>/task/*/schedstat field 1 (ns)
	CPUMethodStat      = "stat+AT_CLKTCK" // /proc/<pid>/stat utime+stime ÷ AT_CLKTCK
	CPUMethodRusage    = "getrusage(RUSAGE_SELF)"
)

// Rusage is this process's resource usage.
type Rusage struct {
	UserNs, SystemNs int64
	// MaxRSSBytes is ru_maxrss (reported by Linux in KiB) in bytes.
	MaxRSSBytes int64
}

// CPUNs is user plus system time.
func (r Rusage) CPUNs() int64 { return r.UserNs + r.SystemNs }

// SelfRusage reads getrusage(RUSAGE_SELF).
func SelfRusage() (Rusage, error) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return Rusage{}, err
	}
	return Rusage{UserNs: ru.Utime.Nano(), SystemNs: ru.Stime.Nano(), MaxRSSBytes: int64(ru.Maxrss) * 1024}, nil
}

// Status holds the resident-set lines of /proc/<pid>/status, in bytes.
type Status struct {
	VmRSS, VmHWM int64
	Threads      int
}

// ProcStatus reads VmRSS, VmHWM and Threads of pid.
func ProcStatus(pid int) (Status, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return Status{}, err
	}
	var st Status
	var seen int
	for _, line := range strings.Split(string(data), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		switch name {
		case "VmRSS", "VmHWM":
			if len(fields) != 2 || fields[1] != "kB" {
				return Status{}, fmt.Errorf("qualify: pid %d %s %q", pid, name, value)
			}
			kb, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil {
				return Status{}, fmt.Errorf("qualify: pid %d %s %q", pid, name, value)
			}
			if name == "VmRSS" {
				st.VmRSS = kb * 1024
			} else {
				st.VmHWM = kb * 1024
			}
			seen++
		case "Threads":
			if len(fields) == 1 {
				st.Threads, _ = strconv.Atoi(fields[0])
			}
		}
	}
	if seen != 2 {
		return Status{}, fmt.Errorf("qualify: pid %d status lacks VmRSS/VmHWM (a zombie or kernel thread?)", pid)
	}
	return st, nil
}

// CPU is a process's consumed CPU time and how it was read.
type CPU struct {
	Ns     int64
	Method string
}

// ProcCPU reads pid's CPU time: the sum of field 1 of every live thread's
// schedstat when available, else utime+stime of /proc/<pid>/stat converted
// with AT_CLKTCK from /proc/self/auxv. schedstat misses threads that have
// already exited; stat includes them at clock-tick resolution. The method
// used is reported.
func ProcCPU(pid int) (CPU, error) {
	if c, err := procCPUSchedstat(pid); err == nil {
		return c, nil
	}
	return procCPUStat(pid)
}

func procCPUSchedstat(pid int) (CPU, error) {
	tasks, err := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/schedstat", pid))
	if err != nil || len(tasks) == 0 {
		return CPU{}, fmt.Errorf("qualify: pid %d has no schedstat", pid)
	}
	var total int64
	for _, t := range tasks {
		data, err := os.ReadFile(t)
		if err != nil {
			if os.IsNotExist(err) {
				continue // thread exited between glob and read
			}
			return CPU{}, err
		}
		fields := strings.Fields(string(data))
		if len(fields) < 1 {
			return CPU{}, fmt.Errorf("qualify: %s %q", t, data)
		}
		ns, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return CPU{}, fmt.Errorf("qualify: %s %q", t, data)
		}
		total += ns
	}
	return CPU{Ns: total, Method: CPUMethodSchedstat}, nil
}

func procCPUStat(pid int) (CPU, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return CPU{}, err
	}
	// comm (field 2) may hold spaces and parentheses: split after the last ')'.
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return CPU{}, fmt.Errorf("qualify: pid %d stat %q", pid, data)
	}
	fields := strings.Fields(string(data[end+1:]))
	// fields[0] is field 3 (state); utime and stime are fields 14 and 15.
	if len(fields) < 13 {
		return CPU{}, fmt.Errorf("qualify: pid %d stat has %d fields", pid, len(fields)+2)
	}
	utime, err1 := strconv.ParseInt(fields[11], 10, 64)
	stime, err2 := strconv.ParseInt(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return CPU{}, fmt.Errorf("qualify: pid %d stat utime/stime", pid)
	}
	hz, err := clockTicks()
	if err != nil {
		return CPU{}, err
	}
	return CPU{Ns: (utime + stime) * (1_000_000_000 / hz), Method: CPUMethodStat}, nil
}

const atClkTck = 17

// clockTicks reads AT_CLKTCK from this process's auxiliary vector.
func clockTicks() (int64, error) {
	data, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return 0, err
	}
	word := int(unsafe.Sizeof(uintptr(0)))
	read := func(b []byte) uint64 {
		if word == 8 {
			return binary.NativeEndian.Uint64(b)
		}
		return uint64(binary.NativeEndian.Uint32(b))
	}
	for i := 0; i+2*word <= len(data); i += 2 * word {
		key, value := read(data[i:]), read(data[i+word:])
		if key == atClkTck {
			if value == 0 || value > 1_000_000_000 {
				return 0, fmt.Errorf("qualify: AT_CLKTCK %d", value)
			}
			return int64(value), nil
		}
		if key == 0 {
			break
		}
	}
	return 0, fmt.Errorf("qualify: no AT_CLKTCK in /proc/self/auxv")
}

// FileSizes returns the Lstat size of every regular file directly in dir
// (for a store: visual.sqlite, visual.sqlite-wal and visual.sqlite-shm
// separately). Missing files are simply absent.
func FileSizes(dir string) (map[string]int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		out[e.Name()] = info.Size()
	}
	return out, nil
}

var fsMagic = map[int64]string{
	0x9123683e: "btrfs",
	0xef53:     "ext4",
	0x01021994: "tmpfs",
	0x58465342: "xfs",
	0x2fc12fc1: "zfs",
	0x794c7630: "overlayfs",
	0x6969:     "nfs",
	0xf2f52010: "f2fs",
	0xca451a4e: "bcachefs",
	0x65735546: "fuse",
	0x858458f6: "ramfs",
	0x5346544e: "ntfs",
	0x4d44:     "vfat",
	0x3153464a: "jfs",
	0x52654973: "reiserfs",
	0x73717368: "squashfs",
	0x9fa0:     "proc",
}

// FSType names the filesystem holding path (statfs f_type).
func FSType(path string) (string, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "", err
	}
	magic := int64(st.Type) & 0xffffffff
	if name, ok := fsMagic[magic]; ok {
		return name, nil
	}
	return fmt.Sprintf("unknown(0x%x)", magic), nil
}

// LoadAverage is /proc/loadavg.
type LoadAverage struct {
	Raw string
}

// LoadAvg reads /proc/loadavg verbatim (trimmed).
func LoadAvg() (LoadAverage, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return LoadAverage{}, err
	}
	return LoadAverage{Raw: strings.TrimSpace(string(data))}, nil
}
