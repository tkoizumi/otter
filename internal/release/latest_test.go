package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// placeRelease publishes one release directory by hand, so a test can control
// the two timestamps Latest orders by without staging a whole job.
func placeRelease(t *testing.T, manager Manager, job, digest string, created, modified time.Time) {
	t.Helper()
	root, err := manager.Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	dir := filepath.Join(root, job, digest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeMetadata(dir, Metadata{Job: job, Digest: digest, CreatedAt: created}); err != nil {
		t.Fatalf("writeMetadata: %v", err)
	}
	if !modified.IsZero() {
		if err := os.Chtimes(dir, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
}

// "Latest" is creation time, not directory order and not the digest. The
// lexicographically greatest digest here is deliberately the OLDER release, so
// an implementation that fell back to the name would pick the wrong one.
func TestLatestPicksTheNewestReleaseByCreationTime(t *testing.T) {
	manager := Manager{DataDir: t.TempDir()}
	now := time.Now().UTC()
	older := strings.Repeat("f", 64)
	newer := strings.Repeat("0", 64)
	placeRelease(t, manager, "counter", older, now.Add(-2*time.Hour), time.Time{})
	placeRelease(t, manager, "counter", newer, now.Add(-1*time.Hour), time.Time{})

	got, ok, err := manager.Latest("counter")
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if got.Digest != newer {
		t.Errorf("Latest = %s, want the newer %s (older is %s)", got.Digest, newer, older)
	}
}

// Two releases created in the same instant fall back to the directory
// modification time, which is when the staged tree was moved into place.
func TestLatestBreaksACreationTieByDirectoryTime(t *testing.T) {
	manager := Manager{DataDir: t.TempDir()}
	created := time.Now().UTC()
	first := strings.Repeat("f", 64)
	second := strings.Repeat("0", 64)
	placeRelease(t, manager, "counter", first, created, created.Add(-1*time.Minute))
	placeRelease(t, manager, "counter", second, created, created.Add(time.Minute))

	got, ok, err := manager.Latest("counter")
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if got.Digest != second {
		t.Errorf("Latest = %s, want the one moved into place last, %s", got.Digest, second)
	}
}

// When both timestamps agree the answer is still deterministic: the greater
// digest wins, so the same store never reports two different "latest" releases.
func TestLatestIsDeterministicWhenTimestampsAgree(t *testing.T) {
	manager := Manager{DataDir: t.TempDir()}
	stamp := time.Now().UTC()
	low := strings.Repeat("0", 64)
	high := strings.Repeat("f", 64)
	placeRelease(t, manager, "counter", low, stamp, stamp)
	placeRelease(t, manager, "counter", high, stamp, stamp)

	got, ok, err := manager.Latest("counter")
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if got.Digest != high {
		t.Errorf("Latest = %s, want the greater digest %s", got.Digest, high)
	}
}

// A job with no release reports that plainly, so the caller can say
// "run otter release" rather than "the store is broken".
func TestLatestReportsAJobWithNoReleases(t *testing.T) {
	manager := Manager{DataDir: t.TempDir()}
	if _, ok, err := manager.Latest("counter"); err != nil || ok {
		t.Fatalf("Latest on an empty store = ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}
