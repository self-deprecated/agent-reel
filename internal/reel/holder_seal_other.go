//go:build !linux && !darwin

package reel

import "os"

func platformLowerSealPriority() error            { return nil }
func openHolderSealLock(string) (*os.File, error) { return nil, platformCheck() }
func tryHolderSealLock(*os.File) (bool, error)    { return false, platformCheck() }
