package archive

import (
	"io"
	"os"
)

func newFIFOReader(fd int, path string) io.ReadCloser { return os.NewFile(uintptr(fd), path) }
