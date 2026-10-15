package archive

import (
	"errors"
	"time"
)

const (
	BlockBytes    = 32 << 10
	MetadataBytes = 16 << 10
	MaxFrames     = 1_000_000
	MaxChunkBytes = 4 << 20
	DecoderMemory = 8 << 20
)

// Limits are the recorder-stream bounds. The visual/1 capture pipeline reads
// LineBytes; the rest describe a legacy archive header, which Verify checks.
type Limits struct {
	LineBytes  int           `json:"lineBytes"`
	ChunkBytes int           `json:"chunkBytes"`
	QueueBytes int           `json:"queueBytes"`
	FlushAge   time.Duration `json:"flushAgeNs"`
	StallAge   time.Duration `json:"stallAgeNs"`
}

func Defaults() Limits { return Limits{1 << 20, 1 << 20, 4 << 20, 2 * time.Second, 15 * time.Second} }
func (l Limits) valid() bool {
	return l.LineBytes > 0 && l.LineBytes <= l.ChunkBytes && l.ChunkBytes <= MaxChunkBytes && l.QueueBytes >= BlockBytes && l.QueueBytes <= 16<<20 && l.QueueBytes%BlockBytes == 0 && l.FlushAge > 0 && l.StallAge > l.FlushAge
}

// StorageError marks a failure of a sink's own durable storage: creating the
// store, publishing a commit or acknowledgement, or a backlog with no durable
// progress within the stall age. Stage is a bounded label (`create`,
// `publish`, `backlog`); Err is the underlying cause, unchanged, so errors.Is
// still matches ENOSPC/EDQUOT/EROFS and the message is unchanged. A readout,
// validation, input or recorder failure is never a StorageError.
type StorageError struct {
	Stage string
	Err   error
}

func (e *StorageError) Error() string { return e.Err.Error() }
func (e *StorageError) Unwrap() error { return e.Err }

// IsStorageFailure reports whether err carries a StorageError anywhere in its
// chain, including through errors.Join and %w wrapping by observers.
func IsStorageFailure(err error) bool {
	var s *StorageError
	return errors.As(err, &s)
}

// RecoveryLimits and RecoveryDefaults are the removed legacy doctor's archive
// budgets. No reader uses them any more; they remain only because the frozen
// V8 qualification harness still names their values in its findings.
type RecoveryLimits struct {
	InputBytes   int64 `json:"inputBytes"`
	DecodedBytes int64 `json:"decodedBytes"`
	OutputBytes  int64 `json:"outputBytes"`
	Entries      int   `json:"entries"`
	Work         int   `json:"work"`
}

func RecoveryDefaults() RecoveryLimits {
	return RecoveryLimits{64 << 20, 256 << 20, 64 << 20, 4096, 32768}
}
