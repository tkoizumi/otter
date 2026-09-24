package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releaseArchive builds the tarball goreleaser publishes: both executables at
// the archive root.
func releaseArchive(t *testing.T, contents map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range contents {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// releaseServer serves one version's archive and checksums file, and reports a
// tampered checksum when asked.
func releaseServer(t *testing.T, version string, archive []byte, checksum string) *httptest.Server {
	t.Helper()
	asset := OtterArchiveName(version, "linux", "amd64")
	mux := http.NewServeMux()
	mux.HandleFunc("/v"+version+"/"+asset, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(archive)
	})
	mux.HandleFunc("/v"+version+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", checksum, asset)
	})
	return httptest.NewServer(mux)
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// A project that is not the runtime's source tree has nothing to compile, so
// the deploy takes the published archive for the platform SSH reported.
func TestReleaseBinariesFetchesAndVerifies(t *testing.T) {
	archive := releaseArchive(t, map[string]string{
		"otter":   "#!/bin/sh\necho otter\n",
		"otterd":  "#!/bin/sh\necho otterd\n",
		"LICENSE": "not a binary\n",
	})
	srv := releaseServer(t, "1.2.3", archive, sum(archive))
	defer srv.Close()

	binDir := t.TempDir()
	cache := t.TempDir()
	r := ReleaseBinaries{Version: "v1.2.3", BaseURL: srv.URL, CacheDir: cache, Stderr: os.Stderr}
	if err := r.Binaries(context.Background(), "linux", "amd64", binDir); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"otter", "otterd"} {
		data, err := os.ReadFile(filepath.Join(binDir, name))
		if err != nil {
			t.Fatalf("%s was not staged: %v", name, err)
		}
		if !strings.Contains(string(data), name) {
			t.Errorf("%s content = %q", name, data)
		}
		info, err := os.Stat(filepath.Join(binDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o100 == 0 {
			t.Errorf("%s is not executable: %v", name, info.Mode())
		}
	}
	if _, err := os.Stat(filepath.Join(binDir, "LICENSE")); err == nil {
		t.Error("an unrelated archive entry was extracted")
	}
}

// A cached pair is reused, so deploying to several hosts fetches once and a
// repeat deploy works with no network at all.
func TestReleaseBinariesReusesTheCache(t *testing.T) {
	archive := releaseArchive(t, map[string]string{"otter": "a", "otterd": "b"})
	srv := releaseServer(t, "1.2.3", archive, sum(archive))

	binDir := t.TempDir()
	cache := t.TempDir()
	r := ReleaseBinaries{Version: "1.2.3", BaseURL: srv.URL, CacheDir: cache}
	if err := r.Binaries(context.Background(), "linux", "amd64", binDir); err != nil {
		t.Fatal(err)
	}
	srv.Close()

	// The server is gone; only the cache can satisfy this.
	second := t.TempDir()
	if err := r.Binaries(context.Background(), "linux", "amd64", second); err != nil {
		t.Fatalf("a cached release was not reused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second, "otterd")); err != nil {
		t.Errorf("otterd was not staged from cache: %v", err)
	}
}

// A corrupted download must fail before anything is unpacked or pushed.
func TestReleaseBinariesRejectsChecksumMismatch(t *testing.T) {
	archive := releaseArchive(t, map[string]string{"otter": "a", "otterd": "b"})
	srv := releaseServer(t, "1.2.3", archive, strings.Repeat("0", 64))
	defer srv.Close()

	r := ReleaseBinaries{Version: "1.2.3", BaseURL: srv.URL, CacheDir: t.TempDir()}
	err := r.Binaries(context.Background(), "linux", "amd64", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("a tampered archive was accepted: %v", err)
	}
}

// A development build has no published archive, and saying so is far clearer
// than a 404 from the release host.
func TestReleaseBinariesRejectsUnreleasedVersions(t *testing.T) {
	r := ReleaseBinaries{Version: "v0.1.12-1-g22aee7c-dirty", CacheDir: t.TempDir()}
	err := r.Binaries(context.Background(), "linux", "amd64", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "--build") {
		t.Errorf("an unreleased version was not explained: %v", err)
	}
}

// An archive that does not actually contain both executables is not a runtime.
func TestReleaseBinariesRequiresBothExecutables(t *testing.T) {
	archive := releaseArchive(t, map[string]string{"otter": "a"})
	srv := releaseServer(t, "1.2.3", archive, sum(archive))
	defer srv.Close()

	r := ReleaseBinaries{Version: "1.2.3", BaseURL: srv.URL, CacheDir: t.TempDir()}
	err := r.Binaries(context.Background(), "linux", "amd64", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "otterd") {
		t.Errorf("an incomplete archive was accepted: %v", err)
	}
}

// --binaries is the offline path, and the platform is checked because pushing a
// macOS executable to a Linux host fails there with a bare exec format error.
func TestDirBinariesChecksThePlatform(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"otter", "otterd"} {
		if err := os.WriteFile(filepath.Join(dir, name), append([]byte{0x7f, 'E', 'L', 'F'}, 0, 0, 0, 0), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	err := DirBinaries{Dir: dir}.Binaries(context.Background(), "darwin", "arm64", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "host is darwin") {
		t.Errorf("a Linux binary aimed at a macOS host was accepted: %v", err)
	}

	binDir := t.TempDir()
	if err := (DirBinaries{Dir: dir}).Binaries(context.Background(), "linux", "amd64", binDir); err != nil {
		t.Fatalf("matching binaries were refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(binDir, "otterd")); err != nil {
		t.Errorf("otterd was not staged: %v", err)
	}
}

// A checkout's bin/ directory holds every platform at once, so --binaries must
// pick the suffixed pair that matches the target rather than the developer's
// own bare binary.
func TestDirBinariesPrefersThePlatformSuffixedPair(t *testing.T) {
	dir := t.TempDir()
	elf := []byte{0x7f, 'E', 'L', 'F', 0, 0, 0, 0}
	macho := []byte{0xcf, 0xfa, 0xed, 0xfe, 0, 0, 0, 0}
	files := map[string][]byte{
		"otter":              macho, // make build: this developer's macOS pair
		"otterd":             macho,
		"otter-linux-amd64":  elf, // make cross
		"otterd-linux-amd64": elf,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	binDir := t.TempDir()
	if err := (DirBinaries{Dir: dir}).Binaries(context.Background(), "linux", "amd64", binDir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"otter", "otterd"} {
		if got, ok := binaryOS(filepath.Join(binDir, name)); !ok || got != "linux" {
			t.Errorf("staged %s is %q, want the linux pair", name, got)
		}
	}
}
