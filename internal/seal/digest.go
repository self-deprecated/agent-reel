package seal

import (
	"bytes"
	"encoding/hex"
	"errors"
)

// ManifestDigest is the sealed file's authenticated manifest digest as
// "sha256:<hex>": the SHA-256 of the manifest that commits every part's
// counts, chains and digests. Open already verified it against the stored
// manifest; this re-reads the stored digest from the same immutable file.
// Callers use it to pin a sealed recording's exact content (for example a
// frozen EDL's sources).
func (f *File) ManifestDigest() (string, error) {
	var data string
	var sum []byte
	if err := f.db.QueryRow("SELECT manifest,digest FROM seal WHERE id=1").Scan(&data, &sum); err != nil {
		return "", err
	}
	if len(sum) != 32 || !bytes.Equal(digest([]byte(data)), sum) {
		return "", errors.New("seal: manifest digest mismatch")
	}
	return "sha256:" + hex.EncodeToString(sum), nil
}
