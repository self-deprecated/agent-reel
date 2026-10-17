//go:build !linux && !darwin

package reel

import "context"

func (b *Import) Start(context.Context, Target, CaptureSpec) (Observer, error) {
	return nil, platformCheck()
}
func ImportFeed(string) error   { return platformCheck() }
func ImportRender(string) error { return platformCheck() }
