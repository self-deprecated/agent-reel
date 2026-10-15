package reel

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const contextPathMax = 4096

// contextPath accepts only an absolute, bounded, control-free path string for
// display/grouping. Anything else is left unknown rather than repaired.
func contextPath(s string) string {
	if !filepath.IsAbs(s) || !activityText(s, contextPathMax) || filepath.Clean(s) != s {
		return ""
	}
	return s
}

// captureContext declares the starting command's working directory and the JJ
// main (default) workspace root as bounded labels. getwd and jj are injected
// so tests never run a real jj; a failing or absent lookup stays unknown.
func captureContext(getwd func() (string, error), jj func(string) (string, error)) *RecordingContext {
	c := &RecordingContext{JJMainSource: "unavailable"}
	if getwd != nil {
		if dir, e := getwd(); e == nil {
			c.WorkingDirectory = contextPath(dir)
		}
	}
	if c.WorkingDirectory != "" && jj != nil {
		if root, e := jj(c.WorkingDirectory); e == nil && contextPath(root) != "" {
			c.JJMain = contextPath(root)
			c.JJMainSource = "jj-workspace-root-default"
		}
	}
	if c.WorkingDirectory == "" && c.JJMain == "" {
		return nil
	}
	return c
}

// jjMainWorkspace asks the installed jj (PATH lookup only) for the default
// workspace root of dir without touching the working copy. It is a bounded
// read-only child: 3s deadline, 8 KiB output, no snapshot or config writes.
func jjMainWorkspace(ctx context.Context, dir string) (string, error) {
	exe, e := exec.LookPath("jj")
	if e != nil {
		return "", e
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--ignore-working-copy", "workspace", "root", "--name", "default")
	cmd.Dir = dir
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, max: 8192}
	cmd.Stderr = io.Discard
	if e = cmd.Run(); e != nil {
		return "", e
	}
	root := strings.TrimRight(out.String(), "\r\n")
	if strings.ContainsAny(root, "\r\n") || contextPath(root) == "" {
		return "", fmt.Errorf("unusable jj workspace root")
	}
	return root, nil
}

type limitedWriter struct {
	w   io.Writer
	max int
	n   int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n+len(p) > l.max {
		return 0, fmt.Errorf("output limit")
	}
	l.n += len(p)
	return l.w.Write(p)
}

func recordingContextNow(ctx context.Context) *RecordingContext {
	return captureContext(os.Getwd, func(dir string) (string, error) { return jjMainWorkspace(ctx, dir) })
}
