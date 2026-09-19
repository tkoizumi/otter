package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tkoizumi/otter/internal/config"
)

// This file turns what a developer typed into the integration id the daemon
// knows.
//
// Integrations are addressed by the `name` in their manifest, which is the
// right identifier for the API and for durable state but not always the thing
// in hand: after `cd integrations/shopify-to-erp`, the thing in hand is the
// directory. Making the CLI accept that directory is what lets `otter run .`
// work the way every other build tool does, without the daemon learning about
// paths or the manifest name becoming positional data.

// resolveIntegrationRef maps an integration reference to its integration id.
//
// A bare name is passed through byte for byte, so nothing about the existing
// `otter run counter` spelling changes. A reference that can only be a
// filesystem path -- ".", "..", "../other", an absolute path, anything with a
// separator, or a literal otter.yaml -- is resolved locally: the manifest is
// read and its `name` is used. Because the daemon discovers integrations from
// the same manifests, a resolved name is exactly the id it is registered
// under, and the daemon gives its own clear answer when the manifest is not
// part of the library it serves.
func resolveIntegrationRef(ref string) (string, error) {
	id, _, err := resolveIntegrationRefDir(ref)
	return id, err
}

// resolveIntegrationRefDir is resolveIntegrationRef plus the directory the
// reference names, for commands that act on the source tree rather than on a
// running daemon.
//
// A bare name returns an empty dir: nothing local knows where an integration
// with that name lives until the workspace is discovered, so the caller decides
// whether to search or to refuse. A path reference is read from disk, and its
// directory is returned so a release snapshots exactly the tree that was named
// instead of searching for a second copy of it.
func resolveIntegrationRefDir(ref string) (id, dir string, err error) {
	if !looksLikePath(ref) {
		return ref, "", nil
	}

	// Relative references are resolved against the working directory, not
	// against the process's own idea of it, so the lookup is testable without
	// changing the process-wide directory.
	target := ref
	if !filepath.IsAbs(target) {
		wd, err := workingDirForTest()
		if err != nil {
			return "", "", err
		}
		target = filepath.Join(wd, ref)
	}
	target = filepath.Clean(target)

	info, err := os.Stat(target)
	if err != nil {
		return "", "", fmt.Errorf("%s: no such file or directory", ref)
	}

	manifest := target
	if info.IsDir() {
		manifest = filepath.Join(target, config.ManifestFileName)
		if _, err := os.Stat(manifest); err != nil {
			return "", "", fmt.Errorf("%s is not an integration: no %s", ref, config.ManifestFileName)
		}
	}

	// Load, not LoadAndValidate: an invalid manifest still names its
	// integration, and the daemon already holds the authoritative reason it
	// cannot run. Parsing locally is only here to recover the id.
	m, err := config.Load(manifest)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(m.Name) == "" {
		return "", "", fmt.Errorf("%s does not declare a name", manifest)
	}
	return m.Name, filepath.Dir(manifest), nil
}

// looksLikePath reports whether ref can only be a filesystem path.
//
// The distinction matters because it decides what a bare word means. It cannot
// shadow an integration name: the manifest name rule requires a leading letter
// or digit, so no name may be "." or ".." or contain a separator. A literal
// otter.yaml is included so `otter run otter.yaml` reads like `otter run .`.
func looksLikePath(ref string) bool {
	switch {
	case ref == "." || ref == "..":
		return true
	case ref == "":
		return false
	case filepath.IsAbs(ref):
		return true
	case strings.ContainsAny(ref, `/\`):
		return true
	case filepath.Base(ref) == config.ManifestFileName:
		return true
	}
	return false
}
