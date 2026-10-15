package qualify

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// Event is one asciicast v3 event at an absolute logical time. AtNs is exact
// integer nanoseconds from the start of the stream; the writer turns it into
// the v3 interval, so only integers take part in a timing decision.
type Event struct {
	AtNs int64
	Kind string // "o", "r", "m" or "x"
	Data string
}

// FormatSeconds renders a nonnegative nanosecond count as the shortest exact
// JSON decimal number of seconds: 50_000_000 → "0.05", 1_000_000_000 → "1".
// visualtime.ParseSeconds reads every value it produces back to the same
// nanosecond count.
func FormatSeconds(ns int64) string {
	if ns < 0 {
		panic("qualify: negative interval")
	}
	whole, frac := ns/1_000_000_000, ns%1_000_000_000
	if frac == 0 {
		return strconv.FormatInt(whole, 10)
	}
	digits := []byte(fmt.Sprintf("%09d", frac))
	for digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
	}
	return strconv.FormatInt(whole, 10) + "." + string(digits)
}

// CastWriter writes an asciicast v3 stream. Intervals are the difference of
// absolute int64 nanosecond times and are never accumulated from formatted
// values, so the sum of every interval equals the last event's time exactly.
type CastWriter struct {
	w      *bufio.Writer
	last   int64
	events int64
	bytes  int64
	err    error
	buf    bytes.Buffer
	enc    *json.Encoder
}

// NewCastWriter writes the v3 header for a cols x rows terminal.
func NewCastWriter(w io.Writer, cols, rows int) (*CastWriter, error) {
	c := &CastWriter{w: bufio.NewWriterSize(w, 1<<16)}
	c.enc = json.NewEncoder(&c.buf)
	c.enc.SetEscapeHTML(false)
	header := fmt.Sprintf("{\"version\":3,\"term\":{\"cols\":%d,\"rows\":%d}}\n", cols, rows)
	c.write([]byte(header))
	return c, c.err
}

func (c *CastWriter) write(p []byte) {
	if c.err != nil {
		return
	}
	n, err := c.w.Write(p)
	c.bytes += int64(n)
	c.err = err
}

// Write appends one event. Times must be nondecreasing.
func (c *CastWriter) Write(e Event) error {
	if c.err != nil {
		return c.err
	}
	if e.AtNs < c.last {
		c.err = fmt.Errorf("qualify: cast event at %d ns precedes %d ns", e.AtNs, c.last)
		return c.err
	}
	switch e.Kind {
	case "o", "r", "m", "x":
	default:
		c.err = fmt.Errorf("qualify: cast event kind %q", e.Kind)
		return c.err
	}
	c.buf.Reset()
	if err := c.enc.Encode(e.Data); err != nil {
		c.err = err
		return err
	}
	data := bytes.TrimSuffix(c.buf.Bytes(), []byte("\n"))
	line := make([]byte, 0, len(data)+32)
	line = append(line, '[')
	line = append(line, FormatSeconds(e.AtNs-c.last)...)
	line = append(line, ",\""...)
	line = append(line, e.Kind...)
	line = append(line, "\","...)
	line = append(line, data...)
	line = append(line, "]\n"...)
	c.write(line)
	c.last = e.AtNs
	c.events++
	return c.err
}

// Close flushes buffered output. It does not close the underlying writer.
func (c *CastWriter) Close() error {
	if c.err != nil {
		return c.err
	}
	c.err = c.w.Flush()
	return c.err
}

// Bytes is the number of bytes written so far, header included.
func (c *CastWriter) Bytes() int64 { return c.bytes }

// Events is the number of events written so far.
func (c *CastWriter) Events() int64 { return c.events }

// LastNs is the absolute time of the last event written.
func (c *CastWriter) LastNs() int64 { return c.last }
