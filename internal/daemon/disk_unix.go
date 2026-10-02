//go:build unix

package daemon

import "syscall"

// diskSpace reports the free and total bytes of the filesystem holding path.
//
// Free is Bavail, not Bfree: a caller asking "can the daemon still write?"
// wants the blocks this process may actually use, which excludes the
// root-reserved blocks that Bfree includes.
func diskSpace(path string) (free, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), nil
}
