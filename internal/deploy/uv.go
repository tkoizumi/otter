package deploy

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

// DefaultUVReleaseBase is where uv publishes its release archives.
const DefaultUVReleaseBase = "https://github.com/astral-sh/uv/releases/download"

// uvReleaseBase returns the release base URL, honouring OTTER_UV_BASE_URL so a
// host or CI without egress to GitHub can point at an internal mirror.
func uvReleaseBase() string {
	if base := strings.TrimSpace(os.Getenv("OTTER_UV_BASE_URL")); base != "" {
		return strings.TrimRight(base, "/")
	}
	return DefaultUVReleaseBase
}

// PinnedUVVersion is the uv shipped by `otter deploy`. It is pinned rather than
// "latest" because the uv version is part of a prepared environment's identity:
// changing it changes every environment digest and forces a rebuild, which
// should be a deliberate act rather than a side effect of deploying.
const PinnedUVVersion = "0.5.9"

// uvTarget maps a GOOS/GOARCH onto uv's release asset naming.
func uvTarget(platform string) (string, error) {
	goos, goarch, err := ParsePlatform(platform)
	if err != nil {
		return "", err
	}
	switch goos + "/" + goarch {
	case "linux/amd64":
		return "x86_64-unknown-linux-gnu", nil
	case "linux/arm64":
		return "aarch64-unknown-linux-gnu", nil
	case "darwin/amd64":
		return "x86_64-apple-darwin", nil
	case "darwin/arm64":
		return "aarch64-apple-darwin", nil
	default:
		return "", fmt.Errorf("uv has no published build for %s/%s", goos, goarch)
	}
}

// UVArchiveURL is the release archive for a target platform.
//
// uv's release assets are named for the platform but not the version, which
// makes the release tag the only thing distinguishing two builds.
func UVArchiveURL(platform, version string) (string, error) {
	target, err := uvTarget(platform)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s/uv-%s.tar.gz", uvReleaseBase(), version, target), nil
}

// cachePath is where a downloaded uv is kept between deploys, so deploying to
// several hosts does not download the same archive repeatedly.
func (b *LocalBuilder) uvCachePath(platform, version string) (string, error) {
	dir, err := b.TempDir()
	if err != nil {
		return "", err
	}
	// OTTER_UV_CACHE lets a shared cache be used, which matters when deploying
	// to several hosts from CI. Otherwise the per-user cache directory is used,
	// falling back to the deploy's own scratch space when it is unavailable.
	root := os.Getenv("OTTER_UV_CACHE")
	if root == "" {
		if userCache, err := os.UserCacheDir(); err == nil && userCache != "" {
			root = filepath.Join(userCache, "otter", "uv")
		} else {
			root = filepath.Join(dir, "uv-cache")
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		// A read-only or sandboxed cache location must not fail the deploy.
		root = filepath.Join(dir, "uv-cache")
		if err := os.MkdirAll(root, 0o755); err != nil {
			return "", err
		}
	}
	return filepath.Join(root, fmt.Sprintf("uv-%s-%s", version, strings.ReplaceAll(platform, "/", "-"))), nil
}

// VendorUV fetches the pinned uv binary for the target platform into outDir's
// tools directory, so the host needs no global uv installation.
//
// A cached copy is reused, and a download is written to a temporary file and
// renamed, so an interrupted fetch can never leave a truncated binary that
// would fail later in a confusing way.
func (b *LocalBuilder) VendorUV(ctx context.Context, cfg Config, outDir string) error {
	platform := cfg.Target.Platform
	version := PinnedUVVersion
	if cfg.UVVersion != "" {
		version = cfg.UVVersion
	}

	cached, err := b.uvCachePath(platform, version)
	if err != nil {
		return err
	}
	if info, err := os.Stat(cached); err != nil || info.Size() == 0 {
		url, err := UVArchiveURL(platform, version)
		if err != nil {
			return err
		}
		if err := b.fetchUV(ctx, url, cached); err != nil {
			return err
		}
	}

	bin := filepath.Join(outDir, "tools", "uv")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	return copyFile(cached, filepath.Join(bin, "uv"), 0o755)
}

// fetchUV downloads and extracts the uv archive into dest.
func (b *LocalBuilder) fetchUV(ctx context.Context, url, dest string) error {
	fmt.Fprintf(b.Stderr, "fetching %s\n", url)

	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download uv from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download uv from %s: unexpected status %s\n"+
			"hint: set OTTER_UV_BASE_URL to a mirror, install uv on the host and pass --no-uv, "+
			"or place it at <remote-dir>/tools/uv/uv", url, resp.Status)
	}

	// The archive is streamed to a temporary file, extracted, and the binary
	// is copied into place. Extraction in memory would not survive a corrupted
	// archive with a clear error.
	tmpDir, err := os.MkdirTemp(filepath.Dir(dest), ".uv-download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	archive := filepath.Join(tmpDir, "uv.tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(file, hasher), resp.Body); err != nil {
		file.Close()
		return fmt.Errorf("download uv: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}

	extracted, err := extractUVBinary(archive, tmpDir)
	if err != nil {
		return err
	}
	if err := copyFile(extracted, dest, 0o755); err != nil {
		return err
	}
	fmt.Fprintf(b.Stderr, "uv %s ready (sha256 %s)\n", PinnedUVVersion, hex.EncodeToString(hasher.Sum(nil))[:12])
	return nil
}
