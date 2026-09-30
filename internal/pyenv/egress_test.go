package pyenv

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// localPython returns the interpreter the fake uv symlinks and the pin that
// matches it, so preparation can be exercised end to end without a download.
func localPython(t *testing.T) (string, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	out, err := exec.Command(python, "-c", "import sys; print('.'.join(map(str,sys.version_info[:3])))").Output()
	if err != nil {
		t.Fatal(err)
	}
	return python, strings.TrimSpace(string(out))
}

// fakeUV writes a uv that records every invocation in logPath and, when it is
// asked to install or sync, produces the layout and interpreter preparation
// expects. installOutput is echoed when it is asked to install, so a test can
// reproduce uv's own words for a failure.
func fakeUV(t *testing.T, dataDir, python, installOutput string) (uv, logPath string) {
	t.Helper()
	uv = filepath.Join(dataDir, "tools", "uv", "uv")
	logPath = filepath.Join(dataDir, "uv-invocations.log")
	if err := os.MkdirAll(filepath.Dir(uv), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + shellQuote(logPath) + "\n" +
		"if [ \"$1\" = --version ]; then echo 'uv test'; exit 0; fi\n"
	if installOutput != "" {
		script += "if [ \"$1\" = python ]; then echo " + shellQuote(installOutput) + "; exit 1; fi\n"
	} else {
		script += "if [ \"$1\" = python ]; then exit 0; fi\n"
	}
	script += "mkdir -p \"$UV_PROJECT_ENVIRONMENT/bin\"\nln -sf " + shellQuote(python) + " \"$UV_PROJECT_ENVIRONMENT/bin/python\"\n"
	if err := os.WriteFile(uv, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return uv, logPath
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func readLog(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		t.Fatal(err)
	}
	return string(body)
}

// probeRecorder is an injected Prober that records what it was asked to check.
type probeRecorder struct {
	seen []string
	fail map[string]error
}

func (p *probeRecorder) probe(_ context.Context, url string) error {
	p.seen = append(p.seen, url)
	if err := p.fail[url]; err != nil {
		return err
	}
	return nil
}

// clearRouteEnv removes every uv variable preparation follows or refuses, so a
// test does not depend on the environment it happens to run in.
func clearRouteEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"UV_DEFAULT_INDEX", "UV_INDEX_URL", "UV_PYTHON_INSTALL_MIRROR",
		"UV_INDEX", "UV_EXTRA_INDEX_URL", "UV_FIND_LINKS", "UV_NO_INDEX",
	} {
		t.Setenv(name, "")
	}
}

// The preflight must run before anything is fetched, and its failure must name
// the endpoint, the pin and the platform. Removing the check from Prepare makes
// this test fail two ways: preparation reaches uv and succeeds, and no error
// mentions reachability.
func TestPrepareFailsBeforeFetchingWhenAnEndpointIsUnreachable(t *testing.T) {
	clearRouteEnv(t)
	python, pin := localPython(t)
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, pin)
	uv, logPath := fakeUV(t, data, python, "")

	probe := &probeRecorder{fail: map[string]error{
		DefaultPackageIndex: errors.New("dial tcp: lookup pypi.org: no such host"),
	}}
	m := Manager{DataDir: data, Probe: probe.probe}
	_, err := m.Prepare(context.Background(), dir, "one", uv)
	if err == nil {
		t.Fatal("preparation succeeded with an unreachable package index")
	}
	if !errors.Is(err, ErrEgressUnreachable) {
		t.Fatalf("failure is not a preflight failure: %v", err)
	}
	for _, want := range []string{DefaultPackageIndex, pin, targetPlatform(), "package index", "--skip-egress-check"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("preflight failure does not mention %q:\n%v", want, err)
		}
	}
	// Nothing was fetched: no install and no sync reached uv, and no readiness
	// marker was published.
	log := readLog(t, logPath)
	if strings.Contains(log, "install") || strings.Contains(log, "sync") {
		t.Errorf("a fetch ran despite the failed preflight:\n%s", log)
	}
	if !strings.Contains(log, "--version") {
		t.Errorf("uv was never consulted, so the fixture proves nothing:\n%q", log)
	}
	if _, err := m.GetReady(Spec{Job: "one", Python: pin, Digest: strings.Repeat("0", 64)}); err == nil {
		t.Error("a readiness marker was published despite the failed preflight")
	}
	// The second endpoint is not probed after the first fails, so the failure
	// stays about one host and one route.
	if len(probe.seen) != 1 || probe.seen[0] != DefaultPackageIndex {
		t.Errorf("probed %v, want only the failing endpoint", probe.seen)
	}
}

