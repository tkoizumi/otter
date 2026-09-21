package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tkoizumi/otter/internal/identity"
)

// Observe walks root and reports what is actually there, without minting an
// identifier, writing a marker or touching the registry.
//
// knownPaths are canonical source paths the registry already owns. They are
// observed too, so that a registered directory whose manifest was deleted is
// described explicitly instead of being inferred from its absence in the
// manifest walk. Only a directory that is definitively gone is reported
// Exists=false; an unreadable one is reported as an error and marks the scan
// incomplete, because a failed read is never evidence of deletion.
func Observe(root string, knownPaths []string) (identity.Scan, error) {
	if strings.TrimSpace(root) == "" {
		return identity.Scan{}, fmt.Errorf("integrations directory must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return identity.Scan{}, fmt.Errorf("resolve integrations directory %s: %w", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return identity.Scan{}, fmt.Errorf("integrations directory %s: %w", abs, err)
	}
	if !info.IsDir() {
		return identity.Scan{}, fmt.Errorf("integrations path %s is not a directory", abs)
	}

	scan := identity.Scan{Complete: true}
	seen := map[string]bool{}

	walkErr := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			scan.Complete = false
			scan.Errors = append(scan.Errors, fmt.Sprintf("%s: %v", path, err))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path == abs {
				return nil
			}
			if skippedDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() != identity.ManifestFileName {
			return nil
		}
		obs := observeManifest(path)
		if !seen[obs.Path] {
			seen[obs.Path] = true
			scan.Observations = append(scan.Observations, obs)
		}
		return nil
	})
	if walkErr != nil {
		return identity.Scan{}, fmt.Errorf("walk integrations directory %s: %w", abs, walkErr)
	}

	for _, path := range knownPaths {
		canonical, err := identity.Canonical(path)
		if err != nil {
			canonical = filepath.Clean(path)
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		scan.Observations = append(scan.Observations, observePath(canonical))
	}

	return scan, nil
}

// observePath describes a path the registry knows about.
func observePath(canonical string) identity.Observation {
	obs := identity.Observation{Path: canonical}

	info, err := os.Lstat(canonical)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return obs
		}
		obs.Exists = true
		obs.ScanError = err.Error()
		return obs
	}
	if !info.IsDir() {
		obs.Exists = true
		obs.ScanError = "not a directory"
		return obs
	}
	obs.Exists = true

	manifest := filepath.Join(canonical, identity.ManifestFileName)
	if _, err := os.Lstat(manifest); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			obs.Marker, obs.MarkerID, obs.MarkerError = readMarker(canonical)
			return obs
		}
		obs.ScanError = err.Error()
		return obs
	}
	return observeManifest(manifest)
}

// observeManifest describes one otter.yaml.
func observeManifest(manifestPath string) identity.Observation {
	dir := filepath.Dir(manifestPath)
	canonical, err := identity.Canonical(dir)
	if err != nil {
		canonical = filepath.Clean(dir)
	}

	obs := identity.Observation{Path: canonical, Exists: true, ManifestPresent: true}
	obs.Marker, obs.MarkerID, obs.MarkerError = readMarker(canonical)

	m, err := Load(manifestPath)
	if err != nil {
		obs.ManifestError = err.Error()
		return obs
	}
	obs.Name = m.Name
	if err := m.Validate(); err != nil {
		obs.ManifestError = err.Error()
		return obs
	}
	obs.ManifestValid = true
	return obs
}

// readMarker classifies the marker without ever failing the scan: a marker we
// cannot trust is an observation the planner acts on, not a walk error.
func readMarker(dir string) (identity.MarkerState, identity.ID, string) {
	id, err := identity.ReadMarker(dir)
	switch {
	case err == nil:
		return identity.MarkerValid, id, ""
	case errors.Is(err, identity.ErrNoMarker):
		return identity.MarkerNone, "", ""
	case errors.Is(err, identity.ErrUnsafeMarker):
		return identity.MarkerUnsafe, "", err.Error()
	default:
		return identity.MarkerMalformed, "", err.Error()
	}
}
