//go:build !unix

package datalock

import "errors"

// LockFileName is the lock file inside the data directory.
const LockFileName = "otter.lock"

// Acquire fails on platforms without advisory file locking. Identity mutation
// requires exclusive ownership, so guessing here would be worse than refusing.
func Acquire(string) (*Lock, error) {
	return nil, errors.New("datalock: data directory ownership is unsupported on this platform")
}
