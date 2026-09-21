package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tkoizumi/otter/internal/identity"
)

// Builder performs the local half of a deploy: cross-compiling the binaries and
// assembling the tree that will be pushed.
//
// It is an interface so the converge can be tested without invoking Go or
// writing a repository's worth of files. The real implementation is
// LocalBuilder.
type Builder interface {
	// TempDir returns the scratch directory for a deploy. It is created once
	// and removed by Cleanup.
	TempDir() (string, error)
	Build(ctx context.Context, cfg Config, outDir string) error
	// VendorUV fetches the pinned uv binary for the target platform into
	// outDir, so the host needs nothing installed in advance.
	VendorUV(ctx context.Context, cfg Config, outDir string) error
	Stage(cfg Config, outDir string) error
	// Revision hashes exactly what was staged, so two deploys of the same
	// source produce the same value and a changed map or manifest produces a
	// different one.
	Revision(outDir string) (string, error)
	Cleanup()
}

// LocalBuilder builds with the local Go toolchain and stages files with plain
// file copies. No Docker, no remote toolchain, no cgo: the SQLite driver is
// pure Go, so a cross-compiled binary is the whole artifact.
type LocalBuilder struct {
	// GoBinary is the go command to use. Defaults to "go" on PATH.
	GoBinary string
	// Stdout and Stderr receive build output.
	Stdout io.Writer
	Stderr io.Writer

	tmpDir string
}

// NewLocalBuilder returns a builder writing to the given streams.
func NewLocalBuilder(stdout, stderr io.Writer) *LocalBuilder {
	return &LocalBuilder{GoBinary: "go", Stdout: stdout, Stderr: stderr}
}

// TempDir creates (once) and returns the scratch directory for a deploy.
func (b *LocalBuilder) TempDir() (string, error) {
	if b.tmpDir != "" {
		return b.tmpDir, nil
	}
	dir, err := os.MkdirTemp("", "otter-deploy-build-")
	if err != nil {
		return "", fmt.Errorf("create build directory: %w", err)
	}
	b.tmpDir = dir
	return dir, nil
}

// Cleanup removes the scratch directory.
func (b *LocalBuilder) Cleanup() {
	if b.tmpDir != "" {
		_ = os.RemoveAll(b.tmpDir)
		b.tmpDir = ""
	}
}

// Build cross-compiles otterd and otter into outDir/bin.
//
// The version is injected the same way the Makefile does it so that a deployed
// binary reports the same version string as a local `make build`.
func (b *LocalBuilder) Build(ctx context.Context, cfg Config, outDir string) error {
	goos, goarch, err := ParsePlatform(cfg.Target.Platform)
	if err != nil {
		return err
	}

	binDir := filepath.Join(outDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", binDir, err)
	}

	goBin := b.GoBinary
	if goBin == "" {
		goBin = "go"
	}

	for _, cmd := range []struct{ name, pkg string }{
		{"otterd", "./cmd/otterd"},
		{"otter", "./cmd/otter"},
	} {
		args := []string{
			"build",
			"-trimpath",
			"-ldflags", "-s -w -X main.version=" + cfg.Version,
			"-o", filepath.Join(binDir, cmd.name),
			cmd.pkg,
		}
		if err := b.runGo(ctx, cfg.RepoRoot, goos, goarch, args...); err != nil {
			return fmt.Errorf("build %s for %s/%s: %w", cmd.name, goos, goarch, err)
		}
	}
	return nil
}

