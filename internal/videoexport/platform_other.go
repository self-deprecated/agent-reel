//go:build !linux && !darwin

package videoexport

import "os/exec"

func platformSupported() bool { return false }
func ownCommand(*exec.Cmd)    {}
