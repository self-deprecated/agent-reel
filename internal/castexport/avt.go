// Package castexport transforms caller-verified, immutable asciicast-v3 sources.
// Publication is the caller's responsibility: discard output on any error.
package castexport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	MaxFeedBytes       = 4 << 20
	MaxCheckpointBytes = 16 << 20
)

// Checkpoint contains terminal STATE, not the raw output prefix or scrollback.
// An inactive primary screen is retained when required by alternate-screen exit.
type Checkpoint struct {
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	Init string `json:"init"`
}

// Terminal is a single-owner streaming emulator session. Feed/Resize/Snapshot
// calls occur in cast order; a DVR index can take many snapshots in one pass.
type Terminal interface {
	Feed([]byte) error
	Resize(cols, rows int) error
	Snapshot() (Checkpoint, error)
	Close() error
}

type TerminalFactory func(context.Context, int, int) (Terminal, error)

// HelperProtocol is the checkpoint helper protocol version this build speaks.
// The helper answers the `P` handshake with "AVTP" and its version as a
// big-endian u32; see tools/avt-checkpoint/README.md.
const HelperProtocol = 2

const (
	helperMagic = "AVTP"
	// handshakeTimeout bounds the one handshake round trip even for a capture
	// context without a deadline.
	handshakeTimeout = 10 * time.Second
	// stderrTail bounds the helper diagnostics kept for error messages.
	stderrTail = 512
	// deathWait bounds how long a failure waits for the helper's stderr to
	// close before reporting what it said so far.
	deathWait = time.Second
)

// HelperMismatchError reports a checkpoint helper that did not complete the
// protocol handshake with the version this areel requires. Got is the version
// the helper reported, or 0 when it did not answer (Reason says why; an older
// helper rejects the handshake and exits) or answered garbage (Garbled). Hint,
// when set by the caller, says where the executable came from.
type HelperMismatchError struct {
	Executable string
	Want, Got  int
	Garbled    bool
	Reason     string
	Stderr     string
	Hint       string
}

func (e *HelperMismatchError) Error() string {
	var b strings.Builder
	switch {
	case e.Garbled:
		fmt.Fprintf(&b, "checkpoint helper %s is not a compatible areel-avt-checkpoint (protocol version %d required; %s)", e.Executable, e.Want, e.Reason)
	case e.Got == 0:
		fmt.Fprintf(&b, "checkpoint helper %s is older than this areel (protocol version %d required; %s)", e.Executable, e.Want, e.Reason)
	case e.Got < e.Want:
		fmt.Fprintf(&b, "checkpoint helper %s is older than this areel (protocol version %d required, helper speaks %d)", e.Executable, e.Want, e.Got)
	default:
		fmt.Fprintf(&b, "checkpoint helper %s is newer than this areel (protocol version %d required, helper speaks %d)", e.Executable, e.Want, e.Got)
	}
	if e.Hint != "" {
		b.WriteString("; ")
		b.WriteString(e.Hint)
	}
	if e.Stderr != "" {
		b.WriteString("; helper said: ")
		b.WriteString(e.Stderr)
	}
	return b.String()
}

// tail keeps the last stderrTail bytes the helper wrote to stderr.
type tail struct {
	mu   sync.Mutex
	buf  []byte
	done chan struct{}
}

func (t *tail) drain(r *os.File) {
	defer close(t.done)
	defer r.Close()
	chunk := make([]byte, 512)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			t.mu.Lock()
			t.buf = append(t.buf, chunk[:n]...)
			if len(t.buf) > stderrTail {
				t.buf = append(t.buf[:0], t.buf[len(t.buf)-stderrTail:]...)
			}
			t.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// said waits (bounded) for the helper's stderr to close and returns its tail
// as one line.
func (t *tail) said(wait time.Duration) string {
	select {
	case <-t.done:
	case <-time.After(wait):
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	text := strings.ToValidUTF8(string(bytes.TrimSpace(t.buf)), "?")
	return strings.Join(strings.Fields(text), " ")
}

// AVT is an owned subprocess containing the recorder's exact avt 0.18.0 emulator.
// Go owns all cast/timeline logic; the helper only feeds, resizes and dumps state.
type AVT struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	input  io.WriteCloser
	output io.ReadCloser
	// reader buffers the single reply stream; cell encodings are variable length.
	reader *bufio.Reader
	cancel context.CancelFunc
	closed bool
	stderr *tail
}

func validGeometry(cols, rows int) bool {
	return cols > 0 && rows > 0 && cols <= 512 && rows <= 512 && cols*rows <= 32768
}

// NewAVT launches an explicitly provided local helper executable. The context
// must have a deadline so blocked helper I/O cannot retain an export indefinitely.
func NewAVT(ctx context.Context, executable string, cols, rows int) (*AVT, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("checkpoint context requires a deadline")
	}
	return newAVT(ctx, executable, cols, rows)
}

