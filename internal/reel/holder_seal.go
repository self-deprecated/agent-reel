package reel

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/seal"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// Tests may attach seal.WithProbe to the returned context. Production has no
// hook, environment switch, or fault-injection command.
var holderSealHook func(context.Context, string, string) context.Context

var lowerSealPriority = platformLowerSealPriority

// holderBeforeIndex runs after the seal lock is released and before the text
// index is built (tests only).
var holderBeforeIndex func(root string)

func holderSealContext(ctx context.Context, step, dir string) context.Context {
	if holderSealHook != nil {
		return holderSealHook(ctx, step, dir)
	}
	return ctx
}

func sealRootPrivacy(root string) error {
	st, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if st.Mode() != os.ModeDir|0700 || !privateOwner(st) {
		return fmt.Errorf("library root must be owned by the current user and 0700; fix ownership if needed, then chmod 700 %s", shellQuote(root))
	}
	return nil
}

func (m *machine) sealStopped(ctx context.Context, tpl string) {
	dir := filepath.Join(m.store.Root, "recordings", m.state.ID)
	var priorityErr error
	complete := func(line string) {
		if priorityErr != nil {
			line += "; lower priority: " + sealDiagnostic(priorityErr)
		}
		m.say(line)
	}
	fail := func(step string, err error) {
		complete("automatic seal " + step + ": " + sealDiagnostic(err) + "; run: areel recover " + m.state.ID)
	}
	if err := sealRootPrivacy(m.store.Root); err != nil {
		fail("root privacy", err)
		return
	}
	if tpl == "" {
		tpl = "{id}"
	}
	name, err := renderSourceSealName(tpl, m.state, dir)
	if err != nil {
		fail("name", err)
		return
	}
	// Linux scheduling priorities belong to the calling OS thread. Keep the
	// sealing worker on that thread until both verification and removal finish.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	priorityErr = lowerSealPriority()
	lock, err := acquireHolderSealLock(ctx, m.store.Root, m.say)
	if err != nil {
		fail("lock", err)
		return
	}
	// Released before the text index is built (an atomic build of its own),
	// so other holders' seals never queue behind indexing.
	release := sync.OnceFunc(func() { unlockFile(lock); _ = lock.Close() })
	defer release()
	ctx = holderSealContext(ctx, "before_seal", dir)
	dst := filepath.Join(m.store.Root, name+".areel")
	result, err := seal.Seal(ctx, seal.Source{RecordingDir: dir}, seal.Destination{Path: dst}, seal.Options{
		Limits: visualstore.DefaultLimits(), ActivityLimits: activitystore.DefaultLimits(), Sealer: "areel " + Version,
	})
	if err != nil {
		fail("build/verify", err)
		return
	}
	ctx = holderSealContext(ctx, "before_delete", dir)
	_, err = seal.RemoveSource(ctx, seal.Removal{RecordingDir: dir, SealedPath: dst, Proof: &result.Verification,
		Limits: visualstore.DefaultLimits(), ActivityLimits: activitystore.DefaultLimits()})
	if err != nil {
		fail("remove source", err)
		return
	}
	holderSealContext(ctx, "after_delete", dir)
	complete("automatic seal complete")
	release()
	if holderBeforeIndex != nil {
		holderBeforeIndex(m.store.Root)
	}
	// The text index is a derived cache: built here, still at the lowered
	// priority, and never a reason to report the seal as failed.
	if err := holderIndexAfterSeal(withSealName(ctx, tpl), m.store.Root, m.state.ID); err != nil {
		m.say("text index not built: " + sealDiagnostic(err) + "; run: areel index " + m.state.ID)
	}
}

// The persistent inode is shared by all holders in a library. Closing its file
// descriptor (including process death) releases flock; never unlink seal.lock.
func acquireHolderSealLock(ctx context.Context, root string, notify func(string)) (*os.File, error) {
	f, err := openHolderSealLock(filepath.Join(root, "seal.lock"))
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
		}
	}()
	retry := time.NewTicker(250 * time.Millisecond)
	defer retry.Stop()
	warning := time.NewTimer(2 * time.Second)
	defer warning.Stop()
	waited := false
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		ok, err = tryHolderSealLock(f)
		if err != nil {
			return nil, err
		}
		if ok {
			if waited {
				notify("automatic seal acquired library lock after waiting")
			}
			return f, nil
		}
		waited = true
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-warning.C:
			notify("automatic seal waiting for another seal in this library")
		case <-retry.C:
		}
	}
}

// renderSourceSealName supplies the store creation instant only when the
// template actually needs the date fallback. The renderer remains standalone.
func renderSourceSealName(tpl string, state State, dir string) (string, error) {
	if (strings.Contains(tpl, "{date}") || strings.Contains(tpl, "{time}")) &&
		(len(state.Segments) == 0 || state.Segments[0].Started.IsZero()) {
		r, e := visualstore.OpenReader(filepath.Join(dir, "visual"))
		if e != nil {
			return "", e
		}
		if e = r.Close(); e != nil {
			return "", e
		}
		u := url.URL{Scheme: "file", Path: filepath.Join(dir, "visual", visualstore.DatabaseName), RawQuery: "mode=ro"}
		db, e := sql.Open("sqlite", u.String())
		if e != nil {
			return "", e
		}
		defer db.Close()
		var created string
		if e = db.QueryRow("SELECT value FROM meta WHERE key='created' AND length(value)<=64").Scan(&created); e != nil {
			return "", e
		}
		state.created, e = time.Parse(time.RFC3339Nano, created)
		if e != nil {
			return "", e
		}
	}
	return renderSealName(tpl, state)
}
func sealDiagnostic(err error) string {
	text := strings.Join(strings.Fields(err.Error()), " ")
	if len(text) > 2048 {
		text = text[:2048] + " [truncated]"
	}
	return text
}
