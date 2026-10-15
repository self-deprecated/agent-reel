package qualify

import "golang.org/x/sys/unix"

func collectHostEnvironment(e *Environment) {
	e.CPUModel, _ = unix.Sysctl("machdep.cpu.brand_string")
	mem, _ := unix.SysctlUint64("hw.memsize")
	e.MemTotalBytes = int64(mem)
	e.Kernel, _ = unix.Sysctl("kern.osrelease")
}