// NewCaptureAVT launches the same helper for a live capture, which has no
// natural deadline: it lasts as long as the recording segment. The context must
// instead be cancellable, and the capture owner cancels it to stop the helper,
// including while a reply is blocked.
func NewCaptureAVT(ctx context.Context, executable string, cols, rows int) (*AVT, error) {
	if ctx.Done() == nil {
		return nil, fmt.Errorf("capture checkpoint context must be cancellable")
	}
	return newAVT(ctx, executable, cols, rows)
}

func newAVT(ctx context.Context, executable string, cols, rows int) (*AVT, error) {
	if executable == "" || !validGeometry(cols, rows) {
		return nil, fmt.Errorf("invalid checkpoint helper or geometry")
	}
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, executable)
	cmd.WaitDelay = 2 * time.Second
	// A file (not a writer) so exec copies nothing: the drain goroutine owns
	// the read end and never blocks the helper or Wait.
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stderr = errWrite
	in, err := cmd.StdinPipe()
	if err != nil {
		errRead.Close()
		errWrite.Close()
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		errRead.Close()
		errWrite.Close()
		cancel()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		errRead.Close()
		errWrite.Close()
		cancel()
		return nil, err
	}
	errWrite.Close()
	stderr := &tail{done: make(chan struct{})}
	go stderr.drain(errRead)
	a := &AVT{cmd: cmd, input: in, output: out, reader: bufio.NewReader(out), cancel: cancel, stderr: stderr}
	if err = a.handshake(executable, cols, rows); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

// handshake sends the start header and the `P` version request, and reads the
// one reply. Any failure, including a helper that exits on the unknown
// operation, is a HelperMismatchError naming the executable.
func (a *AVT) handshake(executable string, cols, rows int) error {
	request := make([]byte, 13)
	copy(request, "AVT1")
	binary.BigEndian.PutUint32(request[4:], uint32(cols))
	binary.BigEndian.PutUint32(request[8:], uint32(rows))
	request[12] = 'P'
	timer := time.AfterFunc(handshakeTimeout, a.cancel)
	reply := make([]byte, 8)
	err := a.write(request)
	if err == nil {
		_, err = io.ReadFull(a.reader, reply)
	}
	if !timer.Stop() && err == nil {
		err = context.DeadlineExceeded
	}
	mismatch := &HelperMismatchError{Executable: executable, Want: HelperProtocol}
	switch {
	case err != nil:
		mismatch.Reason = "it did not answer the version handshake"
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			mismatch.Reason += ": " + err.Error()
		}
		mismatch.Stderr = a.stderr.said(deathWait)
		return mismatch
	case string(reply[:4]) != helperMagic:
		mismatch.Garbled = true
		mismatch.Reason = fmt.Sprintf("version handshake reply %q is not %q", reply[:4], helperMagic)
		return mismatch
	}
	mismatch.Got = int(binary.BigEndian.Uint32(reply[4:]))
	if mismatch.Got == HelperProtocol {
		return nil
	}
	if mismatch.Got == 0 {
		mismatch.Garbled = true
		mismatch.Reason = "helper reported version 0"
	}
	return mismatch
}

