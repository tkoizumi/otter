// Package release stages and activates immutable source snapshots.
//
// A release is a self-contained copy of a job plus the shared code it
// imports, placed under a directory named after a digest of its contents. The
// point is that a running or queued attempt executes a known snapshot instead
// of whatever happens to be on disk when it starts, and that activating a new
// snapshot cannot rewrite files underneath an attempt that is already running.
//
// The central design decision is the layout: a release mirrors the repository
// layout rather than flattening it. The placement rule is one base shared by
// everything a release carries:
//
//	base = the closest common ancestor of the jobs discovery root,
//	       the job directory and every captured shared tree
//
// The job lands at rel(base, jobDir) and each shared tree at
// rel(base, liveTreeDir). The release root plays the part of base, so every
// relative declaration in the manifest resolves exactly as it does in the
// checkout. For the canonical layout that means:
//
//	<data dir>/.releases/<identity>/<release-digest>/
//	├── jobs/<job>/     the snapshot
//	├── lib/                            shared code, at the same relative depth
//	└── otter-release.json              metadata, including the environment digest
//
// but the rule is the same for the other shapes a workspace can have: a flat
// workspace places the job at <name> and shared code beside it, and a
// grouped workspace at group/<name> and group/lib/python. `otter deploy` does
// not change the rule: it stages each job at
// `<remote>/jobs/<name>` and every declared tree at the depth the
// manifest names from there, so where the base falls follows from the manifest
// rather than from a hardcoded layout.
//
// The activation links live at <data dir>/.releases/active/<job>. They
// sit outside the jobs tree on purpose: `otter deploy` rsyncs that tree
// with --delete, and a symlink inside it would be replaced by a directory, or
// worse, written through into the snapshot. Each link targets its release
// relatively, so the data directory can be moved or restored elsewhere and
// every link keeps resolving.
//
// Nothing has to be rewritten to activate a release: the manifest travels
// verbatim and a manifest that works locally works in a release. The live
// jobs tree stays exactly as it was pushed -- discovery and every
// path-derived behaviour (the working directory, the SDK's relative
// resolution, `otter inspect`) keep using it -- while each run resolves the
// digest to execute through the activation link of its own identity.
// The releases root is dot-prefixed, which the existing discovery walk already
// skips, so snapshots are never discovered as jobs themselves.
package release

import (
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

	"github.com/tkoizumi/otter/internal/identity"
)

// DirName is the releases root inside the data directory. It is dot-prefixed
// so config.Discover skips it.
const DirName = ".releases"

// activeDirName holds the activation symlinks.
const activeDirName = "active"

// ActiveDirName is the activation directory inside the releases root. It is
// exported so callers enumerating the releases root can tell it apart from an
// job directory.
const ActiveDirName = activeDirName

// ManifestFileName is the metadata file written inside a release.
const ManifestFileName = "otter-release.json"

// JobRoot is where a job was placed by releases staged before
// Metadata.JobPath existed. It is only a fallback: every release staged
// today records its real placement.
const JobRoot = "jobs"

// DefaultKeep is how many inactive releases per job are retained.
// Retention is bounded so a long-lived install cannot grow without limit, and
// a release referenced by queued or running work is never removed.
const DefaultKeep = 3

