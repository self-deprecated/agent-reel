package reel

// LiveCapability is immutable recorder provenance, not an authoritative URL or
// lifecycle control handle. It is optional and only belongs to this segment.
// Readers must check active state and revalidate socket ownership before use.
type LiveCapability struct {
	Version   int            `json:"version"`
	Recording string         `json:"recording"`
	Segment   int            `json:"segment"`
	Socket    recorderSocket `json:"socket"`
}

type recorderSocket struct {
	Process          ProcessIdentity `json:"process"`
	NetworkNamespace string          `json:"networkNamespace"`
	Inode            uint64          `json:"inode"`
	Port             uint16          `json:"port"`
}

type liveReporter interface{ Live() *LiveCapability }
