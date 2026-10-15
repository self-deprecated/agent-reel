//go:build !linux && !darwin

package reel

import "context"

func Watch(string) error { return platformCheck() }
func (z *Zellij) Start(context.Context, Target, CaptureSpec) (Observer, error) {
	return nil, platformCheck()
}
