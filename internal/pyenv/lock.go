package pyenv

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
// uv, so these reach the resolver even though otter never set them.
//
// They are only a problem when the lock does not already record what they name:
// a lock cut with the same extra source records it, and uv then syncs with the
// variable set (measured: a lock recording both a default and an extra index,
// `--default-index A` alone exits 1, `--default-index A` with the extra source
// in the variable exits 0). So the refusal below is conditional on the named
// source not being recorded.
//
// UV_NO_INDEX is deliberately not here: with no index configured uv installs
// from the URLs the lock records, and disabling an index that is not used
// changes nothing.
var additiveRouteVars = []string{"UV_INDEX", "UV_EXTRA_INDEX_URL", "UV_FIND_LINKS"}

// additiveSourceValues returns every source the additive variables name, in
// declaration order. uv takes comma-separated values for these variables, so
// that is the split here; a value containing a comma would be split too, which
// can only make preparation refuse a spelling it cannot check. Together with
// checkLockIndex this is why nothing is checked when no route is configured.
//
// uv also accepts a named index, `name=url`, and that spelling is unwrapped
// here: the part after `=` must be an absolute URL and the part before it a
// valid index name, so a find-links directory containing `=` is not misread.
func additiveSourceValues() []string {
	var out []string
	for _, name := range additiveRouteVars {
		for _, value := range strings.Split(os.Getenv(name), ",") {
			if value = strings.TrimSpace(value); value != "" {
				out = append(out, unwrapNamedIndex(value))
			}
		}
	}
	return out
}

// indexNamePattern is what uv accepts as an index name in the `name=url`
// spelling.
var indexNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// unwrapNamedIndex turns uv's `name=url` index spelling into its URL, and leaves
// anything else -- including a plain URL or a path -- alone.
func unwrapNamedIndex(value string) string {
	name, rest, ok := strings.Cut(value, "=")
	if !ok || name == "" || !indexNamePattern.MatchString(name) {
		return value
	}
	parsed, err := url.Parse(rest)
	if err != nil || parsed.Scheme == "" || (parsed.Host == "" && parsed.Scheme != "file") {
		return value
	}
	return rest
}

// checkLockIndex refuses a route the lock cannot satisfy, before anything is
// built.
//
// Nothing is checked when the lock cannot be read or records no registry at
// all: preparation must not invent a refusal out of its own parse failure, and
// with no registry package there is no source an index could change. Nothing is
// checked either when no route is configured: uv then installs from the URLs the
// lock records, which is a working preparation.
//
// Otherwise two things must hold, and both are measured against uv 0.5.9:
//
//   - no added source may be one the lock does not record. This is deliberately
//     conservative: uv refuses an unrecorded source only when it changes the
//     resolution (measured both ways: an unrecorded index carrying a package
//     the lock needs exits 2, an unrecorded empty one exits 0), and preparation
//     cannot resolve, so it cannot tell the two apart from the lock. It refuses
//     both rather than let uv fail with a bare "lockfile needs to be updated",
//     and says so; the divergences it accepts are listed in
//     TestDeliberateConservativeDivergences. A source the lock *does* record is
//     accepted: a lock cut with a default and an extra index records both, and
//     the variable then supplies what `--index` alone cannot.
//   - every registry the lock records must be reachable from the configured
//     route: the configured index, or uv's default index when none is
//     configured, plus the added sources. A lock recording two registries with
//     only one supplied is a resolution failure uv reports as "not found in the
//     package registry".
func (m Manager) checkLockIndex(dir string) error {
	lock := filepath.Join(dir, "uv.lock")
	recorded, err := lockRegistries(lock)
	if err != nil || len(recorded) == 0 {
		return nil
	}
	configured := m.configuredIndex()
	added := setVars(additiveRouteVars)
	if configured == "" && len(added) == 0 {
		return nil
	}
	lockDir := filepath.Dir(lock)
	if unrecorded := unrecordedSources(lock, recorded); len(unrecorded) > 0 {
		return fmt.Errorf(
			"%w: %s names %s, which the lock does not record; %s records %s\n%s",
			ErrLockIndexMismatch, strings.Join(added, " and "), strings.Join(unrecorded, ", "),
			lock, strings.Join(recorded, ", "), additiveHint(strings.Join(added, " and "), unrecorded, recorded))
	}
	sources := make([]string, 0, len(recorded)+1)
	if configured != "" {
		sources = append(sources, configured)
	} else {
		// uv always has a default index in play unless it is disabled, and with
		// no flag configured that default is PyPI.
		sources = append(sources, DefaultPackageIndex)
	}
	sources = append(sources, additiveSourceValues()...)
	for _, registry := range recorded {
		if anySourceMatches(sources, registry, lockDir) {
			continue
		}
		if len(recorded) == 1 {
			return fmt.Errorf(
				"%w: %s was locked against %s, but preparation is configured with %s\n%s",
				ErrLockIndexMismatch, lock, registry, sources[0], mismatchHint(sources[0], recorded))
		}
		return fmt.Errorf(
			"%w: %s records %s as well, which this route does not supply; preparation has %s\n%s",
			ErrLockIndexMismatch, lock, registry, strings.Join(sources, ", "), extraSourceHint(registry))
	}
	return nil
}

