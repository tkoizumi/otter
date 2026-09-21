//go:build unix

package identity

import (
	"fmt"
	"os"
	"syscall"
)

// openNoFollow opens path without following a final symlink. Combined with the
// Lstat/SameFile check in ReadMarker this closes the swap window between the
// pathname validation and the read.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// rejectHardLinked refuses a marker with more than one link. A hard-linked
// marker means the same identity file is reachable from another path, which is
// exactly the aliasing this design must not silently accept.
func rejectHardLinked(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if st.Nlink > 1 {
		return fmt.Errorf("%w: marker has %d hard links", ErrUnsafeMarker, st.Nlink)
	}
	return nil
}

// syncDir flushes a directory entry change so a renamed marker survives a
// crash.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
