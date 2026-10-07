package daemon

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/release"
)

// maxPackageBytes bounds an uploaded release package, so a malformed or hostile
// upload cannot fill the disk before it is rejected for any other reason.
const maxPackageBytes = 512 << 20

// InstallRelease unpacks a portable release package, VERIFIES its digest against
// the bytes, and installs it into the release store.
//
// The order is the whole point. Extraction to a staging directory first, then
// release.VerifyPackage recomputing the digest over the extracted tree, and only
// then a move into place. An upload cannot land in the store under a name that
// was merely asserted:
//
//	a directory name must be a verified content address, not an assertion.
//
// Installation is deliberately NOT gated on maintenance, and the distinction from
// activation is the whole reason. Installing ADDS a release to the store under its
// verified digest; it does not change what the runtime serves, because the active
// release is a separate per-job link that only activation moves. Nothing a running
// attempt resolves through is touched.
//
// Gating it would also make the apply sequence worse, not safer: the sequence
// fetches and installs BEFORE entering maintenance precisely so a failed download
// does not gate a serving runtime. Requiring the gate here would invert that and
// gate first, which is the thing that design avoids.
func (d *Daemon) InstallRelease(ctx context.Context, r io.Reader) (api.ReleaseView, error) {
	_ = ctx

	staging, err := os.MkdirTemp(d.cfg.DataDir, ".install-*")
	if err != nil {
		return api.ReleaseView{}, fmt.Errorf("install release: %w", err)
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

	if err := unpackPackage(io.LimitReader(r, maxPackageBytes), staging); err != nil {
		return api.ReleaseView{}, fmt.Errorf("install release: %w", err)
	}

	verified, err := release.VerifyPackage(staging)
	if err != nil {
		// Every verification failure is the UPLOADER's problem, so each maps to a
		// 4xx the CLI can act on rather than a 500 that reads as our bug. The
		// mismatch case keeps its own message naming both digests, because
		// "invalid package" alone would not tell the uploader which half is wrong.
		return api.ReleaseView{}, fmt.Errorf("install release: %w", mapVerifyError(err))
	}

	meta, err := release.InstallVerified(d.cfg.DataDir, staging, verified)
	if err != nil {
		return api.ReleaseView{}, fmt.Errorf("install release: %w", err)
	}
	installed = true
	d.log.Info("release_installed", "job", meta.Job, "digest", meta.Digest)
	return api.ReleaseView{Job: meta.Job, Digest: meta.Digest, Digest_: meta.Digest}, nil
}

// unpackPackage extracts a gzipped tar into dest.
//
// Every entry is checked before it is written. A release package is written by us
// but travels through Cloud and a network, so the unpacker assumes nothing: an
// absolute path, a `..` component, or a symlink escaping the tree would otherwise
// let a package write anywhere on the host -- including outside the release store
// it is about to be verified in.
func unpackPackage(r io.Reader, dest string) error {
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

// mapVerifyError turns a release verification failure into an API error class.
//
// It lives here rather than in the API layer because release semantics are the
// daemon's business, and because the api package cannot import release without a
// cycle -- release already imports api for its error kinds.
//
// A digest mismatch is ErrInvalid rather than a new kind: from the uploader's
// point of view it IS an invalid package, and its message is what distinguishes
// "your bytes are wrong" from "this file is not a package at all".
func mapVerifyError(err error) error {
	switch {
	case errors.Is(err, release.ErrDigestMismatch):
		return fmt.Errorf("%w: %s", api.ErrInvalid, err.Error())
	case errors.Is(err, release.ErrInvalidPackage):
		return fmt.Errorf("%w: %s", api.ErrInvalid, err.Error())
	default:
		return err
	}
}
