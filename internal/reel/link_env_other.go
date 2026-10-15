//go:build !linux && !darwin

package reel

// procSessionParentValues has no process environment source off Linux.
func procSessionParentValues(string, string) []string { return nil }
