package reel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

// libraryConfig is the private LIBRARY/config.json: the configured backend and
// the optional storage warning thresholds (docs/recording.md). The file is
// user-authored, strict (unknown members are refused) and read by the CLI and
// the supervisor before a holder is spawned and by the holder itself when it
// starts, which pins the thresholds for that holder's logical recording.
type libraryConfig struct {
	Backend  string            `json:"backend,omitempty"`
	Warnings StorageThresholds `json:"warnings,omitempty"`
	// Format is accepted only for backward compatibility with files written
	// while visual/1 had to be selected: absent or activityFormatIndexed. Every
	// recording is visual/1 regardless; the legacy raw format has been removed,
	// so "" or any other value is a configuration error.
	Format *string `json:"format,omitempty"`
}

// StorageThresholds are warning thresholds only. Zero or absent disables a
// threshold; there is no default. Crossing one records a warning and notifies;
// it never stops, rotates, deletes or otherwise mutates a recording.
type StorageThresholds struct {
	// StoredBytes compares against the visual store's allocated bytes, which
	// span every segment of the logical recording.
	StoredBytes int64 `json:"storedBytes,omitempty"`
	// FootageSeconds compares against the visual store's durable source
	// footage of every segment (the recorder's own relative clock; not wall
	// time, never accepted-but-uncommitted footage).
	FootageSeconds float64 `json:"footageSeconds,omitempty"`
}

const (
	storageBytesMax   = int64(1) << 50     // 1 PiB
	storageSecondsMax = 10 * 366 * 86400.0 // ten years of footage
)

func (t StorageThresholds) validate() error {
	if t.StoredBytes < 0 || t.StoredBytes > storageBytesMax {
		return fmt.Errorf("warnings.storedBytes must be an integer between 0 (disabled) and %d", storageBytesMax)
	}
	if math.IsNaN(t.FootageSeconds) || math.IsInf(t.FootageSeconds, 0) || t.FootageSeconds < 0 || t.FootageSeconds > storageSecondsMax {
		return fmt.Errorf("warnings.footageSeconds must be a finite number between 0 (disabled) and %.0f", storageSecondsMax)
	}
	return nil
}
func (t StorageThresholds) enabled() bool { return t.StoredBytes > 0 || t.FootageSeconds > 0 }

// readLibraryConfig decodes the private config file strictly. A missing file
// is an empty configuration (no backend, every threshold disabled); an unsafe,
// oversized, malformed or out-of-bounds file is an error, never a default.
func readLibraryConfig(root string) (libraryConfig, error) {
	var config libraryConfig
	path := filepath.Join(root, "config.json")
	var raw json.RawMessage
	if e := readJSON(path, &raw); e != nil {
		if errors.Is(e, os.ErrNotExist) {
			return config, nil
		}
		return config, fmt.Errorf("library config: %w", e)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if e := decoder.Decode(&config); e != nil {
		return libraryConfig{}, fmt.Errorf("library config: %w", e)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return libraryConfig{}, fmt.Errorf("library config: trailing data")
	}
	if e := config.Warnings.validate(); e != nil {
		return libraryConfig{}, fmt.Errorf("library config: %w", e)
	}
	if config.Format != nil && *config.Format != activityFormatIndexed {
		return libraryConfig{}, fmt.Errorf("library config: format must be absent or %q (the legacy raw format has been removed)", activityFormatIndexed)
	}
	return config, nil
}
