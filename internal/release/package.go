package release

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PackageFile is the portable form of a release: a gzipped tar of the release
// directory, paths relative to its root, carrying otter-release.json (the
// identity claim) and the bytes that back it (the evidence).
//
// IDENTITY TRAVELS ASIDE FROM THE ARCHIVE, and that separation is the point of
// this function. The release digest is the release's identity; the SHA-256 of
// the archive bytes is a transport checksum that changes whenever the encoder
// does. A producer publishes BOTH: the digest is what a runtime is asked to
// activate, and the archive hash is what a download verifies in transit. Rolling
// them into one value is exactly the confusion that made Cloud ask a runtime to
// activate a digest the runtime had never computed.
//
// The archive is DETERMINISTIC: fixed entry order, zeroed timestamps, ownership
// and names, so unchanged content produces byte-identical bytes and therefore the
// same artifact_sha256. That is what makes an upload idempotent without the
// uploader having to remember what it sent.
const PackageFile = "package.tar.gz"

// packagePrefix names the algorithm of the archive checksum, matching the
// canonical digest form used everywhere else.
const packageDigestPrefix = "sha256:"

// Package writes root as a portable release package and returns the release
// metadata it carries plus the canonical SHA-256 of the archive bytes.
//
// The returned digest is `Metadata.Digest` READ FROM THE MANIFEST, never
// recomputed here: recomputing is the INSTALLER's job (VerifyPackage), and a
// producer that recomputed its own identity would be verifying itself. This
// function serializes; it does not decide what the release is.
//
// The returned artifact hash IS computed here, because it is a property of the
// bytes this call produces. It is a checksum, not an identity -- the two are
// named differently on purpose.
func Package(root string, w io.Writer) (Metadata, string, error) {
	meta, err := readMetadata(root)
	if err != nil {
		return Metadata{}, "", err
	}
	if strings.TrimSpace(meta.Digest) == "" {
		return Metadata{}, "", fmt.Errorf("%w: the release records no digest", ErrInvalidPackage)
	}

	h := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(w, h))
	// A zero ModTime keeps the gzip header free of a wall-clock value; the OS
	// byte is pinned to 255 ("unknown") so two hosts agree byte for byte.
	gz.Header.ModTime = time.Time{}
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)

	if err := writePackageEntries(root, tw); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		return Metadata{}, "", err
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return Metadata{}, "", fmt.Errorf("close package: %w", err)
	}
	if err := gz.Close(); err != nil {
		return Metadata{}, "", fmt.Errorf("close package: %w", err)
	}
	return meta, packageDigestPrefix + hex.EncodeToString(h.Sum(nil)), nil
}

// writePackageEntries walks root in a fixed order and writes one tar entry per
// path. Directories are emitted so an empty directory survives the trip; they do
// not affect the digest, which only hashes files.
func writePackageEntries(root string, tw *tar.Writer) error {
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk release %s: %w", root, err)
	}
	sort.Strings(paths)

	for _, path := range paths {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:    name,
			Mode:    int64(info.Mode().Perm()),
			ModTime: time.Time{},
			Uid:     0,
			Gid:     0,
			Uname:   "",
			Gname:   "",
			Format:  tar.FormatPAX,
		}
		switch {
		case info.IsDir():
			hdr.Typeflag = tar.TypeDir
			hdr.Name = name + "/"
		case info.Mode()&os.ModeSymlink != 0:
			// Preserved as the link it is, not followed and not dereferenced: the
			// release digest hashes a symlink by its target, so the package has
			// to carry the target or verification cannot reproduce the digest.
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = target
		case info.Mode().IsRegular():
			hdr.Typeflag = tar.TypeReg
			hdr.Size = info.Size()
		default:
			return fmt.Errorf("release contains %s, which has no place in a package (mode %s)", name, info.Mode())
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write package header %s: %w", name, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		if _, err := io.Copy(tw, in); err != nil {
			in.Close()
			return fmt.Errorf("write package entry %s: %w", name, err)
		}
		if err := in.Close(); err != nil {
			return err
		}
	}
	return nil
}
