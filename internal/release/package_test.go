package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// unpackForTest extracts a package the way the runtime's installer does, so the
// test exercises the real boundary rather than a digest computed in place.
func unpackForTest(t *testing.T, data []byte, dest string) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("package is not gzip: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("read package: %v", err)
		}
		target := filepath.Join(dest, filepath.FromSlash(hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			out, err := os.Create(target)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.CopyN(out, tr, hdr.Size); err != nil {
				t.Fatal(err)
			}
			out.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected entry type %d for %s", hdr.Typeflag, hdr.Name)
		}
	}
}

// A staged release packaged and then extracted must still verify: the digest the
// producer published is the digest a receiver recomputes from the bytes.
func TestPackageRoundTripsToTheSameReleaseDigest(t *testing.T) {
	f := newFixture(t, "one")
	meta := f.stage(t, "env-1")
	staged, err := f.manager().Dir(f.name, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	packaged, artifact, err := Package(staged, &buf)
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	if packaged.Digest != meta.Digest {
		t.Errorf("package carries digest %s, staged %s", packaged.Digest, meta.Digest)
	}

	// The artifact hash is a property of the bytes the encoder produced, and it
	// is SEPARATE from the release identity.
	sum := sha256.Sum256(buf.Bytes())
	if want := "sha256:" + hex.EncodeToString(sum[:]); artifact != want {
		t.Errorf("artifact hash = %s, want %s", artifact, want)
	}
	if artifact == packaged.Digest {
		t.Fatal("artifact hash and release digest are the same value; they are different things")
	}

	extracted := filepath.Join(t.TempDir(), "extracted")
	if err := os.MkdirAll(extracted, 0o755); err != nil {
		t.Fatal(err)
	}
	unpackForTest(t, buf.Bytes(), extracted)

	verified, err := VerifyPackage(extracted)
	if err != nil {
		t.Fatalf("a packaged release must verify after extraction: %v", err)
	}
	if verified.Digest != meta.Digest {
		t.Errorf("verified digest %s, staged %s", verified.Digest, meta.Digest)
	}
}

// Unchanged content must produce byte-identical archives, or a re-upload would
// look like new content and idempotency would be lost.
func TestPackageIsDeterministic(t *testing.T) {
	f := newFixture(t, "one")
	meta := f.stage(t, "env-1")
	staged, err := f.manager().Dir(f.name, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	var first, second bytes.Buffer
	if _, _, err := Package(staged, &first); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Package(staged, &second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("packaging the same release twice produced different bytes")
	}
}
