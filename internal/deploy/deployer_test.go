package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeRunner records what the converge asked the host to do, and answers the
// two questions the deployer asks: what platform the host is, and whether the
// daemon is healthy.
type fakeRunner struct {
	platform string
	records  []string
	healthy  bool

	// remoteToken simulates a token already installed on the host.
	remoteToken string
	// failInstall makes the install script fail.
	failInstall bool
	// failPushTo, when non-empty and a suffix of a push's remote directory,
	// makes that push fail with failPushErr. Pinning the directory rather than
	// counting calls keeps a test attached to the step it means.
	failPushTo  string
	failPushErr error
	// failScript, when non-empty and contained in a script, makes that script
	// fail with failScriptErr. It is the general form of failInstall, and it
	// carries the exact error so a test can assert errors.Is through the
	// deploy's report.
	failScript    string
	failScriptErr error
	// healthURL, when set, makes the health check talk to a real HTTP server
	// instead of returning a canned response.
	healthURL string
	// bindingsJSON is what the destination runtime answers `jobs --json --all`
	// with, so a converge can record destination identities.
	bindingsJSON string
	// workspaceList is what the host answers the workspace-record scan with:
	// JSON records, the marker, then the ports already listening.
	workspaceList string
}

func (f *fakeRunner) RunStream(_ context.Context, command string, stdout, _ io.Writer) error {
	f.records = append(f.records, "stream: "+command)
	if f.bindingsJSON != "" && strings.Contains(command, "jobs --json") && stdout != nil {
		_, _ = io.WriteString(stdout, f.bindingsJSON)
	}
	return nil
}

// Open and Platform mirror the real SSH runner, which the deployer finds by
// type assertion. A fake that omits them would silently skip platform
// detection, so the tests would pass against behaviour the real runner does
// not have.
func (f *fakeRunner) Open(_ context.Context) error {
	f.records = append(f.records, "open")
	return nil
}

func (f *fakeRunner) Platform(_ context.Context) (string, error) {
	f.records = append(f.records, "platform")
	if f.platform == "" {
		return "linux/arm64", nil
	}
	return f.platform, nil
}

func (f *fakeRunner) RunScript(_ context.Context, script string) error {
	f.records = append(f.records, "script: "+script)
	if f.failScript != "" && strings.Contains(script, f.failScript) {
		return injected(f.failScriptErr, "script failed")
	}
	if f.failInstall && strings.Contains(script, "systemctl restart") {
		return fmt.Errorf("remote: systemctl restart failed")
	}
	return nil
}

func (f *fakeRunner) Output(ctx context.Context, command string) (string, error) {
	f.records = append(f.records, "output: "+command)
	switch {
	case strings.Contains(command, "uname"):
		return "Linux\naarch64\n", nil
	case strings.Contains(command, "/health"):
		if f.healthURL != "" {
			return fetchHealth(ctx, f.healthURL, command)
		}
		if !f.healthy {
			return "", fmt.Errorf("curl: (7) connection refused")
		}
		return `{"status":"ok","version":"test"}`, nil
	case strings.Contains(command, "OTTER_API_TOKEN"):
		return f.remoteToken, nil
	case strings.Contains(command, workspacePortsMarker):
		return f.workspaceList, nil
	}
	return "", nil
}

// fetchHealth plays the part of curl on the host: it parses the URL and the
// Authorization header out of the rendered command and performs the request.
func fetchHealth(ctx context.Context, url, command string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if match := bearerRe.FindStringSubmatch(command); match != nil {
		req.Header.Set("Authorization", "Bearer "+match[1])
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("curl: (22) The requested URL returned error: %d", resp.StatusCode)
	}
	return string(body), nil
}

var bearerRe = regexp.MustCompile(`Authorization: Bearer ([A-Za-z0-9_\-]+)`)

func (f *fakeRunner) Push(_ context.Context, localDir, remoteDir string, extra ...string) error {
	f.records = append(f.records, fmt.Sprintf("push: %s -> %s %v", localDir, remoteDir, extra))
	if f.failPushTo != "" && strings.HasSuffix(remoteDir, f.failPushTo) {
		return injected(f.failPushErr, "push failed")
	}
	return nil
}

// injected returns the error a test asked for, so the test can assert
// errors.Is against the exact cause instead of against its text.
func injected(err error, fallback string) error {
	if err != nil {
		return err
	}
	return errors.New(fallback)
}

func (f *fakeRunner) Close() error { return nil }

func (f *fakeRunner) scripts() []string {
	var scripts []string
	for _, r := range f.records {
		if strings.HasPrefix(r, "script: ") {
			scripts = append(scripts, strings.TrimPrefix(r, "script: "))
		}
	}
	return scripts
}

func (f *fakeRunner) pushed() []string {
	var pushed []string
	for _, r := range f.records {
		if strings.HasPrefix(r, "push: ") {
			pushed = append(pushed, r)
		}
	}
	return pushed
}

// fakeBuilder stages a real file tree so that Revision has something to hash.
type fakeBuilder struct {
	dir      string
	contents map[string]string
	// failBuild, when set, is what Build returns. It is how a test proves a
	// failure before the first host mutation carries no partial report.
	failBuild error
}

func newFakeBuilder(t *testing.T) *fakeBuilder {
	t.Helper()
	return &fakeBuilder{dir: t.TempDir()}
}

func (b *fakeBuilder) TempDir() (string, error) { return b.dir, nil }

func (b *fakeBuilder) VendorUV(_ context.Context, _ Config, outDir string) error {
	if err := os.MkdirAll(filepath.Join(outDir, "tools", "uv"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "tools", "uv", "uv"), []byte("fake-uv"), 0o755)
}

