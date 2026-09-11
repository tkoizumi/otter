// Package sdk embeds the Otter Python SDK into the daemon binary and
// extracts it into the data directory at startup.
//
// Embedding is what makes Otter a genuinely single binary: `import otter`
// works for every integration without a pip install, and the SDK version
// always matches the daemon version.
package sdk

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DirName is the directory (relative to the data directory) that the SDK is
// extracted into.
const DirName = "sdk"

// FS holds the Python SDK sources under the python/ prefix.
//
// The `all:` prefix is required: a bare directory pattern silently skips files
// whose names begin with "." or "_", which would omit __init__.py and
// _client.py and break `import otter` for every integration.
//
//go:embed all:python
var FS embed.FS

// Extract writes the embedded SDK into <dataDir>/sdk/python and returns the
// directory to prepend to PYTHONPATH.
//
// The returned path is absolute: integration processes run with their working
// directory set to the integration directory, so a relative PYTHONPATH would
// resolve somewhere else entirely.
//
// Files are only rewritten when their contents differ, so restarting the
// daemon is cheap.
func Extract(dataDir string) (string, error) {
	if dataDir == "" {
		return "", fmt.Errorf("sdk: data directory must not be empty")
	}

	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("sdk: resolve data directory %s: %w", dataDir, err)
	}

	root := filepath.Join(absDataDir, DirName)
	target := filepath.Join(root, "python")

	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", fmt.Errorf("sdk: create %s: %w", target, err)
	}

	err = fs.WalkDir(FS, "python", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Defensive: bytecode caches must never reach the data directory. The
		// build drops them before embedding, and children run with
		// PYTHONDONTWRITEBYTECODE=1 so they are not recreated.
		if d.IsDir() && d.Name() == "__pycache__" {
			return fs.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".pyc") {
			return nil
		}

		dest := filepath.Join(root, filepath.FromSlash(path))
		if d.IsDir() {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", dest, err)
			}
			return nil
		}

		data, err := fs.ReadFile(FS, path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}

		if existing, err := os.ReadFile(dest); err == nil && bytes.Equal(existing, data) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(dest), err)
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dest, err)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("sdk: extract into %s: %w", target, err)
	}

	return target, nil
}