// Metadata describes a staged release.
type Metadata struct {
	Job    string `json:"job"`
	Digest string `json:"digest"`
	// JobPath is the job directory inside the release,
	// slash-normalized and relative to the release root. It is the single
	// source of truth for where the snapshot's code lives, because the
	// placement depends on where the job and its shared code sat in the
	// checkout. Empty on releases staged before this field existed; the
	// resolver then assumes JobRoot/<job>.
	JobPath string `json:"job_path,omitempty"`
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

// UnmarshalJSON reads historical release metadata without rewriting snapshots.
// New metadata is always written with job fields.
func (m *Metadata) UnmarshalJSON(data []byte) error {
	type current Metadata
	var wire struct {
		current
		LegacyJob  string `json:"integration"`
		LegacyPath string `json:"integration_path"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*m = Metadata(wire.current)
	if m.Job == "" {
		m.Job = wire.LegacyJob
	}
	if m.JobPath == "" {
		m.JobPath = wire.LegacyPath
	}
	if m.JobPath == "" && wire.LegacyJob != "" {
		if err := validName(m.Job); err != nil {
			return err
		}
		m.JobPath = filepath.ToSlash(filepath.Join("integrations", m.Job))
	}
	return nil
}

// JobRel returns the job's placement inside a release,
// validated: relative, no "..", and with the fallback for metadata written
// before the field existed.
func (m Metadata) JobRel() (string, error) {
	raw := strings.TrimSpace(m.JobPath)
	if raw == "" {
		if err := validName(m.Job); err != nil {
			return "", err
		}
		return filepath.ToSlash(filepath.Join(JobRoot, m.Job)), nil
	}
	clean := filepath.Clean(filepath.FromSlash(raw))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("release: job_path %q escapes the release root", raw)
	}
	return filepath.ToSlash(clean), nil
}

// SourceDir resolves the job directory inside the release directory
// releaseRoot. It is THE resolver: staging writes JobPath, and
// preparation, `--list`, `--activate`, ActiveSourceDir and the worker all read
// the placement back through here, so a hand-edited or escaping value is
// rejected exactly once, in one place.
func (m Metadata) SourceDir(releaseRoot string) (string, error) {
	rel, err := m.JobRel()
	if err != nil {
		return "", err
	}
	return safeJoin(releaseRoot, rel)
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
func (m Manager) Dir(job, digest string) (string, error) {
	if err := validName(job); err != nil {
		return "", err
	}
	if len(digest) != 64 {
		return "", fmt.Errorf("release: %q is not a release digest", digest)
	}
	root, err := m.Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, job, digest), nil
}

// ActivePath returns the activation symlink for a job.
func (m Manager) ActivePath(job string) (string, error) {
	if err := validName(job); err != nil {
		return "", err
	}
	root, err := m.Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, activeDirName, job), nil
}

// Active resolves the release a job is currently serving.
//
// A missing symlink means the job has never been released, which is
// the normal state for a job that does not use managed Python. The
// second return value reports whether a release exists, so callers can
// distinguish "not released" from "broken".
func (m Manager) Active(job string) (Metadata, bool, error) {
	path, err := m.ActivePath(job)
	if err != nil {
		return Metadata{}, false, err
	}
	dir, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Metadata{}, false, nil
		}
		return Metadata{}, false, fmt.Errorf("resolve active release for %s: %w", job, err)
	}
	meta, err := readMetadata(dir)
	if err != nil {
		return Metadata{}, true, err
	}
	return meta, true, nil
}

// Metadata reads one release's metadata.
func (m Manager) Metadata(job, digest string) (Metadata, error) {
	dir, err := m.Dir(job, digest)
	if err != nil {
		return Metadata{}, err
	}
	return readMetadata(dir)
}

// DeleteAll removes every staged release belonging to one job and its
// activation pointer. It is used when an identity is deleted; releases of any
// other job are never touched, and removal is by identity directory
// rather than a glob.
func (m Manager) DeleteAll(job string) error {
	if err := validName(job); err != nil {
		return err
	}
	root, err := m.Root()
	if err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(root, job)); err != nil {
		return fmt.Errorf("release: remove releases for %s: %w", job, err)
	}
	active, err := m.ActivePath(job)
	if err != nil {
		return err
	}
	if err := os.Remove(active); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("release: remove active pointer for %s: %w", job, err)
	}
	return nil
}

// SourceDir resolves the directory one staged release's code lives in.
func (m Manager) SourceDir(meta Metadata) (string, error) {
	dir, err := m.Dir(meta.Job, meta.Digest)
	if err != nil {
		return "", err
	}
	return meta.SourceDir(dir)
}

// ActiveSourceDir resolves the directory a job should execute from,
// given a data directory. The second return value is false when the
// job has never been released, so the caller can fall back to the live
// source tree for jobs that did not opt in.
func ActiveSourceDir(dataDir, job string) (string, string, bool, error) {
	manager := Manager{DataDir: dataDir}
	meta, ok, err := manager.Active(job)
	if err != nil || !ok {
		return "", "", false, err
	}
	src, err := manager.SourceDir(meta)
	if err != nil {
		return "", "", false, err
	}
	return src, meta.Digest, true, nil
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
		case "__pycache__", ".git", ".otter", ".pytest_cache", ".venv", "venv", "node_modules",
			".mypy_cache", ".tox", "tests":
			return true
		}
		return false
	}
	switch {
	case name == identity.MarkerFileName || identity.IsMarkerTempName(name):
		// The identity marker is instance metadata, not released code. Keeping
		// it out of both the copy and the digest means identity churn never
		// invalidates a release, and a snapshot never carries a claim to an
		// identity it does not own.
		return true
	case name == ".env" || strings.HasSuffix(name, ".env"):
		return true
	case strings.HasSuffix(name, ".graphql"):
		// A query document is run-time input: the job reads it while it
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

func validName(job string) error {
	if strings.TrimSpace(job) == "" {
		return errors.New("release: job name is empty")
	}
	if strings.ContainsAny(job, `/\`) || job == "." || job == ".." {
		return fmt.Errorf("release: invalid job name %q", job)
	}
	return nil
}