// died annotates a helper I/O failure with what the helper said on stderr.
func (a *AVT) died(what string, err error) error {
	if said := a.stderr.said(deathWait); said != "" {
		return fmt.Errorf("%s: %w; helper said: %s", what, err, said)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// CheckHelper launches executable, completes the protocol handshake and
// closes it. It returns a HelperMismatchError for an incompatible helper.
func CheckHelper(ctx context.Context, executable string) error {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout+deathWait)
	defer cancel()
	a, err := NewAVT(ctx, executable, 80, 24)
	if err != nil {
		return err
	}
	a.mu.Lock()
	err = a.write([]byte{'Q'})
	a.mu.Unlock()
	if cerr := a.Close(); err == nil {
		err = cerr
	}
	return err
}

func (a *AVT) write(data []byte) error {
	if a.closed {
		return fmt.Errorf("checkpoint session closed")
	}
	if _, err := a.input.Write(data); err != nil {
		return a.died("checkpoint helper write", err)
	}
	return nil
}
func (a *AVT) Feed(data []byte) error {
	if len(data) > MaxFeedBytes || !utf8.Valid(data) {
		return fmt.Errorf("checkpoint feed exceeds bound or is not UTF-8")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	header := make([]byte, 5)
	header[0] = 'F'
	binary.BigEndian.PutUint32(header[1:], uint32(len(data)))
	if err := a.write(header); err != nil {
		return err
	}
	return a.write(data)
}
func (a *AVT) Resize(cols, rows int) error {
	if !validGeometry(cols, rows) {
		return fmt.Errorf("checkpoint geometry exceeds bound")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	data := make([]byte, 9)
	data[0] = 'R'
	binary.BigEndian.PutUint32(data[1:], uint32(cols))
	binary.BigEndian.PutUint32(data[5:], uint32(rows))
	return a.write(data)
}
func (a *AVT) Snapshot() (Checkpoint, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var result Checkpoint
	if err := a.write([]byte{'D'}); err != nil {
		return result, err
	}
	header := make([]byte, 12)
	if _, err := io.ReadFull(a.reader, header); err != nil {
		return result, a.died("checkpoint helper response", err)
	}
	result.Cols = int(binary.BigEndian.Uint32(header))
	result.Rows = int(binary.BigEndian.Uint32(header[4:]))
	n := binary.BigEndian.Uint32(header[8:])
	if !validGeometry(result.Cols, result.Rows) || n > MaxCheckpointBytes {
		return Checkpoint{}, fmt.Errorf("checkpoint helper response exceeds bound")
	}
	data := make([]byte, int(n))
	if _, err := io.ReadFull(a.reader, data); err != nil {
		return Checkpoint{}, a.died("checkpoint helper result", err)
	}
	if !utf8.Valid(data) {
		return Checkpoint{}, fmt.Errorf("invalid checkpoint UTF-8")
	}
	result.Init = string(data)
	return result, nil
}

// number reads one big-endian unsigned 32-bit field of a helper reply.
func (a *AVT) number() (int, error) {
	var field [4]byte
	if _, err := io.ReadFull(a.reader, field[:]); err != nil {
		return 0, a.died("checkpoint helper response", err)
	}
	return int(binary.BigEndian.Uint32(field[:])), nil
}

// visibleHeader reads the geometry and cursor every readout reply carries.
func (a *AVT) visibleHeader() (int, int, Cursor, error) {
	cols, err := a.number()
	if err != nil {
		return 0, 0, Cursor{}, err
	}
	rows, err := a.number()
	if err != nil {
		return 0, 0, Cursor{}, err
	}
	if !validGeometry(cols, rows) {
		return 0, 0, Cursor{}, fmt.Errorf("visible readout geometry exceeds bound")
	}
	col, err := a.number()
	if err != nil {
		return 0, 0, Cursor{}, err
	}
	row, err := a.number()
	if err != nil {
		return 0, 0, Cursor{}, err
	}
	flag, err := a.reader.ReadByte()
	if err != nil {
		return 0, 0, Cursor{}, a.died("checkpoint helper response", err)
	}
	if flag > 1 {
		return 0, 0, Cursor{}, fmt.Errorf("visible cursor visibility %d", flag)
	}
	cursor := Cursor{Col: col, Row: row, Visible: flag == 1}
	if err := validCursor(cursor, cols, rows); err != nil {
		return 0, 0, Cursor{}, err
	}
	return cols, rows, cursor, nil
}

// Visible returns the whole canonical presentation and clears the helper's
// accumulated changed-row set. Nothing read from the helper is coerced: a reply
// that violates the contract is an error, not a repaired screen.
func (a *AVT) Visible() (Screen, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.write([]byte{'V'}); err != nil {
		return Screen{}, err
	}
	cols, rows, cursor, err := a.visibleHeader()
	if err != nil {
		return Screen{}, err
	}
	n, err := a.number()
	if err != nil {
		return Screen{}, err
	}
	if n > MaxViewBytes {
		return Screen{}, fmt.Errorf("visible readout exceeds bound")
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(a.reader, payload); err != nil {
		return Screen{}, a.died("checkpoint helper result", err)
	}
	grid, err := decodeScreen(payload, cols, rows)
	if err != nil {
		return Screen{}, err
	}
	return Screen{Cols: cols, Rows: rows, Cursor: cursor, Cells: grid}, nil
}

// Changed returns the rows avt reported dirty since the previous Visible or
// Changed, and clears that set. Dirty rows are hints from the emulator; only
// cell comparison decides whether the presentation actually changed. A cursor
// move alone never reports a row, and the cursor is reported here regardless.
func (a *AVT) Changed() (ScreenDelta, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.write([]byte{'W'}); err != nil {
		return ScreenDelta{}, err
	}
	cols, rows, cursor, err := a.visibleHeader()
	if err != nil {
		return ScreenDelta{}, err
	}
	count, err := a.number()
	if err != nil {
		return ScreenDelta{}, err
	}
	if count > rows {
		return ScreenDelta{}, fmt.Errorf("visible delta reports %d of %d rows", count, rows)
	}
	delta := ScreenDelta{Cols: cols, Rows: rows, Cursor: cursor, Updates: make([]RowUpdate, 0, count)}
	total, previous := 0, -1
	for i := 0; i < count; i++ {
		index, err := a.number()
		if err != nil {
			return ScreenDelta{}, err
		}
		if index <= previous || index >= rows {
			return ScreenDelta{}, fmt.Errorf("visible delta row %d is not ascending and in range", index)
		}
		previous = index
		cells, n, err := decodeRow(a.reader, cols)
		if err != nil {
			return ScreenDelta{}, fmt.Errorf("visible delta row %d: %w", index, err)
		}
		total += n + 4
		if total > MaxViewBytes {
			return ScreenDelta{}, fmt.Errorf("visible delta exceeds bound")
		}
		delta.Updates = append(delta.Updates, RowUpdate{Row: index, Cells: cells})
	}
	return delta, nil
}

func (a *AVT) Close() error {
	// Cancel before locking to interrupt any in-flight pipe I/O.
	a.cancel()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	_ = a.input.Close()
	_ = a.output.Close()
	_ = a.cmd.Wait()
	return nil
}
