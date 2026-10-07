package release

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyRelease makes a standalone copy of a staged release, standing in for the
// bytes that would travel through Cloud.
func copyRelease(t *testing.T, from, to string) {
	t.Helper()
	if err := copyTree(from, to, nil); err != nil {
		t.Fatalf("copy release: %v", err)
	}
}

// THE test this whole format exists for: a release that travelled as bytes still
// verifies, because the digest is recomputed from the content rather than trusted
// from the name.
func TestVerifyPackageAcceptsAReleaseThatTravelled(t *testing.T) {
	f := newFixture(t, "one")
	meta := f.stage(t, "env-1")
	if meta.Layout.JobPath == "" {
		t.Fatal("the staged metadata records no layout, so nothing downstream can verify it")
	}

	staged, err := f.manager().Dir(f.name, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	travelled := filepath.Join(t.TempDir(), "package")
	copyRelease(t, staged, travelled)

	got, err := VerifyPackage(travelled)
	if err != nil {
		t.Fatalf("a release that only travelled by copy must still verify: %v", err)
	}
	if got.Digest != meta.Digest {
		t.Errorf("recomputed %s, staged %s", got.Digest, meta.Digest)
	}
}

// THE assertion-vs-address case, and the only one that proves verification is
// real. Content is intact; the MANIFEST claims a different digest. A verifier
// that compared names would accept this.
func TestVerifyPackageRefusesAClaimedDigestTheContentDoesNotProduce(t *testing.T) {
	f := newFixture(t, "one")
	meta := f.stage(t, "env-1")
	staged, err := f.manager().Dir(f.name, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(t.TempDir(), "package")
	copyRelease(t, staged, pkg)

	// Rewrite the manifest's digest to something the content does not hash to.
	manifest := filepath.Join(pkg, ManifestFileName)
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["digest"] = strings.Repeat("a", 64)
	edited, _ := json.Marshal(m)
	if err := os.WriteFile(manifest, edited, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = VerifyPackage(pkg)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("a package whose content does not produce its claimed digest must be refused, got: %v", err)
	}
	// Both digests must be named, or an operator cannot tell a corrupted transfer
	// from a wrong manifest from a producer bug -- each with a different fix.
	if !strings.Contains(err.Error(), strings.Repeat("a", 64)) {
		t.Errorf("the refusal must name the CLAIMED digest: %v", err)
	}
}

// Altered CONTENT with an honest manifest is the other direction, and it must be
// caught too: this is what a corrupted or tampered transfer looks like.
func TestVerifyPackageRefusesAlteredContent(t *testing.T) {
	f := newFixture(t, "one")
	meta := f.stage(t, "env-1")
	staged, err := f.manager().Dir(f.name, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(t.TempDir(), "package")
	copyRelease(t, staged, pkg)

	// One byte changed inside the job's own code.
	if err := os.WriteFile(filepath.Join(pkg, meta.Layout.JobPath, "main.py"),
		[]byte("print('tampered')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := VerifyPackage(pkg); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("altered content must be refused, got: %v", err)
	}
}

// A package from before the layout was recorded cannot be verified, and the
// refusal must say WHY: guessing the layout is the failure the field prevents.
func TestVerifyPackageRefusesAPackageWithoutALayout(t *testing.T) {
	f := newFixture(t, "one")
	meta := f.stage(t, "env-1")
	staged, err := f.manager().Dir(f.name, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(t.TempDir(), "package")
	copyRelease(t, staged, pkg)

	manifest := filepath.Join(pkg, ManifestFileName)
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "layout")
	edited, _ := json.Marshal(m)
	if err := os.WriteFile(manifest, edited, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = VerifyPackage(pkg)
	if !errors.Is(err, ErrInvalidPackage) {
		t.Fatalf("a package with no layout must be refused as invalid, got: %v", err)
	}
	if !strings.Contains(err.Error(), "layout") {
		t.Errorf("the refusal must name the missing layout rather than being generic: %v", err)
	}
}

// The placement is part of the identity, so changing where the job sits inside an
// otherwise identical package must change the recomputed digest.
func TestVerifyPackageIsSensitiveToPlacement(t *testing.T) {
	f := newFixture(t, "one")
	meta := f.stage(t, "env-1")
	staged, err := f.manager().Dir(f.name, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(t.TempDir(), "package")
	copyRelease(t, staged, pkg)

	// Move the job to a different placement and update the manifest to match, so
	// everything is self-consistent EXCEPT the recorded digest.
	moved := "moved/one"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(pkg, moved)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(pkg, meta.Layout.JobPath), filepath.Join(pkg, moved)); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(pkg, ManifestFileName)
	raw, _ := os.ReadFile(manifest)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["job_path"] = moved
	layout, _ := m["layout"].(map[string]any)
	layout["JobPath"] = moved
	edited, _ := json.Marshal(m)
	_ = os.WriteFile(manifest, edited, 0o644)

	if _, err := VerifyPackage(pkg); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("moving the job must change the digest, got: %v", err)
	}
}

// Installation takes a VerifiedPackage, so there is no call that installs bytes
// under an unverified name. The type is the enforcement.
func TestInstallVerifiedPlacesTheReleaseUnderItsRecomputedDigest(t *testing.T) {
	f := newFixture(t, "one")
	meta := f.stage(t, "env-1")
	staged, err := f.manager().Dir(f.name, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(t.TempDir(), "package")
	copyRelease(t, staged, pkg)

	verified, err := VerifyPackage(pkg)
	if err != nil {
		t.Fatal(err)
	}

	other := Manager{DataDir: t.TempDir()}
	installed, err := InstallVerified(other.DataDir, pkg, verified)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if installed.Digest != meta.Digest {
		t.Errorf("installed digest %s, want %s", installed.Digest, meta.Digest)
	}
	dir, err := other.Dir(f.name, installed.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ManifestFileName)); err != nil {
		t.Errorf("the release is not in the store at its digest: %v", err)
	}
	// And it verifies AGAIN from where it landed, so installation did not change
	// the content it was verified against.
	if _, err := VerifyPackage(dir); err != nil {
		t.Errorf("the installed release no longer verifies: %v", err)
	}
}

// Reinstalling the same package is a no-op, not a rewrite: the digest is the
// identity, so a present release with that name is already the right content, and
// rewriting it could disturb a running attempt resolved through it.
func TestInstallVerifiedReusesAnExistingRelease(t *testing.T) {
	f := newFixture(t, "one")
	meta := f.stage(t, "env-1")
	staged, err := f.manager().Dir(f.name, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(t.TempDir(), "package")
	copyRelease(t, staged, pkg)
	verified, err := VerifyPackage(pkg)
	if err != nil {
		t.Fatal(err)
	}

	other := Manager{DataDir: t.TempDir()}
	first, err := InstallVerified(other.DataDir, pkg, verified)
	if err != nil {
		t.Fatal(err)
	}

	pkg2 := filepath.Join(t.TempDir(), "package2")
	copyRelease(t, staged, pkg2)
	v2, err := VerifyPackage(pkg2)
	if err != nil {
		t.Fatal(err)
	}
	second, err := InstallVerified(other.DataDir, pkg2, v2)
	if err != nil {
		t.Fatalf("reinstalling an identical release must succeed: %v", err)
	}
	if second.Digest != first.Digest {
		t.Errorf("reinstall produced a different digest: %s vs %s", second.Digest, first.Digest)
	}
}
