package deploy

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uvCachePath decides where a downloaded uv is kept between deploys. These
// tests pin its fallback contract: a cache location that cannot actually be
// written to must quietly degrade to the deploy's scratch space, never fail the
// deploy and never be accepted only to fail later, mid-download.
//
// The read-only case is the one that used to be wrong. os.MkdirAll returns nil
// for an existing directory regardless of its mode, so an OTTER_UV_CACHE that
// already existed but was not writable was used as-is.

const (
	uvTestPlatform = "linux/amd64"
	uvTestVersion  = PinnedUVVersion
)

// uvTestBuilder returns a builder whose scratch directory is known, so a test
// can assert the exact fallback path instead of a prefix, and a cleanup that
// removes it.
func uvTestBuilder(t *testing.T) (*LocalBuilder, string) {
	t.Helper()
	b := NewLocalBuilder(io.Discard, io.Discard)
	t.Cleanup(b.Cleanup)
	dir, err := b.TempDir()
	if err != nil {
		t.Fatalf("create builder scratch dir: %v", err)
	}
	return b, dir
}

// uvTestArtifact is the per-version file uvCachePath must place under root.
func uvTestArtifact(root string) string {
	return filepath.Join(root, "uv-"+uvTestVersion+"-"+strings.ReplaceAll(uvTestPlatform, "/", "-"))
}

// An OTTER_UV_CACHE that already exists but is read-only must not be accepted:
// MkdirAll already succeeded, so the only way to notice is to probe a write.
// The fallback is the deploy's own scratch space, and no error is reported
// because a bad cache must never fail a deploy.
func TestUVCachePathFallsBackWhenEnvCacheIsReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		// Root bypasses the permission bits, so the probe would succeed and the
		// test would silently stop proving anything about the fallback.
		t.Skip("running as root: root can write to a 0555 directory, so this test cannot prove the fallback")
	}

	b, scratch := uvTestBuilder(t)
	readOnly := filepath.Join(t.TempDir(), "shared-uv-cache")
	if err := os.Mkdir(readOnly, 0o555); err != nil {
		t.Fatalf("create read-only cache: %v", err)
	}
	// Restore write permission before cleanup, or t.TempDir cannot prune the
	// parent of a directory it cannot empty.
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o755) })
	t.Setenv("OTTER_UV_CACHE", readOnly)

	got, err := b.uvCachePath(uvTestPlatform, uvTestVersion)
	if err != nil {
		t.Fatalf("uvCachePath with a read-only OTTER_UV_CACHE: %v, want the scratch fallback and no error", err)
	}
	if want := uvTestArtifact(filepath.Join(scratch, "uv-cache")); got != want {
		t.Errorf("uvCachePath with a read-only OTTER_UV_CACHE = %q, want the scratch fallback %q", got, want)
	}

	// The probe must clean up after itself; a rejected cache is left untouched.
	entries, err := os.ReadDir(readOnly)
	if err != nil {
		t.Fatalf("read read-only cache: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("read-only cache contains %d entries after the probe, want 0: %v", len(entries), entries)
	}
}

// A writable OTTER_UV_CACHE is the shared-cache path and must still be used
// as-is; falling back here would silently make every deploy re-download uv.
func TestUVCachePathUsesWritableEnvCache(t *testing.T) {
	b, scratch := uvTestBuilder(t)
	cache := t.TempDir()
	t.Setenv("OTTER_UV_CACHE", cache)

	got, err := b.uvCachePath(uvTestPlatform, uvTestVersion)
	if err != nil {
		t.Fatalf("uvCachePath with a writable OTTER_UV_CACHE: %v", err)
	}
	if want := uvTestArtifact(cache); got != want {
		t.Errorf("uvCachePath with a writable OTTER_UV_CACHE = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, scratch) {
		t.Errorf("uvCachePath with a writable OTTER_UV_CACHE fell back to scratch: %q", got)
	}
}

// OTTER_UV_CACHE may name a directory that does not exist yet, including
// missing parents; MkdirAll is expected to create the whole chain and the
// result to be used, not rejected and replaced by the fallback.
func TestUVCachePathCreatesMissingEnvCacheParent(t *testing.T) {
	b, _ := uvTestBuilder(t)
	cache := filepath.Join(t.TempDir(), "shared", "otter", "uv")
	t.Setenv("OTTER_UV_CACHE", cache)

	got, err := b.uvCachePath(uvTestPlatform, uvTestVersion)
	if err != nil {
		t.Fatalf("uvCachePath with a missing OTTER_UV_CACHE parent: %v", err)
	}
	if want := uvTestArtifact(cache); got != want {
		t.Errorf("uvCachePath with a missing OTTER_UV_CACHE parent = %q, want %q", got, want)
	}
	info, err := os.Stat(cache)
	if err != nil {
		t.Fatalf("stat created cache %s: %v", cache, err)
	}
	if !info.IsDir() {
		t.Errorf("%s is not a directory", cache)
	}
}

// The pre-existing MkdirAll-failure branch must survive: an OTTER_UV_CACHE that
// cannot be created at all (here its parent is a regular file) falls back to
// the deploy's scratch space rather than erroring.
func TestUVCachePathFallsBackWhenEnvCacheCannotBeCreated(t *testing.T) {
	b, scratch := uvTestBuilder(t)
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("a file, not a directory\n"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	t.Setenv("OTTER_UV_CACHE", filepath.Join(blocker, "uv-cache"))

	got, err := b.uvCachePath(uvTestPlatform, uvTestVersion)
	if err != nil {
		t.Fatalf("uvCachePath with an uncreatable OTTER_UV_CACHE: %v, want the scratch fallback and no error", err)
	}
	if want := uvTestArtifact(filepath.Join(scratch, "uv-cache")); got != want {
		t.Errorf("uvCachePath with an uncreatable OTTER_UV_CACHE = %q, want the scratch fallback %q", got, want)
	}
}
