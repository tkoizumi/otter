package deploy

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// This file is how a deploy gets its executables when the project being
// deployed is not the runtime's own source tree.
//
// A deploy target is a *project*: a directory with integrations in it. Most of
// them are Python, not Go, so `go build ./cmd/otterd` is not available and
// demanding a checkout of the runtime repository would make the split between
// the runtime and its integrations a fiction. The runtime is already published
// as a per-platform archive containing both binaries, so that is what a project
// deploy consumes. A Go checkout still builds from source (see LocalBuilder),
// and --binaries covers the offline case.

// DefaultOtterReleaseBase is where Otter publishes its release archives.
const DefaultOtterReleaseBase = "https://github.com/tkoizumi/otter/releases/download"

// otterReleaseBase returns the release base URL, honouring
// OTTER_RELEASE_BASE_URL so a host or CI without egress to GitHub can point at
// an internal mirror.
func otterReleaseBase() string {
	if base := strings.TrimSpace(os.Getenv("OTTER_RELEASE_BASE_URL")); base != "" {
		return strings.TrimRight(base, "/")
	}
	return DefaultOtterReleaseBase
}

// releaseVersionPattern is what a published release looks like. A development
// build reports things like "v0.1.12-1-g22aee7c-dirty" or "dev", and there is
// no archive for those: saying so immediately is better than a 404 from GitHub.
var releaseVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// IsReleasedVersion reports whether a version string names a published release,
// which is the only kind of build an archive can be fetched for.
func IsReleasedVersion(version string) bool {
	return releaseVersionPattern.MatchString(strings.TrimPrefix(strings.TrimSpace(version), "v"))
}

// ReleaseBinaries obtains otterd and otter from a published release archive for
// the detected target platform.
type ReleaseBinaries struct {
	// Version is the release to fetch, with or without a leading v.
	Version string
	// CacheDir overrides where fetched binaries are kept between deploys.
	CacheDir string
	// BaseURL overrides the release base. Empty uses otterReleaseBase().
	BaseURL string
	// Client overrides the HTTP client. Empty uses a bounded default.
	Client *http.Client
	// Stderr receives progress lines, one per fetch.
	Stderr io.Writer
}

// OtterArchiveName is the published artifact name for a version and platform.
// It matches the archives goreleaser produces: otter_<version>_<os>_<arch>.
func OtterArchiveName(version, goos, goarch string) string {
	return fmt.Sprintf("otter_%s_%s_%s.tar.gz", version, goos, goarch)
}

// releaseDir is the per-release download directory, e.g.
// https://github.com/tkoizumi/otter/releases/download/v0.1.13.
func (r ReleaseBinaries) releaseDir() string {
	base := r.BaseURL
	if base == "" {
		base = otterReleaseBase()
	}
	return fmt.Sprintf("%s/v%s", strings.TrimRight(base, "/"), r.NormalizedVersion())
}

// NormalizedVersion is the version without the leading v, which is the spelling
// used in archive names.
func (r ReleaseBinaries) NormalizedVersion() string {
	return strings.TrimPrefix(strings.TrimSpace(r.Version), "v")
}

// Describe explains the source for the deploy plan.
func (r ReleaseBinaries) Describe(platform string) string {
	return fmt.Sprintf("fetch release %s for %s", r.NormalizedVersion(), platform)
}

// Binaries fetches the two executables for goos/goarch into binDir.
func (r ReleaseBinaries) Binaries(ctx context.Context, goos, goarch, binDir string) error {
	version := r.NormalizedVersion()
	if !IsReleasedVersion(version) {
		return fmt.Errorf(
			"this build reports version %q, which is not a published Otter release\n"+
				"hint: compile a checkout with --build --source <dir>, use executables you "+
				"already have with --binaries <dir>, or deploy with an installed released otter",
			r.Version)
	}

	cacheDir, err := r.cacheDir(version, goos, goarch)
	if err != nil {
		return err
	}

	// A cached pair is reused, so deploying to several hosts downloads once.
	if !r.cached(cacheDir) {
		if err := r.fetch(ctx, version, goos, goarch, cacheDir); err != nil {
			return err
		}
	}

	for _, name := range []string{"otterd", "otter"} {
		if err := copyFile(filepath.Join(cacheDir, name), filepath.Join(binDir, name), 0o755); err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
	}
	return nil
}

