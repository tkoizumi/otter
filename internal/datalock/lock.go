// Package datalock provides the exclusive advisory lock on a data directory.
//
// Exactly one writer may own a data directory at a time. The daemon holds this
// lock for its whole lifetime; a CLI that needs to mutate identity offline must
// acquire the same lock, so it can never become a second registry writer
// racing the daemon.
package datalock

import "errors"

// ErrLocked reports that another process already owns the data directory.
var ErrLocked = errors.New("datalock: data directory is already owned by another process")

// Lock is a held exclusive lock. Close releases it. A nil *Lock is a valid
// no-op so callers can defer unconditionally.
type Lock struct {
	release func() error
}

// Close releases the lock. It is safe to call more than once.
func (l *Lock) Close() error {
	if l == nil || l.release == nil {
		return nil
	}
	release := l.release
	l.release = nil
	return release()
}
