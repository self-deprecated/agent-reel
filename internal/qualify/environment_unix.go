//go:build linux || darwin

package qualify

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Environment is environment.json (plan decision 9): the host and tool
// context of one qualification run. The runner writes it; nothing in it is a
// measurement judged by a budget.
type Environment struct {
	CPUModel      string            `json:"cpuModel"`
	NProc         int               `json:"nproc"`
	MemTotalBytes int64             `json:"memTotalBytes"`
	Kernel        string            `json:"kernel"`
	Artifact      string            `json:"artifactDir"`
	ArtifactFS    string            `json:"artifactFsType"`
	FreeBytes     int64             `json:"artifactFreeBytesAtStart"`
	GoVersion     string            `json:"goVersion"`
	Helper        string            `json:"helper"`
	HelperSHA256  string            `json:"helperSha256"`
	Tools         map[string]string `json:"tools"`
	NoFileSoft    uint64            `json:"ulimitNofileSoft"`
	NoFileHard    uint64            `json:"ulimitNofileHard"`
	Started       string            `json:"started"`
	Finished      string            `json:"finished,omitempty"`
	LoadStart     string            `json:"loadavgStart"`
	LoadEnd       string            `json:"loadavgEnd,omitempty"`
	Profile       string            `json:"profile"`
	InNixSandbox  bool              `json:"inNixSandbox"`
}

// CollectEnvironment gathers the start-of-run environment for the artifact
// directory dir. Tool versions are read from the binaries named by the
// qualification environment ($AREEL_TEST_ASCIINEMA, $AREEL_TEST_CHROMIUM,
// $AREEL_TEST_FFMPEG); a tool whose version cannot be read records the error
// text instead.
func CollectEnvironment(dir, profile, helper string) Environment {
	e := Environment{NProc: runtime.NumCPU(), Artifact: dir, Profile: profile, Helper: helper, Tools: map[string]string{},
		Started: time.Now().UTC().Format(time.RFC3339), InNixSandbox: os.Getenv("NIX_BUILD_TOP") != ""}
	collectHostEnvironment(&e)
	if fs, err := FSType(dir); err == nil {
		e.ArtifactFS = fs
	}
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) == nil {
		e.FreeBytes = int64(st.Bavail) * int64(st.Bsize)
	}
	e.GoVersion = toolVersion("go", "version")
	if helper != "" {
		if sum, err := fileSHA256(helper); err == nil {
			e.HelperSHA256 = sum
		} else {
			e.HelperSHA256 = "error: " + err.Error()
		}
	}
	for name, env := range map[string]string{"asciinema": "AREEL_TEST_ASCIINEMA", "chromium": "AREEL_TEST_CHROMIUM", "ffmpeg": "AREEL_TEST_FFMPEG", "ffprobe": "AREEL_TEST_FFPROBE"} {
		path := os.Getenv(env)
		if path == "" {
			e.Tools[name] = "unset (" + env + ")"
			continue
		}
		flag := "--version"
		if name == "ffmpeg" || name == "ffprobe" {
			flag = "-version"
		}
		e.Tools[name] = path + ": " + toolVersion(path, flag)
	}
	var rl syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl) == nil {
		e.NoFileSoft, e.NoFileHard = rl.Cur, rl.Max
	}
	if la, err := LoadAvg(); err == nil {
		e.LoadStart = la.Raw
	}
	return e
}

// Finish records the end of the run.
func (e *Environment) Finish() {
	e.Finished = time.Now().UTC().Format(time.RFC3339)
	if la, err := LoadAvg(); err == nil {
		e.LoadEnd = la.Raw
	}
}

// toolVersion runs "path flag" with a short deadline and returns the first
// output line, or the error.
func toolVersion(path, flag string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, flag).CombinedOutput()
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if err != nil {
		return "error: " + err.Error() + ": " + line
	}
	return line
}
