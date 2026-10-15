package videoexport

import "github.com/self-deprecated/agent-reel/internal/darwinproc"

func processRSS(pid int) (int64, error) { return darwinproc.ResidentBytes(pid) }
