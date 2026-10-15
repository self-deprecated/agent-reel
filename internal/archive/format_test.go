//go:build linux || darwin

package archive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const castHeader = `{"version":3,"term":{"cols":80,"rows":24},"idle_time_limit":1.7976931348623157e308}` + "\n"

// validateCast feeds a whole cast through one Validator, line by line, as the
// visual/1 clock does; a final line without its newline is passed as is.
func validateCast(data []byte) error {
	v := Validator{Geometry: Geometry{80, 24}}
	for len(data) > 0 {
		n := bytes.IndexByte(data, '\n')
		line := data
		if n >= 0 {
			line = data[:n+1]
		}
		if e := v.Line(line); e != nil {
			return e
		}
		data = data[len(line):]
	}
	return v.Complete()
}

func TestValidatorFormat(t *testing.T) {
	base := []byte(castHeader + "[0,\"o\",\"ok\"]\n[0,\"x\",\"0\"]\n")
	for _, good := range [][]byte{base, []byte(castHeader + "[-0,\"r\",\"100x30\"]\n[0,\"m\",\"marker\"]\n[0,\"o\",\"ok\"]\n[1.234567890123e-3,\"o\",\"λ 界\"]\n[0,\"x\",\"0\"]\n")} {
		if e := validateCast(good); e != nil {
			t.Fatalf("valid cast refused: %v\n%s", e, good)
		}
	}
	cases := map[string][]byte{
		"truncated": base[:len(base)-1], "no-exit": []byte(castHeader + "[0,\"o\",\"ok\"]\n"),
		"no-output":        []byte(castHeader + "[0,\"x\",\"0\"]\n"),
		"duplicate-header": append([]byte(castHeader), base...),
		"after-exit":       append(append([]byte{}, base...), []byte("[0,\"o\",\"after\"]\n")...),
		"non-utf8":         []byte(castHeader + "[0,\"o\",\"\xff\"]\n[0,\"x\",\"0\"]\n"),
	}
	for _, event := range []string{`[null,"o","x"]`, `["0","o","x"]`, `[-1,"o","x"]`, `[-1e-999,"o","x"]`, `[1e999,"o","x"]`, `[0,"o",null]`, `[0,"o",{}]`, `[0,"i","secret"]`, `[0,"?","unknown"]`, `[0,"r","0x24"]`, `[0,"r","80x65536"]`, `[0,"r","+80x24"]`, `[0,"x","137"]`, `[0,"o","x",1]`} {
		cases[event] = []byte(castHeader + event + "\n[0,\"x\",\"0\"]\n")
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if validateCast(data) == nil {
				t.Fatal("malformed cast accepted")
			}
		})
	}
}

func TestStorageErrorClassification(t *testing.T) {
	cause := fmt.Errorf("write: %w", os.ErrNotExist)
	failure := &StorageError{"publish", cause}
	if !IsStorageFailure(failure) || !IsStorageFailure(fmt.Errorf("wrapped: %w", failure)) || !IsStorageFailure(errors.Join(errors.New("other"), failure)) {
		t.Fatal("storage failure not classified through wrapping")
	}
	if !errors.Is(failure, os.ErrNotExist) || failure.Error() != cause.Error() {
		t.Fatal("storage failure hides or rewrites its cause")
	}
	if IsStorageFailure(cause) || IsStorageFailure(nil) {
		t.Fatal("plain failure classified as storage")
	}
	backlog := &StorageError{"backlog", errors.New("visual capture backlog: no durable progress for 15s")}
	if !IsStorageFailure(backlog) || backlog.Error() != "visual capture backlog: no durable progress for 15s" {
		t.Fatal("backlog classification or message changed")
	}
}

// The startup writer keeps the reader from seeing EOF before the producer
// opens; buffered data drains before the final EOF after ProducerDone.
func TestFIFOStartupWriterAndDrain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.fifo")
	fifo, e := OpenFIFO(path)
	if e != nil {
		t.Fatal(e)
	}
	defer fifo.Reader.Close()
	if st, e := os.Lstat(path); e != nil || st.Mode()&os.ModeNamedPipe == 0 || st.Mode().Perm() != 0600 {
		t.Fatalf("fifo mode %v %v", st, e)
	}
	if _, e := OpenFIFO(path); e == nil {
		t.Fatal("existing path reused")
	}
	producer, e := os.OpenFile(path, os.O_WRONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = producer.WriteString("data\n"); e != nil {
		t.Fatal(e)
	}
	if e = producer.Close(); e != nil {
		t.Fatal(e)
	}
	got := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(fifo.Reader)
		got <- data
	}()
	select {
	case data := <-got:
		t.Fatalf("EOF before ProducerDone: %q", data)
	case <-time.After(100 * time.Millisecond):
	}
	fifo.ProducerDone()
	fifo.ProducerDone()
	select {
	case data := <-got:
		if string(data) != "data\n" {
			t.Fatalf("drained %q", data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no EOF after ProducerDone")
	}
}

func TestFIFOCloseInterruptsReader(t *testing.T) {
	fifo, e := OpenFIFO(filepath.Join(t.TempDir(), "capture.fifo"))
	if e != nil {
		t.Fatal(e)
	}
	defer fifo.ProducerDone()
	done := make(chan error, 1)
	go func() { _, e := fifo.Reader.Read(make([]byte, 1)); done <- e }()
	time.Sleep(20 * time.Millisecond)
	if e := fifo.Reader.Close(); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("read succeeded after close")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO read did not stop")
	}
}
