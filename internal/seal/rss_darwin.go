package seal

import "syscall"

func peakRSS() int64 {
	var r syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &r) != nil {
		return 0
	}
	return r.Maxrss
}
