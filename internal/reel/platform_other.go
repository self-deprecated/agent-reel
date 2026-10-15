//go:build !linux && !darwin

package reel

import (
	"fmt"
	"net"
	"os"
	"os/exec"
)

type ProcessIdentity struct {
	PID          int    `json:"pid"`
	Start        string `json:"start"`
	Boot         string `json:"boot"`
	PIDNamespace string `json:"pidNamespace,omitempty"`
}

func platformCheck() error {
	return fmt.Errorf("recording platform unsupported/unverified: Linux is the tested platform; macOS unverified")
}
func identify(int) (ProcessIdentity, error) { return ProcessIdentity{}, platformCheck() }
func owned(ProcessIdentity) bool            { return false }
func detach(*exec.Cmd)                      {}
func lockFile(*os.File) error               { return platformCheck() }
func unlockFile(*os.File)                   {}
func privateOwner(os.FileInfo) bool         { return false }
func peerSameUser(net.Conn) bool            { return false }

func currentProcView() (processView, error) { return processView{}, platformCheck() }
