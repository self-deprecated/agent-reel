package seal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/archive"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

const activityDirName = "activity-index"
const activityAckName = "activity-index/acknowledged.json"

var sourceBounds = map[string]int64{
	"state.json": 4 << 20, "launch.json": 16 << 10,
	visualcapture.AcknowledgedName: visualcapture.MaxAcknowledgedBytes,
	activityAckName:                256,
}

type target struct {
	Backend  string `json:"backend"`
	Instance string `json:"instance"`
}
type state struct {
	ID       string `json:"id"`
	Target   target `json:"target"`
	Format   string `json:"format"`
	Status   string `json:"status"`
	Pending  string `json:"pending"`
	Segments []struct {
		Path string `json:"path"`
	} `json:"segments"`
}

func (s state) identity() archive.Identity {
	return archive.Identity{Recording: s.ID, Backend: s.Target.Backend, Instance: s.Target.Instance}
}

type sourceData struct {
	state           state
	files           map[string][]byte
	visual          visualcapture.Acknowledged
	activity        *activitystore.Expectation
	activityDir     string
	allocated       int64
	visualReport    visualstore.Report
	views           int64
	activityRows    int64
	rawViews        int64
	rawActivityRows int64
}

func readBounded(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > max {
		return nil, fmt.Errorf("%s is not a bounded regular file", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > max {
		return nil, fmt.Errorf("%s changed type or size", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, max)
	}
	return b, nil
}
func decodeSource(files map[string][]byte, kind Kind) (sourceData, error) {
	out := sourceData{files: files}
	if err := json.Unmarshal(files["state.json"], &out.state); err != nil {
		return out, err
	}
	s := out.state
	if s.ID == "" || s.Target.Backend == "" || s.Target.Instance == "" || s.Format != "visual/1" || !((kind == KindComplete && s.Status == "stopped" && s.Pending == "") || (kind == KindInterrupted && s.Status == "interrupted" && validPending(s.Pending))) {
		return out, errors.New("state must identify a visual/1 recording eligible for the requested kind")
	}
	for _, seg := range s.Segments {
		if seg.Path != visualcapture.StoreDirName {
			return out, errors.New("state segment path is not visual")
		}
	}
	var launch struct {
		Target target `json:"target"`
	}
	if err := json.Unmarshal(files["launch.json"], &launch); err != nil {
		return out, err
	}
	if launch.Target != s.Target {
		return out, errors.New("launch target differs from state")
	}
	var err error
	out.visual, err = visualcapture.ParseAcknowledged(files[visualcapture.AcknowledgedName])
	if err != nil {
		return out, err
	}
	if kind == KindComplete && !out.visual.Complete {
		return out, errors.New("visual acknowledgement is incomplete")
	}
	if data, ok := files[activityAckName]; ok {
		out.activity, err = activitystore.ParseAcknowledged(data)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}
func refuse(err error) error { return &RefusalError{Err: err} }
func eligibility(ctx context.Context, dir, dst string, opts Options) (sourceData, error) {
	var out sourceData
	if err := ctx.Err(); err != nil {
		return out, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return out, refuse(err)
	}
	if !info.IsDir() {
		return out, refuse(errors.New("source must be a recording directory"))
	}
	// Existing destinations have their own stable error, including symlinks.
	if _, err := os.Lstat(dst); err == nil {
		return out, ErrExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return out, refuse(err)
	}
	info, err = os.Lstat(filepath.Dir(dst))
	if err != nil {
		return out, refuse(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode() != os.ModeDir|0700 || !ok || int(stat.Uid) != os.Getuid() {
		return out, refuse(errors.New("destination parent must be an owned 0700 directory"))
	}
	// Output inside the source would violate the read-only source contract,
	// including when an ancestor of either explicit path is a symlink.
	sourceReal, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return out, refuse(err)
	}
	parentReal, err := filepath.EvalSymlinks(filepath.Dir(dst))
	if err != nil {
		return out, refuse(err)
	}
	relative, err := filepath.Rel(sourceReal, parentReal)
	if err != nil {
		return out, refuse(err)
	}
	if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return out, refuse(errors.New("destination must be outside the source recording directory"))
	}
	if _, err := visualcapture.LoadAcknowledged(filepath.Join(dir, visualcapture.AcknowledgedName)); err != nil {
		return out, refuse(err)
	}
	adir := filepath.Join(dir, activityDirName)
	hasActivity := false
	if info, err := os.Lstat(adir); err == nil {
		if !info.IsDir() {
			return out, refuse(errors.New("activity-index is not a directory"))
		}
		hasActivity = true
		if _, err := activitystore.LoadAcknowledged(adir); err != nil {
			return out, refuse(err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return out, refuse(err)
	}
	files := map[string][]byte{}
	for name, max := range sourceBounds {
		if name == activityAckName && !hasActivity {
			continue
		}
		b, err := readBounded(filepath.Join(dir, filepath.FromSlash(name)), max)
		if err != nil {
			return out, refuse(err)
		}
		files[name] = b
	}
	out, err = decodeSource(files, opts.Kind)
	if err != nil {
		return out, refuse(err)
	}
	vr, err := visualstore.OpenReader(filepath.Join(dir, visualcapture.StoreDirName))
	if err != nil {
		return out, refuse(err)
	}
	identity := vr.Identity()
	err = vr.Close()
	if err != nil {
		return out, err
	}
	if identity != out.state.identity() {
		return out, refuse(errors.New("visual identity differs from state"))
	}
	if opts.Kind == KindInterrupted {
		v, err := visualstore.Verify(ctx, filepath.Join(dir, visualcapture.StoreDirName), out.visual.Expectation(), opts.Limits)
		if err != nil {
			if operational(ctx, err) {
				return out, err
			}
			return out, refuse(err)
		}
		out.visualReport = v
		out.views = min(out.visual.Views, v.Prefix.Views)
		if v.Status == visualstore.StatusUnverified || out.views < 1 {
			return out, refuse(errors.New("no verified acknowledged visual prefix"))
		}
		chain, err := sourceBlockChain(ctx, filepath.Join(dir, visualcapture.StoreDirName), out.views)
		if err != nil {
			return out, refuse(fmt.Errorf("prefix must end a source block: %w", err))
		}
		if out.views == out.visual.Views && !bytes.Equal(chain, out.visual.Chain) {
			return out, refuse(errors.New("prefix chain differs from acknowledgement"))
		}
		out.rawViews, err = visualstore.CommittedCounts(filepath.Join(dir, visualcapture.StoreDirName))
		if err != nil || out.rawViews < out.views {
			return out, refuse(fmt.Errorf("cannot establish raw visual count: %v", err))
		}
	}
	if hasActivity {
		out.activityDir = adir
		ar, err := activitystore.OpenReader(adir)
		if err != nil {
			return out, refuse(err)
		}
		identity := ar.Identity()
		err = ar.Close()
		if err != nil {
			return out, err
		}
		if identity != out.state.identity() {
			return out, refuse(errors.New("activity identity differs from state"))
		}
		a, err := activitystore.Verify(ctx, adir, out.activity, opts.ActivityLimits)
		if err != nil {
			if operational(ctx, err) {
				return out, err
			}
			return out, refuse(err)
		}
		if opts.Kind == KindComplete && (a.Status != activitystore.StatusOK || a.Acknowledged != activitystore.AcknowledgedPresent || a.Prefix.Seq != out.activity.Seq) {
			return out, refuse(fmt.Errorf("activity source is not exactly acknowledged: %s %s %s", a.Status, a.FailedItem, a.Reason))
		}
		out.activityRows = min(out.activity.Seq, a.Prefix.Seq)
		if opts.Kind == KindInterrupted {
			if a.Status != activitystore.StatusOK && a.Status != activitystore.StatusDamaged {
				return out, refuse(errors.New("unverified activity source"))
			}
			out.rawActivityRows, err = activitystore.CommittedRows(adir)
			if err != nil || out.rawActivityRows < out.activityRows {
				return out, refuse(fmt.Errorf("cannot establish raw activity count: %v", err))
			}
		}
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			out.allocated += st.Blocks * 512
		}
		return nil
	})
	return out, err
}
func operational(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, visualstore.ErrLimit) || errors.Is(err, activitystore.ErrLimit)
}
func recheckSource(dir string, src sourceData) error {
	for name, want := range src.files {
		got, err := readBounded(filepath.Join(dir, filepath.FromSlash(name)), sourceBounds[name])
		if err != nil {
			return refuse(err)
		}
		if !bytes.Equal(got, want) {
			return refuse(fmt.Errorf("source file %s changed during seal", name))
		}
	}
	if src.activity == nil {
		if _, err := os.Lstat(filepath.Join(dir, activityDirName)); !errors.Is(err, os.ErrNotExist) {
			return refuse(errors.New("activity source appeared during seal"))
		}
	}
	return nil
}

func validPending(s string) bool { return s == "" || s == "pause" || s == "stop" }

// The doctor already authenticated the prefix; this asserts its source block
// boundary and obtains that exact chain without interpreting an interior view.
func sourceBlockChain(ctx context.Context, dir string, n int64) ([]byte, error) {
	u := url.URL{Scheme: "file", Path: filepath.Join(dir, visualstore.DatabaseName), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var chain []byte
	err = db.QueryRowContext(ctx, "SELECT last_chain FROM view_blocks WHERE last_seq=?", n).Scan(&chain)
	return chain, err
}
