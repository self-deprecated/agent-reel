package visualstore

import (
	"os"

	"github.com/self-deprecated/agent-reel/internal/darwinproc"
)

func openFilePaths() ([]string, error) { return darwinproc.FilePaths(os.Getpid()) }
