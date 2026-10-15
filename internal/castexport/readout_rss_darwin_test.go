package castexport

import "github.com/self-deprecated/agent-reel/internal/darwinproc"

func helperRSS(pid int) (int64, error) { return darwinproc.ResidentBytes(pid) }
