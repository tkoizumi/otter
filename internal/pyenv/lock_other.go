//go:build !unix

package pyenv

import "errors"

type fileLock struct{}

func acquireLock(string) (*fileLock, error) {
	return nil, errors.New("managed Python preparation is unsupported on this platform")
}
func (*fileLock) Close() error { return nil }
