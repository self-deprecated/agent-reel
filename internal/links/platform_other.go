//go:build !unix

package links

import (
	"errors"
	"os"
)

var errPlatform = errors.New("links need a unix platform")

func owned(os.FileInfo) bool                { return false }
func openNoFollow(string) (*os.File, error) { return nil, errPlatform }
func lock(string) (func(), error)           { return nil, errPlatform }
