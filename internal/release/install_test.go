package release

import (
	"bytes"
	"errors"
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
