//go:build unix

package daemon

import "testing"

func TestDataDirectoryHasSingleOwner(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireDataLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireDataLock(dir); err == nil {
		t.Fatal("second daemon acquired the same data directory")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireDataLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = second.Close()
}