// cacheDir is where one version's binaries for one platform are kept.
//
// An explicit CacheDir is honoured strictly: a cache the operator named but
// cannot write is a configuration error worth reporting. The implicit per-user
// cache falls back to the system temporary directory instead, so a read-only or
// absent home directory degrades to "not cached" rather than failing the
// deploy.
func (r ReleaseBinaries) cacheDir(version, goos, goarch string) (string, error) {
	name := version + "_" + goos + "_" + goarch
	if r.CacheDir != "" {
		dir := filepath.Join(r.CacheDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create release cache %s: %w", dir, err)
		}
		return dir, nil
	}

	var candidates []string
	if userCache, err := os.UserCacheDir(); err == nil && userCache != "" {
		candidates = append(candidates, filepath.Join(userCache, "otter", "releases"))
	}
	candidates = append(candidates, filepath.Join(os.TempDir(), "otter-releases"))

	var lastErr error
	for _, root := range candidates {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			lastErr = err
			continue
		}
		return dir, nil
	}
	return "", fmt.Errorf("no writable release cache: %w", lastErr)
}

// cached reports whether both executables are already unpacked.
func (r ReleaseBinaries) cached(dir string) bool {
	for _, name := range []string{"otterd", "otter"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return false
		}
	}
	return true
}

// fetch downloads, verifies and unpacks one release archive.
//
// The checksum is checked against the release's own checksums.txt before
// anything is unpacked. It is fetched over the same TLS connection as the
// archive, so it is a guard against a truncated or corrupted download rather
// than against a compromised origin -- but a corrupt runtime binary is exactly
// the failure that would otherwise surface later, on the host, as an
// inexplicable crash.
func (r ReleaseBinaries) fetch(ctx context.Context, version, goos, goarch, dest string) error {
	asset := OtterArchiveName(version, goos, goarch)
	dir := r.releaseDir()

	want, err := r.expectedChecksum(ctx, dir, asset)
	if err != nil {
		return err
	}

	url := dir + "/" + asset
	r.logf("fetching %s\n", url)

	tmpDir, err := os.MkdirTemp(filepath.Dir(dest), ".otter-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	archive := filepath.Join(tmpDir, asset)
	got, err := r.download(ctx, url, archive)
	if err != nil {
		return err
	}
	if want != "" && !strings.EqualFold(want, got) {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", asset, want, got)
	}

	extracted, err := extractOtterBinaries(archive, tmpDir)
	if err != nil {
		return err
	}
	// Move into the cache last, so an interrupted fetch never leaves a partial
	// pair that a later deploy would trust.
	for _, name := range []string{"otterd", "otter"} {
		if err := copyFile(filepath.Join(extracted, name), filepath.Join(dest, name), 0o755); err != nil {
			return fmt.Errorf("unpack %s: %w", name, err)
		}
	}
	r.logf("otter %s %s/%s ready (sha256 %s)\n", version, goos, goarch, got[:12])
	return nil
}

// logf reports progress when a writer was supplied. A nil writer is legal: the
// deployer always sets one, and a test should not have to provide a sink.
func (r ReleaseBinaries) logf(format string, args ...any) {
	if r.Stderr == nil {
		return
	}
	fmt.Fprintf(r.Stderr, format, args...)
}

// expectedChecksum reads the archive's sha256 out of the release's
// checksums.txt. A missing file is an error unless the operator explicitly
// opted out, because silently trusting an unverified binary is worse than a
// clear failure.
func (r ReleaseBinaries) expectedChecksum(ctx context.Context, dir, asset string) (string, error) {
	if skip := strings.TrimSpace(os.Getenv("OTTER_RELEASE_SKIP_CHECKSUM")); skip == "1" || strings.EqualFold(skip, "true") {
		r.logf("warning: OTTER_RELEASE_SKIP_CHECKSUM is set; the downloaded runtime is not verified\n")
		return "", nil
	}

	body, status, err := r.get(ctx, dir+"/checksums.txt")
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf(
			"download checksums.txt: unexpected status %d\n"+
				"hint: if this mirror does not publish checksums, set OTTER_RELEASE_SKIP_CHECKSUM=1, "+
				"or pass --binaries <dir>", status)
	}
	defer body.Close()

	data, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read checksums.txt: %w", err)
	}
	// goreleaser writes "<sha256>  <name>", two spaces.
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != asset {
			continue
		}
		return fields[0], nil
	}
	return "", fmt.Errorf("checksums.txt lists no entry for %s", asset)
}

