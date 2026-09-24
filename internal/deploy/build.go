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

// BinarySource supplies the two executables for a target platform when they are
// not compiled from source.
//
// It exists because a deploy target is a project, not necessarily a Go
// checkout: a workspace that holds only Python integrations has no cmd/otterd
// to build, and requiring a copy of the runtime repository to deploy would make
// the split between the runtime and its integrations a fiction.
type BinarySource interface {
	// Binaries puts otterd and otter for goos/goarch into binDir.
	Binaries(ctx context.Context, goos, goarch, binDir string) error
}

// Describer is implemented by a BinarySource that can explain, in one line,
// where the executables will come from.
//
// The deploy plan uses it, because "whose code is about to run on my host?" is
// a question a dry run should answer without building anything. Otherwise the
// choice stays invisible until the step that acts on it.
type Describer interface {
	Describe(platform string) string
}

// LocalBuilder compiles with the local Go toolchain, or takes the binaries from
// a BinarySource, and stages files with plain file copies. No Docker, no remote
// toolchain, no cgo: the SQLite driver is pure Go, so a cross-compiled binary
// is the whole artifact.
type LocalBuilder struct {
	// GoBinary is the go command to use. Defaults to "go" on PATH.
	GoBinary string
	// Stdout and Stderr receive build output.
	Stdout io.Writer
	Stderr io.Writer

	// Binaries supplies the executables instead of compiling them. A nil
	// Binaries means "compile from the project's Go source".
	Binaries BinarySource

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

// Build produces otterd and otter in outDir/bin, either from the configured
// BinarySource or by compiling the project itself.
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

	if b.Binaries != nil {
		return b.Binaries.Binaries(ctx, goos, goarch, binDir)
	}

	// No source was configured, so the project is assumed to be the runtime
	// checkout. The command line always names one (see binarySourceFor); this
	// is the default a caller that builds a LocalBuilder by hand gets.
	return compileBinaries(ctx, CompileSource{
		Dir:      cfg.ProjectRoot,
		Version:  cfg.Version,
		GoBinary: b.GoBinary,
		Stdout:   b.Stdout,
		Stderr:   b.Stderr,
	}, goos, goarch, binDir)
}

// CompileSource compiles the runtime from a Go checkout.
//
// It is a BinarySource so that "which checkout" and "which archive" are the
// same kind of decision to the rest of the deploy. It exists because the
// project being deployed is usually not the runtime's source tree: a Python
// workspace has no cmd/otterd, while the person running the command may well
// have a checkout next to it.
type CompileSource struct {
	// Dir is the checkout holding cmd/otterd and cmd/otter.
	Dir string
	// Version is written into both binaries, so the host reports what was
	// actually built.
	Version string
	// GoBinary is the go command to use. Empty means "go" on PATH.
	GoBinary string
	// Stdout and Stderr receive the toolchain's output.
	Stdout io.Writer
	Stderr io.Writer
}

// Binaries compiles both executables for goos/goarch.
func (c CompileSource) Binaries(ctx context.Context, goos, goarch, binDir string) error {
	if _, err := os.Stat(filepath.Join(c.Dir, "cmd", "otterd")); err != nil {
		return fmt.Errorf("%s is not an Otter checkout: no cmd/otterd in it", c.Dir)
	}
	fmt.Fprintf(writerOr(c.Stderr, io.Discard), "compiling otterd and otter from %s\n", c.Dir)
	return compileBinaries(ctx, c, goos, goarch, binDir)
}

// Describe explains the source for the deploy plan.
func (c CompileSource) Describe(string) string {
	return "compile otterd and otter from " + c.Dir
}

// compileBinaries runs the two go builds.
func compileBinaries(ctx context.Context, src CompileSource, goos, goarch, binDir string) error {
	goBin := src.GoBinary
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
			"-ldflags", "-s -w -X main.version=" + src.Version,
			"-o", filepath.Join(binDir, cmd.name),
			cmd.pkg,
		}
		if err := runGoIn(ctx, src.Dir, goBin, goos, goarch, args, src.Stdout, src.Stderr); err != nil {
			return fmt.Errorf("build %s for %s/%s: %w", cmd.name, goos, goarch, err)
		}
	}
	return nil
}

