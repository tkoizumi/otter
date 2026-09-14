//go:build linux

package pyenv

import (
	"os"
	"strings"
)

// detectLibc distinguishes glibc from musl. The two cannot share a prepared
// interpreter, so the variant is part of an environment's identity rather than
// something inferred at execution time.
//
// The check is deliberately conservative: anything that does not look like musl
// is reported as glibc, which is the supported default for this release.
func detectLibc() string {
	if data, err := os.ReadFile("/proc/self/maps"); err == nil {
		if strings.Contains(string(data), "musl") {
			return "musl"
		}
	}
	if _, err := os.Stat("/lib/ld-musl-x86_64.so.1"); err == nil {
		return "musl"
	}
	if _, err := os.Stat("/lib/ld-musl-aarch64.so.1"); err == nil {
		return "musl"
	}
	return "glibc"
}
