package videoexport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Chapter is a half-open output frame range [StartFrame, EndFrame) with a title.
type Chapter struct {
	Title      string `json:"title"`
	StartFrame int    `json:"startFrame"`
	EndFrame   int    `json:"endFrame"`
}

const (
	// MaxChapters bounds embedded chapters (editmap allows at most 256 clips).
	MaxChapters = 1024
	// MaxChapterTitleBytes bounds a sanitized chapter title.
	MaxChapterTitleBytes = 128
)

// SanitizeChapterTitle returns the title FFmpeg embeds and ffprobe reads back:
// every control character (including newline and carriage return) becomes a
// space and surrounding spaces are trimmed. ffmetadata escaping of '=', ';', '#'
// and '\' happens separately in the private metadata file and round-trips.
func SanitizeChapterTitle(title string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) {
			return ' '
		}
		return r
	}, title))
}

// checkChapters validates contiguous chapters covering [0, frames) and returns
// them with sanitized titles. An empty list means no chapters.
func checkChapters(chapters []Chapter, frames int) ([]Chapter, error) {
	if len(chapters) == 0 {
		return nil, nil
	}
	if len(chapters) > MaxChapters {
		return nil, fmt.Errorf("video chapters exceed %d", MaxChapters)
	}
	out := make([]Chapter, len(chapters))
	next := 0
	for i, c := range chapters {
		if c.StartFrame != next || c.EndFrame <= c.StartFrame || c.EndFrame > frames {
			return nil, fmt.Errorf("video chapter %d [%d,%d) is not contiguous over [0,%d)", i+1, c.StartFrame, c.EndFrame, frames)
		}
		if !utf8.ValidString(c.Title) {
			return nil, fmt.Errorf("video chapter %d title is not UTF-8", i+1)
		}
		title := SanitizeChapterTitle(c.Title)
		if title == "" || len(title) > MaxChapterTitleBytes {
			return nil, fmt.Errorf("video chapter %d title must be 1..%d bytes", i+1, MaxChapterTitleBytes)
		}
		out[i] = Chapter{Title: title, StartFrame: c.StartFrame, EndFrame: c.EndFrame}
		next = c.EndFrame
	}
	if next != frames {
		return nil, fmt.Errorf("video chapters end at frame %d, not %d", next, frames)
	}
	return out, nil
}

// escapeMetadata applies FFmpeg's ffmetadata escaping for a value.
func escapeMetadata(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '=', ';', '#', '\\', '\n':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ffmetadata renders sanitized chapters with TIMEBASE=Den/Num, so START and END
// are exact output frame numbers.
func ffmetadata(rate Rate, chapters []Chapter) string {
	var b strings.Builder
	b.WriteString(";FFMETADATA1\n")
	for _, c := range chapters {
		fmt.Fprintf(&b, "[CHAPTER]\nTIMEBASE=%d/%d\nSTART=%d\nEND=%d\ntitle=%s\n", rate.Den, rate.Num, c.StartFrame, c.EndFrame, escapeMetadata(c.Title))
	}
	return b.String()
}

func writeMetadata(path string, rate Rate, chapters []Chapter) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = io.WriteString(f, ffmetadata(rate, chapters))
	return errors.Join(err, f.Close())
}

// ProbedChapter is one chapter as ffprobe reads it back from the container.
type ProbedChapter struct {
	Title      string `json:"title"`
	TimeBase   string `json:"timeBase"`
	Start      int64  `json:"start"`
	End        int64  `json:"end"`
	StartFrame int    `json:"startFrame"`
	EndFrame   int    `json:"endFrame"`
}

type probeChapters struct {
	Chapters []struct {
		TimeBase string `json:"time_base"`
		Start    int64  `json:"start"`
		End      int64  `json:"end"`
		Tags     struct {
			Title string `json:"title"`
		} `json:"tags"`
	} `json:"chapters"`
}

// roundFrame returns round-half-up(ticks·timeBase·rate) using exact rationals.
func roundFrame(ticks int64, timeBase string, rate Rate) (int, error) {
	tb, ok := new(big.Rat).SetString(timeBase)
	if !ok || tb.Sign() <= 0 || ticks < 0 {
		return 0, fmt.Errorf("invalid probed chapter time %d·%s", ticks, timeBase)
	}
	x := new(big.Rat).Mul(new(big.Rat).SetInt64(ticks), tb)
	x.Mul(x, new(big.Rat).SetFrac64(rate.Num, rate.Den))
	x.Add(x, big.NewRat(1, 2))
	q := new(big.Int).Quo(x.Num(), x.Denom())
	if !q.IsInt64() || q.Int64() > MaxFrames+1 {
		return 0, errors.New("probed chapter beyond frame bound")
	}
	return int(q.Int64()), nil
}

// matchChapters compares ffprobe -show_chapters JSON with the requested chapters:
// count, exact titles and round(start·Rate) == StartFrame, round(end·Rate) ==
// EndFrame. Containers may quantize chapter times (MP4/MOV use milliseconds), so
// the comparison is on the nearest frame, never a looser tolerance.
func matchChapters(output []byte, rate Rate, want []Chapter) ([]ProbedChapter, error) {
	var probe probeChapters
	if err := json.Unmarshal(output, &probe); err != nil {
		return nil, fmt.Errorf("video chapter verification: %w", err)
	}
	if len(probe.Chapters) != len(want) {
		return nil, fmt.Errorf("video chapter count %d, want %d", len(probe.Chapters), len(want))
	}
	got := make([]ProbedChapter, len(want))
	for i, c := range probe.Chapters {
		start, err := roundFrame(c.Start, c.TimeBase, rate)
		if err != nil {
			return nil, err
		}
		end, err := roundFrame(c.End, c.TimeBase, rate)
		if err != nil {
			return nil, err
		}
		got[i] = ProbedChapter{Title: c.Tags.Title, TimeBase: c.TimeBase, Start: c.Start, End: c.End, StartFrame: start, EndFrame: end}
		if c.Tags.Title != want[i].Title || start != want[i].StartFrame || end != want[i].EndFrame {
			return nil, fmt.Errorf("video chapter %d is %q [%d,%d), want %q [%d,%d)", i+1, c.Tags.Title, start, end, want[i].Title, want[i].StartFrame, want[i].EndFrame)
		}
	}
	return got, nil
}

func (r *Renderer) verifyChapters(ctx context.Context, path string, rate Rate, want []Chapter) ([]ProbedChapter, error) {
	cmd := exec.CommandContext(ctx, r.config.FFprobe, "-v", "error", "-show_chapters", "-of", "json", path)
	ownCommand(cmd)
	output := &cappedBuffer{limit: 1 << 20}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("video chapter verification: %w", err)
	}
	if output.overflow {
		return nil, errors.New("video chapter verification output exceeds bound")
	}
	return matchChapters(output.data, rate, want)
}

// cappedBuffer keeps at most limit bytes and records overflow instead of
// silently truncating a structured document.
type cappedBuffer struct {
	limit    int
	data     []byte
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > b.limit {
		b.overflow = true
		b.data = append(b.data, p[:max(0, b.limit-len(b.data))]...)
		return len(p), nil
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func formatRat(x *big.Rat) string {
	if x.IsInt() {
		return x.Num().String()
	}
	return x.Num().String() + "/" + x.Denom().String()
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