func (b *fakeBuilder) Build(_ context.Context, cfg Config, outDir string) error {
	if b.failBuild != nil {
		return b.failBuild
	}
	return os.WriteFile(filepath.Join(outDir, "otterd"), []byte("binary-"+cfg.Version), 0o755)
}

func (b *fakeBuilder) Stage(_ Config, outDir string) error {
	files := b.contents
	if files == nil {
		files = map[string]string{
			"jobs/counter/main.py":                  "print('counter')",
			"lib/python/example_shared/__init__.py": "",
		}
	}
	for rel, content := range files {
		path := filepath.Join(outDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (b *fakeBuilder) Revision(outDir string) (string, error) {
	return (&LocalBuilder{}).Revision(outDir)
}

func (b *fakeBuilder) Cleanup() {}

// newTestDeployer assembles a deployer pointed at a temporary "repository".
func newTestDeployer(t *testing.T, runner *fakeRunner, builder *fakeBuilder) (*Deployer, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sharedPath := filepath.Join(repo, SharedEnvFileName)
	if err := os.WriteFile(sharedPath, []byte("SHOPIFY_CLIENT_ID=abc\nSHOPIFY_CLIENT_SECRET=def\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		ProjectRoot: repo,
		Jobs: []Job{{
			Name:  "counter",
			Label: "counter",
			Dir:   filepath.Join(repo, LocalJobsDir, "counter"),
		}},
		SharedEnv: sharedPath,
		Version:   "v0.1.0-test",
		Timeout:   30_000_000_000,
		Target: Target{
			Host:          "droplet",
			Port:          22,
			RemoteDir:     "/opt/otter",
			RunAsUser:     "otter",
			WorkspaceID:   "11111111-2222-3333-4444-555555555555",
			WorkspaceSlug: "counter",
		},
	}

	var stdout, stderr bytes.Buffer
	return &Deployer{
		Config:  cfg,
		Store:   NewStateStore(repo),
		Builder: builder,
		Runner:  runner,
		Stdout:  &stdout,
		Stderr:  &stderr,
	}, &stdout, &stderr
}

func TestRunDetectsPlatformAndDeploys(t *testing.T) {
	runner := &fakeRunner{healthy: true}
	deployer, _, _ := newTestDeployer(t, runner, newFakeBuilder(t))

	result, err := deployer.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.Platform != "linux/arm64" {
		t.Errorf("Platform = %q, want the value the host reported", result.Platform)
	}
	if !result.FirstDeploy {
		t.Error("FirstDeploy = false on the first run")
	}
	if !result.RevealToken {
		t.Error("RevealToken = false on the first run; the operator would have no way to learn the token")
	}
	if result.APIToken == "" {
		t.Error("no API token was generated")
	}
	if result.Revision == "" {
		t.Error("no revision was recorded")
	}

	// The binaries must be pushed, and the sources must not clobber them.
	pushed := strings.Join(runner.pushed(), "\n")
	if !strings.Contains(pushed, "/bin") {
		t.Errorf("binaries were not pushed:\n%s", pushed)
	}

	// The secrets must travel in a script, never in a command line: argv is
	// world readable on the host.
	for _, record := range runner.records {
		if strings.HasPrefix(record, "script: ") && strings.Contains(record, "SHOPIFY_CLIENT_SECRET=def") {
			continue // this is the intended path
		}
		if strings.Contains(record, "SHOPIFY_CLIENT_SECRET=def") {
			t.Errorf("a secret appeared outside a stdin script: %s", record)
		}
	}
}

// P0-12's deliverable is Castor's credentials in the `0600` environment file,
// with the manifest declaring names only. The test above asserts the values
// travel over stdin rather than argv; this one asserts the file those bytes land
// in is created owner-only. A `0644` env file would expose every credential to
// every user on the host, and the unit hands it to the daemon as its
// environment.
func TestDeployWritesEnvFilesOwnerOnly(t *testing.T) {
	runner := &fakeRunner{healthy: true}
	deployer, _, _ := newTestDeployer(t, runner, newFakeBuilder(t))

	// Exercise both files a real deploy writes: the workspace's shared
	// credentials and the daemon-wide settings, each of which may carry a
	// token (a notification webhook URL carries one in its path).
	daemonEnv := filepath.Join(t.TempDir(), DaemonEnvFileName)
	if err := os.WriteFile(daemonEnv,
		[]byte("OTTER_NOTIFY_URL=http://example.test/hook/not-a-real-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deployer.Config.DaemonEnv = daemonEnv

	if _, err := deployer.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := map[string]bool{
		deployer.Config.Target.SharedEnvFilePath(): false,
		deployer.Config.Target.DaemonEnvFilePath(): false,
	}

	wroteAny := false
	for _, script := range runner.scripts() {
		if !strings.Contains(script, "OTTER_ENV_EOF") {
			continue
		}
		wroteAny = true

		// umask first, because the redirect creates the file before chmod can
		// narrow it: without the umask the content is briefly world readable.
		if !strings.Contains(script, "umask 077") {
			t.Errorf("an env-file script has no `umask 077`, so the file is briefly world readable:\n%s", script)
		}
		if !strings.Contains(script, "install -d -m 0700 "+ShellQuote(deployer.Config.Target.EnvDir())) {
			t.Errorf("an env-file script does not create the env directory owner-only:\n%s", script)
		}
		for path := range want {
			if strings.Contains(script, "chmod 0600 "+ShellQuote(path)) {
				want[path] = true
			}
		}
	}

	if !wroteAny {
		t.Fatal("no deploy step wrote an environment file; the assertion proved nothing")
	}
	for path, narrowed := range want {
		if !narrowed {
			t.Errorf("no deploy step chmod 0600 %s", path)
		}
	}
}

func TestRunIsIdempotentAndReusesTheToken(t *testing.T) {
	runner := &fakeRunner{healthy: true}
	deployer, _, _ := newTestDeployer(t, runner, newFakeBuilder(t))

	first, err := deployer.Run(context.Background())
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}

	second, err := deployer.Run(context.Background())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if second.APIToken != first.APIToken {
		t.Errorf("the second deploy changed the token: %q -> %q", first.APIToken, second.APIToken)
	}
	if second.RevealToken {
		t.Error("the second deploy printed the token again; the operator already has it")
	}
	if second.FirstDeploy {
		t.Error("the second deploy reported itself as the first")
	}
	// Same source, same revision: this is what lets an operator trust that a
	// no-op deploy really is a no-op.
	if second.Revision != first.Revision {
		t.Errorf("revision changed between identical deploys: %q != %q", second.Revision, first.Revision)
	}
}

func TestRunRevisionTracksSourceChanges(t *testing.T) {
	runner := &fakeRunner{healthy: true}
	builder := newFakeBuilder(t)
	deployer, _, _ := newTestDeployer(t, runner, builder)

	first, err := deployer.Run(context.Background())
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}

	builder.contents = map[string]string{
		"jobs/counter/main.py":                  "print('counter and more')",
		"lib/python/example_shared/__init__.py": "",
	}
	second, err := deployer.Run(context.Background())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if second.Revision == first.Revision {
		t.Error("the revision did not change when the staged content changed")
	}
}

func TestRunReusesTokenAlreadyOnTheHost(t *testing.T) {
	runner := &fakeRunner{healthy: true, remoteToken: "existing-token"}
	deployer, _, _ := newTestDeployer(t, runner, newFakeBuilder(t))

	result, err := deployer.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.APIToken != "existing-token" {
		t.Errorf("APIToken = %q, want the token already installed on the host", result.APIToken)
	}
}

func TestRunRotateToken(t *testing.T) {
	runner := &fakeRunner{healthy: true}
	deployer, _, _ := newTestDeployer(t, runner, newFakeBuilder(t))

	first, err := deployer.Run(context.Background())
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Run mutates the token it resolved, so clear it before asking for a new
	// one: --rotate-token and --api-token are mutually exclusive by design.
	deployer.Config.Target.RotateAPIToken = true
	deployer.Config.Target.APIToken = ""
	second, err := deployer.Run(context.Background())
	if err != nil {
		t.Fatalf("rotating Run: %v", err)
	}

	if second.APIToken == first.APIToken {
		t.Error("--rotate-token did not change the token")
	}
	if !second.RevealToken {
		t.Error("a rotated token must be shown to the operator")
	}
}

func TestRunNeverRestartsWhenInstallFails(t *testing.T) {
	runner := &fakeRunner{healthy: true, failInstall: true}
	deployer, _, _ := newTestDeployer(t, runner, newFakeBuilder(t))

	if _, err := deployer.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded even though the install script failed")
	}

	// A failed install must not leave a state file claiming success.
	if _, ok, _ := deployer.Store.Load(); ok {
		t.Error("state was recorded despite a failed install")
	}
}

func TestHealthCheckFailureIsReported(t *testing.T) {
	runner := &fakeRunner{healthy: false}
	deployer, _, _ := newTestDeployer(t, runner, newFakeBuilder(t))
	// Keep the test quick: the deploy waits up to 30s for health otherwise.
	deployer.Config.Timeout = 2 * time.Second

	err := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := deployer.Run(ctx)
		return err
	}()
	if err == nil {
		t.Fatal("Run succeeded although the daemon never became healthy")
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("health failure is not explained: %v", err)
	}
	if strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("health failure leaked a bare context error: %v", err)
	}
}

// TestHealthCheckAgainstARealServer drives the verify step with a real HTTP
// listener instead of a canned response, so the Authorization header, the
// status handling and the retry loop are exercised the way they are on a host.
func TestHealthCheckAgainstARealServer(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"status":"ok","version":"test"}`)
	}))
	defer server.Close()

	target := DefaultTarget()
	target.Host = "droplet"
	target.Platform = "linux/arm64"
	target.APIToken = "token-abc"

	runner := &fakeRunner{healthy: true}
	runner.healthURL = server.URL + "/health"

	var stderr bytes.Buffer
	deployer := &Deployer{
		Config: Config{
			ProjectRoot: t.TempDir(),
			Version:     "test",
			Timeout:     5 * time.Second,
			Target:      target,
		},
		Runner: runner,
		Stderr: &stderr,
	}
	if err := deployer.waitForHealth(context.Background()); err != nil {
		t.Fatalf("waitForHealth: %v\n%s", err, stderr.String())
	}
	if gotAuth != "Bearer token-abc" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer token-abc")
	}
}

// TestHealthCheckRetriesThenSucceeds proves the wait actually waits: a daemon
// that needs a moment to bind must not fail the deploy.
func TestHealthCheckRetriesThenSucceeds(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests < 3 {
			http.Error(w, "not yet", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer server.Close()

	target := DefaultTarget()
	target.Host = "droplet"
	target.Platform = "linux/arm64"

	runner := &fakeRunner{healthy: true}
	runner.healthURL = server.URL + "/health"

	deployer := &Deployer{
		Config: Config{ProjectRoot: t.TempDir(), Version: "test", Timeout: 10 * time.Second, Target: target},
		Runner: runner,
		Stderr: &bytes.Buffer{},
	}
	if err := deployer.waitForHealth(context.Background()); err != nil {
		t.Fatalf("waitForHealth: %v", err)
	}
	if requests < 3 {
		t.Errorf("made %d requests, want at least 3", requests)
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	runner := &fakeRunner{healthy: true}
	deployer, _, _ := newTestDeployer(t, runner, newFakeBuilder(t))
	deployer.Config.DryRun = true

	result, err := deployer.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.DryRun {
		t.Error("DryRun result was not marked as a dry run")
	}

	for _, record := range runner.records {
		if strings.HasPrefix(record, "push:") || strings.HasPrefix(record, "script:") {
			t.Errorf("a dry run performed a change: %s", record)
		}
	}
	if _, ok, _ := deployer.Store.Load(); ok {
		t.Error("a dry run wrote state")
	}
}

func TestDestroyKeepsDataAndClearsState(t *testing.T) {
	runner := &fakeRunner{healthy: true}
	deployer, _, _ := newTestDeployer(t, runner, newFakeBuilder(t))

	if _, err := deployer.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if err := deployer.Destroy(context.Background(), true); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	scripts := runner.scripts()
	last := scripts[len(scripts)-1]
	if strings.Contains(last, "rm -rf '/opt/otter/data'") {
		t.Errorf("Destroy(keepData=true) deleted the data directory:\n%s", last)
	}
	if _, ok, _ := deployer.Store.Load(); ok {
		t.Error("Destroy left the state file behind")
	}
}

// A managed job must be released on the host before the daemon
// restarts, so a failed release leaves the previous deployment serving.
func TestRunReleasesManagedPythonBeforeRestart(t *testing.T) {
	runner := &fakeRunner{healthy: true}
	builder := newFakeBuilder(t)
	builder.contents = map[string]string{
		"jobs/counter/otter.yaml": "version: 1\nname: counter\npython:\n  mode: managed\n",
		"jobs/counter/main.py":    "print('counter')",
	}
	deployer, _, stderr := newTestDeployer(t, runner, builder)
	mustWriteManifest(t, deployer.Config, "counter", "managed")

	if _, err := deployer.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, stderr.String())
	}

	scripts := runner.scripts()
	prepareIdx, installIdx := -1, -1
	for i, script := range scripts {
		if strings.Contains(script, "release") && prepareIdx < 0 {
			prepareIdx = i
		}
		if strings.Contains(script, "systemctl restart") && installIdx < 0 {
			installIdx = i
		}
	}
	if prepareIdx < 0 {
		t.Fatalf("no release step was run:\n%s", stderr.String())
	}
	if installIdx >= 0 && prepareIdx > installIdx {
		t.Error("release ran after the restart; a failed release would then break the running deployment")
	}
}

// A host that reaches its dependencies through an internal mirror must have
// that route carried into the release the deploy runs on it. A deploy that
// dropped it would prepare against PyPI and fail on a host that has no route
// there -- which is exactly the host the flags exist for.
func TestDeployPassesThePreparationRouteToTheHostRelease(t *testing.T) {
	const (
		index  = "https://mirror.internal.example/simple"
		mirror = "https://mirror.internal.example/python-build-standalone"
		jobAPI = "https://api.example.com/graphql"
	)

	releaseScript := func(t *testing.T, configure func(*Deployer)) string {
		t.Helper()
		runner := &fakeRunner{healthy: true}
		builder := newFakeBuilder(t)
		builder.contents = map[string]string{
			"jobs/counter/otter.yaml": "version: 1\nname: counter\npython:\n  mode: managed\n",
			"jobs/counter/main.py":    "print('counter')",
		}
		deployer, _, stderr := newTestDeployer(t, runner, builder)
		mustWriteManifest(t, deployer.Config, "counter", "managed")
		configure(deployer)
		if _, err := deployer.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v\n%s", err, stderr.String())
		}
		for _, script := range runner.scripts() {
			if strings.Contains(script, `"$CLI" release`) {
				return script
			}
		}
		t.Fatal("no release script was run")
		return ""
	}

	t.Run("configured route is passed through", func(t *testing.T) {
		script := releaseScript(t, func(d *Deployer) {
			d.Index = index
			d.PythonMirror = mirror
			d.EgressEndpoints = []string{jobAPI}
			d.SkipEgressCheck = true
		})
		for _, want := range []string{
			"--index " + ShellQuote(index),
			"--python-mirror " + ShellQuote(mirror),
			"--egress-endpoint " + ShellQuote(jobAPI),
			"--skip-egress-check",
		} {
			if !strings.Contains(script, want) {
				t.Errorf("release on the host is missing %q:\n%s", want, script)
			}
		}
	})

	t.Run("an unconfigured deploy passes nothing", func(t *testing.T) {
		script := releaseScript(t, func(*Deployer) {})
		for _, unwanted := range []string{"--index", "--python-mirror", "--egress-endpoint", "--skip-egress-check"} {
			if strings.Contains(script, unwanted) {
				t.Errorf("an unconfigured deploy passed %q to the host:\n%s", unwanted, script)
			}
		}
	})
}

// A job that did not opt into managed Python is still released -- a
// run executes the active release -- but nothing about it needs uv, so a host
// with no managed jobs gets no vendored toolchain.
func TestExternalPythonIsReleasedWithoutPreparation(t *testing.T) {
	runner := &fakeRunner{healthy: true}
	deployer, _, stderr := newTestDeployer(t, runner, newFakeBuilder(t))
	mustWriteManifest(t, deployer.Config, "counter", "")

	if _, err := deployer.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, stderr.String())
	}

	released := false
	for _, script := range runner.scripts() {
		if !strings.Contains(script, `"$CLI" release`) {
			continue
		}
		released = true
		if strings.Contains(script, "--uv") {
			t.Errorf("an external job was prepared with uv:\n%s", script)
		}
	}
	if !released {
		t.Error("an external job was not released, so its runs would be refused")
	}
	if strings.Contains(stderr.String(), "vendoring uv") || strings.Contains(stderr.String(), "pushing uv") {
		t.Errorf("uv was shipped for a workspace with no managed jobs:\n%s", stderr.String())
	}
}

// mustWriteManifest writes the local manifest the deployer reads to decide
// whether preparation is needed.
func mustWriteManifest(t *testing.T, cfg Config, name, mode string) {
	t.Helper()
	dir := ""
	for _, integ := range cfg.Jobs {
		if integ.Name == name {
			dir = integ.Dir
		}
	}
	if dir == "" {
		t.Fatalf("no job named %s in the test config", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "version: 1\nname: " + name + "\n"
	if mode != "" {
		body += "python:\n  mode: " + mode + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A deploy limited to one job must not let --delete remove the others
// from the host: they are on the remote and not in the source tree being
// pushed, which is exactly what --delete removes by default.
func TestLimitedDeployProtectsOtherJobs(t *testing.T) {
	runner := &fakeRunner{healthy: true}
	deployer, _, _ := newTestDeployer(t, runner, newFakeBuilder(t))
	deployer.Config.Limited = true

	if err := deployer.pushSources(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("pushSources: %v", err)
	}

	var pushed string
	for _, record := range runner.pushed() {
		pushed += record + "\n"
	}
	if !strings.Contains(pushed, "protect /jobs/***") {
		t.Errorf("a limited deploy did not protect the other jobs:\n%s", pushed)
	}

	// An unrestricted deploy should not carry the protection, so removing an
	// job locally still removes it remotely.
	runner.records = nil
	deployer.Config.Limited = false
	if err := deployer.pushSources(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(runner.pushed(), "\n"), "protect") {
		t.Error("an unrestricted deploy protected the jobs tree")
	}
}

// A limited deploy of the classic layout carries shared code that lands outside
// jobs/ (a manifest declaring ../../lib/python). --delete would remove
// it, breaking every job this deploy is not carrying, so each tree the
// deploy does carry is protected by name too.
func TestLimitedDeployProtectsSharedTreesOutsideJobs(t *testing.T) {
	project := t.TempDir()
	runner := &fakeRunner{healthy: true}
	deployer := &Deployer{
		Config: Config{
			ProjectRoot: project,
			Limited:     true,
			Jobs: []Job{{
				Name:  "one",
				Dir:   filepath.Join(project, "jobs", "one"),
				Trees: []string{filepath.Join(project, "lib", "python")},
			}},
		},
		Runner: runner,
		Stdout: io.Discard,
		Stderr: io.Discard,
	}

	if err := deployer.pushSources(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("pushSources: %v", err)
	}
	pushed := strings.Join(runner.pushed(), "\n")
	if !strings.Contains(pushed, "protect /lib/***") {
		t.Errorf("a limited deploy did not protect the shared library outside jobs/:\n%s", pushed)
	}
	if !strings.Contains(pushed, "protect /jobs/***") {
		t.Errorf("a limited deploy did not protect the other jobs:\n%s", pushed)
	}
}

// A converge records the identity the destination assigned to each
// job, so a later deploy can tell "same instance, new code" from "a
// new instance" without guessing. Local and remote ids are independent, and
// the record is the only place they are related.
func TestRunRecordsDestinationBindings(t *testing.T) {
	runner := &fakeRunner{
		healthy: true,
		bindingsJSON: `[{"id":"remote-identity-1","name":"counter",` +
			`"path":"/opt/otter/jobs/counter","status":"active"},` +
			`{"id":"retired-1","name":"gone","path":"/opt/otter/gone","status":"retired"}]`,
	}
	deployer, _, _ := newTestDeployer(t, runner, newFakeBuilder(t))

	result, err := deployer.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Bindings) != 1 {
		t.Fatalf("bindings = %+v, want just the active registration", result.Bindings)
	}
	if result.Bindings[0].ID != "remote-identity-1" || result.Bindings[0].Name != "counter" {
		t.Fatalf("binding = %+v", result.Bindings[0])
	}

	// It is persisted, which is what makes it usable by `deploy --status` and
	// by the next deploy.
	all, ok, err := deployer.Store.Load()
	if err != nil || !ok {
		t.Fatalf("load state: ok=%v err=%v", ok, err)
	}
	st := all.Deploys["droplet"]
	if len(st.Bindings) != 1 || st.Bindings[0].ID != "remote-identity-1" {
		t.Fatalf("stored bindings = %+v", st.Bindings)
	}

	// The command reads the registry rather than the API, so it works with the
	// runtime stopped, and it names the destination data directory explicitly.
	var bindingsCommand string
	for _, record := range runner.records {
		if strings.Contains(record, "jobs --json") {
			bindingsCommand = record
		}
	}
	target := deployer.Config.Target
	if !strings.Contains(bindingsCommand, "'"+target.DataDir+"'") {
		t.Fatalf("bindings command does not name the destination data directory: %q", bindingsCommand)
	}
	if !strings.Contains(bindingsCommand, "--all") {
		t.Errorf("bindings command does not ask for every registration: %q", bindingsCommand)
	}
	// A name-only lookup would find another workspace's job: the paths
	// have to be this workspace's.
	if !strings.Contains(bindingsCommand, "/workspaces/") {
		t.Errorf("bindings command is not workspace-scoped: %q", bindingsCommand)
	}
}

// A destination whose runtime cannot answer is not fatal: the deploy succeeds
// and simply records no bindings.
func TestRunToleratesMissingDestinationBindings(t *testing.T) {
	runner := &fakeRunner{healthy: true}
	deployer, _, _ := newTestDeployer(t, runner, newFakeBuilder(t))

	result, err := deployer.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Bindings) != 0 {
		t.Fatalf("bindings = %+v, want none", result.Bindings)
	}
}

// A workspace sits two levels inside the install root, and rsync creates its
// destination but not the levels above it. So the layout has to exist before
// the first push -- and ownership has to be claimed after it, because the push
// writes as the login user and the daemon runs as the service account.
func TestLayoutPrecedesPushAndOwnershipFollowsIt(t *testing.T) {
	runner := &fakeRunner{healthy: true}
	deployer, _, stderr := newTestDeployer(t, runner, newFakeBuilder(t))
	if _, err := deployer.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, stderr.String())
	}

	layout, ownership, firstPush, lastPush := -1, -1, -1, -1
	for i, rec := range runner.records {
		switch {
		case strings.Contains(rec, `install -d -m 0755 -o "$RUN_AS" -g "$RUN_AS" "$WORKSPACE_DIR"`):
			if layout < 0 {
				layout = i
			}
		case strings.Contains(rec, "for dir in bin jobs lib tools; do"):
			if ownership < 0 {
				ownership = i
			}
		case strings.HasPrefix(rec, "push: "):
			if firstPush < 0 {
				firstPush = i
			}
			lastPush = i
		}
	}
	if layout < 0 || ownership < 0 || firstPush < 0 {
		t.Fatalf("steps missing (layout=%d ownership=%d push=%d):\n%s",
			layout, ownership, firstPush, strings.Join(runner.records, "\n"))
	}
	if layout > firstPush {
		t.Error("the workspace directory is created after the first push; rsync cannot create two missing levels")
	}
	if ownership < lastPush {
		t.Error("ownership is claimed before the push finishes, so the pushed tree stays root-owned")
	}
}

// --- partial deploys -------------------------------------------------------
//
// A deploy that cannot finish must say what state it left behind. Each test
// below injects one failure at one step and reads the report the operator
// would see: what landed, what failed, what was never attempted, and what the
// host is serving now. The motivating incident is OPEN-TASKS.md §4: a release
// aborted after the binaries, the env files and three of six jobs had landed,
// leaving the daemon on the old binary with mixed job releases.

// TestPartialDeployAtPushBinaries pins the whole report, line for line: the
// order of the completed steps, the step that failed, the first step that was
// never attempted, and the consequence for a host whose unit was never
// touched.
func TestPartialDeployAtPushBinaries(t *testing.T) {
	cause := errors.New("rsync: connection reset by peer")
	runner := &fakeRunner{healthy: true, failPushTo: "/bin", failPushErr: cause}
	deployer, _, stderr := newTestDeployer(t, runner, newFakeBuilder(t))

	_, err := deployer.Run(context.Background())
	if err == nil {
		t.Fatalf("Run succeeded although pushing the binaries failed\n%s", stderr.String())
	}

	var partial *PartialDeployError
	if !errors.As(err, &partial) {
		t.Fatalf("failure is not a *PartialDeployError: %T: %v", err, err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is does not reach the injected cause: %v", err)
	}

	const want = `deploy stopped at sync-binaries: rsync: connection reset by peer
Mutations completed: 2
  prepare
  sync-sources
Failed: sync-binaries
Not reached:
  ownership
  workspace-record
  daemon-env
  shared-env
  release
  install-restart
  health
Consequence: the previous release is still serving; nothing was restarted`
	if report := err.Error(); report != want {
		t.Errorf("report:\n%s\n\nwant:\n%s", report, want)
	}
}

// A release that fails must still name the two environment files that already
// landed, and must not pretend the restart was attempted.
func TestPartialDeployAtRelease(t *testing.T) {
	cause := errors.New(`remote: "retired-job" is not a registered job`)
	runner := &fakeRunner{healthy: true, failScript: `"$CLI" release`, failScriptErr: cause}
	err, report := runFailedDeploy(t, runner, nil)

	if !errors.Is(err, cause) {
		t.Errorf("errors.Is does not reach the injected cause: %v", err)
	}
	if !strings.HasPrefix(report, "deploy stopped at release: ") {
		t.Errorf("report does not name the release as the failed stage:\n%s", report)
	}
	completed := reportSection(t, report, "Mutations completed: ")
	for _, stage := range []string{"daemon-env", "shared-env"} {
		if !listsStage(completed, stage) {
			t.Errorf("%s is not listed as completed:\n%s", stage, report)
		}
	}
	if listsStage(completed, "release") {
		t.Errorf("the failed release is listed as completed:\n%s", report)
	}
	notReached := reportSection(t, report, "Not reached:")
	for _, stage := range []string{"install-restart", "health"} {
		if !listsStage(notReached, stage) {
			t.Errorf("%s is not listed as not reached:\n%s", stage, report)
		}
	}
	if !strings.Contains(report, "the previous release is still serving; nothing was restarted") {
		t.Errorf("report does not say the old release is still serving:\n%s", report)
	}
}

// The activate script writes the unit and restarts the daemon. From here the
// previous release can no longer be assumed to be serving, and the report has
// to stop reassuring the operator.
func TestPartialDeployAtInstallRestart(t *testing.T) {
	cause := errors.New("remote: Job for otterd.service failed")
	runner := &fakeRunner{healthy: true, failScript: "systemctl restart", failScriptErr: cause}
	_, report := runFailedDeploy(t, runner, nil)

	if !strings.HasPrefix(report, "deploy stopped at install-restart: ") {
		t.Errorf("report does not name install-restart as the failed stage:\n%s", report)
	}
	completed := reportSection(t, report, "Mutations completed: ")
	if !listsStage(completed, "release") {
		t.Errorf("release is not listed as completed:\n%s", report)
	}
	if listsStage(completed, "install-restart") {
		t.Errorf("the failed install-restart is listed as completed:\n%s", report)
	}
	if !strings.Contains(report, "Failed: install-restart") {
		t.Errorf("report does not name the failed stage after the mutation list:\n%s", report)
	}
	if !strings.Contains(report, "the new unit was written; the previous release may no longer be serving") {
		t.Errorf("report does not say the new unit was written:\n%s", report)
	}
	notReached := reportSection(t, report, "Not reached:")
	if len(notReached) != 1 || notReached[0] != "health" {
		t.Errorf("not reached = %v, want only health:\n%s", notReached, report)
	}
}

// A daemon that was restarted but never answered is the one case where the new
// release is installed and broken, and the report must say that rather than
// hint at a rollback.
func TestPartialDeployAtHealth(t *testing.T) {
	runner := &fakeRunner{healthy: false}
	_, report := runFailedDeploy(t, runner, func(d *Deployer) {
		// Keep the test quick: the deploy would otherwise wait up to 30s.
		d.Config.Timeout = 100 * time.Millisecond
	})

	if !strings.HasPrefix(report, "deploy stopped at health: ") {
		t.Errorf("report does not name health as the failed stage:\n%s", report)
	}
	completed := reportSection(t, report, "Mutations completed: ")
	if !listsStage(completed, "install-restart") {
		t.Errorf("install-restart is not listed as completed:\n%s", report)
	}
	if listsStage(completed, "health") {
		t.Errorf("the failed health check is listed as completed:\n%s", report)
	}
	if !strings.Contains(report, "the new release is installed but did not become healthy") {
		t.Errorf("report does not say the new release is installed but unhealthy:\n%s", report)
	}
	notReached := reportSection(t, report, "Not reached:")
	if len(notReached) != 1 || notReached[0] != "(none)" {
		t.Errorf("not reached = %v, want (none):\n%s", notReached, report)
	}
	// The failure cause and the deployer's own hint survive the report.
	if !strings.Contains(report, "did not answer") {
		t.Errorf("report lost the health failure's diagnosis:\n%s", report)
	}
	if !strings.Contains(report, "hint: inspect it with") {
		t.Errorf("report lost the health failure's hint:\n%s", report)
	}
}

// A failure before the first host mutation must stay a bare error: nothing
// landed, so a mutation report would be noise that implies otherwise.
func TestBuildFailureLeavesNoPartialReport(t *testing.T) {
	cause := errors.New("compile: undefined: counter")
	builder := newFakeBuilder(t)
	builder.failBuild = cause
	runner := &fakeRunner{healthy: true}
	deployer, _, stderr := newTestDeployer(t, runner, builder)

	_, err := deployer.Run(context.Background())
	if err == nil {
		t.Fatalf("Run succeeded although the build failed\n%s", stderr.String())
	}
	if err != cause {
		t.Errorf("error = %v, want the bare builder cause unwrapped", err)
	}
	for _, marker := range []string{"deploy stopped at", "Mutations completed", "Not reached", "Consequence"} {
		if strings.Contains(err.Error(), marker) {
			t.Errorf("a pre-mutation failure carries a partial report (%q):\n%s", marker, err)
		}
	}
	for _, record := range runner.records {
		if strings.HasPrefix(record, "script: ") || strings.HasPrefix(record, "push: ") {
			t.Errorf("a build failure still mutated the host: %s", record)
		}
	}
}

// The success path is unchanged: no report, and the same Result the CLI
// already prints.
func TestSuccessfulDeployCarriesNoPartialReport(t *testing.T) {
	runner := &fakeRunner{healthy: true}
	deployer, _, stderr := newTestDeployer(t, runner, newFakeBuilder(t))

	result, err := deployer.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, stderr.String())
	}
	if result == nil {
		t.Fatal("Run returned no result")
	}
	if result.Version != deployer.Config.Version {
		t.Errorf("Version = %q, want %q", result.Version, deployer.Config.Version)
	}
	if len(result.Jobs) != 1 || result.Jobs[0] != "counter" {
		t.Errorf("Jobs = %v, want the one deployed job", result.Jobs)
	}
	if result.ServiceUnit == "" || result.Revision == "" {
		t.Errorf("result is incomplete: %+v", result)
	}
	if !result.FirstDeploy {
		t.Error("FirstDeploy = false on the first run")
	}
	for _, marker := range []string{"deploy stopped at", "Mutations completed", "Not reached", "Consequence:"} {
		if strings.Contains(stderr.String(), marker) {
			t.Errorf("a successful deploy printed a partial report (%q):\n%s", marker, stderr.String())
		}
	}
}

// A state save that fails after the host converged is not a failed deploy: the
// host is running the new release and only this checkout's record is missing.
// The report has to say exactly that, because "failed" would send the operator
// looking for a rollback that is not needed.
func TestPartialDeployAfterConvergenceReportsOnlyTheLocalState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores a read-only directory, so the state save cannot be made to fail")
	}

	runner := &fakeRunner{healthy: true}
	deployer, _, stderr := newTestDeployer(t, runner, newFakeBuilder(t))
	if err := os.MkdirAll(deployer.Store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(deployer.Store.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(deployer.Store.Dir, 0o700) })

	_, err := deployer.Run(context.Background())
	if err == nil {
		t.Fatalf("Run succeeded although the local state could not be saved\n%s", stderr.String())
	}

	var partial *PartialDeployError
	if !errors.As(err, &partial) {
		t.Fatalf("failure is not a *PartialDeployError: %T: %v", err, err)
	}
	if partial.Stage != "local-state" {
		t.Errorf("Stage = %q, want local-state", partial.Stage)
	}

	report := err.Error()
	if !strings.HasPrefix(report, "deploy stopped at local-state: ") {
		t.Errorf("report does not name the local state as the failed step:\n%s", report)
	}
	completed := reportSection(t, report, "Mutations completed: ")
	if len(completed) == 0 || completed[len(completed)-1] != "health" {
		t.Errorf("completed = %v, want every host stage ending in health:\n%s", completed, report)
	}
	notReached := reportSection(t, report, "Not reached:")
	if len(notReached) != 1 || notReached[0] != "(none)" {
		t.Errorf("not reached = %v, want (none):\n%s", notReached, report)
	}
	if !strings.Contains(report, "the host is deployed and serving the new release; the local state file was not saved") {
		t.Errorf("report does not say the host is deployed while the local state is not:\n%s", report)
	}
	// The host really did converge: the daemon was restarted before the save.
	if !strings.Contains(strings.Join(runner.scripts(), "\n"), "systemctl restart") {
		t.Error("the host was not converged, so the test proved nothing about a post-convergence failure")
	}
}

// A failure at the first host mutation is still a failure with nothing to
// report: the account and layout script failed before anything landed. The
// deployer's own hint must survive that boundary unchanged.
func TestFirstStageFailureLeavesNoPartialReport(t *testing.T) {
	cause := errors.New("sudo: a password is required")
	runner := &fakeRunner{healthy: true, failScript: "useradd --system", failScriptErr: cause}
	err, report := runFailedDeploy(t, runner, nil)

	for _, marker := range []string{"deploy stopped at", "Mutations completed", "Not reached", "Consequence"} {
		if strings.Contains(report, marker) {
			t.Errorf("a first-stage failure carries a partial report (%q):\n%s", marker, report)
		}
	}
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is does not reach the injected cause: %v", err)
	}
	if !strings.Contains(report, "hint: systemd needs root") {
		t.Errorf("the prepare failure lost its sudo hint:\n%s", report)
	}
}

// The report's step list is the run's own plan: a config that skips uv or the
// release must not list them as reached or as not reached.
func TestPartialDeployStagesFollowTheConfig(t *testing.T) {
	// The claim-ownership script, which runs after every push and before the
	// workspace record, so it makes the completed list easy to check.
	const failOwnership = "for dir in bin jobs lib tools; do"

	t.Run("uv is a stage only when it is vendored", func(t *testing.T) {
		runner := &fakeRunner{healthy: true, failScript: failOwnership,
			failScriptErr: errors.New("ownership failed")}
		_, report := runFailedDeploy(t, runner, func(d *Deployer) {
			// A managed job is what makes the deploy vendor and push uv.
			mustWriteManifest(t, d.Config, "counter", "managed")
		})

		completed := reportSection(t, report, "Mutations completed: ")
		if got := strings.Join(completed, ","); got != "prepare,sync-sources,sync-binaries,sync-uv" {
			t.Errorf("completed = %v, want the uv push after the binaries:\n%s", completed, report)
		}
		notReached := reportSection(t, report, "Not reached:")
		if !listsStage(notReached, "release") {
			t.Errorf("release is planned but missing from not reached:\n%s", report)
		}
	})

	t.Run("release is a stage only when there are jobs", func(t *testing.T) {
		runner := &fakeRunner{healthy: true, failScript: failOwnership,
			failScriptErr: errors.New("ownership failed")}
		_, report := runFailedDeploy(t, runner, func(d *Deployer) {
			d.Config.Jobs = nil
		})

		completed := reportSection(t, report, "Mutations completed: ")
		if got := strings.Join(completed, ","); got != "prepare,sync-sources,sync-binaries" {
			t.Errorf("completed = %v, want no uv and no release:\n%s", completed, report)
		}
		notReached := reportSection(t, report, "Not reached:")
		for _, stage := range []string{"release", "sync-uv"} {
			if listsStage(notReached, stage) {
				t.Errorf("%s is not part of this run but is listed as not reached:\n%s", stage, report)
			}
		}
	})
}

// runFailedDeploy runs one converge that must fail, and returns the error and
// its rendered report.
func runFailedDeploy(t *testing.T, runner *fakeRunner, configure func(*Deployer)) (error, string) {
	t.Helper()
	deployer, _, stderr := newTestDeployer(t, runner, newFakeBuilder(t))
	if configure != nil {
		configure(deployer)
	}
	_, err := deployer.Run(context.Background())
	if err == nil {
		t.Fatalf("Run succeeded although the injected failure never fired\n%s", stderr.String())
	}
	return err, err.Error()
}

// reportSection returns the indented items under one report header, so a test
// reads the text the operator sees instead of reaching into the error fields.
func reportSection(t *testing.T, report, header string) []string {
	t.Helper()
	lines := strings.Split(report, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, header) {
			continue
		}
		var out []string
		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, "  ") {
				break
			}
			out = append(out, strings.TrimSpace(next))
		}
		return out
	}
	t.Fatalf("report has no %q section:\n%s", header, report)
	return nil
}

// listsStage reports whether a report section names one stage.
func listsStage(stages []string, want string) bool {
	for _, stage := range stages {
		if stage == want {
			return true
		}
	}
	return false
}