// anySourceMatches reports whether a recorded registry is one of the configured
// sources.
func anySourceMatches(sources []string, registry, lockDir string) bool {
	for _, source := range sources {
		if sourceMatches(source, registry, lockDir) {
			return true
		}
	}
	return false
}

// unrecordedSources returns the additive sources the lock does not record.
func unrecordedSources(lock string, recorded []string) []string {
	var out []string
	for _, value := range additiveSourceValues() {
		recordedSource := false
		for _, registry := range recorded {
			if sourceMatches(value, registry, filepath.Dir(lock)) {
				recordedSource = true
				break
			}
		}
		if !recordedSource {
			out = append(out, value)
		}
	}
	return out
}

// sourceMatches reports whether a configured source and a recorded registry are
// the same source. URL forms are compared the way uv parses them; anything else
// is a filesystem path -- which is what a find-links lock records, relative to
// the lock file -- and is compared after resolving both against the lock's
// directory.
func sourceMatches(value, registry, lockDir string) bool {
	if equalIndexURL(value, registry) {
		return true
	}
	if strings.Contains(value, "://") || strings.Contains(registry, "://") {
		return false
	}
	resolve := func(path string) string {
		if !filepath.IsAbs(path) {
			path = filepath.Join(lockDir, path)
		}
		return filepath.Clean(path)
	}
	return resolve(value) == resolve(registry)
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

// mismatchHint is the actionable half of an index/lock disagreement. The
// primary remedy is the one that always satisfies --locked -- omitting --index
// makes uv install from the URLs the lock itself records -- because re-locking
// is only as good as this comparison's model of uv's URL parser.
func mismatchHint(configured string, recorded []string) string {
	return "hint: --index cannot redirect an existing lock. `uv sync --locked` re-resolves against the\n" +
		"configured index and refuses when the result would change the lock, even when the two\n" +
		"indexes serve identical artifacts. Omit --index and uv installs from the URLs the lock\n" +
		"records (" + strings.Join(recorded, ", ") + "), which always satisfies --locked; or re-lock against\n" +
		"the index you are deploying with (uv lock --default-index " + configured + ") and commit it."
}

// additiveHint is the actionable half of an additive-source refusal. Every
// remedy here terminates: omitting the variable always satisfies --locked, and
// re-locking only helps when the added source really supplies a package (uv
// records a registry only for packages that came from it), which the hint says.
// The earlier wording promised that re-locking alone would be accepted, which
// looped for a source that supplies nothing.
func additiveHint(names string, unrecorded, recorded []string) string {
	return "hint: otter cannot tell from the lock whether this source would change the resolution, so it\n" +
		"refuses rather than let `uv sync --locked` fail with a bare lockfile error. Omit " + names + " and\n" +
		"--index, and uv installs from the URLs the lock records (" + strings.Join(recorded, ", ") + "), which always\n" +
		"satisfies --locked. If the added source really does supply a package the lock needs, re-lock with\n" +
		"it configured (" + names + "=" + unrecorded[0] + " uv lock ...) so the lock records that source, and\n" +
		"preparation accepts a source the lock records."
}

// extraSourceHint is the actionable half of "the lock records a source this
// route does not supply". Supplying it with the variable uv reads is a
// terminating remedy precisely because the lock records it.
func extraSourceHint(registry string) string {
	return "hint: the lock was cut with more than one source. Supply the missing one with the variable\n" +
		"uv reads (UV_EXTRA_INDEX_URL=" + registry + " or UV_INDEX=" + registry + "), which preparation\n" +
		"accepts because the lock records it, or omit --index and the added variables and uv installs\n" +
		"from the URLs the lock records."
}

// packageIndexURLs is where preparation will actually fetch packages from: the
// configured index and any added source that is a URL, or -- when it configures
// none -- the registries the lock records, which is where uv installs from.
func (m Manager) packageIndexURLs(dir string) []string {
	var out []string
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return
		}
		for _, existing := range out {
			if equalIndexURL(existing, candidate) {
				return
			}
		}
		out = append(out, candidate)
	}
	// The operator's spelling is canonicalised before it is probed: uv parses a
	// backslash as a slash, decodes a percent-encoded host and normalises a
	// port, while an HTTP client handed the raw string can refuse it outright.
	// Probing the canonical form is probing what will actually be fetched.
	add(canonicalIndexURL(m.configuredIndex()))
	for _, value := range additiveSourceValues() {
		if strings.Contains(value, "://") {
			add(canonicalIndexURL(value))
		}
	}
	if len(out) > 0 {
		return out
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
// phase-0/2026-09-30-p0-08-egress-managed-python.txt, ROUND 3 to ROUND 5):
//
//   - host case, and nothing else about the case of the URL: uv cannot use an
//     upper-case scheme, and `%2f` and `%2F` are different indexes;
//   - a redundant default port, a port's leading zeros, and a backslash where a
//     special scheme (http, https, file) requires a slash;
//   - credentials: a lock recording `http://u:p@host/simple` matches a
//     configured `http://host/simple`, and mismatched credentials too;
//   - the root slash uv gives an empty path, dot segments in literal and
//     percent-encoded spelling, and the WHATWG IPv4 spellings (short forms,
//     hex and octal parts, a percent-encoded host);
//   - the case of a percent escape is preserved (see above), and so are a
//     non-empty trailing slash, an empty query (`…/simple?`), an empty fragment
//     (`…/simple#`) and a doubled slash.
//
// Known deliberate divergences, where otter refuses a pair uv accepts, live in
// TestDeliberateConservativeDivergences rather than here: a Unicode IDNA host.
func equalIndexURL(a, b string) bool { return canonicalIndexURL(a) == canonicalIndexURL(b) }

// canonicalIndexURL is equalIndexURL's normal form: the URL as uv itself would
// have it after parsing. A value it cannot parse is compared as written.
func canonicalIndexURL(raw string) string {
	raw = strings.TrimSpace(raw)
	scheme := ""
	if colon := strings.Index(raw, ":"); colon > 0 {
		scheme = raw[:colon]
	}
	// WHATWG treats a backslash as a slash in a special URL, and uv does too
	// (measured), so normalise before parsing: Go's parser would otherwise read
	// the backslashes as part of the authority.
	if specialScheme(scheme) {
		raw = strings.ReplaceAll(raw, "\\", "/")
	}
	// Go's parser rejects a percent escape in a host outright, while WHATWG
	// decodes the host before parsing it, so the host is repaired first.
	raw = decodeRawHost(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || scheme == "" {
		return raw
	}
	host, port := strings.ToLower(parsed.Hostname()), parsed.Port()
	lowerScheme := strings.ToLower(scheme)
	if port != "" {
		// WHATWG parses a port numerically, so `08080` and `8080` are the same
		// port.
		port = strings.TrimLeft(port, "0")
		if port == "" {
			port = "0"
		}
	}
	if (lowerScheme == "https" && port == "443") || (lowerScheme == "http" && port == "80") {
		port = ""
	}
	host = canonicalIPv4(decodeHost(host))
	switch {
	case port != "":
		host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		host = "[" + host + "]"
	}
	path := removeDotSegments(parsed.EscapedPath())
	if path == "" {
		path = "/"
	}
	var out strings.Builder
	out.WriteString(scheme + "://")
	// Credentials are deliberately not part of the comparison: uv folds them
	// away, measured in both the "same credentials" and "different credentials"
	// directions.
	out.WriteString(host)
	out.WriteString(path)
	if parsed.RawQuery != "" || parsed.ForceQuery {
		out.WriteString("?" + parsed.RawQuery)
	}
	// Go has no ForceFragment, so an empty fragment is detected from the string
	// as written. uv records `…#` and treats it as a different index from `…`
	// (measured), and dropping it accepted a pair uv refuses.
	if strings.Contains(raw, "#") {
		out.WriteString("#" + parsed.EscapedFragment())
	}
	return out.String()
}

// specialScheme reports whether a scheme is one the WHATWG URL standard treats
// as "special", where a backslash is a slash. Index URLs are http, https or
// file in practice, and uv applies the rule to all three.
func specialScheme(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "http", "https", "file", "ftp", "ws", "wss":
		return true
	}
	return false
}

// decodeRawHost percent-decodes the host component of a raw URL before Go parses
// it, leaving the userinfo, the port and everything after the authority alone.
// A host it cannot decode safely is returned unchanged, which leaves the URL
// unparsable and therefore refused.
func decodeRawHost(raw string) string {
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 0 {
		return raw
	}
	rest := raw[schemeEnd+3:]
	end := strings.IndexAny(rest, "/?#")
	authority, tail := rest, ""
	if end >= 0 {
		authority, tail = rest[:end], rest[end:]
	}
	if !strings.Contains(authority, "%") {
		return raw
	}
	userinfo := ""
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		userinfo, authority = authority[:at+1], authority[at+1:]
	}
	host, port := authority, ""
	if !strings.HasPrefix(host, "[") {
		if colon := strings.LastIndex(host, ":"); colon >= 0 {
			host, port = host[:colon], host[colon:]
		}
	}
	return raw[:schemeEnd+3] + userinfo + decodeHost(host) + port + tail
}

