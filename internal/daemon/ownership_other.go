//go:build !unix

package daemon

import "errors"

type dataLock struct{}

func acquireDataLock(string) (*dataLock, error) {
	return nil, errors.New("data directory ownership is unsupported on this platform")
}
func (*dataLock) Close() error { return nil }
