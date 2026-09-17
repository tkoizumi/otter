// Package release stages and activates immutable source snapshots.
//
// A release is a self-contained copy of an integration plus the shared code it
// imports, placed under a directory named after a digest of its contents. The
// point is that a running or queued attempt executes a known snapshot instead
// of whatever happens to be on disk when it starts, and that activating a new
// snapshot cannot rewrite files underneath an attempt that is already running.
//
// The central design decision is the layout: a release mirrors the repository
// layout rather than flattening it.
//
//	<data dir>/.releases/<integration>/<release-digest>/
//	├── integrations/<integration>/     the snapshot
//	├── lib/                            shared code, at the same relative depth
//	└── otter-release.json              metadata, including the environment digest
//
// The activation links live at <data dir>/.releases/active/<integration>. They
// sit outside the integrations tree on purpose: `otter deploy` rsyncs that tree
// with --delete, and a symlink inside it would be replaced by a directory, or
// worse, written through into the snapshot.
//
// Mirroring is what lets a manifest keep a relative declaration such as
// `python.path: [../../lib/python]` verbatim: the release root plays the part
// of the repository root, so every relative path resolves exactly as it does in
// the checkout. Nothing has to be rewritten to activate a release, and a
// manifest that works locally works in a release.
//
// The live integrations tree keeps a symlink per managed integration pointing
// at the active release, so discovery and every path-derived behaviour (the
// working directory, the SDK's relative resolution, `otter inspect`) work
// unchanged. The releases root is dot-prefixed, which the existing discovery
// walk already skips, so snapshots are never discovered as integrations
// themselves.
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DirName is the releases root inside the data directory. It is dot-prefixed
// so config.Discover skips it.
const DirName = ".releases"

// activeDirName holds the activation symlinks.
const activeDirName = "active"

// ManifestFileName is the metadata file written inside a release.
const ManifestFileName = "otter-release.json"

// DefaultKeep is how many inactive releases per integration are retained.
// Retention is bounded so a long-lived install cannot grow without limit, and
// a release referenced by queued or running work is never removed.
const DefaultKeep = 3

// Metadata describes a staged release.
type Metadata struct {
	Integration string `json:"integration"`
	Digest      string `json:"digest"`
	// Environment is the prepared Python environment digest this release was
	// validated against. It is recorded rather than implied so a report can
	// answer "what did this release run on?".
	Environment string `json:"environment,omitempty"`
	// Source is the directory the snapshot was taken from, for diagnostics.
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
	// Entrypoint is the manifest's entrypoint at stage time.
	Entrypoint string `json:"entrypoint,omitempty"`
	// GitRevision and GitDirty record the commit the release was built from.
	// They are metadata for traceability: a release is never blocked on the
	// working tree being clean, because testing uncommitted work is the normal
	// development loop.
	GitRevision string `json:"git_revision,omitempty"`
	GitDirty    bool   `json:"git_dirty,omitempty"`
}

// Manager owns the releases under one data directory.
type Manager struct{ DataDir string }

func (m Manager) root() (string, error) {
	if strings.TrimSpace(m.DataDir) == "" {
		return "", errors.New("release: data directory is empty")
	}
	return filepath.Abs(m.DataDir)
}

// Root is the releases root for this data directory.
func (m Manager) Root() (string, error) {
	root, err := m.root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, DirName), nil
}

// Dir returns the directory of one release.
func (m Manager) Dir(integration, digest string) (string, error) {
	if err := validName(integration); err != nil {
		return "", err
	}
	if len(digest) != 64 {
		return "", fmt.Errorf("release: %q is not a release digest", digest)
	}
	root, err := m.Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, integration, digest), nil
}

// ActivePath returns the activation symlink for an integration.
func (m Manager) ActivePath(integration string) (string, error) {
	if err := validName(integration); err != nil {
		return "", err
	}
	root, err := m.Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, activeDirName, integration), nil
}

// Active resolves the release an integration is currently serving.
//
// A missing symlink means the integration has never been released, which is
// the normal state for an integration that does not use managed Python. The
// second return value reports whether a release exists, so callers can
// distinguish "not released" from "broken".
func (m Manager) Active(integration string) (Metadata, bool, error) {
	path, err := m.ActivePath(integration)
	if err != nil {
		return Metadata{}, false, err
	}
	dir, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Metadata{}, false, nil
		}
		return Metadata{}, false, fmt.Errorf("resolve active release for %s: %w", integration, err)
	}
	meta, err := readMetadata(dir)
	if err != nil {
		return Metadata{}, true, err
	}
	return meta, true, nil
}

// Metadata reads one release's metadata.
func (m Manager) Metadata(integration, digest string) (Metadata, error) {
	dir, err := m.Dir(integration, digest)
	if err != nil {
		return Metadata{}, err
	}
	return readMetadata(dir)
}

// SourceDir returns the integration directory inside a release: the path the
// daemon should treat as the integration's directory.
func SourceDir(root string, integration string) string {
	return filepath.Join(root, "integrations", integration)
}

