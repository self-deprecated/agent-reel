package reel

import (
	"fmt"

	"github.com/self-deprecated/agent-reel/internal/darwinproc"
)

func snapshotRecorderSocket(id ProcessIdentity) (recorderSocket, error) {
	if id.PID < 2 || !owned(id) {
		return recorderSocket{}, fmt.Errorf("live process provenance changed")
	}
	port, generation, e := darwinproc.Listener(id.PID)
	if e != nil {
		return recorderSocket{}, fmt.Errorf("owned live listener unavailable: %w", e)
	}
	if !owned(id) {
		return recorderSocket{}, fmt.Errorf("live process changed during observation")
	}
	// Inode carries the kernel's socket generation on Darwin. This detects
	// closing/reopening a listener even when its port and FD are reused.
	return recorderSocket{Process: id, NetworkNamespace: "darwin:host", Inode: generation, Port: port}, nil
}
func discoverRecorderSocket(id ProcessIdentity) (recorderSocket, error) {
	first, e := snapshotRecorderSocket(id)
	if e != nil {
		return recorderSocket{}, e
	}
	second, e := snapshotRecorderSocket(id)
	if e != nil {
		return recorderSocket{}, e
	}
	if first != second {
		return recorderSocket{}, fmt.Errorf("live socket changed during discovery")
	}
	return first, nil
}
func validateRecorderSocket(s recorderSocket) error {
	actual, e := discoverRecorderSocket(s.Process)
	if e != nil {
		return e
	}
	if actual != s {
		return fmt.Errorf("live socket ownership changed")
	}
	return nil
}
