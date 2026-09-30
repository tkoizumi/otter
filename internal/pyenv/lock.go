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

// configuredIndex is the index preparation will resolve packages against, or ""
// when it configures none. The flag wins; the variables uv itself reads are the
// fallback, because preparation passes its environment through and uv would
// honour them even though otter never set them.
func (m Manager) configuredIndex() string {
	return firstSet(m.Index, os.Getenv("UV_DEFAULT_INDEX"), os.Getenv("UV_INDEX_URL"))
}

// additiveRouteVars are the variables that add a package source without
// replacing the default index. Preparation passes its environment through to
// uv, so these reach the resolver even though otter never set them, and a lock
// cannot be checked against them: with a lock that records a registry, all
// three turn `uv sync --locked` into the bare "The lockfile ... needs to be
// updated" exit 2 that this file exists to replace (measured with uv 0.5.9 in
// docs/evidence/phase-0/2026-09-30-p0-08-egress-managed-python.txt).
//
// UV_NO_INDEX is deliberately not here: with no index configured uv installs
// from the URLs the lock records, and disabling an index that is not used
// changes nothing.
var additiveRouteVars = []string{"UV_INDEX", "UV_EXTRA_INDEX_URL", "UV_FIND_LINKS"}

// checkLockIndex refuses a route the lock cannot satisfy, before anything is
// built.
//
// Nothing is checked when the lock cannot be read or records no registry at
// all: preparation must not invent a refusal out of its own parse failure, and
// with no registry package there is no source an index could change.
//
// When the lock does record registries, a configured index must match them, and
// an additive-source variable must not be set at all: either one makes `uv sync
// --locked` re-resolve into a different lock, which uv reports as a bare exit 2.
// With neither, nothing is checked: uv installs from the URLs the lock records,
// which is a working preparation and not a mismatch.
func (m Manager) checkLockIndex(dir string) error {
	lock := filepath.Join(dir, "uv.lock")
	recorded, err := lockRegistries(lock)
	if err != nil || len(recorded) == 0 {
		return nil
	}
	if added := setVars(additiveRouteVars); len(added) > 0 {
		names := strings.Join(added, " and ")
		return fmt.Errorf(
			"%w: %s is set, which adds a package source the lock cannot be checked against; %s records %s\n%s",
			ErrLockIndexMismatch, names, lock, strings.Join(recorded, ", "), additiveHint(names, recorded))
	}
	configured := m.configuredIndex()
	if configured == "" {
		return nil
	}
	for _, registry := range recorded {
		if equalIndexURL(registry, configured) {
			continue
		}
		return fmt.Errorf(
			"%w: %s was locked against %s, but preparation is configured with %s\n%s",
			ErrLockIndexMismatch, lock, registry, configured, mismatchHint(configured, registry))
	}
	return nil
}

// setVars returns the named environment variables that are set to something.
func setVars(names []string) []string {
	var set []string
	for _, name := range names {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			set = append(set, name)
		}
	}
	return set
}

// mismatchHint is the actionable half of an index/lock disagreement. Both ways
// out terminate: following the lock needs no lock change, and re-locking names
// the very spelling uv records, which this check compares canonically.
func mismatchHint(configured, recorded string) string {
	return "hint: --index cannot redirect an existing lock. `uv sync --locked` re-resolves against the\n" +
		"configured index and refuses when the result would change the lock, even when the two\n" +
		"indexes serve identical artifacts. Prepare against the index the lock records (--index " + recorded + ",\n" +
		"or no --index at all, which makes uv fetch from the URLs the lock itself records), or\n" +
		"regenerate the lock against the index you are deploying with (uv lock --default-index " + configured + ")\n" +
		"and commit it: uv records that index in the canonical spelling this check compares."
}

