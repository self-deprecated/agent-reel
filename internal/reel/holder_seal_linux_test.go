package reel

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHolderSealPriorityLinux(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("priority test requires an unprivileged child")
	}
	if os.Getenv("AREEL_SEAL_PRIORITY_CHILD") == "1" {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := platformLowerSealPriority(); err != nil {
			t.Fatal(err)
		}
		priority, err := unix.Getpriority(unix.PRIO_PROCESS, 0)
		// Linux's raw syscall returns 20 - nice (x/sys does not convert it).
		if err != nil || 20-priority < 10 {
			t.Fatalf("nice = %d: %v", 20-priority, err)
		}
		ioPriority, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, 1, 0, 0)
		if errno != 0 || ioPriority != (2<<13)|7 {
			t.Fatalf("I/O priority = %d: %v", ioPriority, errno)
		}
		t.Logf("nice=%d I/O=best-effort/7", 20-priority)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHolderSealPriorityLinux$", "-test.v")
	child.Env = append(os.Environ(), "AREEL_SEAL_PRIORITY_CHILD=1")
	out, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("priority child: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
