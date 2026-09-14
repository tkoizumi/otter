//go:build !linux

package pyenv

// detectLibc is only meaningful on Linux. Other supported development targets
// report their own platform string through targetPlatform instead.
func detectLibc() string { return "native" }
