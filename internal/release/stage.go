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

// SharedTree is a shared directory captured alongside the integration.
//
// Source is the live directory; Name is where it lands in the release root,
// slash-normalized, as computed by Plan. The two must reproduce the tree's
// depth relative to the integration directory, or a manifest's relative
// python.path stops resolving after activation.
type SharedTree struct {
	Source string
	Name   string
}

// StageWithLayout copies an integration and the shared code it imports into a
// new release directory and publishes its metadata, using the placements Plan
// computed.
//
// The copy happens into a temporary directory that is renamed into place, so a
// release directory either exists complete or not at all. Two stagings of
// identical inputs produce the same digest, so re-staging an unchanged
// integration returns the existing release instead of creating a second copy.
//
// A declared shared tree that is missing or is not a directory is an error
// rather than a skip: a release that silently drops a tree would import the
// live copy on the machine that made it and fail on the machine that runs it.
func (m Manager) StageWithLayout(integration, sourceDir string, layout Layout, environmentDigest string) (Metadata, error) {
	if err := validName(integration); err != nil {
		return Metadata{}, err
	}
	if _, err := os.Stat(filepath.Join(sourceDir, "otter.yaml")); err != nil {
		return Metadata{}, fmt.Errorf("release: %s has no otter.yaml: %w", sourceDir, err)
	}
	if err := layout.validate(); err != nil {
		return Metadata{}, err
	}
	for _, tree := range layout.Trees {
		info, err := os.Stat(tree.Source)
		if err != nil {
			return Metadata{}, fmt.Errorf("release: shared tree %s is missing: %w", tree.Source, err)
		}
		if !info.IsDir() {
			return Metadata{}, fmt.Errorf("release: shared tree %s is not a directory", tree.Source)
		}
	}

	digest, err := layout.Digest(sourceDir, environmentDigest)
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

	// A captured tree that contains the staging directory would copy the
	// release into itself. That happens when the data directory lives inside
	// the integration directory -- an integration at the workspace root with
	// the default data location is the common case -- so it is refused
	// explicitly rather than left to run out of disk.
	captured := make([]string, 0, len(layout.Trees)+1)
	captured = append(captured, filepath.Clean(sourceDir))
	for _, tree := range layout.Trees {
		captured = append(captured, filepath.Clean(tree.Source))
	}
	for _, live := range captured {
		if within(tmp, live) {
			return Metadata{}, fmt.Errorf(
				"release: the data directory %s is inside %s, so a snapshot would copy itself; "+
					"release with a data directory outside the integration", filepath.Dir(root), live)
		}
	}

	// Every symlink a release preserves has to resolve inside one of these
	// trees. Anything else is either live code the release would import on this
	// machine or a dangling link on the host.
	roots := captured

	src, err := safeJoin(tmp, layout.IntegrationPath)
	if err != nil {
		return Metadata{}, err
	}
	if err := copyTree(sourceDir, src, roots); err != nil {
		return Metadata{}, fmt.Errorf("stage %s: %w", integration, err)
	}
	for _, tree := range layout.Trees {
		dest, err := safeJoin(tmp, tree.Name)
		if err != nil {
			return Metadata{}, err
		}
		if err := copyTree(tree.Source, dest, roots); err != nil {
			return Metadata{}, fmt.Errorf("stage shared %s: %w", tree.Source, err)
		}
	}

	git := ReadGitState(context.Background(), sourceDir)
	meta := Metadata{
		Integration:     integration,
		Digest:          digest,
		IntegrationPath: layout.IntegrationPath,
		Environment:     environmentDigest,
		Source:          sourceDir,
		CreatedAt:       time.Now().UTC(),
		GitRevision:     git.Revision,
		GitDirty:        git.Dirty,
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

// safeJoin joins a relative release path onto a release root, refusing anything
// that would escape it. A manifest's python.path and a release's recorded
// IntegrationPath are both untrusted by the time they are read back, so they
// must not be able to reach outside the snapshot.
//
// The root itself (".") is allowed: an integration can sit at the release root
// when the discovery root is the integration directory.
func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("release: path %q escapes the release root", name)
	}
	joined := filepath.Join(root, clean)
	if joined != filepath.Clean(root) && !strings.HasPrefix(joined, filepath.Clean(root)+string(os.PathSeparator)) {
		return "", fmt.Errorf("release: path %q escapes the release root", name)
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
//
// A preserved symlink must resolve inside one of roots, the live directories
// the release captures. Links within a tree, or from one captured tree into
// another, stay relative-identical in the release and are preserved. A link
// that resolves anywhere else -- including an absolute target, which can never
// point inside a release on another host -- is rejected: locally it would
// import live code that the snapshot pretends to contain, and on the host it
// would dangle.
func copyTree(src, dst string, roots []string) error {
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
			if !linkInside(path, link, roots) {
				return fmt.Errorf("symlink %s -> %s resolves outside the release; "+
					"a release cannot import live code or ship a dangling link", path, link)
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		}
		return copyFile(path, target, info.Mode())
	})
}

// linkInside reports whether a symlink at livePath resolves into one of the
// captured live roots. The check is lexical, so an internal link whose target
// does not exist yet is still preserved; only links that leave the captured
// trees are rejected.
func linkInside(livePath, link string, roots []string) bool {
	if filepath.IsAbs(link) {
		return false
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(livePath), link))
	for _, root := range roots {
		if within(resolved, root) {
			return true
		}
	}
	return false
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
