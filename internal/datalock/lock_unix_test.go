//go:build unix

package datalock

import "testing"

func TestDataDirectoryHasSingleOwner(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir); err == nil {
		t.Fatal("a second owner acquired the same data directory")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = second.Close()
}