// What the preflight checks is the configuration preparation will use: the
// configured index and mirror replace the defaults, and declared job endpoints
// are added. A probe that ignored configuration would pass this only by
// accident.
func TestPreflightChecksTheConfiguredEndpoints(t *testing.T) {
	const (
		index  = "https://mirror.internal.example/simple"
		mirror = "https://mirror.internal.example/python-build-standalone"
		jobAPI = "https://api.example.com/graphql"
	)

	t.Run("defaults when nothing is configured", func(t *testing.T) {
		clearRouteEnv(t)
		probe := &probeRecorder{fail: map[string]error{}}
		m := Manager{DataDir: t.TempDir(), Probe: probe.probe}
		if err := m.checkEgress(context.Background(), "", Spec{Python: "3.13.1"}); err != nil {
			t.Fatal(err)
		}
		want := []string{DefaultPackageIndex, DefaultPythonMirror}
		if fmt.Sprint(probe.seen) != fmt.Sprint(want) {
			t.Errorf("probed %v, want the defaults %v", probe.seen, want)
		}
	})

	t.Run("configured index, mirror and job endpoint", func(t *testing.T) {
		probe := &probeRecorder{fail: map[string]error{}}
		m := Manager{
			DataDir:        t.TempDir(),
			Index:          index,
			PythonMirror:   mirror,
			ExtraEndpoints: []string{jobAPI, "  "},
			Probe:          probe.probe,
		}
		if err := m.checkEgress(context.Background(), "", Spec{Python: "3.13.1"}); err != nil {
			t.Fatal(err)
		}
		want := []string{index, mirror, jobAPI}
		if fmt.Sprint(probe.seen) != fmt.Sprint(want) {
			t.Errorf("probed %v, want %v", probe.seen, want)
		}
	})
}

// uv reads UV_DEFAULT_INDEX and UV_PYTHON_INSTALL_MIRROR itself, and
// preparation passes the environment through, so the preflight has to follow
// them: an operator whose mirror lives only in the environment would otherwise
// be checked against PyPI and refused a preparation uv would have completed.
func TestPreflightFollowsTheUVEnvironmentWhenNothingIsConfigured(t *testing.T) {
	const (
		envIndex  = "https://env-mirror.internal/simple"
		envMirror = "https://env-mirror.internal/python-build-standalone"
		flagIndex = "https://flag-mirror.internal/simple"
	)
	clearRouteEnv(t)
	t.Setenv("UV_DEFAULT_INDEX", envIndex)
	t.Setenv("UV_PYTHON_INSTALL_MIRROR", envMirror)

	probe := &probeRecorder{fail: map[string]error{}}
	m := Manager{DataDir: t.TempDir(), Probe: probe.probe}
	if err := m.checkEgress(context.Background(), "", Spec{Python: "3.13.1"}); err != nil {
		t.Fatal(err)
	}
	want := []string{envIndex, envMirror}
	if fmt.Sprint(probe.seen) != fmt.Sprint(want) {
		t.Errorf("probed %v, want the environment's endpoints %v", probe.seen, want)
	}

	// A configured flag still wins: it is what the fetch will actually use.
	probe.seen = nil
	configured := Manager{DataDir: t.TempDir(), Index: flagIndex, Probe: probe.probe}
	if err := configured.checkEgress(context.Background(), "", Spec{Python: "3.13.1"}); err != nil {
		t.Fatal(err)
	}
	want = []string{flagIndex, envMirror}
	if fmt.Sprint(probe.seen) != fmt.Sprint(want) {
		t.Errorf("probed %v, want the flag to override the environment %v", probe.seen, want)
	}
}

