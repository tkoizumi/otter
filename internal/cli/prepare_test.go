package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A managed job whose environment can be prepared without a download: the fake
// uv records what it was asked to do, claims success for the interpreter
// install, and symlinks the interpreter this machine already has so the
// version check preparation performs against the pin passes.
//
// It returns the fake uv path, its invocation log, and the pin that matches the
// local interpreter.
func fakeManagedJob(t *testing.T) (jobDir, uvPath, logPath, pin string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix test fixture")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	out, err := exec.Command(python, "-c", "import sys; print('.'.join(map(str,sys.version_info[:3])))").Output()
	if err != nil {
		t.Fatal(err)
	}
	pin = strings.TrimSpace(string(out))

	root := t.TempDir()
	jobDir = filepath.Join(root, "jobs", "counter")
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"otter.yaml":      "version: 1\nname: counter\nentrypoint: main.py\npython:\n  mode: managed\n",
		"main.py":         "print('counter')\n",
		".python-version": pin + "\n",
		"pyproject.toml":  "[project]\nname = 'counter'\nversion = '0.1.0'\nrequires-python = '>=3.10'\n",
		"uv.lock":         "version = 1\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(jobDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	uvPath = filepath.Join(root, "uv")
	logPath = filepath.Join(root, "uv-invocations.log")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + logPath + "\n" +
		"if [ \"$1\" = --version ]; then echo 'uv test'; exit 0; fi\n" +
		"if [ \"$1\" = python ]; then exit 0; fi\n" +
		"mkdir -p \"$UV_PROJECT_ENVIRONMENT/bin\"\nln -sf " + python + " \"$UV_PROJECT_ENVIRONMENT/bin/python\"\n"
	if err := os.WriteFile(uvPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return jobDir, uvPath, logPath, pin
}

// `otter prepare --index/--python-mirror/--egress-endpoint` must reach the uv
// invocations that fetch, and the preflight must check exactly those endpoints.
// Parsing the flags and dropping them is the failure this test exists to catch:
// the host would keep fetching from PyPI while the operator believes it is
// using the internal mirror.
//
// The endpoints here are local servers, so the test proves the passthrough
// without reaching the real network.
func TestPreparePassesTheConfiguredRouteToTheFetchPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	jobDir, uvPath, logPath, pin := fakeManagedJob(t)
	dataDir := filepath.Join(filepath.Dir(filepath.Dir(jobDir)), "data")

	// Any HTTP answer proves reachability, so a server that answers 404
	// everywhere stands in for a mirror that is reachable but has no directory
	// listing at its root -- which is what both real endpoints look like.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer server.Close()
	index := server.URL + "/simple"
	mirror := server.URL + "/python-build-standalone"
	jobAPI := server.URL + "/graphql"

	original := workingDirForTest
	workingDirForTest = func() (string, error) { return filepath.Dir(filepath.Dir(jobDir)), nil }
	defer func() { workingDirForTest = original }()

	var out, errOut bytes.Buffer
	app := New("test", &out, &errOut)
	code := app.cmdPrepare(context.Background(), []string{
		"--jobs", filepath.Join(filepath.Dir(filepath.Dir(jobDir)), "jobs"),
		"--data", dataDir,
		"--uv", uvPath,
		"--index", index,
		"--python-mirror", mirror,
		"--egress-endpoint", jobAPI,
		jobDir,
	})
	if code != 0 {
		t.Fatalf("prepare exited %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(body)
	var install, sync string
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		switch {
		case strings.HasPrefix(line, "python install"):
			install = line
		case strings.HasPrefix(line, "sync"):
			sync = line
		}
	}
	if install == "" || sync == "" {
		t.Fatalf("preparation did not run both commands:\n%s\nstderr:\n%s", log, errOut.String())
	}
	if !strings.Contains(install, "--mirror "+mirror) {
		t.Errorf("interpreter install did not use the configured mirror:\n%s", install)
	}
	if !strings.Contains(sync, "--default-index "+index) {
		t.Errorf("dependency sync did not use the configured index:\n%s", sync)
	}
	// The preflight must be visible: the transcript is what tells an operator
	// whose job then fails that the network was already ruled out.
	for _, want := range []string{"egress preflight ok", index, mirror, jobAPI, pin} {
		if !strings.Contains(errOut.String(), want) && !strings.Contains(out.String(), want) {
			t.Errorf("preparation output does not mention %q:\nstdout:\n%s\nstderr:\n%s", want, out.String(), errOut.String())
		}
	}
	if !strings.Contains(out.String(), "Python "+pin) {
		t.Errorf("preparation did not report the pinned interpreter:\n%s", out.String())
	}
}

// The same command with nothing configured must pass no route flag to uv, which
// is what keeps the default path byte-for-byte what it was. The probe is
// injected rather than left to the real network, and it is asked for the
// default endpoints, so this test says the same thing on a connected machine
// and on one behind a black-hole proxy.
func TestPrepareUnconfiguredPassesNoRouteFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	jobDir, uvPath, logPath, _ := fakeManagedJob(t)
	dataDir := filepath.Join(filepath.Dir(filepath.Dir(jobDir)), "data")

	original := workingDirForTest
	workingDirForTest = func() (string, error) { return filepath.Dir(filepath.Dir(jobDir)), nil }
	defer func() { workingDirForTest = original }()

	var probed []string
	var out, errOut bytes.Buffer
	app := New("test", &out, &errOut)
	app.egressProbe = func(_ context.Context, url string) error {
		probed = append(probed, url)
		return nil
	}
	code := app.cmdPrepare(context.Background(), []string{
		"--jobs", filepath.Join(filepath.Dir(filepath.Dir(jobDir)), "jobs"),
		"--data", dataDir,
		"--uv", uvPath,
		jobDir,
	})
	if code != 0 {
		t.Fatalf("prepare exited %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if strings.HasPrefix(line, "--version") {
			continue
		}
		if strings.Contains(line, "--mirror") || strings.Contains(line, "--default-index") {
			t.Errorf("an unconfigured prepare passed a route flag to uv: %s", line)
		}
	}
	// The preflight ran, and it asked about uv's defaults: this is what the
	// operator sees on a host that configures nothing.
	want := []string{"https://pypi.org/simple", "https://github.com/indygreg/python-build-standalone/releases/download"}
	if len(probed) != len(want) || probed[0] != want[0] || probed[1] != want[1] {
		t.Errorf("an unconfigured prepare probed %v, want %v", probed, want)
	}
	if !strings.Contains(errOut.String(), "egress preflight ok") {
		t.Errorf("the preflight was not reported:\n%s", errOut.String())
	}
}

