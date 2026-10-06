package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestFetchStoresAVerifiedReleaseUnderItsDigest(t *testing.T) {
	content := []byte("a release tarball")
	digest := digestOf(content)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()
	dir := t.TempDir()

	f := &Fetcher{Dir: dir}
	if err := f.Fetch(context.Background(), Release{Digest: digest, URL: srv.URL}); err != nil {
		t.Fatalf("a matching digest must succeed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, digest))
	if err != nil {
		t.Fatalf("release not stored under its digest: %v", err)
	}
	if string(got) != string(content) {
		t.Error("stored content differs from what was served")
	}
}

// Content-addressing is what stops a compromised control plane substituting
// release content. A mismatch must be its own error, because it is the one
// outcome that must not be retried blindly or logged as transient.
func TestFetchRefusesContentThatDoesNotMatchTheDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("substituted content"))
	}))
	defer srv.Close()
	dir := t.TempDir()

	f := &Fetcher{Dir: dir}
	err := f.Fetch(context.Background(), Release{Digest: digestOf([]byte("what was asked for")), URL: srv.URL})
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}
	// Nothing may be left at the digest path: a partial or wrong file there would
	// look like a release to activation.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("a failed fetch left %q behind", e.Name())
	}
}

// "sha256:<hex>" and a bare hex digest name the same content, so a caller must
// not be able to pass one form and compare unequal to the other.
func TestFetchAcceptsBothDigestForms(t *testing.T) {
	content := []byte("x")
	digest := digestOf(content)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	for _, form := range []string{digest, "sha256:" + digest, "SHA256:" + digest} {
		dir := t.TempDir()
		f := &Fetcher{Dir: dir}
		if err := f.Fetch(context.Background(), Release{Digest: form, URL: srv.URL}); err != nil {
			t.Errorf("digest form %q refused: %v", form, err)
		}
	}
}

// A release already present is not downloaded again. That is what makes retrying
// a whole operation cheap, and it is only sound because the name is the content.
func TestFetchSkipsAnAlreadyPresentRelease(t *testing.T) {
	var served bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		_, _ = w.Write([]byte("content"))
	}))
	defer srv.Close()
	content := []byte("content")
	digest := digestOf(content)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, digest), content, 0o644); err != nil {
		t.Fatal(err)
	}
	f := &Fetcher{Dir: dir}
	if err := f.Fetch(context.Background(), Release{Digest: digest, URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if served {
		t.Error("an already-present release was downloaded again")
	}
}

// A release with no digest cannot be verified, and unverifiable content must not
// be fetched at all rather than fetched and trusted.
func TestFetchRefusesAReleaseWithNoDigest(t *testing.T) {
	dir := t.TempDir()
	f := &Fetcher{Dir: dir}
	if err := f.Fetch(context.Background(), Release{URL: "http://example.invalid/r"}); err == nil {
		t.Fatal("expected a refusal")
	}
	for _, bad := range []string{"", "not-a-digest", "sha256:short", "sha256:" + string(make([]byte, 64))} {
		if _, err := normalizeDigest(bad); err == nil {
			t.Errorf("normalizeDigest accepted %q", bad)
		}
	}
}

// A download larger than the limit stops rather than filling the disk, because
// the control plane naming an enormous release must not be able to exhaust a
// tenant's volume.
func TestFetchEnforcesASizeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 4096))
	}))
	defer srv.Close()
	dir := t.TempDir()
	f := &Fetcher{Dir: dir, MaxBytes: 1024}
	if err := f.Fetch(context.Background(), Release{Digest: digestOf([]byte("x")), URL: srv.URL}); err == nil {
		t.Fatal("expected the size limit to refuse the download")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("an over-limit download left %d file(s) behind", len(entries))
	}
}
