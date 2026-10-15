//go:build linux && !browser

package reel

import "testing"

func qualifySyntheticLiveBrowser(*testing.T, recorderSocket, []byte) {}
