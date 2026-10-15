//go:build linux || darwin

package reel

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/self-deprecated/agent-reel/internal/archive"
)

// streamSink is a test captureSink that stands in for the visual/1 store
// where a test exercises the owned PTY/FIFO lifecycle or the live socket, not
// the store: it validates the recorder's asciicast stream line by line and
// keeps the validated lines in memory. Nothing is written to disk. As with
// the real sink, failure and completion are separate signals, and a failure
// closes the input so a blocked recorder cannot wedge the lifecycle.
type streamSink struct {
	input        io.ReadCloser
	validator    archive.Validator
	done, failed chan struct{}
	once         sync.Once
	mu           sync.Mutex
	err          error
	raw          bytes.Buffer
	complete     bool
	output       atomic.Bool
}

func startStreamSink(input io.ReadCloser, geometry archive.Geometry) *streamSink {
	s := &streamSink{input: input, validator: archive.Validator{Geometry: geometry}, done: make(chan struct{}), failed: make(chan struct{})}
	go s.run()
	return s
}
func (s *streamSink) run() {
	defer close(s.done)
	r := bufio.NewReaderSize(s.input, archive.Defaults().LineBytes)
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			s.fail(fmt.Errorf("recorder line exceeds %d bytes", r.Size()))
			return
		}
		if len(line) > 0 && line[len(line)-1] == '\n' {
			if e := s.validator.Line(line); e != nil {
				s.fail(e)
				return
			}
			s.mu.Lock()
			s.raw.Write(line)
			s.mu.Unlock()
			if s.validator.Output {
				s.output.Store(true)
			}
		}
		if err == io.EOF {
			if len(line) > 0 && line[len(line)-1] != '\n' {
				s.fail(fmt.Errorf("incomplete recorder line at EOF"))
				return
			}
			s.mu.Lock()
			s.complete = s.validator.Complete() == nil
			s.mu.Unlock()
			return
		}
		if err != nil {
			s.fail(err)
			return
		}
	}
}
func (s *streamSink) fail(e error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = e
		s.mu.Unlock()
		close(s.failed)
		_ = s.input.Close()
	})
}
func (s *streamSink) Done() <-chan struct{}   { return s.done }
func (s *streamSink) Failed() <-chan struct{} { return s.failed }
func (s *streamSink) Err() error              { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *streamSink) OutputObserved() bool    { return s.output.Load() }

// cast returns a copy of the validated lines so far.
func (s *streamSink) cast() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.raw.Bytes())
}

// size is the number of validated bytes so far.
func (s *streamSink) size() int { s.mu.Lock(); defer s.mu.Unlock(); return s.raw.Len() }

// completed reports whether the stream ended with a valid exit.
func (s *streamSink) completed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.complete }