// download streams url into dest and returns the sha256 of what was written.
func (r ReleaseBinaries) download(ctx context.Context, url, dest string) (string, error) {
	body, status, err := r.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer body.Close()
	if status != http.StatusOK {
		return "", fmt.Errorf("download %s: unexpected status %d\n"+
			"hint: check that release v%s exists for this platform, or pass --build/--binaries",
			url, status, r.NormalizedVersion())
	}

	file, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(file, hasher), body); err != nil {
		file.Close()
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// get performs one GET, returning the body and status. The caller closes the
// body; a non-200 is returned with the body so the caller can decide.
func (r ReleaseBinaries) get(ctx context.Context, url string) (io.ReadCloser, int, error) {
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch %s: %w", url, err)
	}
	return resp.Body, resp.StatusCode, nil
}

// DirBinaries supplies otterd and otter from a local directory, which is the
// offline and air-gapped path.
type DirBinaries struct {
	// Dir holds otterd and otter built for the target platform.
	Dir string
}

// Binaries copies the two executables into binDir.
//
// The platform each binary was built for is read from its header and checked
// against the target that SSH reported. Pushing a macOS binary to a Linux host
// otherwise fails on the host with a bare `exec format error`, which names
// neither the file nor the fix.
// Describe explains the source for the deploy plan.
func (d DirBinaries) Describe(platform string) string {
	return "use otterd and otter from " + d.Dir + " for " + platform
}

// Binaries copies the two executables into binDir.
func (d DirBinaries) Binaries(_ context.Context, goos, goarch, binDir string) error {
	if strings.TrimSpace(d.Dir) == "" {
		return errors.New("--binaries needs a directory holding otterd and otter")
	}
	for _, name := range []string{"otterd", "otter"} {
		src, ok := binaryIn(d.Dir, name, goos, goarch)
		if !ok {
			return fmt.Errorf("--binaries %s: no %s or %s-%s-%s in it",
				d.Dir, name, name, goos, goarch)
		}
		if got, ok := binaryOS(src); ok && got != goos {
			return fmt.Errorf("%s is a %s executable but the host is %s; build %s/%s binaries",
				src, got, goos, goos, goarch)
		}
		if err := copyFile(src, filepath.Join(binDir, name), 0o755); err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
	}
	return nil
}

// binaryIn finds one executable in a directory, accepting either the bare name
// or the platform-suffixed name `make cross` writes.
//
// The suffixed form is tried first and both are checked against the target
// platform, because a checkout's bin/ holds every platform at once: reading the
// bare name there would pick up the developer's own macOS build and then fail
// the platform check for a Linux host.
func binaryIn(dir, name, goos, goarch string) (string, bool) {
	for _, candidate := range []string{name + "-" + goos + "-" + goarch, name} {
		path := filepath.Join(dir, candidate)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path, true
		}
	}
	return "", false
}

// binaryOS identifies the operating system a native executable was built for,
// or reports false when the format is not one it recognises.
//
// Go's own binary formats are enough here: Otter ships as ELF on Linux and
// Mach-O on macOS, and those four magics cover every archive a release or a
// local build can produce.
func binaryOS(path string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()

	var magic [4]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil {
		return "", false
	}
	switch {
	case magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F':
		return "linux", true
	case magic == [4]byte{0xfe, 0xed, 0xfa, 0xce}, // 32-bit big-endian
		magic == [4]byte{0xfe, 0xed, 0xfa, 0xcf}, // 64-bit big-endian
		magic == [4]byte{0xce, 0xfa, 0xed, 0xfe}, // 32-bit little-endian
		magic == [4]byte{0xcf, 0xfa, 0xed, 0xfe}: // 64-bit little-endian
		return "darwin", true
	}
	return "", false
}

// extractOtterBinaries unpacks otterd and otter out of a release archive into
// dest, returning dest.
//
// Only regular files whose base name is one of the two are considered, and the
// destination is always inside dest, so an archive with an unexpected layout
// cannot write outside the extraction directory.
func extractOtterBinaries(archive, dest string) (string, error) {
	file, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer file.Close()

	gz, err := gzip.NewReader(file)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", filepath.Base(archive), err)
	}
	defer gz.Close()

	found := map[string]bool{}
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read %s: %w", filepath.Base(archive), err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.Base(header.Name)
		if name != "otterd" && name != "otter" {
			continue
		}
		out, err := os.OpenFile(filepath.Join(dest, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, reader); err != nil {
			out.Close()
			return "", fmt.Errorf("unpack %s: %w", name, err)
		}
		if err := out.Close(); err != nil {
			return "", err
		}
		found[name] = true
	}

	for _, name := range []string{"otterd", "otter"} {
		if !found[name] {
			return "", fmt.Errorf("%s contains no %s executable", filepath.Base(archive), name)
		}
	}
	return dest, nil
}
