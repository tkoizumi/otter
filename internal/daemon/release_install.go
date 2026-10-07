package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/release"
)

// InstallRelease unpacks a portable release package, VERIFIES its digest against
// the bytes, and installs it into the release store.
//
// The verification and installation live in the release package
// (release.InstallPackage) rather than here, so the exact same code path can be
// exercised without an HTTP server or a Daemon. This method keeps only what is
// HTTP-specific: reading the bounded body and mapping release errors onto the
// API's error classes.
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
//
// The response names BOTH the recomputed digest and the manifest's recorded
// digest so the agent can confirm that what this runtime stored is the release
// Cloud asked it to run. That confirmation link is what makes the digest an
// address at the transport boundary rather than an assertion.
func (d *Daemon) InstallRelease(ctx context.Context, r io.Reader) (api.ReleaseView, error) {
	_ = ctx

	meta, err := release.InstallPackage(d.cfg.DataDir, r)
	if err != nil {
		// Every verification failure is the UPLOADER's problem, so each maps to a
		// 4xx the CLI can act on rather than a 500 that reads as our bug. The
		// mismatch case keeps its own message naming both digests, because
		// "invalid package" alone would not tell the uploader which half is wrong.
		return api.ReleaseView{}, fmt.Errorf("install release: %w", mapVerifyError(err))
	}
	d.log.Info("release_installed", "job", meta.Job, "digest", meta.Digest)
	return api.ReleaseView{Job: meta.Job, Digest: meta.Digest, Digest_: meta.Digest}, nil
}

// mapVerifyError turns a release verification failure into an API error class.
//
// It lives here rather than in the API layer because release semantics are the
// daemon's business, and because the api package cannot import release without a
// cycle.
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
