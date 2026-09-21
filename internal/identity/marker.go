package identity

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MarkerFileName is the source marker inside every registered integration
// directory. It is written by the runtime, never authored by hand, and is
// excluded from release snapshots and deployment revisions.
const MarkerFileName = ".otter-id"

// markerTempPrefix is the prefix for the temporary file used to replace a
// marker atomically. It is exported through MarkerTempPrefix so the release
// and deployment systems can exclude in-flight temporary markers too.
const markerTempPrefix = ".otter-id.tmp-"

// MarkerTempPrefix is the temporary-marker filename prefix.
const MarkerTempPrefix = markerTempPrefix

// ErrNoMarker reports a directory that has no marker file.
var ErrNoMarker = errors.New("identity: no marker")

// ErrUnsafeMarker reports a marker that exists but is not a plain regular
// file: a symlink, a directory, a FIFO, or a multiply-linked file. Otter
// refuses to read or replace such a marker rather than following it.
var ErrUnsafeMarker = errors.New("identity: unsafe marker")

// MarkerPath returns the marker path inside dir.
func MarkerPath(dir string) string { return filepath.Join(dir, MarkerFileName) }

// ReadMarker reads the identifier recorded in dir.
//
// It returns ErrNoMarker when there is no marker at all, and ErrUnsafeMarker
// when the marker is not a regular file owned outright by this path. The
// checks are applied to the opened descriptor, not only to a pathname lookup,
// so a symlink swapped in between the check and the read cannot redirect it.
func ReadMarker(dir string) (ID, error) {
	path := MarkerPath(dir)

	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNoMarker
		}
		return "", fmt.Errorf("identity: stat marker %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: %s is a symlink", ErrUnsafeMarker, path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s is not a regular file", ErrUnsafeMarker, path)
	}
	if err := rejectHardLinked(info); err != nil {
		return "", fmt.Errorf("%w: %s", err, path)
	}

	f, err := openNoFollow(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNoMarker
		}
		return "", fmt.Errorf("identity: open marker %s: %w", path, err)
	}
	defer f.Close()

	opened, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("identity: stat opened marker %s: %w", path, err)
	}
	if !opened.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s changed type during open", ErrUnsafeMarker, path)
	}
	if !os.SameFile(info, opened) {
		// The pathname was replaced between the check and the open. Refuse:
		// the marker we validated is not the marker we would read.
		return "", fmt.Errorf("%w: %s changed during open", ErrUnsafeMarker, path)
	}

	body, err := io.ReadAll(io.LimitReader(f, MaxIDLength+2))
	if err != nil {
		return "", fmt.Errorf("identity: read marker %s: %w", path, err)
	}
	id, err := ParseMarkerBody(body)
	if err != nil {
		return "", fmt.Errorf("identity: %s: %w", path, err)
	}
	return id, nil
}

// MarkerExists reports whether a marker file is present, without validating
// it. Callers that need the identifier must use ReadMarker.
func MarkerExists(dir string) bool {
	_, err := os.Lstat(MarkerPath(dir))
	return err == nil
}

// WriteMarker records id in dir atomically.
//
// The identifier is written to a temporary file in the same directory, flushed
// to stable storage, and renamed over the marker, so a crash never leaves a
// half-written marker that a later scan would misread. An existing marker that
// is a symlink or otherwise unsafe is refused rather than replaced.
func WriteMarker(dir string, id ID) error {
	if id.IsZero() {
		return fmt.Errorf("identity: refusing to write an empty marker in %s", dir)
	}
	if _, err := Parse(id.String()); err != nil {
		return fmt.Errorf("identity: refusing to write marker in %s: %w", dir, err)
	}

	path := MarkerPath(dir)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: refusing to replace symlink %s", ErrUnsafeMarker, path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: refusing to replace non-regular file %s", ErrUnsafeMarker, path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("identity: stat marker %s: %w", path, err)
	}

	tmp, err := os.CreateTemp(dir, markerTempPrefix)
	if err != nil {
		return fmt.Errorf("identity: create temporary marker in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Remove the temporary file on every failure path. After a successful
	// rename it no longer exists, and the error is ignored on purpose.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("identity: set marker permissions in %s: %w", dir, err)
	}
	if _, err := tmp.Write(MarkerBody(id)); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("identity: write temporary marker in %s: %w", dir, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("identity: sync temporary marker in %s: %w", dir, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("identity: close temporary marker in %s: %w", dir, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("identity: replace marker %s: %w", path, err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("identity: sync directory %s: %w", dir, err)
	}
	return nil
}

// RemoveMarker deletes dir's marker. A missing marker is not an error. An
// unsafe marker is refused.
func RemoveMarker(dir string) error {
	path := MarkerPath(dir)
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("identity: stat marker %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: refusing to remove symlink %s", ErrUnsafeMarker, path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: refusing to remove non-regular file %s", ErrUnsafeMarker, path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("identity: remove marker %s: %w", path, err)
	}
	return syncDir(dir)
}

// IsMarkerTempName reports whether a filename is one of the temporary files
// WriteMarker creates. Release staging and deployment use it to skip
// leftover temporary markers.
func IsMarkerTempName(name string) bool {
	return len(name) > len(markerTempPrefix) && name[:len(markerTempPrefix)] == markerTempPrefix
}
