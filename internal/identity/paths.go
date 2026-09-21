package identity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Canonical returns the canonical form of a source path.
//
// Identity is bound to a directory, so two spellings of the same directory
// must produce the same key. Symlinks are resolved when the path exists, which
// is what stops an alternate path to the same tree from registering twice.
// A path that does not exist is still cleaned and made absolute: operations on
// a deleted source need to name it, not fail on it.
func Canonical(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("identity: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("identity: resolve %s: %w", path, err)
	}
	abs = filepath.Clean(abs)

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The leaf does not exist yet -- a move destination, say. Resolve
			// the parent instead, so a path under a symlinked directory (macOS
			// /var, for one) still canonicalizes to the same form as a path
			// that does exist.
			parent := filepath.Dir(abs)
			if parent != abs {
				if resolvedParent, perr := filepath.EvalSymlinks(parent); perr == nil {
					return filepath.Join(resolvedParent, filepath.Base(abs)), nil
				}
			}
			return abs, nil
		}
		// A permission error resolving an ancestor is not a reason to invent
		// a path; the caller must see it.
		return "", fmt.Errorf("identity: canonicalize %s: %w", abs, err)
	}
	return filepath.Clean(resolved), nil
}

// Within reports whether path is root or lives inside it. The check is
// path-aware, not a string prefix, so `/srv/int-2` is not inside `/srv/int`.
func Within(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

// SameFile reports whether two existing paths refer to the same filesystem
// object. It is a best-effort alias check: it cannot see through bind mounts
// or network filesystems, which is a documented limitation.
func SameFile(a, b string) bool {
	infoA, err := os.Stat(a)
	if err != nil {
		return false
	}
	infoB, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(infoA, infoB)
}

// IsNested reports whether child is a proper descendant of parent.
func IsNested(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	return parent != child && Within(parent, child)
}
