package seal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

var ErrNotEqual = errors.New("sealed source metadata is not equal")
var ErrTail = errors.New("source has a tail or unreadable committed counts")

// TailError retains the raw counts and the reason for doubt.
type TailError struct {
	Views, ActivityRows int64
	Reason              string
}

func (e *TailError) Error() string {
	return fmt.Sprintf("%v: %s (views=%d activityRows=%d)", ErrTail, e.Reason, e.Views, e.ActivityRows)
}
func (e *TailError) Is(target error) bool { return target == ErrTail }

type Removal struct {
	Proof                    *Verification
	RecordingDir, SealedPath string
	Limits                   visualstore.Limits
	ActivityLimits           activitystore.Limits
	DiscardTail              bool
}
type RemovalReport struct {
	Verified    string // "proof" or "fresh"
	Removed     bool
	FreedBytes  int64 // allocated filesystem bytes in the removed tree
	RenamedPath string
}

var idPattern = regexp.MustCompile("^[0-9a-f]{32}$")
var deletingPattern = regexp.MustCompile("^\\.([0-9a-f]{32})\\.deleting-[0-9a-f]{16}$")
var tempPattern = regexp.MustCompile("^\\.[A-Za-z0-9_-][A-Za-z0-9._-]*\\.areel\\.tmp-[0-9a-f]{16}$")
var salvagePattern = regexp.MustCompile("^\\.[A-Za-z0-9_-][A-Za-z0-9._-]*\\.areel\\.salvage-[0-9a-f]{16}$")

func IsTempName(name string) bool    { return tempPattern.MatchString(name) }
func IsSalvageName(name string) bool { return salvagePattern.MatchString(name) }
func IsDeletingName(name string) (string, bool) {
	m := deletingPattern.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}
