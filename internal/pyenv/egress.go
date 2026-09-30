package pyenv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Preparation has no offline bundle: it fetches the pinned interpreter from
// python-build-standalone and the locked dependencies from a package index.
// When a host cannot reach one of them, the failure arrives in the middle of a
// fetch, wearing whatever disguise the operation was under -- a DNS error, a
// TLS timeout, or, confusingly, "No download found for request" when the
// *catalogue* has no build for the pin and the platform.
//
// The preflight in this file asks the network question first, before anything
// is fetched, so the two answers stop looking alike: a preflight failure is a
// reachability problem, and a preflight that passed means a later download
// failure is not one.
//
// There is deliberately exactly one package index and one interpreter source
// to check, because that is what preparation uses: uv is run with an explicit
// index (or none) and an explicit mirror (or none), never with inherited
// configuration.

// DefaultPackageIndex is the index uv resolves and syncs from when no index is
// configured. The spelling matters twice over: it is uv's own default, and it
// is the string uv records in `uv.lock` for a package that came from PyPI. A
// trailing slash would make the lock check refuse every ordinary lock.
const DefaultPackageIndex = "https://pypi.org/simple"

// DefaultPythonMirror is where uv downloads managed interpreters from when no
// mirror is configured. It is a *root*, not a file: uv 0.5.9 (the release
// `otter deploy` vendors) strips this prefix from each build's URL and appends
// the remainder to `--mirror`/`UV_PYTHON_INSTALL_MIRROR`, so a mirror replaces
// exactly this prefix. uv refuses a mirror it cannot use that way, with
// "A mirror was provided via ..., but the URL does not match the expected
// format".
//
// uv names the pre-transfer repository; GitHub redirects it to the current
// one, and the probe below follows redirects, so either spelling proves the
// same route.
const DefaultPythonMirror = "https://github.com/indygreg/python-build-standalone/releases/download"

// EgressEndpoint is one place preparation has to reach.
type EgressEndpoint struct {
	// Name is the short label an error or a progress line uses.
	Name string
	// URL is what is probed.
	URL string
}

// Prober reports whether one endpoint is reachable. It is what makes the
// preflight testable without a network: preparation calls it instead of
// opening a connection itself, and a test injects a stub.
//
// The contract is reachability, not success. Nil means a connection to the
// endpoint completed and it answered; any error means it did not.
type Prober func(ctx context.Context, url string) error

// HTTPProbe is the Prober used when none is injected.
//
// It issues one bounded request and accepts any HTTP response, including the
// 404 that the base of a download path answers and the 401 a private mirror
// answers before it is authenticated. That leniency is the point: the question
// is whether the host has a working route and TLS path to the endpoint, and a
// status that is not 200 still answers it. Ignoring the status is also what
// keeps the check honest about what it does not prove -- whether a *specific*
// build exists is a different question, answered by uv's own catalogue after
// the preflight has already ruled the network out.
//
// Proxy and TLS settings come from the process environment, the same
// `HTTPS_PROXY`/`NO_PROXY`/`SSL_CERT_FILE` variables uv itself honours, so a
// host that reaches the endpoint only through a proxy is checked through it
// too.
func HTTPProbe(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// One byte is enough to complete a response, and it keeps a probe of a
	// large simple-index page from downloading it.
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("User-Agent", "otter-egress-preflight")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
	_ = resp.Body.Close()
	return nil
}

// ErrEgressUnreachable reports that an endpoint preparation needs could not be
// reached, which is decided before anything is fetched.
var ErrEgressUnreachable = errors.New("egress preflight failed")

