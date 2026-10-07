package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/release"
)

// This is the integration test the release-identity contract specifies:
// build -> upload -> download -> install -> activate -> report success, plus the
// two refusal cases.
//
// It runs the REAL release staging, the REAL portable packager, the REAL
// verification and installation, and the REAL agent Fetch over HTTP. Only Cloud
// and the runtime's HTTP shell are test doubles; every step that decides identity
// is production code.
//
// The assertion that matters is NOT "it installed". It is that the digest the
// runtime installed under is the digest it RECOMPUTED from the bytes and equals
// the one the producer computed. A test that stopped at success would pass even
// if identity had been asserted rather than verified -- which is exactly the
// failure this design exists to prevent.

// cycleFixture builds a checkout-shaped workspace and stages one release,
// returning the release metadata and the release directory.
func cycleFixture(t *testing.T) (release.Metadata, string) {
	t.Helper()
	root, data := t.TempDir(), t.TempDir()
	source := filepath.Join(root, "jobs", "one")
	shared := filepath.Join(root, "lib")
	for _, dir := range []string{source, shared} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(source, "otter.yaml"),
		"version: 1\nname: one\nentrypoint: main.py\npython:\n  mode: managed\n  path:\n    - ../../lib\n")
	writeFile(filepath.Join(source, "main.py"), "print('hello')\n")
	writeFile(filepath.Join(shared, "shared_lib.py"), "VALUE = 1\n")

	layout, err := release.Plan(root, source, []release.SharedTree{{Source: shared}})
	if err != nil {
		t.Fatal(err)
	}
	manager := release.Manager{DataDir: data}
	meta, err := manager.StageWithLayout("one", source, layout, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := manager.Dir("one", meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	return meta, dir
}

// installRuntimeServer is an otterd-shaped HTTP runtime that performs the REAL
// verification and installation for every uploaded package.
func installRuntimeServer(t *testing.T, dataDir string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": "v-test"})
		case r.URL.Path == "/v1/runtime/maintenance":
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]any{"mode": "serving", "running": 0})
			default:
				w.WriteHeader(http.StatusOK)
			}
		case r.URL.Path == "/v1/runtime/releases/install" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			meta, err := release.InstallPackage(dataDir, bytes.NewReader(body))
			if err != nil {
				// A refusal names what went wrong, which for the two cases under
				// test is both digests.
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"job": meta.Job, "digest": meta.Digest, "recorded_digest": meta.Digest,
			})
		case r.URL.Path == "/v1/runtime/releases/activate" && r.Method == http.MethodPost:
			var body struct {
				Digest string `json:"digest"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if err := (release.Manager{DataDir: dataDir}).Activate("one", body.Digest); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/runtime/releases/active" && r.Method == http.MethodGet:
			active := []map[string]string{}
			if meta, ok, err := (release.Manager{DataDir: dataDir}).Active("one"); err == nil && ok {
				active = append(active, map[string]string{"job": meta.Job, "digest": meta.Digest})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"active": active})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
}

// cloudServer serves one package under one release identity, exactly as Cloud
// does after the identity fix: the URL is the identity, the bytes are the
// package, and the transport checksum travels in its own header.
func cloudServer(t *testing.T, releaseDigest string, archive []byte, artifact string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/agent/v1/releases/" + releaseDigest
		if r.URL.Path != want {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("x-otter-digest", releaseDigest)
		w.Header().Set("x-otter-artifact-sha256", artifact)
		_, _ = w.Write(archive)
	}))
}

// TestTheReleaseIdentityCycleInstallsActivatesAndReports is case 1: a valid
// package travels CLI -> Cloud -> runtime, the runtime recomputes the digest,
// installs under it, activates it, and the agent reports applied WITH that digest.
func TestTheReleaseIdentityCycleInstallsActivatesAndReports(t *testing.T) {
	meta, dir := cycleFixture(t)
	var archive bytes.Buffer
	packaged, artifact, err := release.Package(dir, &archive)
	if err != nil {
		t.Fatal(err)
	}
	if packaged.Digest != meta.Digest {
		t.Fatalf("packaged digest %s, staged %s", packaged.Digest, meta.Digest)
	}
	// The two values must be different things or this test proves nothing about
	// the separation.
	if artifact == meta.Digest {
		t.Fatal("the archive checksum equals the release digest; the fixture cannot detect conflation")
	}

	// Cloud speaks the canonical prefixed form; the store is keyed by bare hex.
	// The whole chain must agree on the identity across both spellings.
	canonical := "sha256:" + meta.Digest
	cloud := cloudServer(t, canonical, archive.Bytes(), artifact)
	defer cloud.Close()
	dataDir := t.TempDir()
	runtime := installRuntimeServer(t, dataDir)
	defer runtime.Close()

	rt := &RuntimeHTTP{BaseURL: runtime.URL, Token: "runtime-token", ReleaseClient: cloud.Client()}
	rep, err := Apply(context.Background(), rt,
		&Desired{Generation: 1, Release: Release{Digest: canonical, URL: cloud.URL + "/agent/v1/releases/" + canonical, Size: int64(archive.Len())}},
		0, Drain{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("apply returned an unexpected error: %v", err)
	}
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("outcome = %s, want applied: %s", rep.Outcome, rep.Reason)
	}
	if got := rep.Observed["release_digest"]; got != canonical {
		t.Errorf("reported digest %s, want %s", got, canonical)
	}

	// The runtime's own store must hold the release and be serving it, and the
	// digest must be the one the producer computed.
	manager := release.Manager{DataDir: dataDir}
	if _, err := manager.Metadata("one", meta.Digest); err != nil {
		t.Fatalf("the release was not installed under its canonical digest: %v", err)
	}
	active, ok, err := manager.Active("one")
	if err != nil || !ok {
		t.Fatalf("the release was not activated: ok=%v err=%v", ok, err)
	}
	if active.Digest != meta.Digest {
		t.Errorf("active digest %s, want %s", active.Digest, meta.Digest)
	}
}

// TestTheCycleRefusesAlteredContent is case 2: the CONTENT was altered after the
// digest was computed. The package still claims the original identity, and the
// runtime must refuse it, naming both digests.
func TestTheCycleRefusesAlteredContent(t *testing.T) {
	meta, dir := cycleFixture(t)
	// Package a tampered copy so the archive is internally consistent except for
	// the content itself.
	tampered := t.TempDir()
	copyTreeForTest(t, dir, tampered)
	if err := os.WriteFile(filepath.Join(tampered, "jobs", "one", "main.py"), []byte("print('tampered')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	_, artifact, err := release.Package(tampered, &archive)
	if err != nil {
		t.Fatal(err)
	}

	canonical := "sha256:" + meta.Digest
	cloud := cloudServer(t, canonical, archive.Bytes(), artifact)
	defer cloud.Close()
	dataDir := t.TempDir()
	runtime := installRuntimeServer(t, dataDir)
	defer runtime.Close()

	rt := &RuntimeHTTP{BaseURL: runtime.URL, Token: "runtime-token", ReleaseClient: cloud.Client()}
	rep, _ := Apply(context.Background(), rt,
		&Desired{Generation: 1, Release: Release{Digest: canonical, URL: cloud.URL + "/agent/v1/releases/" + canonical}},
		0, Drain{Timeout: 5 * time.Second})
	if rep.Outcome != OutcomeFailed {
		t.Fatalf("altered content must not apply; outcome = %s (%s)", rep.Outcome, rep.Reason)
	}
	if !strings.Contains(rep.Reason, meta.Digest) {
		t.Errorf("the refusal must name the claimed digest: %s", rep.Reason)
	}
	// Nothing may have been installed or activated.
	if _, ok, _ := (release.Manager{DataDir: dataDir}).Active("one"); ok {
		t.Error("a refused package must not have been activated")
	}
}

// TestTheCycleRefusesAManifestThatClaimsADifferentDigest is case 3, the
// assertion-vs-address case and the only one that proves verification is real.
// The content is intact; the MANIFEST claims a digest the content does not
// produce. A verifier that compared names would accept this.
func TestTheCycleRefusesAManifestThatClaimsADifferentDigest(t *testing.T) {
	meta, dir := cycleFixture(t)
	// The release digest travels bare (no "sha256:" prefix) inside the manifest,
	// matching what `otter release` writes.
	lied := strings.Repeat("a", 64)
	relabelled := t.TempDir()
	copyTreeForTest(t, dir, relabelled)

	manifestPath := filepath.Join(relabelled, release.ManifestFileName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["digest"] = lied
	edited, _ := json.Marshal(m)
	if err := os.WriteFile(manifestPath, edited, 0o644); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	packaged, artifact, err := release.Package(relabelled, &archive)
	if err != nil {
		t.Fatal(err)
	}
	if packaged.Digest != lied {
		t.Fatalf("the fixture manifest should claim %s, got %s", lied, packaged.Digest)
	}

	// Cloud is told to run the CLAIMED identity, so the runtime is asked to store
	// content under a name the content does not produce.
	canonical := "sha256:" + lied
	cloud := cloudServer(t, canonical, archive.Bytes(), artifact)
	defer cloud.Close()
	dataDir := t.TempDir()
	runtime := installRuntimeServer(t, dataDir)
	defer runtime.Close()

	rt := &RuntimeHTTP{BaseURL: runtime.URL, Token: "runtime-token", ReleaseClient: cloud.Client()}
	rep, _ := Apply(context.Background(), rt,
		&Desired{Generation: 1, Release: Release{Digest: canonical, URL: cloud.URL + "/agent/v1/releases/" + canonical}},
		0, Drain{Timeout: 5 * time.Second})
	if rep.Outcome != OutcomeFailed {
		t.Fatalf("a package whose manifest lies about its digest must not apply; outcome = %s (%s)", rep.Outcome, rep.Reason)
	}
	if !strings.Contains(rep.Reason, lied) {
		t.Errorf("the refusal must name the claimed digest: %s", rep.Reason)
	}
	if !strings.Contains(rep.Reason, meta.Digest) {
		t.Errorf("the refusal must name the digest the content actually produces (%s): %s", meta.Digest, rep.Reason)
	}
}

// copyTreeForTest copies a release directory faithfully enough for packaging.
func copyTreeForTest(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, info.Mode().Perm())
	})
	if err != nil {
		t.Fatalf("copy release: %v", err)
	}
}
