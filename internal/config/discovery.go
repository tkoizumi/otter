package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Integration is the result of discovery: either a valid, runnable
// integration or an invalid one that must be reported without taking the
// daemon down.
type Integration struct {
	// ID is the integration name from the manifest. When the manifest cannot
	// be parsed at all, the directory name is used so the problem is still
	// addressable.
	ID           string
	Dir          string
	ManifestPath string

	// Manifest is nil when the manifest could not be parsed.
	Manifest *Manifest

	// Valid reports whether the manifest parsed and validated cleanly.
	Valid bool

	// Error explains why the integration is invalid.
	Error string
}

// skippedDirs are directories that never contain integrations and are
// expensive or pointless to walk.
var skippedDirs = map[string]bool{
	".git":          true,
	".hg":           true,
	".svn":          true,
	".cache":        true,
	".venv":         true,
	"venv":          true,
	"node_modules":  true,
	"__pycache__":   true,
	".mypy_cache":   true,
	".pytest_cache": true,
	".tox":          true,
	"dist":          true,
	"build":         true,
}

// Discover recursively finds every otter.yaml under root, loads it and
// validates it. Invalid integrations are returned with Valid=false and an
// Error message rather than causing a failure: a single broken manifest must
// never stop the daemon from serving the rest.
func Discover(root string) ([]*Integration, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("integrations directory must not be empty")
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve integrations directory %s: %w", root, err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("integrations directory %s: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("integrations path %s is not a directory", abs)
	}

	var findings []*Integration

	walkErr := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subdirectory should not abort discovery.
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
		if d.Name() != ManifestFileName {
			return nil
		}
		findings = append(findings, loadIntegration(path))
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk integrations directory %s: %w", abs, walkErr)
	}

	sort.Slice(findings, func(i, j int) bool { return findings[i].Dir < findings[j].Dir })

	// Names must be unique: they are the identifier used by the API, the CLI
	// and durable state. Later duplicates are marked invalid instead of
	// silently shadowing the first.
	seen := map[string]string{}
	for _, it := range findings {
		if !it.Valid {
			continue
		}
		if first, dup := seen[it.ID]; dup {
			it.Valid = false
			it.Error = fmt.Sprintf("duplicate integration name %q: already defined in %s", it.ID, first)
			continue
		}
		seen[it.ID] = it.Dir
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].ID != findings[j].ID {
			return findings[i].ID < findings[j].ID
		}
		return findings[i].Dir < findings[j].Dir
	})

	return findings, nil
}

func loadIntegration(manifestPath string) *Integration {
	it := &Integration{
		Dir:          filepath.Dir(manifestPath),
		ManifestPath: manifestPath,
	}

	m, err := Load(manifestPath)
	if err != nil {
		it.ID = filepath.Base(it.Dir)
		it.Error = err.Error()
		return it
	}
	it.ID = m.Name
	if it.ID == "" {
		it.ID = filepath.Base(it.Dir)
	}
	it.Manifest = m

	if err := m.Validate(); err != nil {
		it.Error = err.Error()
		return it
	}

	it.Valid = true
	return it
}