// --skip-egress-check is the documented escape hatch, so it has to reach the
// manager. The probe is injected and *fails*: without the flag the very same
// command must refuse preparation with that failure, and with it the command
// must proceed. A test that left the real probe in place would go green on any
// machine that can reach PyPI whichever way the flag were wired.
func TestSkipEgressCheckReachesTheManager(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	injected := errors.New("injected egress failure")

	run := func(t *testing.T, args ...string) (int, string, string) {
		t.Helper()
		jobDir, uvPath, _, _ := fakeManagedJob(t)
		dataDir := filepath.Join(filepath.Dir(filepath.Dir(jobDir)), "data")
		original := workingDirForTest
		workingDirForTest = func() (string, error) { return filepath.Dir(filepath.Dir(jobDir)), nil }
		defer func() { workingDirForTest = original }()

		var out, errOut bytes.Buffer
		app := New("test", &out, &errOut)
		app.egressProbe = func(context.Context, string) error { return injected }
		full := append([]string{
			"--jobs", filepath.Join(filepath.Dir(filepath.Dir(jobDir)), "jobs"),
			"--data", dataDir,
			"--uv", uvPath,
		}, args...)
		full = append(full, jobDir)
		return app.cmdPrepare(context.Background(), full), out.String(), errOut.String()
	}

	t.Run("without the flag the failing probe stops preparation", func(t *testing.T) {
		code, _, errOut := run(t)
		if code == 0 {
			t.Fatalf("preparation ignored a failing probe:\n%s", errOut)
		}
		if !strings.Contains(errOut, injected.Error()) {
			t.Errorf("the failure is not the injected probe's, so the flag is not what was tested:\n%s", errOut)
		}
	})

	t.Run("with the flag preparation proceeds", func(t *testing.T) {
		code, out, errOut := run(t, "--skip-egress-check")
		if code != 0 {
			t.Fatalf("prepare exited %d with --skip-egress-check\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
		}
		if strings.Contains(errOut, injected.Error()) {
			t.Errorf("the probe ran despite --skip-egress-check:\n%s", errOut)
		}
		if strings.Contains(errOut, "egress preflight ok") {
			t.Errorf("a skipped preflight was reported as having run:\n%s", errOut)
		}
	})
}