// additiveHint is the actionable half of an additive-source refusal. Unsetting
// the variable is the step that terminates in every case; re-locking only helps
// if the same source is then configured with --index instead.
func additiveHint(names string, recorded []string) string {
	return "hint: `uv sync --locked` re-resolves with that added source and refuses when the result\n" +
		"would change the lock -- the failure this check exists to explain. Unset " + names + " and prepare\n" +
		"against the index the lock records (--index " + recorded[0] + ", or no --index at all), or re-lock\n" +
		"against the source you actually need (uv lock --default-index <url>) and configure it with\n" +
		"--index <url>, which preparation can check."
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
// it. What is folded is what uv 0.5.9 was measured to fold (docs/evidence/
// phase-0/2026-09-30-p0-08-egress-managed-python.txt): scheme and host case, a
// redundant default port, the root slash uv gives an empty path, and dot
// segments. What is not folded matters just as much: a non-empty trailing slash
// and a percent escape are both significant, and forgiving either would wave
// through a sync uv then refuses.
func equalIndexURL(a, b string) bool { return canonicalIndexURL(a) == canonicalIndexURL(b) }

// canonicalIndexURL is equalIndexURL's normal form: the URL as uv itself would
// have it after parsing. A value it cannot parse is compared as written.
func canonicalIndexURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return strings.TrimSpace(raw)
	}
	scheme := strings.ToLower(parsed.Scheme)
	host, port := strings.ToLower(parsed.Hostname()), parsed.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	switch {
	case port != "":
		host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		host = "[" + host + "]"
	}
	path := removeDotSegments(upperPercentEscapes(parsed.EscapedPath()))
	if path == "" {
		path = "/"
	}
	var out strings.Builder
	out.WriteString(scheme + "://")
	if parsed.User != nil {
		out.WriteString(parsed.User.String() + "@")
	}
	out.WriteString(host)
	out.WriteString(path)
	if parsed.RawQuery != "" {
		out.WriteString("?" + parsed.RawQuery)
	}
	if parsed.Fragment != "" {
		out.WriteString("#" + parsed.Fragment)
	}
	return out.String()
}

// removeDotSegments applies RFC 3986 section 5.2.4 to a path. Rust's URL parser
// does this when it parses, so uv resolves `http://host/a/../b` to
// `http://host/b`; a check that did not would refuse a configured index uv
// accepts. Only literal `.` and `..` segments are removed, and empty segments
// are left alone -- a doubled slash is a different path.
func removeDotSegments(path string) string {
	var out strings.Builder
	for in := path; in != ""; {
		switch {
		case strings.HasPrefix(in, "../"):
			in = in[3:]
		case strings.HasPrefix(in, "./"):
			in = in[2:]
		case strings.HasPrefix(in, "/./"):
			in = "/" + in[3:]
		case in == "/.":
			in = "/"
		case strings.HasPrefix(in, "/../"):
			in = "/" + in[4:]
			trimLastSegment(&out)
		case in == "/..":
			in = "/"
			trimLastSegment(&out)
		case in == "." || in == "..":
			in = ""
		default:
			start := 0
			if in[0] == '/' {
				start = 1
			}
			next := strings.IndexByte(in[start:], '/')
			if next < 0 {
				out.WriteString(in)
				in = ""
			} else {
				out.WriteString(in[:start+next])
				in = in[start+next:]
			}
		}
	}
	return out.String()
}

// trimLastSegment drops the last path segment and the slash before it.
func trimLastSegment(out *strings.Builder) {
	written := out.String()
	out.Reset()
	if i := strings.LastIndexByte(written, '/'); i > 0 {
		out.WriteString(written[:i])
	}
}

// upperPercentEscapes uppercases the hex digits of percent escapes, which is
// how Rust's URL parser serializes them (`%2f` and `%2F` are one escape). It
// deliberately does not decode them: uv treats `/sim%70le` and `/simple` as
// different indexes, so decoding here would accept a sync uv refuses.
func upperPercentEscapes(path string) string {
	isHex := func(c byte) bool {
		return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
	}
	var out strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] == '%' && i+2 < len(path) && isHex(path[i+1]) && isHex(path[i+2]) {
			out.WriteByte('%')
			out.WriteByte(upperHex(path[i+1]))
			out.WriteByte(upperHex(path[i+2]))
			i += 2
			continue
		}
		out.WriteByte(path[i])
	}
	return out.String()
}

func upperHex(c byte) byte {
	if c >= 'a' && c <= 'f' {
		return c - 'a' + 'A'
	}
	return c
}
