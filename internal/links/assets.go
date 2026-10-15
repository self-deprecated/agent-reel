package links

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// assetTypes are the only asset extensions and their served content types.
var assetTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".webp": "image/webp", ".gif": "image/gif",
}

// AssetExtension returns the lower-case, allowed extension of a source file.
func AssetExtension(src string) (string, error) {
	ext := strings.ToLower(filepath.Ext(src))
	if _, ok := assetTypes[ext]; !ok {
		return "", fmt.Errorf("screenshot must be .png, .jpg, .jpeg, .webp or .gif (got %q)", filepath.Ext(src))
	}
	return ext, nil
}

// ContentType returns the served content type of an asset ref.
func ContentType(asset string) string { return assetTypes[strings.ToLower(filepath.Ext(asset))] }

// ImportAsset copies src (a regular file, not a symlink, at most AssetMax)
// into <root>/assets/<parent>/<linkID><ext> and returns its ref and digest.
// An existing destination is refused, never overwritten.
func ImportAsset(root, parent, linkID, src string) (ref, digest string, err error) {
	if !ValidRecordingID(parent) || !ValidLinkID(linkID) {
		return "", "", fmt.Errorf("invalid asset identity")
	}
	ext, err := AssetExtension(src)
	if err != nil {
		return "", "", err
	}
	in, err := openNoFollow(src)
	if err != nil {
		return "", "", fmt.Errorf("screenshot: %w", err)
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return "", "", err
	}
	if !st.Mode().IsRegular() || st.Size() > AssetMax {
		return "", "", fmt.Errorf("screenshot must be a regular file of at most %d bytes", AssetMax)
	}
	if err = privateDir(root); err != nil {
		return "", "", err
	}
	dir := filepath.Join(root, AssetsDir)
	if err = ensureDir(dir); err != nil {
		return "", "", err
	}
	dir = filepath.Join(dir, parent)
	if err = ensureDir(dir); err != nil {
		return "", "", err
	}
	file := linkID + ext
	if !validAssetFile.MatchString(file) {
		return "", "", fmt.Errorf("invalid asset file name %q", file)
	}
	dst := filepath.Join(dir, file)
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, AssetMax+1))
	if err == nil && n > AssetMax {
		err = fmt.Errorf("screenshot exceeds %d bytes", AssetMax)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = syncDir(dir)
	}
	if err != nil {
		_ = os.Remove(dst)
		return "", "", err
	}
	return AssetRef(parent, file), "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// RemoveAsset deletes an imported asset file (used when a manifest write that
// would reference it fails).
func RemoveAsset(root, ref string) error {
	if !strings.HasPrefix(ref, AssetsDir+"/") || strings.Contains(ref, "..") {
		return fmt.Errorf("invalid asset ref")
	}
	return os.Remove(filepath.Join(root, filepath.FromSlash(ref)))
}

// ErrAssetDigest is returned when an asset's bytes no longer match.
var ErrAssetDigest = errors.New("asset digest mismatch")

// OpenAsset reads a screenshot link's asset for serving: private directories,
// a private regular file opened without following symlinks, bounded size and
// the manifest digest. It never reads a path ref.
func OpenAsset(root, parent string, l Link) (data []byte, contentType string, err error) {
	if l.Kind != KindScreenshot || l.Ref.Asset == "" || l.Digest == nil {
		return nil, "", fmt.Errorf("link %s has no servable asset", l.ID)
	}
	if err = l.Validate(parent); err != nil {
		return nil, "", err
	}
	for _, d := range []string{root, filepath.Join(root, AssetsDir), filepath.Join(root, AssetsDir, parent)} {
		if err = privateDir(d); err != nil {
			return nil, "", err
		}
	}
	data, err = readPrivate(filepath.Join(root, filepath.FromSlash(l.Ref.Asset)), AssetMax)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(sum[:]) != *l.Digest {
		return nil, "", ErrAssetDigest
	}
	return data, ContentType(l.Ref.Asset), nil
}

// CheckAsset reports "", "asset-missing" or "asset-digest-mismatch" for a
// screenshot link (it reads and hashes the file when verify is set, else only
// checks it exists).
func CheckAsset(root, parent string, l Link, verify bool) string {
	if l.Kind != KindScreenshot {
		return ""
	}
	if !verify {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(l.Ref.Asset))); err != nil {
			return "asset-missing"
		}
		return ""
	}
	if _, _, err := OpenAsset(root, parent, l); errors.Is(err, ErrAssetDigest) {
		return "asset-digest-mismatch"
	} else if err != nil {
		return "asset-missing"
	}
	return ""
}
