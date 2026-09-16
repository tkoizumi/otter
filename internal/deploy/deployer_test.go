package deploy

import (
	"bytes"
	"context"
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
	// healthURL, when set, makes the health check talk to a real HTTP server
	// instead of returning a canned response.
	healthURL string
}

func (f *fakeRunner) RunStream(_ context.Context, command string, _, _ io.Writer) error {
	f.records = append(f.records, "stream: "+command)
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
	return nil
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
	return os.WriteFile(filepath.Join(outDir, "otterd"), []byte("binary-"+cfg.Version), 0o755)
}

func (b *fakeBuilder) Stage(_ Config, outDir string) error {
	files := b.contents
	if files == nil {
		files = map[string]string{
			"integrations/counter/main.py":            "print('counter')",
			"lib/python/otter_connectors/__init__.py": "",
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
		RepoRoot:         repo,
		IntegrationsPath: filepath.Join(repo, LocalIntegrationsDir),
		Integrations:     []string{"counter"},
		SharedEnv:        sharedPath,
		Version:          "v0.1.0-test",
		Timeout:          30_000_000_000,
		Target: Target{
			Host:        "droplet",
			Port:        22,
			RemoteDir:   "/opt/otter",
			ServiceName: "otterd",
			RunAsUser:   "otter",
			DataDir:     "/opt/otter/data",
			Listen:      "127.0.0.1:7337",
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
		"integrations/counter/main.py":            "print('counter and more')",
		"lib/python/otter_connectors/__init__.py": "",
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
			RepoRoot: t.TempDir(),
			Version:  "test",
			Timeout:  5 * time.Second,
			Target:   target,
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
		Config: Config{RepoRoot: t.TempDir(), Version: "test", Timeout: 10 * time.Second, Target: target},
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

// A managed integration must be released on the host before the daemon
// restarts, so a failed release leaves the previous deployment serving.
func TestRunReleasesManagedPythonBeforeRestart(t *testing.T) {
	runner := &fakeRunner{healthy: true}
	builder := newFakeBuilder(t)
	builder.contents = map[string]string{
		"integrations/counter/otter.yaml": "version: 1\nname: counter\npython:\n  mode: managed\n",
		"integrations/counter/main.py":    "print('counter')",
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

// An integration that did not opt in must not trigger preparation, so a host
// with no managed integrations needs no uv at all.
func TestRunSkipsPreparationForExternalPython(t *testing.T) {
	runner := &fakeRunner{healthy: true}
	deployer, _, stderr := newTestDeployer(t, runner, newFakeBuilder(t))
	mustWriteManifest(t, deployer.Config, "counter", "")

	if _, err := deployer.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, stderr.String())
	}
	for _, script := range runner.scripts() {
		if strings.Contains(script, `"$CLI" release`) {
			t.Errorf("external integration triggered a release:\n%s", script)
		}
	}
	if !strings.Contains(stderr.String(), "no managed integrations") {
		t.Errorf("deploy did not report that the release step was skipped:\n%s", stderr.String())
	}
}

// mustWriteManifest writes the local manifest the deployer reads to decide
// whether preparation is needed.
func mustWriteManifest(t *testing.T, cfg Config, name, mode string) {
	t.Helper()
	dir := filepath.Join(cfg.IntegrationsPath, name)
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

// A deploy limited to one integration must not let --delete remove the others
// from the host: they are on the remote and not in the source tree being
// pushed, which is exactly what --delete removes by default.
func TestLimitedDeployProtectsOtherIntegrations(t *testing.T) {
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
	if !strings.Contains(pushed, "protect /integrations/***") {
		t.Errorf("a limited deploy did not protect the other integrations:\n%s", pushed)
	}

	// An unrestricted deploy should not carry the protection, so removing an
	// integration locally still removes it remotely.
	runner.records = nil
	deployer.Config.Limited = false
	if err := deployer.pushSources(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(runner.pushed(), "\n"), "protect") {
		t.Error("an unrestricted deploy protected the integrations tree")
	}
}
