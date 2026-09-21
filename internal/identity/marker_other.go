//go:build !unix

package identity

import "os"

// openNoFollow has no O_NOFOLLOW equivalent here; ReadMarker still verifies
// the opened descriptor with SameFile, which is the strongest portable check.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY, 0)
}

// rejectHardLinked cannot inspect link counts portably.
func rejectHardLinked(os.FileInfo) error { return nil }

// syncDir is a no-op: directories are not fsync-able in the same way here.
func syncDir(string) error { return nil }
