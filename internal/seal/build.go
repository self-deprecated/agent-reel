package seal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/visualcapture"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// A per-operation test probe. A panic models interruption, retaining the
// private temporary file, never publishing it. There is no global hook.
type hookKey struct{}

func sealStep(ctx context.Context, part string, n int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if hook, ok := ctx.Value(hookKey{}).(func(string, int64) error); ok {
		if err := hook(part, n); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func transaction(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// Seal accepts explicit paths. It never opens the source for writing. The
// caller must keep the source quiescent; the embedded owner records are checked
// byte-for-byte again before publication. No file is created until structural
// eligibility succeeds; a source verification refusal removes the temporary file.
// Options must contain valid limits.
func Seal(ctx context.Context, source Source, destination Destination, opts Options) (result Result, err error) {
	if opts.Kind == "" {
		opts.Kind = KindComplete
	}
	if opts.Kind != KindComplete && opts.Kind != KindInterrupted {
		return result, refuse(errors.New("unknown seal kind"))
	}
	if opts.Kind == KindInterrupted && (opts.Recovery.SourceStatus == "" || !validPending(opts.Recovery.SourcePending) || opts.Recovery.RecoveredAt.IsZero() || opts.Recovery.Recoverer == "") {
		return result, refuse(errors.New("interrupted seal requires recovery provenance"))
	}
	started := time.Now()
	result.PeakRSSBeforeBytes = peakRSS()
	if err = opts.Limits.Validate(); err != nil {
		return
	}
	if err = opts.ActivityLimits.Validate(); err != nil {
		return
	}
	var srcDir, dst string
	srcDir, err = filepath.Abs(source.RecordingDir)
	if err != nil {
		return
	}
	dst, err = filepath.Abs(destination.Path)
	if err != nil {
		return
	}
	if source.RecordingDir == "" || destination.Path == "" {
		err = refuse(errors.New("source and destination paths are required"))
		return
	}
	// This namespace is reserved for interrupted builds, even for explicit out.
	if reservedName(filepath.Base(dst)) {
		err = refuse(errors.New("destination uses reserved temporary name"))
		return
	}
	var src sourceData
	src, err = eligibility(ctx, srcDir, dst, opts)
	if err != nil {
		return
	}
	if err = sealStep(ctx, "eligible", 0); err != nil {
		return
	}
	if opts.Sealer == "" {
		opts.Sealer = "unknown"
	}
	var token [8]byte
	if _, err = rand.Read(token[:]); err != nil {
		return
	}
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-"+hex.EncodeToString(token[:]))
	var db *sql.DB
	db, err = visualstore.CreateSealedFile(tmp)
	if err != nil {
		return
	}
	linked, success := false, false
	scratch := ""
	// Register first, so scratch cleanup runs after the temp-file cleanup even
	// during panic unwinding. Abrupt process death leaves reserved names.
	defer func() {
		if scratch != "" {
			err = errors.Join(err, os.RemoveAll(scratch))
			if err != nil {
				result = Result{}
			}
		}
	}()
	defer func() {
		// Preserve complete builds' existing panic-leftover behavior. Interrupted
		// builds also remove their temp file before the scratch cleanup runs.
		if p := recover(); p != nil {
			if db != nil {
				_ = db.Close()
			}
			if opts.Kind == KindInterrupted {
				if linked {
					_ = removeIfPresent(dst)
				}
				_ = removeIfPresent(tmp)
			}
			panic(p)
		}
		if db != nil {
			err = errors.Join(err, db.Close())
		}
		if !success {
			if e := ctx.Err(); e != nil {
				err = errors.Join(e, err)
			}
			if linked {
				err = errors.Join(err, removeIfPresent(dst))
			}
			err = errors.Join(err, removeIfPresent(tmp))
			if linked {
				err = errors.Join(err, syncFile(filepath.Dir(dst)))
			}
		}
		if err != nil {
			result = Result{}
		}
	}()
	if _, err = db.ExecContext(ctx, activitystore.SealedSchemaSQL+schemaSQL); err != nil {
		return
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	err = transaction(ctx, db, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "INSERT INTO meta VALUES('sealed',?),('sealer',?)", stamp, opts.Sealer)
		return e
	})
	if err != nil {
		return
	}
	visualDir := filepath.Join(srcDir, visualcapture.StoreDirName)
	if opts.Kind == KindInterrupted {
		candidate := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".salvage-"+hex.EncodeToString(token[:]))
		// Own the reserved container exclusively before registering cleanup. A
		// collision must never let cleanup delete somebody else's directory.
		if err = os.Mkdir(candidate, 0700); err != nil {
			return
		}
		scratch = candidate
		if err = sealStep(ctx, "salvage", 0); err != nil {
			return
		}
		var salvaged visualstore.SalvageReport
		salvaged, err = visualstore.SalvageWith(ctx, visualDir, filepath.Join(scratch, "visual"), opts.Limits, visualstore.SalvageOptions{MaxViews: src.views})
		if err != nil {
			return
		}
		if salvaged.Copied.Views != src.views {
			err = refuse(errors.New("salvage did not copy the exact block-boundary prefix"))
			return
		}
		visualDir = filepath.Join(scratch, "visual")
		if err = sealStep(ctx, "salvage", src.views); err != nil {
			return
		}
	}
	var visual visualstore.SealSummary
	visualOpts := visualstore.SealOptions{AllowIncomplete: opts.Kind == KindInterrupted,
		BeforeVerify: func() error { return sealStep(ctx, "source_verify", 0) }}
	if opts.Kind == KindComplete {
		visualOpts.Expectation = src.visual.Expectation()
		visualOpts.Report = &src.visualReport
	}
	visual, err = visualstore.SealVisualWith(ctx, visualDir, db, opts.Limits, visualOpts)
	if err != nil {
		if errors.Is(err, visualstore.ErrSealRefused) {
			err = refuse(err)
		}
		return
	}
	if opts.Kind == KindComplete {
		src.views = src.visualReport.Counts.Views
	}
	if err = sealStep(ctx, "visual", visual.Counts.Views); err != nil {
		return
	}
	var activity activitystore.SealSummary
	if opts.Kind == KindInterrupted {
		activity, err = activitystore.SealActivityPrefix(ctx, src.activityDir, db, opts.ActivityLimits, src.activityRows)
	} else {
		activity, err = activitystore.SealActivity(ctx, src.activityDir, db, opts.ActivityLimits)
	}
	if err != nil {
		return
	}
	if err = sealStep(ctx, "activity", activity.Rows); err != nil {
		return
	}
	err = transaction(ctx, db, func(tx *sql.Tx) error {
		names := make([]string, 0, len(src.files))
		for name := range src.files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO source_files VALUES(?,?)", name, src.files[name]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return
	}
	m := Manifest{Kind: opts.Kind, Schema: ManifestSchema, SealedAt: stamp, Sealer: opts.Sealer, SealBlockViews: visualstore.SealBlockViews, Source: SourceAccounting{AllocatedBytes: src.allocated}}
	for key, field := range map[string]*string{"recording": &m.Recording, "backend": &m.Backend, "instance": &m.Instance, "created": &m.Created, "source_format": &m.SourceFormat} {
		if err = db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", key).Scan(field); err != nil {
			return
		}
	}
	fillParts(&m, visual, activity, src)
	if opts.Kind == KindInterrupted {
		m.Recovery = &RecoveryPart{
			SourceStatus: opts.Recovery.SourceStatus, SourcePending: opts.Recovery.SourcePending,
			SourceVisualStatus: src.visualReport.Status, SourceFailedItem: src.visualReport.FailedItem, SourceReason: src.visualReport.Reason,
			SourceViews: src.rawViews, SourceActivityRows: src.rawActivityRows,
			DroppedViews: src.rawViews - visual.Counts.Views, DroppedActivityRows: src.rawActivityRows - activity.Rows,
			RecoveredAt: opts.Recovery.RecoveredAt.UTC().Format(time.RFC3339Nano), Recoverer: opts.Recovery.Recoverer, RecoveredFrom: opts.Recovery.RecoveredFrom,
		}
	}
	var data, sum []byte
	data, sum, err = manifestBytes(m)
	if err != nil {
		return
	}
	err = transaction(ctx, db, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "INSERT INTO seal VALUES(1,?,?)", string(data), sum)
		return e
	})
	if err != nil {
		return
	}
	err = db.Close()
	db = nil
	if err != nil {
		return
	}
	if err = syncFile(tmp); err != nil {
		return
	}
	if err = sealStep(ctx, "source_recheck", 0); err != nil {
		return
	}
	if err = recheckSource(srcDir, src); err != nil {
		return
	}
	var proof Verification
	proof, err = verifyFile(ctx, tmp, opts.Limits, opts.ActivityLimits, true)
	if err != nil {
		return
	}
	if proof.Report.Status != StatusOK {
		err = fmt.Errorf("independent seal verification: %s: %s", proof.Report.FailedItem, proof.Report.Reason)
		return
	}
	if scratch != "" {
		if err = os.RemoveAll(scratch); err != nil {
			return
		}
		scratch = ""
	}
	if err = sealStep(ctx, "publish", 0); err != nil {
		return
	}
	// Check the source again after independent verification, closing that longer
	// window as well. The caller still owns quiescence.
	if err = recheckSource(srcDir, src); err != nil {
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	if err = os.Link(tmp, dst); err != nil {
		if errors.Is(err, os.ErrExist) {
			err = fmt.Errorf("%w: %s", ErrExists, dst)
		}
		return
	}
	linked = true
	if err = sealStep(ctx, "linked", 0); err != nil {
		return
	}
	if err = visualstore.CheckSealedFile(dst); err != nil {
		return
	}
	var info os.FileInfo
	info, err = os.Lstat(dst)
	if err != nil {
		return
	}
	if !proof.matches(info) {
		err = refuse(errors.New("published seal changed after verification"))
		return
	}
	if err = os.Remove(tmp); err != nil {
		return
	}
	if err = syncFile(filepath.Dir(dst)); err != nil {
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	proof.Path, proof.sys = dst, info
	result.Verification = proof
	result.Path = dst
	result.Manifest = m
	result.FileSize = info.Size()
	result.SourceAllocatedBytes = src.allocated
	result.Elapsed = time.Since(started)
	result.PeakRSSAfterBytes = peakRSS()
	success = true
	return
}
func removeIfPresent(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