// The deprecated UV_INDEX_URL spelling is still honoured by uv, so it is still
// an endpoint preparation can fetch from.
func TestPreflightFollowsTheDeprecatedIndexVariable(t *testing.T) {
	clearRouteEnv(t)
	t.Setenv("UV_INDEX_URL", "https://deprecated-mirror.internal/simple")

	probe := &probeRecorder{fail: map[string]error{}}
	m := Manager{DataDir: t.TempDir(), Probe: probe.probe}
	if err := m.checkEgress(context.Background(), "", Spec{Python: "3.13.1"}); err != nil {
		t.Fatal(err)
	}
	if probe.seen[0] != "https://deprecated-mirror.internal/simple" {
		t.Errorf("probed %v, want UV_INDEX_URL to be followed", probe.seen)
	}
}

// The configured index and mirror must reach the commands that fetch, and must
// be absent from them when nothing is configured: this is a passthrough, not a
// new default.
func TestPreparePassesTheRouteToUVAndNothingWhenUnconfigured(t *testing.T) {
	const (
		index  = "https://mirror.internal.example/simple"
		mirror = "https://mirror.internal.example/python-build-standalone"
	)

	run := func(t *testing.T, m Manager) string {
		t.Helper()
		python, pin := localPython(t)
		dir, data := t.TempDir(), t.TempDir()
		writeInputs(t, dir, pin)
		uv, logPath := fakeUV(t, data, python, "")
		m.DataDir, m.Probe = data, (&probeRecorder{fail: map[string]error{}}).probe
		if _, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
			t.Fatal(err)
		}
		return readLog(t, logPath)
	}

	t.Run("unconfigured leaves both commands at their defaults", func(t *testing.T) {
		clearRouteEnv(t)
		log := run(t, Manager{})
		install, sync := "", ""
		for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
			switch {
			case strings.HasPrefix(line, "python install"):
				install = line
			case strings.HasPrefix(line, "sync"):
				sync = line
			}
		}
		if install == "" || sync == "" {
			t.Fatalf("preparation did not run both commands:\n%s", log)
		}
		for _, command := range []string{install, sync} {
			if strings.Contains(command, "--mirror") || strings.Contains(command, "--default-index") {
				t.Errorf("an unconfigured preparation passed a route flag: %s", command)
			}
		}
		// --no-config is the reason the passthrough cannot be a config file:
		// it must stay pinned, and the flags above are what carries the route.
		if !strings.Contains(sync, "--no-config") {
			t.Errorf("preparation stopped disabling configuration discovery: %s", sync)
		}
	})

	t.Run("configured index and mirror reach the fetching commands", func(t *testing.T) {
		log := run(t, Manager{Index: index, PythonMirror: mirror})
		var install, sync string
		for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
			switch {
			case strings.HasPrefix(line, "python install"):
				install = line
			case strings.HasPrefix(line, "sync"):
				sync = line
			}
		}
		if !strings.Contains(install, "--mirror "+mirror) {
			t.Errorf("interpreter install did not use the configured mirror:\n%s", install)
		}
		if !strings.Contains(sync, "--default-index "+index) {
			t.Errorf("dependency sync did not use the configured index:\n%s", sync)
		}
		if strings.Contains(sync, " --index "+index) {
			t.Errorf("the index was passed as an additional index, leaving PyPI in play:\n%s", sync)
		}
	})
}

// The route is not part of the environment identity. If it were, the daemon
// (which binds a run at submission) and `otter release --activate` (a
// rollback) would each resolve an identity they were never told the route for,
// and would look for an environment preparation never built.
func TestEnvironmentIdentityDoesNotDependOnTheRoute(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, "3.13.5")

	plain, err := Manager{DataDir: data}.Resolve(dir, "one", "uv test")
	if err != nil {
		t.Fatal(err)
	}
	mirrored, err := Manager{
		DataDir:      data,
		Index:        "https://mirror.internal.example/simple",
		PythonMirror: "https://mirror.internal.example/python-build-standalone",
	}.Resolve(dir, "one", "uv test")
	if err != nil {
		t.Fatal(err)
	}
	if plain.Digest != mirrored.Digest || plain.Policy != mirrored.Policy {
		t.Errorf("the fetch route changed the environment identity:\n  %s\n  %s", plain.Policy, mirrored.Policy)
	}
}