// writerOr returns w, or a discard sink when the caller supplied none.
func writerOr(w io.Writer, fallback io.Writer) io.Writer {
	if w == nil {
		return fallback
	}
	return w
}

// Stage copies the runtime tree into outDir: every integration and the shared
// trees those integrations declare, and nothing else.
//
// The result is the layout the host releases from. An integration lands at
// <remote>/integrations/<name>, and a shared tree lands at the relative depth
// its manifest declares, measured from that same directory. A manifest saying
// `../lib/python` therefore puts the tree at <remote>/integrations/lib/python
// and one saying `../../lib/python` puts it at <remote>/lib/python. Either way
// the declaration resolves verbatim on the host, because the placement rule
// preserves the geometry rather than hardcoding one repository's shape.
//
// What is left out matters as much as what is included. Tests are not needed
// to run an integration, and bytecode caches must never be shipped: a stale
// .pyc compiled for a different Python would shadow the real module.
func (b *LocalBuilder) Stage(cfg Config, outDir string) error {
	placed := map[string]string{}
	for _, integ := range cfg.Integrations {
		dst := filepath.Join(outDir, LocalIntegrationsDir, integ.Name)
		if err := copyTree(integ.Dir, dst, stageSkip); err != nil {
			return err
		}

		for _, tree := range integ.Trees {
			rel, err := TreePlacement(integ, tree)
			if err != nil {
				return err
			}
			// Two integrations may legitimately share one library. Two
			// different libraries claiming one path is a collision the host
			// could not resolve, so it is refused here while both sources are
			// still known.
			if prev, ok := placed[rel]; ok {
				if prev != tree {
					return fmt.Errorf("two shared trees would land at %s: %s and %s", rel, prev, tree)
				}
				continue
			}
			placed[rel] = tree
			if err := copyTree(tree, filepath.Join(outDir, filepath.FromSlash(rel)), stageSkip); err != nil {
				return err
			}
		}
	}
	return nil
}

// TreePlacement is where one shared tree lands, relative to the remote install
// root, for one integration.
//
// It is the same rule release.Plan applies on the host: preserve the relative
// geometry between the integration and the code it imports, because that is
// what a relative python.path depends on. The integration is placed at
// integrations/<name>, so a declaration of ../lib/python becomes
// integrations/lib/python while ../../lib/python becomes lib/python.
func TreePlacement(integ Integration, tree string) (string, error) {
	rel, err := filepath.Rel(integ.Dir, tree)
	if err != nil {
		return "", fmt.Errorf("place shared tree %s relative to %s: %w", tree, integ.Dir, err)
	}
	placed := filepath.ToSlash(filepath.Join(LocalIntegrationsDir, integ.Name, rel))
	if placed == "." || placed == ".." || strings.HasPrefix(placed, "../") || strings.HasPrefix(placed, "/") {
		return "", fmt.Errorf(
			"shared tree %s cannot be placed from integration %s: %s escapes the install root",
			tree, integ.Name, filepath.ToSlash(rel))
	}
	return placed, nil
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

// runGoIn invokes the Go toolchain in workDir for the target platform.
//
// GOTOOLCHAIN=local is not optional here: the module declares a minimum Go
// version, and without this the toolchain would try to download a newer one
// mid-deploy on a machine that may have no network or no business fetching a
// compiler. Failing with "go.mod requires go >= x" is the honest outcome.
func runGoIn(ctx context.Context, workDir, goBin, goos, goarch string, args []string, stdout, stderr io.Writer) error {
	if goBin == "" {
		goBin = "go"
	}
	cmd := exec.CommandContext(ctx, goBin, args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"GOOS="+goos,
		"GOARCH="+goarch,
		"CGO_ENABLED=0",
		"GOTOOLCHAIN=local",
	)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
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
