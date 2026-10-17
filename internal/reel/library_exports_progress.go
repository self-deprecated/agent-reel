package reel

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/self-deprecated/agent-reel/internal/videoexport"
)

// Export deadlines scale with the work. Every job starts with exportAge for
// freezing, planning and derived casts; once a video plan is known the
// deadline becomes exportAge plus exportFrameBudget per output frame, capped at
// exportAgeLimit. exportFrameBudget assumes at least 5 rendered frames per
// second, under half the measured renderer throughput (about 11.5 frames/s for
// a 200x62 terminal at 1600x900, docs/exports.md), so an 8-hour recording at 1 fps
// (about 32,000 frames) gets about 136 minutes instead of being killed at 30.
// exportAge is also how long a finished job is retained.
const (
	exportAge         = 30 * time.Minute
	exportFrameBudget = 200 * time.Millisecond
	exportAgeLimit    = videoexport.MaxJobAgeLimit
)

// errExportDeadline is the cancellation cause of a job whose deadline passed.
var errExportDeadline = errors.New("export deadline expired")

// exportDeadlineFor is the whole-job deadline for a video of frames frames.
func exportDeadlineFor(frames int) time.Duration {
	if frames <= 0 {
		return exportAge
	}
	return min(exportAge+time.Duration(frames)*exportFrameBudget, exportAgeLimit)
}

// Export stages, in order. A job reports the stage it is in.
const (
	exportStagePreparing = "preparing"
	exportStageCasts     = "writing casts"
	exportStageRendering = "rendering video"
	exportStagePackaging = "packaging"
)

// exportProgress is one job's observable progress and its extendable
// deadline. A nil *exportProgress ignores every call.
type exportProgress struct {
	mu                      sync.Mutex
	stage                   string
	framesDone, framesTotal int
	started, deadline       time.Time
	timer                   *time.Timer
}

type exportProgressKey struct{}

// newExportProgress derives a job context from parent whose deadline starts at
// exportAge and can be extended by planFrames. stop releases the timer and
// cancels the context (cause context.Canceled).
func newExportProgress(parent context.Context) (context.Context, *exportProgress, func(error)) {
	ctx, cancel := context.WithCancelCause(parent)
	now := time.Now()
	p := &exportProgress{stage: exportStagePreparing, started: now, deadline: now.Add(exportAge)}
	p.timer = time.AfterFunc(exportAge, func() { cancel(errExportDeadline) })
	stop := func(cause error) {
		p.timer.Stop()
		cancel(cause)
	}
	return context.WithValue(ctx, exportProgressKey{}, p), p, stop
}

func exportProgressFrom(ctx context.Context) *exportProgress {
	p, _ := ctx.Value(exportProgressKey{}).(*exportProgress)
	return p
}

func (p *exportProgress) setStage(stage string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.stage = stage
	p.mu.Unlock()
}

// planFrames records the video's frame count, extends the deadline to
// exportDeadlineFor(frames) from the job start (never shortening it) and
// returns the time left, the render's own deadline.
func (p *exportProgress) planFrames(frames int) time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.framesTotal = frames
	if deadline := p.started.Add(exportDeadlineFor(frames)); deadline.After(p.deadline) {
		p.deadline = deadline
		p.timer.Reset(time.Until(deadline))
	}
	return max(time.Until(p.deadline), time.Second)
}

// frame is videoexport's per-frame callback.
func (p *exportProgress) frame(done, total int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.framesDone, p.framesTotal = done, total
	p.mu.Unlock()
}

// exportProgressView is the job-status projection of a job's progress.
type exportProgressView struct {
	Stage       string `json:"stage,omitempty"`
	FramesDone  int    `json:"framesDone"`
	FramesTotal int    `json:"framesTotal"`
	// Fraction is framesDone/framesTotal while rendering, 1 when complete,
	// and absent when there is no frame count yet.
	Fraction *float64  `json:"fraction,omitempty"`
	Deadline time.Time `json:"deadline"`
}

func (p *exportProgress) view() exportProgressView {
	if p == nil {
		return exportProgressView{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	v := exportProgressView{Stage: p.stage, FramesDone: p.framesDone, FramesTotal: p.framesTotal, Deadline: p.deadline.UTC()}
	if p.framesTotal > 0 {
		f := math.Round(float64(p.framesDone)/float64(p.framesTotal)*1000) / 1000
		v.Fraction = &f
	}
	return v
}

// Label is the plain-text progress line of a running job.
func (v exportProgressView) Label() string {
	if v.Stage == "" {
		return ""
	}
	label := strings.ToUpper(v.Stage[:1]) + v.Stage[1:]
	if v.FramesTotal > 0 {
		label += fmt.Sprintf(" · %s of %s frames", exportCount(v.FramesDone), exportCount(v.FramesTotal))
		if v.Fraction != nil {
			label += fmt.Sprintf(" (%.1f%%)", *v.Fraction*100)
		}
	}
	return label
}

// exportCount formats n with thousands separators.
func exportCount(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// newExportProgressCLI is newExportProgress for the CLI, whose stop takes no
// cause.
func newExportProgressCLI(parent context.Context) (context.Context, *exportProgress, func()) {
	ctx, p, stop := newExportProgress(parent)
	return ctx, p, func() { stop(context.Canceled) }
}
