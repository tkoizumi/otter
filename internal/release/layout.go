package release

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// This file holds the one placement rule every release obeys.
//
// A release mirrors a repository layout so that a manifest's relative
// declarations -- python.path above all -- keep resolving verbatim once the
// job runs from the snapshot. What used to be two independent
// computations (the job at a hardcoded jobs/<name>, each tree
// at its own relative depth) is now a single base:
//
//	base = the closest common ancestor of the jobs discovery root,
//	       the job directory and every captured shared tree
//
// The job lands at rel(base, jobDir) and each tree at
// rel(base, liveTreeDir). Because every captured path is placed relative to the
// same directory, the relative geometry between them is preserved, which is
// exactly what a relative manifest path depends on.
//
// The discovery root takes part because deploy is the case that made the old
// code wrong: `otter release --jobs /opt/otter/jobs` sees the
// job at /opt/otter/jobs/<name> and shared code at
// /opt/otter/lib/python, so the base is /opt/otter and the two land at
// jobs/<name> and lib/python. With the discovery root as the base the
// tree would have to be placed at ../lib/python, which no release path can
// express.

// Layout is where a job and the shared code it imports land inside a
// release. Every path is slash-normalized and relative to the release root.
type Layout struct {
	// JobPath is the job directory's placement.
	JobPath string
	// Trees are the shared trees to capture, each named by its placement.
	Trees []SharedTree
}

// Plan computes the release layout for a job.
//
// trees carry only their Source; Plan assigns each one's Name. A tree inside
// the job directory is dropped because the job's own copy
// already carries it, and a tree nested inside another captured tree is dropped
// for the same reason. A tree that contains the job cannot be captured
// without copying the release into itself, and a path with no common ancestor
// below the filesystem root has no representable placement: both are errors,
// because a silently omitted tree is a release that imports live code.
func Plan(discoveryRoot, jobDir string, trees []SharedTree) (Layout, error) {
	root, err := absDir(discoveryRoot)
	if err != nil {
		return Layout{}, fmt.Errorf("release: jobs root: %w", err)
	}
	dir, err := absDir(jobDir)
	if err != nil {
		return Layout{}, fmt.Errorf("release: job directory: %w", err)
	}

	captured, err := captureList(dir, trees)
	if err != nil {
		return Layout{}, err
	}

	// The base takes in the discovery root as well as the job and each
	// tree. Including it can only move the base upward, never below the
	// directory a command was told to scan.
	base := commonAncestor(root, dir)
	for _, tree := range captured {
		base = commonAncestor(base, tree.Source)
	}
	if filepath.Dir(base) == base {
		return Layout{}, fmt.Errorf(
			"release: %s and its shared code have no common ancestor below the filesystem root, so a release cannot place them at a relative depth",
			jobDir)
	}

	jobPath, err := relWithin(base, dir)
	if err != nil {
		return Layout{}, err
	}
	layout := Layout{JobPath: filepath.ToSlash(jobPath)}
	for _, tree := range captured {
		rel, err := relWithin(base, tree.Source)
		if err != nil {
			return Layout{}, err
		}
		if rel == "." {
			return Layout{}, fmt.Errorf(
				"release: shared tree %s is the release base; capturing it would place the release inside itself",
				tree.Source)
		}
		tree.Name = filepath.ToSlash(rel)
		layout.Trees = append(layout.Trees, tree)
	}
	sort.Slice(layout.Trees, func(i, j int) bool { return layout.Trees[i].Name < layout.Trees[j].Name })
	for i := 1; i < len(layout.Trees); i++ {
		if layout.Trees[i].Name == layout.Trees[i-1].Name {
			return Layout{}, fmt.Errorf("release: two shared trees land at %q", layout.Trees[i].Name)
		}
	}
	return layout, nil
}

// captureList normalizes the requested trees and drops the ones a release does
// not need to copy separately.
func captureList(jobDir string, trees []SharedTree) ([]SharedTree, error) {
	var captured []SharedTree
	seen := map[string]bool{}
	for _, tree := range trees {
		src, err := absDir(tree.Source)
		if err != nil {
			return nil, fmt.Errorf("release: shared tree: %w", err)
		}
		if within(src, jobDir) {
			// Inside the job: its own copy already carries it.
			continue
		}
		if within(jobDir, src) {
			return nil, fmt.Errorf(
				"release: shared tree %s contains the job directory %s; it cannot be captured separately",
				src, jobDir)
		}
		if seen[src] {
			continue
		}
		seen[src] = true
		captured = append(captured, SharedTree{Source: src})
	}

	// A tree nested inside another captured tree is already carried by the
	// outer copy, so keeping both would copy the same files twice.
	out := make([]SharedTree, 0, len(captured))
	for i, tree := range captured {
		nested := false
		for j, other := range captured {
			if i != j && within(tree.Source, other.Source) {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, tree)
		}
	}
	return out, nil
}

// Digest hashes everything that determines what a release executes: the
// job's own files, the shared code it imports, where each of them lands
// inside the release, and the prepared environment it was validated against.
//
// The placement is part of the hash, not just the contents: moving `lib` to
// `vendor`, or a job from `jobs/<name>` to `<name>`, changes
// every relative import the job performs, so it must produce a
// different release. The prefix names the layout generation, so a snapshot laid
// out by an older implementation can never be reused by this one.
func (l Layout) Digest(jobDir, environmentDigest string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "otter-release-v2\x00%s\x00%s\x00",
		filepath.ToSlash(l.JobPath), environmentDigest)

	if err := hashTree(h, jobDir, jobDir); err != nil {
		return "", err
	}
	// Sorted so the digest does not depend on the order the caller listed them.
	sorted := append([]SharedTree(nil), l.Trees...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, tree := range sorted {
		fmt.Fprintf(h, "shared\x00%s\x00", filepath.ToSlash(tree.Name))
		if err := hashTree(h, tree.Source, tree.Source); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// validate confirms that every placement can be joined onto a release root,
// using the same discipline the resolver applies when reading metadata back.
func (l Layout) validate() error {
	if strings.TrimSpace(l.JobPath) == "" {
		return errors.New("release: layout has no job path")
	}
	fake := filepath.Join(string(os.PathSeparator), "release-root")
	if _, err := safeJoin(fake, l.JobPath); err != nil {
		return err
	}
	for _, tree := range l.Trees {
		if strings.TrimSpace(tree.Source) == "" {
			return errors.New("release: shared tree has no source directory")
		}
		if strings.TrimSpace(tree.Name) == "" {
			return fmt.Errorf("release: shared tree %s has no placement", tree.Source)
		}
		if _, err := safeJoin(fake, tree.Name); err != nil {
			return err
		}
	}
	return nil
}

// absDir makes a path absolute and clean without following symlinks: the
// placement rule is about path geometry, not about where a link points.
func absDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("empty directory")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// within reports whether p is dir or lives inside it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// commonAncestor returns the closest common ancestor of two paths: the deepest
// directory that contains both.
func commonAncestor(a, b string) string {
	a, b = filepath.Clean(a), filepath.Clean(b)
	for {
		switch {
		case a == b:
			return a
		case within(b, a):
			return a
		case within(a, b):
			return b
		}
		parent := filepath.Dir(a)
		if parent == a {
			return a
		}
		a = parent
	}
}

// relWithin is filepath.Rel with the escape check the placement rule needs: a
// path outside base has no placement relative to it.
func relWithin(base, p string) (string, error) {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return "", fmt.Errorf("release: place %s relative to %s: %w", p, base, err)
	}
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("release: %s is not below %s", p, base)
	}
	return rel, nil
}
