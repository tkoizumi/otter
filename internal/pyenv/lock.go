package pyenv

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// uv.lock records, for every package that came from an index, the registry it
// came from:
//
//	[[package]]
//	name = "requests"
//	version = "2.31.0"
//	source = { registry = "https://pypi.org/simple" }
//
// That URL is not decoration, and it is the reason a "mirror passthrough"
// cannot be a redirect:
//
//   - `uv sync --locked` re-resolves under the configured index and refuses
//     when the result would change the lock. A configured index that differs
//     from the recorded one therefore fails with uv's own "The lockfile at
//     `uv.lock` needs to be updated, but `--locked` was provided" (exit 2) --
//     even when the artifacts behind both indexes are byte-identical, and even
//     when the difference is only a trailing slash. Without this file, that
//     failure reaches the operator as a bare exit status after preparation has
//     already passed its network check.
//   - With *no* index configured, uv fetches from the URLs the lock records
//     rather than from PyPI. So the recorded registry is where a lock-only
//     preparation actually goes, which is also what the egress preflight has
//     to check.
//
// The consequence, stated once: the package route is carried by the lock -- and
// the lock is part of the declared inputs of the environment identity -- rather
// than by the preparation policy. A configured index that disagrees with the
// lock is refused before anything is fetched, so the route can never silently
// differ from the identity the lock already pins.

// ErrLockIndexMismatch reports that the lock was created against a different
// package index than the one preparation is configured to use, which
// `uv sync --locked` refuses.
var ErrLockIndexMismatch = errors.New("uv.lock and the configured package index disagree")

// lockRegistryPattern finds the inline `source = { registry = "..." }` table
// uv writes for every registry package. A TOML inline table cannot span lines,
// so matching the line is enough, and the pattern tolerates the field order uv
// happens to use.
var lockRegistryPattern = regexp.MustCompile(`source\s*=\s*\{[^}]*\bregistry\s*=\s*"([^"]*)"`)

// lockRegistries returns the distinct package registries a lock file records,
// in the order they appear. A lock with no registry packages (a project with no
// dependencies, or one that depends only on path, git or URL sources) returns
// nothing.
func lockRegistries(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		match := lockRegistryPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		registry := strings.TrimSpace(match[1])
		if registry == "" || seen[registry] {
			continue
		}
		seen[registry] = true
		out = append(out, registry)
	}
	return out, nil
}

// configuredIndex is the index preparation hands to uv, or "" when it
// configures none. The flag wins; the variables uv itself reads are the
// fallback, because preparation passes its environment through and uv would
// honour them even though otter never set them.
func (m Manager) configuredIndex() string {
	return firstSet(m.Index, os.Getenv("UV_DEFAULT_INDEX"), os.Getenv("UV_INDEX_URL"))
}

// checkLockIndex refuses a lock the configured index cannot satisfy, before
// anything is built.
//
// Nothing is checked when no index is configured: uv then installs from the
// URLs the lock records, which is a working preparation and not a mismatch.
// Nothing is checked either when the lock cannot be read or records no
// registry at all -- preparation must not invent a refusal out of its own
// parse failure, and uv still reports its own error.
func (m Manager) checkLockIndex(dir string) error {
	configured := m.configuredIndex()
	if configured == "" {
		return nil
	}
	lock := filepath.Join(dir, "uv.lock")
	recorded, err := lockRegistries(lock)
	if err != nil || len(recorded) == 0 {
		return nil
	}
	for _, registry := range recorded {
		if equalIndexURL(registry, configured) {
			continue
		}
		return fmt.Errorf(
			"%w: %s was locked against %s, but preparation is configured with %s\n"+
				"hint: --index cannot redirect an existing lock. `uv sync --locked` re-resolves against the\n"+
				"configured index and refuses when the result would change the lock, even when the two\n"+
				"indexes serve identical artifacts. Regenerate the lock against the index you are\n"+
				"deploying with (`uv lock --default-index %s`) and commit it, or prepare against the index\n"+
				"the lock records (`--index %s`, or no --index at all, which makes uv fetch from the URLs\n"+
				"the lock itself records).",
			ErrLockIndexMismatch, lock, registry, configured, configured, registry)
	}
	return nil
}

// packageIndexURLs is where preparation will actually fetch packages from: the
// configured index, or the registries the lock records (uv installs from those
// when no index is configured), or the default index.
func (m Manager) packageIndexURLs(dir string) []string {
	if configured := m.configuredIndex(); configured != "" {
		return []string{configured}
	}
	if dir != "" {
		if recorded, err := lockRegistries(filepath.Join(dir, "uv.lock")); err == nil && len(recorded) > 0 {
			return recorded
		}
	}
	return []string{DefaultPackageIndex}
}

// equalIndexURL reports whether two index URLs are the same index as uv sees
// it. Only the parts uv's own URL parser folds are folded -- scheme and host
// case, a redundant default port -- and the path is compared verbatim. That
// last point is deliberate: uv treats `https://mirror/simple` and
// `https://mirror/simple/` as different indexes, and a check that forgave the
// difference would wave through a sync uv then refuses.
func equalIndexURL(a, b string) bool { return canonicalIndexURL(a) == canonicalIndexURL(b) }

// canonicalIndexURL is equalIndexURL's normal form. A value it cannot parse is
// compared as written.
func canonicalIndexURL(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return raw
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	host, port := strings.ToLower(parsed.Hostname()), parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	switch {
	case port != "":
		parsed.Host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		parsed.Host = "[" + host + "]"
	default:
		parsed.Host = host
	}
	return parsed.String()
}
