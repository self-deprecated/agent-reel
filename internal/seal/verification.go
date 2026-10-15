package seal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/self-deprecated/agent-reel/internal/activitystore"
	"github.com/self-deprecated/agent-reel/internal/visualstore"
)

// Verification binds a whole-file verdict to a path and file identity. Treat it
// as read-only. It is an in-process proof, not a serializable authorization.
type Verification struct {
	Path    string
	Report  Report
	Size    int64
	ModTime time.Time
	sys     os.FileInfo
}

func (v Verification) matches(info os.FileInfo) bool {
	return v.sys != nil && os.SameFile(v.sys, info) && v.Size == info.Size() && v.ModTime.Equal(info.ModTime())
}

// VerifyFile is Verify with a stable-file check. Damage still returns a report;
// a change of inode, size or modification time during verification is refused.
func VerifyFile(ctx context.Context, path string, limits visualstore.Limits) (Verification, error) {
	return verifyFile(ctx, path, limits, activitystore.Limits{Work: limits.Work}, false)
}

func verifyFile(ctx context.Context, path string, limits visualstore.Limits, al activitystore.Limits, building bool) (Verification, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return Verification{}, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return Verification{}, err
	}
	rep, err := verify(ctx, path, limits, al, building)
	proof := Verification{Path: path, Report: rep, Size: before.Size(), ModTime: before.ModTime(), sys: before}
	if err != nil {
		return Verification{Report: rep}, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return Verification{Report: rep}, err
	}
	if !proof.matches(after) {
		return Verification{Report: rep}, refuse(errors.New("published seal changed during verification"))
	}
	return proof, nil
}
