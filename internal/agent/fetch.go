package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Fetch downloads a release and verifies it against the digest the control plane
// named.
//
// This is the step that makes a pull-based deploy safe rather than merely
// possible. The control plane can lie about *which* release is current -- that is
// a bounded capability the security review already records -- but it cannot
// substitute release *content*, because a release is content-addressed and this
// check is what enforces it. Skipping it would give that up while still looking
// like it worked, so a mismatch stops the deploy rather than warning.
//
// It is agent-side rather than an endpoint on the runtime: the agent has the URL
// and the digest, and it is the agent that must not be deceived. Asking the
// runtime to fetch and verify would move the check to the component being
// protected.
type Fetcher struct {
	// Dir is where releases are staged for the runtime to activate. The pilot
	// stages into the runtime's own release root so activation finds it.
	Dir string
	// HTTPClient is injectable for tests.
	HTTPClient *http.Client
	// MaxBytes guards against a hostile or broken control plane naming an
	// enormous release.
	MaxBytes int64
}

func (f *Fetcher) client() *http.Client {
	if f.HTTPClient != nil {
		return f.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

// ErrDigestMismatch means the bytes received are not the release that was asked
// for. It is never retried automatically: either the control plane is lying or
// the transfer is corrupt, and in both cases a human should look.
var ErrDigestMismatch = fmt.Errorf("agent: release digest mismatch")

// Fetch downloads to a temporary file, verifies, and only then makes it visible
// under its digest.
//
// The ordering matters: a partially-written or failing download must never be
// observable at the path activation reads, or a crash mid-download would leave
// something that looks like a release and is not.
func (f *Fetcher) Fetch(ctx context.Context, rel Release) error {
	if f.Dir == "" {
		return fmt.Errorf("agent: fetcher has no destination")
	}
	if rel.Digest == "" {
		return fmt.Errorf("agent: release has no digest; refusing to fetch unverifiable content")
	}
	want, err := normalizeDigest(rel.Digest)
	if err != nil {
		return err
	}
	final := filepath.Join(f.Dir, want)
	if _, err := os.Stat(final); err == nil {
		// Content-addressed: the digest IS the identity, so a present file with
		// the right name is the right content and the download is skipped. This
		// is what makes a retry of the whole operation cheap and idempotent.
		return nil
	}
	if rel.URL == "" {
		return fmt.Errorf("agent: release %s has no URL", shortDigest(want))
	}

	if err := os.MkdirAll(f.Dir, 0o755); err != nil {
		return fmt.Errorf("agent: fetch: %w", err)
	}
	tmp, err := os.CreateTemp(f.Dir, ".fetch-*")
	if err != nil {
		return fmt.Errorf("agent: fetch: %w", err)
	}
	tmpName := tmp.Name()
	// On any failure the partial file goes away, so nothing that looks like a
	// release is left behind.
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.URL, nil)
	if err != nil {
		return err
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: release download status %d", ErrRuntimeUnavailable, resp.StatusCode)
	}

	limit := f.MaxBytes
	if limit <= 0 {
		limit = 2 << 30 // 2 GiB: a release larger than this is not a release.
	}
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	if written > limit {
		return fmt.Errorf("agent: release exceeds the %d byte limit", limit)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("agent: fetch: %w", err)
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if got != want {
		// The one outcome that must not be retried blindly, and must not be
		// logged as a transient failure.
		return fmt.Errorf("%w: wanted %s, received %s", ErrDigestMismatch, shortDigest(want), shortDigest(got))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("agent: fetch: %w", err)
	}
	// Rename is atomic within a filesystem, so the release appears under its
	// digest complete or not at all.
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("agent: fetch: publish: %w", err)
	}
	return nil
}

// normalizeDigest accepts "sha256:<hex>" or a bare hex digest and returns the
// bare lowercase hex, so a caller cannot pass a form that compares unequal to the
// same content hashed locally.
//
// The prefix is matched case-insensitively: "SHA256:<hex>" is emitted by some
// tooling, and refusing it would be a compatibility failure rather than a
// security one. The hex itself is lowercased for the same reason -- the digest
// identifies content, and its case carries no information.
func normalizeDigest(d string) (string, error) {
	s := strings.TrimSpace(d)
	s = strings.ToLower(s)
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) != 64 {
		return "", fmt.Errorf("agent: %q is not a sha256 digest", d)
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return "", fmt.Errorf("agent: %q is not a sha256 digest", d)
		}
	}
	return s, nil
}

func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