// Stage copies the runtime tree into outDir: the shared Python library, every
// integration, and nothing else.
//
// The result is the layout the host releases from: <remote>/integrations/<name>
// with shared code at <remote>/lib. That is why deploy runs `otter release
// --integrations <remote>/integrations`: the release base becomes <remote>, and
// the snapshot can place the integration at integrations/<name> and the tree at
// lib/python so the manifest's ../../lib keeps resolving.
//
// What is left out matters as much as what is included. Tests are not needed
// to run an integration, and bytecode caches must never be shipped: a stale
// .pyc compiled for a different Python would shadow the real module.
func (b *LocalBuilder) Stage(cfg Config, outDir string) error {
	libSrc := filepath.Join(cfg.RepoRoot, "lib")
	if _, err := os.Stat(libSrc); err == nil {
		if err := copyTree(libSrc, filepath.Join(outDir, "lib"), stageSkip); err != nil {
			return err
		}
	}

	for _, name := range cfg.Integrations {
		src := filepath.Join(cfg.IntegrationsPath, name)
		dst := filepath.Join(outDir, LocalIntegrationsDir, name)
		if err := copyTree(src, dst, stageSkip); err != nil {
			return err
		}
	}
	return nil
}

// stageSkip reports whether a path inside a staged tree should be left out.
// Everything here is either a secret or reproducible from what is shipped.
func stageSkip(path string, d fs.DirEntry) bool {
	name := d.Name()
	if d.IsDir() {
		switch name {
		case "__pycache__", ".git", ".pytest_cache", ".venv", "venv", "node_modules", "tests":
			return true
		}
		return false
	}
	switch {
	case name == identity.MarkerFileName || identity.IsMarkerTempName(name):
		// The identity marker names a local instance. Shipping it would make
		// the destination adopt this checkout's identity, and every later
		// deploy would fight over it. The destination registers its own.
		return true
	case name == ".env" || strings.HasSuffix(name, ".env"):
		return true // never ship secrets inside a synced tree
	case strings.HasSuffix(name, ".graphql"):
		// A query document is read at run time; the pulled schema beside it is
		// editor and test tooling, 3.5 MB of it for Shopify. Keeping the schema
		// out of the pushed tree also keeps it out of every release staged from
		// that tree -- the two lists must agree on what a release carries.
		return strings.Contains(filepath.ToSlash(path), "/schema/")
	case strings.HasSuffix(name, ".pyc"), strings.HasSuffix(name, ".pyo"):
		return true
	case strings.HasSuffix(name, ".db"), strings.HasSuffix(name, ".db-wal"),
		strings.HasSuffix(name, ".db-shm"):
		return true
	}
	return false
}

// Revision hashes everything under outDir, so identical source yields an
// identical revision.
func (b *LocalBuilder) Revision(outDir string) (string, error) {
	var files []string
	err := filepath.WalkDir(outDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if stageSkip(path, d) {
				return fs.SkipDir
			}
			return nil
		}
		if stageSkip(path, d) {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk %s: %w", outDir, err)
	}
	sort.Strings(files)

	h := sha256.New()
	for _, path := range files {
		rel, err := filepath.Rel(outDir, path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00", rel)

		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// runGo invokes the Go toolchain for the target platform.
//
// GOTOOLCHAIN=local is not optional here: the module declares a minimum Go
// version, and without this the toolchain would try to download a newer one
// mid-deploy on a machine that may have no network or no business fetching a
// compiler. Failing with "go.mod requires go >= x" is the honest outcome.
func (b *LocalBuilder) runGo(ctx context.Context, workDir, goos, goarch string, args ...string) error {
	cmd := exec.CommandContext(ctx, b.GoBinary, args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"GOOS="+goos,
		"GOARCH="+goarch,
		"CGO_ENABLED=0",
		"GOTOOLCHAIN=local",
	)
	cmd.Stdout = b.Stdout
	cmd.Stderr = b.Stderr
	return cmd.Run()
}

// copyTree copies a directory, honouring skip. Permissions are preserved
// closely enough for source files and executables; it is not a backup tool.
func copyTree(src, dst string, skip func(string, fs.DirEntry) bool) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != src && skip(path, d) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		// Symlinks are copied as links rather than followed: a vendored
		// directory may legitimately point outside the tree.
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		}

		return copyFile(path, target, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
