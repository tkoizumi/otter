package release

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrDigestMismatch reports that a package's content does not produce the digest
// it claims.
//
// A distinct error rather than a formatted string so a caller can tell "this
// package is not what it says it is" from "this package is malformed": the first
// is an attack or corruption, the second is a producer bug, and they need
// different responses.
var ErrDigestMismatch = errors.New("release: digest mismatch")

// VerifiedPackage is the outcome of checking an extracted package.
type VerifiedPackage struct {
	Metadata Metadata
	// Digest is the digest RECOMPUTED from the bytes, which on success equals
	// Metadata.Digest.
	Digest string
}

// VerifyPackage recomputes a release's digest from its extracted content and
// refuses unless it matches what the package claims.
//
// THIS IS THE POINT OF THE WHOLE FORMAT. A directory name must be a verified
// content address, not an assertion. If this only compared names, an uploader
// could label arbitrary bytes with any digest and every downstream consumer --
// activation, the control plane's evidence rule, the `active` report -- would be
// trusting a claim.
//
// `root` is the extracted release root: the directory containing the job tree,
// any shared trees, and otter-release.json.
func VerifyPackage(root string) (VerifiedPackage, error) {
	meta, err := readMetadata(root)
	if err != nil {
		return VerifiedPackage{}, err
	}
	if meta.Digest == "" {
		// No claim at all. Refused rather than defaulted: a package that does not
		// say what it is cannot be verified, and treating it as "whatever the
		// content hashes to" would accept a package with no identity.
		return VerifiedPackage{}, fmt.Errorf("%w: the package records no digest", ErrInvalidPackage)
	}
	// A package staged before Layout was recorded cannot be verified, and
	// guessing the layout is the failure this field exists to prevent. Refused
	// with the reason, because "unsupported" alone would send someone looking for
	// a version flag.
	if strings.TrimSpace(meta.Layout.JobPath) == "" {
		return VerifiedPackage{}, fmt.Errorf(
			"%w: the package does not record its layout, so its digest cannot be recomputed; "+
				"rebuild the release with a version that records it",
			ErrInvalidPackage)
	}

	// Recompute over the RECORDED layout rather than one inferred from the
	// directory tree. Every input to the digest is then a fact the producer wrote
	// down, so a disagreement means the content differs and not that the verifier
	// guessed the shape wrong.
	jobDir, err := safeJoin(root, meta.Layout.JobPath)
	if err != nil {
		return VerifiedPackage{}, fmt.Errorf("%w: %s", ErrInvalidPackage, err)
	}
	if info, err := os.Stat(jobDir); err != nil || !info.IsDir() {
		return VerifiedPackage{}, fmt.Errorf(
			"%w: the package does not contain the job at %q", ErrInvalidPackage, meta.Layout.JobPath)
	}

	got, err := meta.Layout.Digest(jobDir, meta.Environment)
	if err != nil {
		return VerifiedPackage{}, fmt.Errorf("%w: recompute digest: %v", ErrInvalidPackage, err)
	}
	if got != meta.Digest {
		// BOTH digests are named. "digest mismatch" alone leaves an operator
		// unable to tell a corrupted transfer from a wrong manifest from a
		// producer bug, and every one of those has a different fix.
		return VerifiedPackage{}, fmt.Errorf(
			"%w: the package claims %s but its content hashes to %s",
			ErrDigestMismatch, meta.Digest, got)
	}
	return VerifiedPackage{Metadata: meta, Digest: got}, nil
}

// ErrInvalidPackage reports a package that cannot be checked at all: malformed,
// incomplete, or missing the facts verification needs.
var ErrInvalidPackage = errors.New("release: invalid package")

// InstallVerified places an already-verified release into the store under its
// RECOMPUTED digest.
//
// It takes a VerifiedPackage rather than a path and a digest, so there is no
// call that installs bytes under an unverified name. The type is the enforcement:
// the only way to obtain one is VerifyPackage.
//
// The move is atomic, and an existing release of the same digest is REUSED rather
// than replaced: the digest is the identity, so a present release with that name
// is already the right content, and rewriting it could disturb a running attempt
// that resolved through it.
func InstallVerified(dataDir string, root string, pkg VerifiedPackage) (Metadata, error) {
	m := Manager{DataDir: dataDir}
	dest, err := m.Dir(pkg.Metadata.Job, pkg.Digest)
	if err != nil {
		return Metadata{}, err
	}
	if _, err := os.Stat(dest); err == nil {
		// Already installed. Identity is content, so this is the same release.
		if existing, readErr := readMetadata(dest); readErr == nil {
			return existing, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Metadata{}, fmt.Errorf("install release: %w", err)
	}
	if err := os.Rename(root, dest); err != nil {
		// A rename across filesystems fails, and a release root may well be on a
		// different mount from a temp upload directory. Falling back to a copy
		// keeps installation working without weakening the verification, which
		// already happened.
		if copyErr := copyTree(root, dest, nil); copyErr != nil {
			return Metadata{}, fmt.Errorf("install release %s: %w", pkg.Digest[:12], copyErr)
		}
		if rmErr := os.RemoveAll(root); rmErr != nil {
			return Metadata{}, fmt.Errorf("install release %s: remove staging: %w", pkg.Digest[:12], rmErr)
		}
	}
	meta, err := readMetadata(dest)
	if err != nil {
		return Metadata{}, fmt.Errorf("install release %s: %w", pkg.Digest[:12], err)
	}
	return meta, nil
}