// endpoints is what a fresh preparation has to reach, in the order it needs
// it: the package index, then the interpreter, then any endpoint the operator
// declared for the job itself.
//
// The package index is not simply "the flag or PyPI". When nothing is
// configured, uv installs from the URLs the lock records, so those are the
// endpoints that answer the question -- checking PyPI would refuse a
// preparation that would have worked from a lock cut against a mirror. When
// something *is* configured, checkLockIndex has already established that it
// matches the lock (or preparation stopped), so the configured index is the one
// to check. The interpreter source follows the same rule: the flag, then the
// variable uv reads, then the default. The additional-index variables
// (UV_INDEX, UV_EXTRA_INDEX_URL, UV_FIND_LINKS) are deliberately not followed:
// they add sources to the default one rather than replacing it.
func (m Manager) endpoints(dir string) []EgressEndpoint {
	indexes := m.packageIndexURLs(dir)
	mirror := firstSet(m.PythonMirror, os.Getenv("UV_PYTHON_INSTALL_MIRROR"), DefaultPythonMirror)
	out := make([]EgressEndpoint, 0, len(indexes)+1+len(m.ExtraEndpoints))
	for _, index := range indexes {
		out = append(out, EgressEndpoint{Name: "package index", URL: index})
	}
	out = append(out, EgressEndpoint{Name: "managed Python downloads", URL: mirror})
	for _, raw := range m.ExtraEndpoints {
		if url := strings.TrimSpace(raw); url != "" {
			out = append(out, EgressEndpoint{Name: "job endpoint", URL: url})
		}
	}
	return out
}

// firstSet returns the first value that is not blank after trimming, so an
// explicitly empty flag falls back the same way an unset one does.
func firstSet(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// describeEgress names the endpoints for a progress line, so a transcript
// records what was checked rather than only that something was.
func describeEgress(endpoints []EgressEndpoint) string {
	parts := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		parts = append(parts, endpoint.Name+" "+endpoint.URL)
	}
	return strings.Join(parts, ", ")
}

// checkEgress probes every endpoint a fresh preparation needs and stops at the
// first one that cannot be reached. It runs before the interpreter is
// installed and before dependencies are synced, so a host with no route fails
// with the route as the reason instead of with the first fetch error.
func (m Manager) checkEgress(ctx context.Context, dir string, spec Spec) error {
	probe := m.Probe
	if probe == nil {
		probe = HTTPProbe
	}
	for _, endpoint := range m.endpoints(dir) {
		if err := probe(ctx, endpoint.URL); err != nil {
			return fmt.Errorf(
				"%w for CPython %s on %s: %s is unreachable (%s): %v\n%s",
				ErrEgressUnreachable, spec.Python, targetPlatform(),
				endpoint.Name, endpoint.URL, err, m.egressHint())
		}
	}
	return nil
}

// egressHint is the actionable half of a preflight failure. It names every way
// out, because which one applies depends on facts the runtime cannot see: the
// host's firewall, whether the operator has a mirror, and whether the host is
// deliberately air-gapped with its toolchain provisioned out of band.
func (m Manager) egressHint() string {
	return "hint: preparation downloads the managed interpreter and the locked dependencies, and\n" +
		"has no offline bundle. Check the host's route and proxy (HTTPS_PROXY and NO_PROXY are\n" +
		"honoured). An interpreter mirror is --python-mirror; packages come from the index\n" +
		"uv.lock records, and --index must name that same index (re-lock with\n" +
		"`uv lock --default-index <url>` to change it). A primed host can provision tools/uv/uv,\n" +
		"python/ and cache/uv/ out of band and run with --skip-egress-check."
}

// installError explains a failed interpreter install. A preflight that passed
// is what makes the difference between "the network is down" and "this platform
// does not publish this pin", and uv reports the second as a plain "No download
// found for request" -- which, from the catalogue it carries, it decides
// locally, before any download. Naming that distinction here is the difference
// between a five-minute fix and an hour of firewall archaeology.
func installError(spec Spec, err error, output string, egressChecked bool) error {
	base := fmt.Errorf("install Python %s: %w: %s", spec.Python, err, output)
	if !egressChecked || !strings.Contains(output, "No download found") {
		return base
	}
	return fmt.Errorf(
		"%w\nhint: the egress preflight passed, so this is not a network failure: %s has no\n"+
			"CPython %s build for %s. Not every patch version is published for every platform;\n"+
			"check the catalogue with `uv python list --all-versions`, or prepare on a host whose\n"+
			"architecture publishes this pin (linux/amd64 is the fallback when arm64 does not).",
		base, "python-build-standalone", spec.Python, targetPlatform())
}
