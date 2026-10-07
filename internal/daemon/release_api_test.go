package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/tkoizumi/otter/internal/release"
)

// The control plane speaks `sha256:<hex>`; the release store is keyed by bare
// hex. Activation must NORMALISE rather than compare spellings: forwarding the
// canonical prefixed form unchanged produced "no job has release sha256:...",
// which reads as "the runtime never saw this release" when the runtime has it.
func TestActivateReleaseAcceptsThePrefixedCanonicalDigest(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)
	d := newGatedDaemon(t, root)
	ctx := context.Background()

	m := release.Manager{DataDir: d.cfg.DataDir}
	relRoot, err := m.Root()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(relRoot)
	if err != nil {
		t.Fatal(err)
	}
	job, digest := "", ""
	for _, e := range entries {
		if !e.IsDir() || e.Name() == release.ActiveDirName {
			continue
		}
		list, err := m.List(e.Name())
		if err != nil || len(list) == 0 {
			continue
		}
		job, digest = e.Name(), list[0].Digest
		break
	}
	if job == "" {
		t.Fatal("the fixture staged no release to activate")
	}

	view, err := d.ActivateRelease(ctx, "sha256:"+digest)
	if err != nil {
		t.Fatalf("a prefixed canonical digest must activate: %v", err)
	}
	if view.Digest != digest {
		t.Errorf("activated %s, want %s", view.Digest, digest)
	}
	active, ok, err := m.Active(job)
	if err != nil || !ok {
		t.Fatalf("no active release after activation: ok=%v err=%v", ok, err)
	}
	if active.Digest != digest {
		t.Errorf("active release %s, want %s", active.Digest, digest)
	}

	// A malformed digest is refused, not searched for: an unknown name and a
	// bad name must not both present as "not found".
	if _, err := d.ActivateRelease(ctx, "sha256:nothex"); err == nil {
		t.Error("a malformed digest must be refused")
	}
}
