package qualify

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const burnChildEnv = "AREEL_QUALIFY_TEST_BURN_CHILD"

// Known work for the sampler test: 64 MiB touched, 400 ms of CPU.
const (
	burnBytes = 64 << 20
	burnCPU   = 400 * time.Millisecond
)

func TestMain(m *testing.M) {
	if os.Getenv(burnChildEnv) == "1" {
		burnChild()
		return
	}
	os.Exit(m.Run())
}

// burnChild touches burnBytes of anonymous memory outside the Go heap, spins
// until its own rusage shows burnCPU, reports, and waits for stdin to close.
func burnChild() {
	mem, err := syscall.Mmap(-1, 0, burnBytes, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		fmt.Println("error", err)
		os.Exit(2)
	}
	for i := 0; i < len(mem); i += 4096 {
		mem[i] = 1
	}
	x := uint64(1)
	for {
		ru, _ := SelfRusage()
		if time.Duration(ru.CPUNs()) >= burnCPU {
			break
		}
		for i := 0; i < 1_000_000; i++ {
			x = x*6364136223846793005 + 1442695040888963407
		}
	}
	ru, _ := SelfRusage()
	fmt.Println("ready", ru.CPUNs(), x&1)
	bufio.NewReader(os.Stdin).ReadString('\n')
	_ = mem[0]
}

func TestProcSamplersAgainstBurningChild(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), burnChildEnv+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); cmd.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "ready") {
		t.Fatalf("child: %q %v", line, err)
	}
	pid := cmd.Process.Pid
	within := func(name string, got, want int64) {
		t.Helper()
		if got < want*3/10 || got > want*3 {
			t.Errorf("%s = %d, want within 0.3x-3x of %d", name, got, want)
		}
	}
	cpu, err := ProcCPU(pid)
	if err != nil {
		t.Fatal(err)
	}
	if cpu.Method != CPUMethodSchedstat && cpu.Method != CPUMethodStat {
		t.Errorf("method %q", cpu.Method)
	}
	within("ProcCPU ("+cpu.Method+")", cpu.Ns, int64(burnCPU))
	stat, err := procCPUStat(pid)
	if err != nil {
		t.Fatal(err)
	}
	within("procCPUStat", stat.Ns, int64(burnCPU))
	if stat.Method != CPUMethodStat {
		t.Errorf("fallback method %q", stat.Method)
	}
	t.Logf("CPU: %s %v, %s %v", cpu.Method, time.Duration(cpu.Ns), stat.Method, time.Duration(stat.Ns))
	st, err := ProcStatus(pid)
	if err != nil {
		t.Fatal(err)
	}
	within("VmHWM", st.VmHWM, burnBytes)
	within("VmRSS", st.VmRSS, burnBytes)
	if st.VmHWM < st.VmRSS || st.Threads < 1 {
		t.Errorf("status %+v", st)
	}
	t.Logf("status: VmRSS %d MiB, VmHWM %d MiB, %d threads", st.VmRSS>>20, st.VmHWM>>20, st.Threads)

	self, err := SelfRusage()
	if err != nil || self.CPUNs() <= 0 || self.MaxRSSBytes <= 0 {
		t.Errorf("self rusage %+v %v", self, err)
	}
	if hz, err := clockTicks(); err != nil || hz <= 0 {
		t.Errorf("AT_CLKTCK %d %v", hz, err)
	}
}

func TestProcHelpers(t *testing.T) {
	dir := t.TempDir()
	name, err := FSType(dir)
	if err != nil || name == "" {
		t.Fatalf("FSType %q %v", name, err)
	}
	t.Logf("artifact fs: %s", name)
	must(t, os.WriteFile(dir+"/visual.sqlite", make([]byte, 4096), 0o600))
	must(t, os.WriteFile(dir+"/visual.sqlite-wal", make([]byte, 100), 0o600))
	must(t, os.Mkdir(dir+"/sub", 0o700))
	must(t, os.Symlink(dir+"/visual.sqlite", dir+"/link"))
	sizes, err := FileSizes(dir)
	if err != nil || len(sizes) != 2 || sizes["visual.sqlite"] != 4096 || sizes["visual.sqlite-wal"] != 100 {
		t.Fatalf("FileSizes %v %v", sizes, err)
	}
	load, err := LoadAvg()
	if err != nil || len(strings.Fields(load.Raw)) != 5 {
		t.Fatalf("LoadAvg %q %v", load.Raw, err)
	}
	if _, err := ProcStatus(-1); err == nil {
		t.Fatal("ProcStatus(-1) succeeded")
	}
}
