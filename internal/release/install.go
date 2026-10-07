package release

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxPackageBytes bounds an uploaded release package, so a malformed or hostile
// upload cannot fill the disk before it is rejected for any other reason.
const maxPackageBytes = 512 << 20

// InstallPackage unpacks a portable release package, VERIFIES its digest against
// the bytes, and installs it into dataDir's release store.
//
// The order is the whole point. Extraction to a staging directory first, then
// VerifyPackage recomputing the digest over the extracted tree, and only then a
// move into place. An upload cannot land in the store under a name that was
// merely asserted:
//
//	a directory name must be a verified content address, not an assertion.
//
// It returns the metadata of the INSTALLED release, whose Digest is the digest
// recomputed from the bytes. A caller (the runtime's own HTTP handler, or the
// agent confirming what was installed) can compare that against the identity it
// was given; nothing here trusts the manifest's claim except as the value to
// check against.
func InstallPackage(dataDir string, r io.Reader) (Metadata, error) {
	staging, err := os.MkdirTemp(dataDir, ".install-*")
	if err != nil {
		return Metadata{}, fmt.Errorf("install release: %w", err)
	}
	// Removed unless the install MOVES the directory into place. A partially
	// unpacked package left in the data directory would be discovered by nothing
	// and cleaned up by no one.
	installed := false
	defer func() {
		if !installed {
			_ = os.RemoveAll(staging)
		}
	}()

	if err := UnpackPackage(io.LimitReader(r, maxPackageBytes), staging); err != nil {
		return Metadata{}, fmt.Errorf("install release: %w", err)
	}

	verified, err := VerifyPackage(staging)
	if err != nil {
		return Metadata{}, fmt.Errorf("install release: %w", err)
	}

	meta, err := InstallVerified(dataDir, staging, verified)
	if err != nil {
		return Metadata{}, fmt.Errorf("install release: %w", err)
	}
	installed = true
	return meta, nil
}

// UnpackPackage extracts a gzipped tar into dest.
//
// Every entry is checked before it is written. A release package is written by us
// but travels through Cloud and a network, so the unpacker assumes nothing: an
// absolute path, a `..` component, or a symlink escaping the tree would otherwise
// let a package write anywhere on the host -- including outside the release store
// it is about to be verified in.
func UnpackPackage(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("package is not a gzipped archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read package: %w", err)
		}
		target, err := safePackagePath(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			// Bounded by the header's own size, so a lying header cannot stream
			// more than it declared.
			if _, err := io.CopyN(out, tr, hdr.Size); err != nil {
				out.Close()
				return fmt.Errorf("write %s: %w", hdr.Name, err)
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Symlinks are REFUSED rather than recreated. The digest hashes a
			// symlink by its target text, so recreating a link whose target escapes
			// the package would let the content verified here read something else
			// when it runs.
			return fmt.Errorf("package contains a symlink (%s -> %s); refusing", hdr.Name, hdr.Linkname)
		default:
			// Devices, FIFOs and hard links have no place in a release and would
			// each need their own safety argument.
			return fmt.Errorf("package contains an unsupported entry %q (type %d)", hdr.Name, hdr.Typeflag)
		}
	}
}

// safePackagePath joins a package entry onto dest, refusing anything that would
// escape it.
func safePackagePath(dest, name string) (string, error) {
	if name == "" {
		return "", errors.New("package contains an entry with an empty name")
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("package entry %q is an absolute path", name)
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("package entry %q escapes the package root", name)
	}
	target := filepath.Join(dest, clean)
	// Belt and braces: the join itself must stay inside dest even if the checks
	// above are ever weakened.
	if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
		return "", fmt.Errorf("package entry %q escapes the package root", name)
	}
	return target, nil
}