// decodeHost percent-decodes a host the way WHATWG does before it is parsed as
// an address or an IPv4 literal. A host whose decoded form contains anything
// outside the host character set is returned unchanged, and IDNA (a Unicode
// host) is deliberately not implemented: both can only refuse a spelling uv
// accepts, never accept one it refuses.
func decodeHost(host string) string {
	if !strings.Contains(host, "%") {
		return host
	}
	var out strings.Builder
	for i := 0; i < len(host); i++ {
		if host[i] == '%' && i+2 < len(host) {
			hi, ok1 := hexValue(host[i+1])
			lo, ok2 := hexValue(host[i+2])
			if ok1 && ok2 {
				out.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		out.WriteByte(host[i])
	}
	decoded := out.String()
	for i := 0; i < len(decoded); i++ {
		c := decoded[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '-' || c == '_' || c == '~':
		default:
			return host
		}
	}
	return decoded
}

func hexValue(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// removeDotSegments applies the path normalization RFC 3986 section 5.2.4 and
// the WHATWG URL standard share, which is what Rust's parser (and therefore uv)
// implements: literal `.` and `..` segments are removed, and so are their
// percent-encoded spellings. `...` is a literal segment, as uv measured. The
// rest is kept verbatim, including a doubled slash and a trailing slash.
func removeDotSegments(path string) string {
	if path == "" {
		return ""
	}
	absolute := strings.HasPrefix(path, "/")
	segments := strings.Split(path, "/")
	out := make([]string, 0, len(segments))
	trailingSlash := false
	for i, segment := range segments {
		last := i == len(segments)-1
		switch {
		case i == 0 && absolute:
			out = append(out, "") // the leading slash
		case isDotSegment(segment):
			if last {
				trailingSlash = true
			}
		case isDoubleDotSegment(segment):
			// Pop the last real segment. The leading marker of an absolute
			// path is not a segment and cannot be popped.
			if len(out) > 0 && !(absolute && len(out) == 1 && out[0] == "") {
				out = out[:len(out)-1]
			}
			if last {
				trailingSlash = true
			}
		default:
			out = append(out, segment)
		}
	}
	result := strings.Join(out, "/")
	if trailingSlash && (result != "" || absolute) && !strings.HasSuffix(result, "/") {
		result += "/"
	}
	return result
}

// isDotSegment and isDoubleDotSegment are the WHATWG definitions, which are
// ASCII case-insensitive about the percent-encoded spelling. `...` is neither.
func isDotSegment(segment string) bool {
	return segment == "." || strings.EqualFold(segment, "%2e")
}

func isDoubleDotSegment(segment string) bool {
	switch strings.ToLower(segment) {
	case "..", ".%2e", "%2e.", "%2e%2e":
		return true
	}
	return false
}

// canonicalIPv4 folds the WHATWG IPv4 spellings uv's parser folds -- one to
// four numeric parts, each decimal, hexadecimal (`0x…`) or octal (a leading
// zero), the last part filling the remaining bytes, with an optional trailing
// dot -- into dotted-quad form. Anything WHATWG would reject (an invalid digit
// for its radix, an empty part, a fifth part, a value out of range) is returned
// unchanged, which can only refuse a spelling uv accepts, never accept one it
// refuses.
func canonicalIPv4(host string) string {
	if host == "" || strings.Contains(host, ":") || strings.Contains(host, "%") {
		return host
	}
	parts := strings.Split(strings.TrimSuffix(host, "."), ".")
	if len(parts) == 0 || len(parts) > 4 {
		return host
	}
	values := make([]uint64, 0, len(parts))
	for _, part := range parts {
		value, ok := ipv4Part(part)
		if !ok {
			return host
		}
		values = append(values, value)
	}
	for _, value := range values[:len(values)-1] {
		if value > 255 {
			return host
		}
	}
	last := values[len(values)-1]
	if last >= uint64(1)<<(8*(5-len(values))) {
		return host
	}
	var quad [4]byte
	for i := 0; i < len(values)-1; i++ {
		quad[i] = byte(values[i])
	}
	for i := 0; i < 5-len(values); i++ {
		quad[3-i] = byte(last >> (8 * i))
	}
	return fmt.Sprintf("%d.%d.%d.%d", quad[0], quad[1], quad[2], quad[3])
}

// ipv4Part parses one WHATWG IPv4 part: hexadecimal with a `0x` prefix, octal
// with a leading zero, decimal otherwise.
func ipv4Part(part string) (uint64, bool) {
	if part == "" {
		return 0, false
	}
	switch {
	case len(part) > 2 && (part[0:2] == "0x" || part[0:2] == "0X"):
		value, err := strconv.ParseUint(part[2:], 16, 64)
		return value, err == nil
	case len(part) > 1 && part[0] == '0':
		value, err := strconv.ParseUint(part[1:], 8, 64)
		return value, err == nil
	default:
		value, err := strconv.ParseUint(part, 10, 64)
		return value, err == nil
	}
}