// A prepared environment must stay reusable, and re-preparing it must not probe
// the network: preparation of nothing is not a network operation, and a host
// whose mirror has since gone away keeps serving its runs.
func TestReusedEnvironmentIsNotProbed(t *testing.T) {
	clearRouteEnv(t)
	python, pin := localPython(t)
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, pin)
	uv, _ := fakeUV(t, data, python, "")

	probe := &probeRecorder{fail: map[string]error{}}
	var logged []string
	m := Manager{DataDir: data, Probe: probe.probe, Logf: func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}}
	if _, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
		t.Fatal(err)
	}
	first := len(probe.seen)
	if first == 0 {
		t.Fatal("a fresh preparation did not check egress")
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "egress preflight ok") ||
		!strings.Contains(logged[0], DefaultPackageIndex) || !strings.Contains(logged[0], DefaultPythonMirror) {
		t.Errorf("the preflight was not reported with the endpoints it checked: %v", logged)
	}
	probe.fail[DefaultPackageIndex] = errors.New("mirror is gone")
	if _, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
		t.Fatalf("a ready environment was re-checked against the network: %v", err)
	}
	if len(probe.seen) != first {
		t.Errorf("reuse probed the network again: %v", probe.seen[first:])
	}
}

// --skip-egress-check must skip the probe, and a skipped probe must not let the
// runtime claim the network was checked: the platform/patch-version hint is only
// trustworthy when the preflight actually ran and passed.
func TestSkipEgressCheckSkipsTheProbeAndWithholdsTheHint(t *testing.T) {
	python, pin := localPython(t)
	blocked := &probeRecorder{fail: map[string]error{DefaultPackageIndex: errors.New("blocked")}}

	t.Run("a blocked endpoint does not fail a skipped check", func(t *testing.T) {
		dir, data := t.TempDir(), t.TempDir()
		writeInputs(t, dir, pin)
		uv, _ := fakeUV(t, data, python, "")
		m := Manager{DataDir: data, SkipEgressCheck: true, Probe: blocked.probe}
		if _, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
			t.Fatalf("skipping the check must not fail preparation for a network reason: %v", err)
		}
		if len(blocked.seen) != 0 {
			t.Errorf("the probe ran despite --skip-egress-check: %v", blocked.seen)
		}
	})

	t.Run("a passing check attributes a catalogue miss", func(t *testing.T) {
		dir, data := t.TempDir(), t.TempDir()
		writeInputs(t, dir, pin)
		uv, _ := fakeUV(t, data, python, "No download found for request: cpython-"+pin+"-linux-aarch64-gnu")
		m := Manager{DataDir: data, Probe: (&probeRecorder{fail: map[string]error{}}).probe}
		_, err := m.Prepare(context.Background(), dir, "one", uv)
		if err == nil {
			t.Fatal("a failed interpreter install was reported as success")
		}
		for _, want := range []string{"egress preflight passed", pin, targetPlatform(), "python-build-standalone", "uv python list --all-versions"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("an unreachable-catalogue failure does not mention %q:\n%v", want, err)
			}
		}
	})

	t.Run("a skipped check does not attribute it", func(t *testing.T) {
		dir, data := t.TempDir(), t.TempDir()
		writeInputs(t, dir, pin)
		uv, _ := fakeUV(t, data, python, "No download found for request: cpython-"+pin+"-linux-aarch64-gnu")
		m := Manager{DataDir: data, SkipEgressCheck: true, Probe: blocked.probe}
		_, err := m.Prepare(context.Background(), dir, "one", uv)
		if err == nil {
			t.Fatal("a failed interpreter install was reported as success")
		}
		if !strings.Contains(err.Error(), "No download found") {
			t.Errorf("the underlying failure was swallowed: %v", err)
		}
		if strings.Contains(err.Error(), "egress preflight passed") {
			t.Errorf("a skipped preflight still claimed the network was reachable:\n%v", err)
		}
	})
}

// The default probe treats any HTTP answer as reachability and a transport
// failure as none, which is what makes it usable against a download *root*
// that legitimately answers 404.
func TestHTTPProbeAcceptsAnyAnswerAndRejectsNoRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer server.Close()
	if err := HTTPProbe(context.Background(), server.URL); err != nil {
		t.Errorf("a 404 from a reachable endpoint was reported as unreachable: %v", err)
	}

	// A listener that was closed refuses the connection: no route, no answer.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	if err := HTTPProbe(context.Background(), "http://"+addr+"/simple/"); err == nil {
		t.Error("a refused connection was reported as reachable")
	}
}
