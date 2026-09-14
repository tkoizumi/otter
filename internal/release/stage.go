package release

import (
	"context"
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

// SharedTree is a top-level directory captured alongside the integration.
//
// Name is where the tree lands in the release root, which must be the same
// relative depth it has relative to the integration directory. Copying
// `../../lib` into the release root as `lib` is what lets a manifest keep
// `python.path: [../../lib/python]` verbatim.
type SharedTree struct {
	Source string
	Name   string
}

// Stage copies an integration and the shared code it imports into a new
// release directory and publishes its metadata.
//
// The copy happens into a temporary directory that is renamed into place, so a
// release directory either exists complete or not at all. Two stagings of
// identical inputs produce the same digest, so re-staging an unchanged
// integration returns the existing release instead of creating a second copy.
func (m Manager) Stage(integration, sourceDir string, sharedDirs []string, environmentDigest string) (Metadata, error) {
	return m.StageWithShared(integration, sourceDir, sharedTreesFrom(sharedDirs), environmentDigest)
}

// sharedTreesFrom keeps the historical []string form working: each directory
// lands under its own base name.
func sharedTreesFrom(dirs []string) []SharedTree {
	out := make([]SharedTree, 0, len(dirs))
	for _, dir := range dirs {
		out = append(out, SharedTree{Source: dir, Name: filepath.Base(dir)})
	}
	return out
}

// StageWithShared stages an integration together with explicitly named shared
// trees.
func (m Manager) StageWithShared(integration, sourceDir string, shared []SharedTree, environmentDigest string) (Metadata, error) {
	if err := validName(integration); err != nil {
		return Metadata{}, err
	}
	if _, err := os.Stat(filepath.Join(sourceDir, "otter.yaml")); err != nil {
		return Metadata{}, fmt.Errorf("release: %s has no otter.yaml: %w", sourceDir, err)
	}

	sources := make([]string, 0, len(shared))
	for _, tree := range shared {
		sources = append(sources, tree.Source)
	}
	digest, err := DigestNamed(sourceDir, shared, environmentDigest)
	if err != nil {
		return Metadata{}, err
	}
	final, err := m.Dir(integration, digest)
	if err != nil {
		return Metadata{}, err
	}

	// Already staged: an identical digest means an identical tree.
	if meta, err := readMetadata(final); err == nil {
		return meta, nil
	}

	root, err := m.Root()
	if err != nil {
		return Metadata{}, err
	}
	if err := os.MkdirAll(filepath.Join(root, integration), 0o700); err != nil {
		return Metadata{}, err
	}

	// A partial directory from an interrupted staging is removed only while
	// holding nothing else: it has no metadata, so no release references it.
	tmp, err := os.MkdirTemp(filepath.Join(root, integration), ".staging-")
	if err != nil {
		return Metadata{}, err
	}
	defer os.RemoveAll(tmp)

	src := filepath.Join(tmp, "integrations", integration)
	if err := copyTree(sourceDir, src); err != nil {
		return Metadata{}, fmt.Errorf("stage %s: %w", integration, err)
	}
	for _, tree := range shared {
		if _, err := os.Stat(tree.Source); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return Metadata{}, err
		}
		dest, err := safeJoin(tmp, tree.Name)
		if err != nil {
			return Metadata{}, err
		}
		if err := copyTree(tree.Source, dest); err != nil {
			return Metadata{}, fmt.Errorf("stage shared %s: %w", tree.Source, err)
		}
	}

	git := ReadGitState(context.Background(), sourceDir)
	meta := Metadata{
		Integration: integration,
		Digest:      digest,
		Environment: environmentDigest,
		Source:      sourceDir,
		CreatedAt:   time.Now().UTC(),
		GitRevision: git.Revision,
		GitDirty:    git.Dirty,
	}
	if err := writeMetadata(tmp, meta); err != nil {
		return Metadata{}, err
	}

	if err := os.Rename(tmp, final); err != nil {
		// A concurrent staging of the same digest won the race; its release is
		// identical, so reuse it rather than failing.
		if existing, readErr := readMetadata(final); readErr == nil {
			return existing, nil
		}
		return Metadata{}, fmt.Errorf("publish release %s: %w", digest[:12], err)
	}
	return meta, nil
}

// Activate switches an integration to a staged release.
//
// The switch is a symlink replacement, performed by creating the replacement
// under a temporary name and renaming it over the previous link: rename(2) is
// atomic, so a reader either sees the old release or the new one and never a
// missing path. A process that has already imported its modules is unaffected
// because the kernel keeps the old files open, which is what makes it safe to
// activate while an attempt is running.
func (m Manager) Activate(integration, digest string) error {
	dir, err := m.Dir(integration, digest)
	if err != nil {
		return err
	}
	if _, err := readMetadata(dir); err != nil {
		return fmt.Errorf("refusing to activate an incomplete release: %w", err)
	}

	link, err := m.ActivePath(integration)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		return err
	}

	tmp := link + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(dir, tmp); err != nil {
		return fmt.Errorf("create activation link: %w", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("activate release %s: %w", digest[:12], err)
	}
	return nil
}

// safeJoin joins a relative name onto a release root, refusing anything that
// would escape it. A manifest's python.path is user input, so it must not be
// able to place content outside the snapshot.
func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("release: shared tree name %q escapes the release root", name)
	}
	joined := filepath.Join(root, clean)
	if !strings.HasPrefix(joined, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("release: shared tree name %q escapes the release root", name)
	}
	return joined, nil
}

// Release is one staged release on disk.
type Release struct {
	Metadata
	// Active reports whether this release is the one an integration serves.
	Active bool
}

// List returns every staged release for an integration, newest first.
func (m Manager) List(integration string) ([]Release, error) {
	if err := validName(integration); err != nil {
		return nil, err
	}
	root, err := m.Root()
	if err != nil {
		return nil, err
	}
	base := filepath.Join(root, integration)
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	active := ""
	if meta, ok, err := m.Active(integration); err == nil && ok {
		active = meta.Digest
	}

	out := make([]Release, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		meta, err := readMetadata(filepath.Join(base, entry.Name()))
		if err != nil {
			continue // an unreadable directory is not a release
		}
		out = append(out, Release{Metadata: meta, Active: meta.Digest == active})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Retain removes inactive releases beyond keep, protecting any digest the
// caller says is still referenced by queued, running, or retrying work.
//
// It never removes the active release, and it never removes the newest
// inactive one, so a rollback always has somewhere to go.
func (m Manager) Retain(integration string, keep int, referenced map[string]bool) ([]string, error) {
	if keep < 1 {
		keep = 1
	}
	releases, err := m.List(integration)
	if err != nil {
		return nil, err
	}

	var removed []string
	kept := 0
	for _, rel := range releases {
		if rel.Active {
			continue
		}
		kept++
		if kept <= keep || referenced[rel.Digest] {
			continue
		}
		dir, err := m.Dir(integration, rel.Digest)
		if err != nil {
			return removed, err
		}
		if err := os.RemoveAll(dir); err != nil {
			return removed, fmt.Errorf("remove release %s: %w", rel.Digest[:12], err)
		}
		removed = append(removed, rel.Digest)
	}
	return removed, nil
}

// copyTree copies a directory, preserving symlinks and skipping everything a
// release must not carry.
func copyTree(src, dst string) error {
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
