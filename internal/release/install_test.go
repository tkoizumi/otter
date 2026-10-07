package release

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

// A package that cannot even be unpacked is the uploader's malformed input, so
// it must carry ErrInvalidPackage and surface as a 4xx. Reporting it as an
// internal error sends the operator to look at the server.
func TestInstallPackageRefusesUnreadableInput(t *testing.T) {
	_, err := InstallPackage(t.TempDir(), strings.NewReader("not a gzipped tar at all"))
	if !errors.Is(err, ErrInvalidPackage) {
		t.Fatalf("malformed input must be an invalid package, got: %v", err)
	}
}

// The install path end to end: package a staged release, install it into a
// different data directory, and confirm it landed under the digest the producer
// computed -- not under anything the archive's bytes happened to hash to.
func TestInstallPackageInstallsUnderTheProducerDigest(t *testing.T) {
	f := newFixture(t, "one")
	meta := f.stage(t, "env-1")
	staged, err := f.manager().Dir(f.name, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	packaged, artifact, err := Package(staged, &archive)
	if err != nil {
		t.Fatal(err)
	}
	if packaged.Digest != meta.Digest {
		t.Fatalf("packaged digest %s, staged %s", packaged.Digest, meta.Digest)
	}

	dataDir := t.TempDir()
	installed, err := InstallPackage(dataDir, bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if installed.Digest != meta.Digest {
		t.Errorf("installed digest %s, want %s", installed.Digest, meta.Digest)
	}
	if _, err := (Manager{DataDir: dataDir}).Metadata(f.name, meta.Digest); err != nil {
		t.Errorf("the release is not in the store at its identity: %v", err)
	}
	// The archive checksum is NOT the identity, and installing must not have
	// created a release named by it.
	checksum := strings.TrimPrefix(artifact, "sha256:")
	if checksum == meta.Digest {
		t.Fatal("the fixture produced a checksum equal to the identity; it proves nothing")
	}
	if _, err := (Manager{DataDir: dataDir}).Metadata(f.name, checksum); err == nil {
		t.Error("a release was installed under the transport checksum, which is not an identity")
	}
}

// An install of a release that is ALREADY in the store is a no-op, and a no-op
// must still clean up after itself.
//
// Cleanup used to be conditional on the install not having succeeded, and
// InstallVerified reports success when it finds the release already installed --
// so every idempotent re-install left its staging directory behind. A pooled
// tenant whose control plane published a stable desired state re-applied the same
// release every few seconds and accumulated 171 of them.
func TestInstallPackageLeavesNoStagingDirectoryWhenAlreadyInstalled(t *testing.T) {
	f := newFixture(t, "one")
	meta := f.stage(t, "env-1")
	staged, err := f.manager().Dir(f.name, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if _, _, err := Package(staged, &archive); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	// Twice: the second call takes the "already installed" path.
	for i := 1; i <= 2; i++ {
		if _, err := InstallPackage(dataDir, bytes.NewReader(archive.Bytes())); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".install-") {
			t.Errorf("staging directory %q survived the install", e.Name())
		}
	}
}
