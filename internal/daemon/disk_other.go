//go:build !unix

package daemon

import "errors"

// diskSpace is unavailable on this platform. Otter ships for linux and darwin,
// where the unix file provides it; this exists so the rest of the build keeps
// compiling, and the health response simply leaves its disk fields zero.
func diskSpace(string) (free, total int64, err error) {
	return 0, 0, errors.New("disk space is not reported on this platform")
}
