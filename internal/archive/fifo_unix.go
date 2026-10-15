//go:build linux || darwin

package archive

import (
	"io"
	"os"
	"sync"
	"syscall"
)

// FIFO holds a startup writer so the reader cannot mistake "recorder not yet
// opened" for EOF. ProducerDone must be called after the owned producer exits,
// not when stop is requested: buffered data then drains before final EOF.
type FIFO struct {
	Reader io.ReadCloser
	writer *os.File
	once   sync.Once
}

func OpenFIFO(path string) (*FIFO, error) {
	if e := syscall.Mkfifo(path, 0600); e != nil {
		return nil, e
	}
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	reader := newFIFOReader(fd, path)
	fd, e = syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if e != nil {
		reader.Close()
		return nil, e
	}
	return &FIFO{Reader: reader, writer: os.NewFile(uintptr(fd), path)}, nil
}
func (f *FIFO) ProducerDone() { f.once.Do(func() { _ = f.writer.Close() }) }