func reservedName(name string) bool {
	_, ok := IsDeletingName(name)
	return ok || IsTempName(name) || IsSalvageName(name) || (strings.HasPrefix(name, ".") && strings.Contains(name, ".tmp-"))
}
func DeletingName(id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", refuse(errors.New("source name must be 32 lowercase hexadecimal digits"))
	}
	var token [8]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return "." + id + ".deleting-" + hex.EncodeToString(token[:]), nil
}
func owned(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
func privateDirectory(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode() != os.ModeDir|0700 || !owned(info) {
		return nil, refuse(fmt.Errorf("%s must be an owned 0700 directory", path))
	}
	return info, nil
}

// RemoveSource requires caller-established quiescence for the entire operation.
// It verifies the published path afresh unless a matching proof is supplied,
// compares exact owner records, and
// refuses any tail or doubt unless explicitly discarded. It never removes or
// modifies SealedPath. The rename and parent fsync precede the first unlink.
func RemoveSource(ctx context.Context, in Removal) (out RemovalReport, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if err = in.Limits.Validate(); err != nil {
		return
	}
	if err = in.ActivityLimits.Validate(); err != nil {
		return
	}
	if in.RecordingDir == "" || in.SealedPath == "" {
		return out, refuse(errors.New("source and sealed paths are required"))
	}
	dir, err := filepath.Abs(in.RecordingDir)
	if err != nil {
		return out, err
	}
	if !idPattern.MatchString(filepath.Base(dir)) {
		return out, refuse(errors.New("source name must be 32 lowercase hexadecimal digits"))
	}
	info, err := privateDirectory(dir)
	if err != nil {
		return out, fmt.Errorf("source: %w", err)
	}
	parent := filepath.Dir(dir)
	if _, err = privateDirectory(parent); err != nil {
		return
	}
	// Resolve ancestors before containment checking; refuse symlink source roots.
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return out, err
	}
	sealed, err := filepath.Abs(in.SealedPath)
	if err != nil {
		return out, err
	}
	realSealed, err := filepath.EvalSymlinks(sealed)
	if err != nil {
		return out, err
	}
	rel, err := filepath.Rel(realDir, realSealed)
	if err != nil {
		return out, err
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return out, refuse(errors.New("sealed path must be outside source"))
	}
	sealInfo, err := os.Lstat(sealed)
	if err != nil {
		return out, err
	}
	var rep Report
	if proof := in.Proof; proof != nil && proof.Path == sealed && proof.matches(sealInfo) && proof.Report.Status == StatusOK && proof.Report.Manifest.Recording == filepath.Base(dir) {
		rep = proof.Report
		out.Verified = "proof"
	} else {
		out.Verified = "fresh"
		rep, err = verify(ctx, sealed, in.Limits, in.ActivityLimits, false)
		if err != nil {
			return out, err
		}
	}
	if rep.Status != StatusOK {
		return out, fmt.Errorf("published seal %s: %s: %s", sealed, rep.FailedItem, rep.Reason)
	}
	f, err := Open(sealed)
	if err != nil {
		return out, err
	}
	defer f.Close()
	// The same publication must have supplied both the full verdict and envelope.
	now, err := os.Lstat(sealed)
	if err != nil {
		return out, err
	}
	if !os.SameFile(sealInfo, now) || sealInfo.Size() != now.Size() || !sealInfo.ModTime().Equal(now.ModTime()) {
		return out, refuse(errors.New("published seal changed"))
	}
	if !reflect.DeepEqual(rep.Manifest, f.Manifest) {
		return out, refuse(errors.New("published envelope changed after verification"))
	}
	if f.Manifest.Recording != filepath.Base(dir) {
		return out, fmt.Errorf("%w: recording identity differs from source name", ErrNotEqual)
	}
	src, err := decodeSource(f.SourceFiles, f.Kind())
	if err != nil {
		return out, err
	}
	if err = recheckSource(dir, src); err != nil {
		return out, fmt.Errorf("%w: %v", ErrNotEqual, err)
	}
	views, ve := visualstore.CommittedCounts(filepath.Join(dir, "visual"))
	var rows int64
	var ae error
	adir := filepath.Join(dir, activityDirName)
	if _, e := os.Lstat(adir); e == nil {
		rows, ae = activitystore.CommittedRows(adir)
	} else if !errors.Is(e, os.ErrNotExist) || !f.Manifest.Activity.Absent {
		ae = e
		if ae == nil {
			ae = errors.New("activity store missing")
		}
	}
	// Exact equality is intentional: a missing prefix is also doubt.
	if !in.DiscardTail && (ve != nil || ae != nil || views != rep.Manifest.Views.Views || rows != rep.Manifest.Activity.Rows) {
		return out, &TailError{Views: views, ActivityRows: rows, Reason: fmt.Sprintf("counts differ or cannot be read: visual=%v activity=%v", ve, ae)}
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		st, e := d.Info()
		if e != nil {
			return e
		}
		if stat, ok := st.Sys().(*syscall.Stat_t); ok {
			out.FreedBytes += stat.Blocks * 512
		}
		return nil
	})
	if err != nil {
		return
	}
	if err = sealStep(ctx, "remove_before_rename", 0); err != nil {
		return
	}
	// Recheck at the commit point as well as after verification.
	if err = recheckSource(dir, src); err != nil {
		return out, fmt.Errorf("%w: %v", ErrNotEqual, err)
	}
	now, err = os.Lstat(dir)
	if err != nil {
		return out, err
	}
	if !os.SameFile(info, now) {
		return out, refuse(errors.New("source directory changed"))
	}
	if err = ctx.Err(); err != nil {
		return
	}
	name, err := DeletingName(filepath.Base(dir))
	if err != nil {
		return out, err
	}
	renamed := filepath.Join(parent, name)
	// An atomic exclusive rename makes a random-name collision a refusal, never an
	// overwrite. Do not replace this with a check followed by os.Rename.
	if err = renameExclusive(dir, renamed); err != nil {
		return out, err
	}
	out.RenamedPath = renamed
	if err = syncFile(parent); err != nil {
		return
	}
	if err = sealStep(ctx, "remove_renamed", 0); err != nil {
		return
	}
	if err = removeTree(ctx, renamed); err != nil {
		return
	}
	if err = syncFile(parent); err != nil {
		return
	}
	out.Removed = true
	return
}

// removeTree exposes a per-entry interruption point for crash-safety tests.
// os.RemoveAll never follows symlinks. Every unlink occurs after the durable
// rename; an error leaves only the reserved directory, never a partial live one.
func removeTree(ctx context.Context, path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for i, entry := range entries {
		if err = sealStep(ctx, "remove_entry", int64(i)); err != nil {
			return err
		}
		if err = os.RemoveAll(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

// RemoveLeftover is a low-level cleanup primitive. The caller must first prove
// the holder absent and, for deleting directories, verify the corresponding
// published seal. It has no library locator or process-observation capability.
func RemoveLeftover(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	name := filepath.Base(absolute)
	_, deleting := IsDeletingName(name)
	directory := deleting || IsSalvageName(name)
	if !directory && !IsTempName(name) {
		return refuse(errors.New("not a reserved leftover name"))
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return err
	}
	if !owned(info) || (directory && (!info.IsDir() || info.Mode() != os.ModeDir|0700)) || (!directory && (!info.Mode().IsRegular() || info.Mode().Perm() != 0600)) {
		return refuse(errors.New("leftover must be owned and have its private reserved type"))
	}
	parent := filepath.Dir(absolute)
	if _, err = privateDirectory(parent); err != nil {
		return err
	}
	// A prior process may have died between rename and its fsync. Persist the
	// leftover name before deleting any of its children.
	if err = syncFile(parent); err != nil {
		return err
	}
	if directory {
		err = os.RemoveAll(absolute)
	} else {
		err = os.Remove(absolute)
	}
	if err != nil {
		return err
	}
	return syncFile(parent)
}