// ActiveSourceDir resolves the directory a managed integration should execute
// from, given a data directory. The second return value is false when the
// integration has never been released, so the caller can fall back to the live
// source tree for integrations that did not opt in.
func ActiveSourceDir(dataDir, integration string) (string, string, bool, error) {
	manager := Manager{DataDir: dataDir}
	meta, ok, err := manager.Active(integration)
	if err != nil || !ok {
		return "", "", false, err
	}
	dir, err := manager.Dir(integration, meta.Digest)
	if err != nil {
		return "", "", false, err
	}
	return SourceDir(dir, integration), meta.Digest, true, nil
}

// Digest hashes everything that determines what a release executes: the
// integration's own files, the shared code it imports, and the prepared
// environment it was validated against.
//
// Two stageings of identical inputs produce the same digest, which is what
// makes redeploying an unchanged integration a no-op rather than a new release.
func Digest(integrationDir string, sharedDirs []string, environmentDigest string) (string, error) {
	return DigestNamed(integrationDir, sharedTreesFrom(sharedDirs), environmentDigest)
}

// DigestNamed hashes an integration and named shared trees.
//
// The name a shared tree lands under is part of the hash, not just its
// contents: moving `lib` to `vendor` changes every relative import the
// integration performs, so it must produce a different release.
func DigestNamed(integrationDir string, shared []SharedTree, environmentDigest string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "otter-release-v1\x00%s\x00%s\x00", filepath.Base(integrationDir), environmentDigest)

	if err := hashTree(h, integrationDir, integrationDir); err != nil {
		return "", err
	}
	// Sorted so the digest does not depend on the order the caller listed them.
	sorted := append([]SharedTree(nil), shared...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, tree := range sorted {
		if _, err := os.Stat(tree.Source); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", err
		}
		fmt.Fprintf(h, "shared\x00%s\x00", filepath.ToSlash(tree.Name))
		if err := hashTree(h, tree.Source, tree.Source); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashTree walks one tree in deterministic order, hashing relative paths and
// contents. Excluded paths are the ones a release must not carry.
func hashTree(h io.Writer, root, dir string) error {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != dir && skip(path, d) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", dir, err)
	}
	sort.Strings(files)

	for _, path := range files {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00", filepath.ToSlash(rel))
		// Symlinks are hashed by target rather than followed: a link that
		// escapes the tree must not silently pull in outside content.
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "->%s\x00", target)
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		h.Write(body)
		h.Write([]byte{0})
	}
	return nil
}

// skip reports whether a path must stay out of a release. These are the same
// classes the deploy path excludes: credentials, caches, and local runtime data
// that would otherwise be captured into an immutable snapshot forever.
func skip(path string, d fs.DirEntry) bool {
	name := d.Name()
	if d.IsDir() {
		switch name {
		case "__pycache__", ".git", ".pytest_cache", ".venv", "venv", "node_modules",
			".mypy_cache", ".tox", "tests":
			return true
		}
		return false
	}
	switch {
	case name == ".env" || strings.HasSuffix(name, ".env"):
		return true
	case strings.HasSuffix(name, ".graphql"):
		// A query document is run-time input: the integration reads it while it
		// runs. The pulled schema beside it is tooling -- editors and tests read
		// it, the runtime never does -- and it is large (3.5 MB for Shopify), so
		// carrying it in every release would grow .releases for nothing. The
		// `/schema/` delimiters keep this from matching a directory that merely
		// starts with those letters, such as `schema-tools`.
		return strings.Contains(filepath.ToSlash(path), "/schema/")
	case strings.HasSuffix(name, ".pyc"), strings.HasSuffix(name, ".pyo"):
		return true
	case strings.HasSuffix(name, ".db"), strings.HasSuffix(name, ".db-wal"),
		strings.HasSuffix(name, ".db-shm"):
		return true
	}
	return false
}

func readMetadata(dir string) (Metadata, error) {
	body, err := os.ReadFile(filepath.Join(dir, ManifestFileName))
	if err != nil {
		return Metadata{}, fmt.Errorf("read release metadata in %s: %w", dir, err)
	}
	var meta Metadata
	if err := json.Unmarshal(body, &meta); err != nil {
		return Metadata{}, fmt.Errorf("parse release metadata in %s: %w", dir, err)
	}
	return meta, nil
}

// writeMetadata publishes the metadata atomically, so a release directory is
// either fully described or not present at all.
func writeMetadata(dir string, meta Metadata) error {
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp := filepath.Join(dir, ManifestFileName+".tmp")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, ManifestFileName))
}

func validName(integration string) error {
	if strings.TrimSpace(integration) == "" {
		return errors.New("release: integration name is empty")
	}
	if strings.ContainsAny(integration, `/\`) || integration == "." || integration == ".." {
		return fmt.Errorf("release: invalid integration name %q", integration)
	}
	return nil
}
