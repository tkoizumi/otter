package deploy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Result is what a successful deploy reports back to the CLI.
type Result struct {
	Host         string
	Platform     string
	Version      string
	Revision     string
	FirstDeploy  bool
	APIToken     string
	RevealToken  bool
	Warnings     []string
	Integrations []string
	Bindings     []Binding
	RemoteDir    string
	Workspace    string
	APIURL       string
	ServiceUnit  string
	DryRun       bool
	Elapsed      time.Duration
}

// Deployer converges one host. Its collaborators are injected so the whole
// sequence can be tested without a server, a compiler or a network.
type Deployer struct {
	Config  Config
	Store   StateStore
	Builder Builder
	// Binaries is the source the builder will use, when it is known. It is
	// only read to describe the build step in the plan; the builder owns the
	// actual work.
	Binaries BinarySource
	Runner   Runner

	// WorkspaceRequest is --workspace: an existing workspace on the host to
	// deploy into. Empty uses the project's own.
	WorkspaceRequest string
	// WorkspaceCreated reports that this run created the workspace rather than
	// adopting one, which is what decides whether a token is new to the
	// operator.
	WorkspaceCreated bool

	// Stdout carries the machine-readable result. Progress goes to Stderr so
	// that the result can be piped somewhere without being polluted.
	Stdout io.Writer
	Stderr io.Writer

	// UV overrides the uv executable used on the host during preparation. The
	// default is the vendored copy under the install root.
	UV string
	// NoUV skips vendoring uv, for hosts that already provide it.
	NoUV bool
	// UVVersion overrides the pinned uv release to vendor.
	UVVersionOverride string
}

// Run performs the converge.
//
// Steps are deliberately ordered so that anything destructive happens only
// after everything reversible has succeeded:
//
//	detect platform -> build -> stage -> push sources -> write secrets ->
//	install unit -> restart -> verify health
//
// The remote data directory is never read, written or deleted at any point.
func (d *Deployer) Run(ctx context.Context) (*Result, error) {
	started := time.Now()

	cfg := d.Config
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// The platform is the one thing a local guess must never decide: building
	// for the wrong architecture yields a binary that fails on the host with a
	// bare "exec format error".
	if cfg.Target.Platform == "" {
		detected, err := d.detectPlatform(ctx)
		if err != nil {
			return nil, err
		}
		cfg.Target.Platform = detected
	} else {
		d.step("platform", "%s (pinned by --platform or state)", cfg.Target.Platform)
	}

	// From here on the deployer's own config is the single source of truth:
	// resolveWorkspace and resolveToken mutate the target, and both the result
	// and the state file are read back from it rather than from this local copy.
	d.Config = cfg

	if cfg.Target.Platform != defaultPlatform() {
		d.step("platform", "building for %s; this machine is %s",
			cfg.Target.Platform, defaultPlatform())
	}

	// Which workspace this deploy owns comes before the token and before any
	// remote path is used: the token lives in the workspace's own environment
	// file, and every other path derives from the workspace name.
	state, hadPrevious, err := d.Store.Load()
	if err != nil {
		return nil, err
	}
	previous, _, err := state.ForHost(cfg.Target.Host)
	if err != nil {
		return nil, err
	}
	if err := d.resolveWorkspace(ctx, previous); err != nil {
		return nil, err
	}
	cfg = d.Config

	// A dry run reports the plan before touching the token: generating,
	// rotating or adopting one is a change, and a plan must be free of side
	// effects even in what it prints.
	if cfg.DryRun {
		return d.plan(cfg.MissingSecrets(cfg.RequiredSecrets()), started), nil
	}

	// Reuse the token from the previous deploy, then the remote file, and only
	// then invent one. Regenerating a token on every deploy would break any
	// client the operator already has, which is a hostile default.
	reveal, err := d.resolveToken(ctx, !d.WorkspaceCreated)
	if err != nil {
		return nil, err
	}
	// resolveToken may have generated, rotated or adopted a token; pick up the
	// result so the rest of the run reports what was actually installed.
	cfg = d.Config

	// --- local build -------------------------------------------------------
	outDir, err := d.Builder.TempDir()
	if err != nil {
		return nil, err
	}

	d.step("build", "cross-compiling otterd and otter for %s", cfg.Target.Platform)
	if err := d.Builder.Build(ctx, cfg, outDir); err != nil {
		return nil, err
	}
	if err := d.Builder.Stage(cfg, outDir); err != nil {
		return nil, err
	}

	// A managed integration needs uv on the host for preparation, and a fresh
	// host has none. Vendoring it keeps the promise that a host needs nothing
	// installed in advance.
	managed := d.managedIntegrations(cfg)
	if len(managed) > 0 && !d.NoUV {
		d.step("build", "vendoring uv %s for %s", d.uvVersion(), cfg.Target.Platform)
		if err := d.Builder.VendorUV(ctx, cfg, outDir); err != nil {
			return nil, err
		}
	}

	revision, err := d.Builder.Revision(outDir)
	if err != nil {
		return nil, err
	}

	// --- prepare the host --------------------------------------------------
	// The account and the directory layout come first. rsync creates its
	// destination directory but not the levels above it, and a workspace sits
	// two levels inside the install root, so /opt/otter/workspaces/<name> has
	// to exist before the first file is sent.
	d.step("prepare", "creating the service account and layout")
	if err := d.Runner.RunScript(ctx, PrepareScript(cfg.Target)); err != nil {
		return nil, d.hint(err)
	}

	// --- push --------------------------------------------------------------
	d.step("sync", "pushing %d integration(s) and the shared library", len(cfg.Integrations))
	if err := d.pushSources(ctx, outDir); err != nil {
		return nil, err
	}
	d.step("sync", "pushing the binaries")
	if err := d.pushBinaries(ctx, outDir); err != nil {
		return nil, err
	}
	if len(managed) > 0 && !d.NoUV {
		d.step("sync", "pushing uv")
		if err := d.pushTools(ctx, outDir); err != nil {
			return nil, err
		}
	}

	// --- ownership ---------------------------------------------------------
	// The push wrote as the login user, so the daemon would not own its own
	// tree without this, and the service account could not write the identity
	// marker each integration needs.
	d.step("prepare", "handing the workspace to %s", cfg.Target.RunAsUser)
	if err := d.Runner.RunScript(ctx, ClaimOwnershipScript(cfg.Target)); err != nil {
		return nil, d.hint(err)
	}

	// Record the workspace before anything else lands. A retry after a failed
	// deploy then finds the same workspace and port instead of starting a
	// second one beside it.
	if err := d.Runner.RunScript(ctx, WriteWorkspaceRecordScript(cfg.Target,
		RecordFor(cfg.Target, time.Now().UTC()))); err != nil {
		return nil, fmt.Errorf("record workspace on %s: %w", cfg.Target, err)
	}

	// --- environment -------------------------------------------------------
	if err := d.writeDaemonEnv(ctx); err != nil {
		return nil, err
	}
	envRevision, err := d.writeSharedEnv(ctx)
	if err != nil {
		return nil, err
	}

	// --- release -----------------------------------------------------------
	// Runs before the restart so a failure leaves the previously deployed
	// runtime serving. Each integration is staged, prepared and then activated
	// in that order, so a candidate that fails preparation never becomes
	// active and never disturbs what is currently running.
	//
	// Every integration is released, not only the managed ones: a run executes
	// the active release, so an unreleased integration would deploy and then
	// refuse to run. Preparation is the managed-only part, and it happens
	// inside the same command.
	// uv is named only when something actually needs preparing: a host with no
	// managed integrations never receives the vendored toolchain, so pointing
	// the release at it would name a path that is not there.
	uvPath := ""
	if len(managed) > 0 {
		uvPath = d.uvPath(cfg)
	}
	if len(cfg.Integrations) > 0 {
		d.step("release", "releasing %d integration(s)", len(cfg.Integrations))
		if err := d.Runner.RunScript(ctx, ReleaseScript(cfg.Target, cfg.IntegrationNames(), uvPath)); err != nil {
			return nil, fmt.Errorf("release integrations on %s: %w", cfg.Target, err)
		}
	} else {
		d.step("release", "no integrations; nothing to release")
	}

	// --- activate ----------------------------------------------------------
	d.step("install", "writing %s and restarting %s", cfg.Target.UnitPath(), cfg.Target.ServiceUnit())
	if err := d.Runner.RunScript(ctx, ActivateScript(cfg.Target)); err != nil {
		return nil, d.hint(err)
	}
	if err := d.Runner.RunScript(ctx, ServiceStatusScript(cfg.Target)); err != nil {
		return nil, d.hint(err)
	}

	d.step("verify", "waiting for the health endpoint")
	if err := d.waitForHealth(ctx); err != nil {
		return nil, err
	}

	// --- bind --------------------------------------------------------------
	// The destination mints its own identity for each integration. Recording
	// which one it chose is what makes a later deploy able to say "same
	// instance, new code" instead of re-registering, and makes the remote
	// registration inspectable from the checkout.
	bindings, err := d.readBindings(ctx, cfg)
	if err != nil {
		d.step("bindings", "destination identities not recorded: %v", err)
	} else {
		d.step("bindings", "recorded %d destination identity(ies)", len(bindings))
	}

	// --- remember ----------------------------------------------------------
	// One record per host: a project may own a workspace on several machines,
	// and this deploy only speaks for the one it just converged.
	state.Put(HostDeploy{
		Host:            cfg.Target.Host,
		Target:          cfg.Target,
		Version:         cfg.Version,
		Revision:        revision,
		SecretsRevision: envRevision,
		DeployedAt:      time.Now().UTC(),
		Bindings:        bindings,
	})
	if err := d.Store.Save(state, cfg.Target.APIToken, cfg.Target.Host, cfg.Target.WorkspaceID); err != nil {
		return nil, err
	}

	d.step("done", "%s deployed to %s in %s", cfg.Version, cfg.Target, time.Since(started).Round(time.Second))
	return &Result{
		Host:         cfg.Target.Host,
		Platform:     cfg.Target.Platform,
		Version:      cfg.Version,
		Revision:     revision,
		FirstDeploy:  !hadPrevious,
		APIToken:     cfg.Target.APIToken,
		RevealToken:  reveal,
		Integrations: cfg.IntegrationNames(),
		Bindings:     bindings,
		RemoteDir:    cfg.Target.RemoteDir,
		Workspace:    cfg.Target.WorkspaceName(),
		APIURL:       cfg.Target.APIURL(),
		ServiceUnit:  cfg.Target.ServiceUnit(),
		Elapsed:      time.Since(started),
	}, nil
}

// readBindings asks the destination runtime for the identity it assigned each
// deployed integration. Local and remote identities are independent, so this
// is the only place the two are related; a host whose runtime predates the
// registry fails here and the deploy records no bindings rather than failing.
func (d *Deployer) readBindings(ctx context.Context, cfg Config) ([]Binding, error) {
	var stdout, stderr strings.Builder
	if err := d.Runner.RunStream(ctx, BindingsScript(cfg.Target), &stdout, &stderr); err != nil {
		return nil, err
	}
	body := strings.TrimSpace(stdout.String())
	if body == "" {
		return nil, nil
	}
	var rows []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Path   string `json:"path"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		return nil, fmt.Errorf("parse destination identities: %w", err)
	}
	out := make([]Binding, 0, len(rows))
	for _, row := range rows {
		if row.Status != "" && row.Status != "active" {
			continue
		}
		out = append(out, Binding{Name: row.Name, ID: row.ID, Path: row.Path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// detectPlatform opens the SSH connection and asks the host what it is. This
// doubles as the reachability check, so a bad host or a missing sudo rule is
// reported before anything is built.
func (d *Deployer) detectPlatform(ctx context.Context) (string, error) {
	if opener, ok := d.Runner.(interface{ Open(context.Context) error }); ok {
		d.step("connect", "checking ssh access to %s", d.Config.Target)
		if err := opener.Open(ctx); err != nil {
			return "", connectError(d.Config.Target, err)
		}
	}
	platformer, ok := d.Runner.(interface {
		Platform(context.Context) (string, error)
	})
	if !ok {
		return d.Config.Target.Platform, nil
	}
	platform, err := platformer.Platform(ctx)
	if err != nil {
		return "", err
	}
	d.step("connect", "%s is %s", d.Config.Target, platform)
	return platform, nil
}

// connectError adds the "check your ssh" hint only when ssh is actually the
// problem. A host that answers ssh but lacks rsync produces a clear message of
// its own, and appending an ssh hint to it sends the operator down the wrong
// path.
func connectError(target Target, err error) error {
	if !isAuthOrTransportFailure(err) {
		return fmt.Errorf("connect to %s: %w", target, err)
	}
	return fmt.Errorf("connect to %s: %w\nhint: otter deploy needs non-interactive ssh "+
		"(a key, or an ssh-agent). Test it with: ssh %s true", target, err, target)
}

// isAuthOrTransportFailure recognises the ssh failures that a key or an agent
// would fix.
func isAuthOrTransportFailure(err error) bool {
	msg := err.Error()
	for _, marker := range []string{
		"Permission denied",
		"Connection refused",
		"Connection timed out",
		"Host key verification failed",
		"no such identity",
		"Operation timed out",
		"Could not resolve hostname",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// resolveToken decides which API token the deployed daemon will require. It
// reports whether the CLI should print the token: a token the operator has not
// seen before is useless to them unless it is shown.
func (d *Deployer) resolveToken(ctx context.Context, hadPrevious bool) (reveal bool, err error) {
	cfg := &d.Config
	target := &cfg.Target

	if target.RotateAPIToken && target.APIToken != "" {
		return false, errors.New("--rotate-token and --api-token are mutually exclusive")
	}

	// 1. The token from this checkout's last deploy. It is already in the
	//    operator's .otter/ directory, so there is nothing new to show them.
	stored, err := d.Store.LoadToken(target.Host, target.WorkspaceID)
	if err != nil {
		return false, err
	}
	if stored != "" && !target.RotateAPIToken {
		target.APIToken = stored
		d.step("token", "reusing the stored API token")
		return false, nil
	}

	// 2. An explicit flag. Run has already installed this token at least once,
	//    so the operator has seen it, but printing it is harmless and helps
	//    when the value came from a config file or the environment.
	if target.APIToken != "" {
		d.step("token", "using the token from --api-token")
		return true, nil
	}
	if target.RotateAPIToken {
		token, err := newToken()
		if err != nil {
			return false, err
		}
		target.APIToken = token
		d.step("token", "rotated: a fresh token was generated")
		return true, nil
	}

	// 3. Whatever the host already has, so redeploying from a second machine
	//    (or after deleting .otter/) does not lock the operator out.
	remote, err := d.readRemoteToken(ctx)
	if err != nil {
		return false, err
	}
	if remote != "" {
		target.APIToken = remote
		d.step("token", "reusing the token already installed on the host")
		return !hadPrevious, nil
	}

	// 4. Nothing anywhere: make one.
	token, err := newToken()
	if err != nil {
		return false, err
	}
	target.APIToken = token
	d.step("token", "generated a new API token")
	return true, nil
}

// readRemoteToken reads OTTER_API_TOKEN out of the installed env files. A
// failure is not fatal: a host that has never been deployed simply has none.
func (d *Deployer) readRemoteToken(ctx context.Context) (string, error) {
	command := "grep -h '^OTTER_API_TOKEN=' " + ShellQuote(d.Config.Target.SharedEnvFilePath()) +
		" 2>/dev/null | head -n1 | cut -d= -f2- || true"
	out, err := d.Runner.Output(ctx, command)
	if err != nil {
		if d.Config.Verbose {
			d.step("token", "could not read a remote token (%v); will create one", err)
		}
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

// pushSources mirrors the runtime tree onto the host.
//
// The excludes are what make a converging push safe. `bin` and `tools` hold
// artifacts pushed separately, and the rest are runtime state that lives under
// the install root: the data directory (SQLite, prepared environments,
// releases) and the deploy bookkeeping. Without them --delete would try to
// remove live data, and rsync would fail on the first non-empty directory it
// could not unlink.
func (d *Deployer) pushSources(ctx context.Context, outDir string) error {
	// Everything derived rather than staged: the binaries and toolchain are
	// pushed separately, and .otter holds this workspace's own state (SQLite,
	// releases, prepared interpreters) plus the record written just before.
	// The leading slash anchors the record pattern to the transfer root so an
	// integration that happens to contain a workspace.json keeps it.
	excludes := []string{"bin", "tools", StateDirName, "/" + WorkspaceRecordName}

	args := make([]string, 0, len(excludes)*2+4)
	for _, name := range excludes {
		args = append(args, "--exclude", name)
	}
	// A limited deploy carries one integration. --delete would then remove
	// every other integration directory from the host, taking deployed code
	// with it, so the rest of the integrations tree is protected from deletion
	// while still being skipped for transfer.
	//
	// A shared tree can land outside integrations/ -- a manifest whose
	// python.path reaches above the integration root puts it there -- so each
	// tree this deploy carries is protected by name as well. Otherwise
	// deploying one integration would delete a library the others import.
	if d.Config.Limited {
		args = append(args, "--filter", "protect /"+LocalIntegrationsDir+"/***")
		for _, rel := range d.stagedTreePaths() {
			args = append(args, "--filter", "protect /"+strings.SplitN(rel, "/", 2)[0]+"/***")
		}
	}
	// The sweep is scoped to this workspace: --delete can no longer reach
	// another workspace's tree, which is what makes several workspaces on one
	// host safe.
	return d.Runner.Push(ctx, outDir, d.Config.Target.WorkspaceDir(), args...)
}

// stagedTreePaths returns the remote-relative placement of every shared tree
// this deploy carries. A tree that cannot be placed was already refused by
// Stage, so a failure here is ignored rather than reported twice.
func (d *Deployer) stagedTreePaths() []string {
	var out []string
	seen := map[string]bool{}
	for _, integ := range d.Config.Integrations {
		for _, tree := range integ.Trees {
			rel, err := TreePlacement(integ, tree)
			if err != nil || seen[rel] {
				continue
			}
			seen[rel] = true
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

// pushBinaries sends the two executables separately from the source tree, so
// the sources can converge with --delete without ever removing bin/. rsync
// writes each file to a temporary name and renames it into place, so a running
// daemon's executable is never truncated underneath it.
func (d *Deployer) pushBinaries(ctx context.Context, outDir string) error {
	return d.Runner.Push(ctx, filepath.Join(outDir, "bin"),
		filepath.Join(d.Config.Target.WorkspaceDir(), "bin"))
}

// pushTools sends the vendored toolchain. It goes separately from the sources
// because the source push excludes it, and separately from the binaries because
// it only exists for deployments that prepare environments.
func (d *Deployer) pushTools(ctx context.Context, outDir string) error {
	return d.Runner.Push(ctx, filepath.Join(outDir, "tools"),
		d.Config.Target.ToolsDir())
}

// writeDaemonEnv uploads the daemon-wide environment file, when the checkout
// has one.
//
// It is written over SSH stdin like the secrets files, because it may hold a
// credential: a Slack webhook URL contains its own token in the path.
func (d *Deployer) writeDaemonEnv(ctx context.Context) error {
	cfg := d.Config
	if cfg.DaemonEnv == "" {
		d.step("env", "no %s; nothing daemon-wide to configure", DaemonEnvFileName)
		return nil
	}

	secrets, err := LoadSecrets(cfg.DaemonEnv)
	if err != nil {
		return fmt.Errorf("read daemon environment %s: %w", cfg.DaemonEnv, err)
	}
	if len(secrets) == 0 {
		d.step("env", "%s is empty; nothing daemon-wide to configure", cfg.DaemonEnv)
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by `otter deploy` from %s. Mode 0600, root owned.\n", DaemonEnvFileName)
	b.WriteString("# Daemon-wide settings, not per-integration secrets. Re-run otter deploy\n")
	b.WriteString("# to refresh; do not edit by hand.\n\n")

	keys := make([]string, 0, len(secrets))
	for key := range secrets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "%s=%s\n", key, secrets[key])
		d.step("env", "  %s", key)
	}

	path := cfg.Target.DaemonEnvFilePath()
	script := `set -e
umask 077
install -d -m 0700 ` + ShellQuote(cfg.Target.EnvDir()) + `
cat > ` + ShellQuote(path) + ` <<'OTTER_ENV_EOF'
` + b.String() + `OTTER_ENV_EOF
chmod 0600 ` + ShellQuote(path) + `
`
	if err := d.Runner.RunScript(ctx, script); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	d.step("env", "wrote %d daemon setting(s) to %s", len(secrets), path)
	return nil
}

// writeSharedEnv writes the credentials file every integration draws from, over
// SSH stdin, and returns a hash of its contents.
//
// One file, not one per integration. The daemon merges every EnvironmentFile=
// into a single process environment and each integration receives only the keys
// its manifest declares, so per-integration files never isolated anything --
// they just made rotating one credential an N-file edit.
//
// Secrets never appear in a command line, not even a quoted one: argv is
// visible to every process on the host for the lifetime of the call.
func (d *Deployer) writeSharedEnv(ctx context.Context) (string, error) {
	cfg := d.Config
	h := newHasher()

	secrets := map[string]string{}
	if cfg.SharedEnv != "" {
		loaded, err := LoadSecrets(cfg.SharedEnv)
		if err != nil {
			return "", fmt.Errorf("read shared environment %s: %w", cfg.SharedEnv, err)
		}
		secrets = loaded
	}

	// The API token lives here too, and it is worth writing even with no
	// secrets: the CLI on the host reads it from this directory to reach a
	// loopback API without an operator exporting it by hand.
	if len(secrets) == 0 && cfg.Target.APIToken == "" {
		d.step("secrets", "nothing shared to configure; no %s written", SharedEnvFileName)
		return h.sum(), nil
	}

	content := SharedEnvFile(cfg.Target.APIToken, secrets)
	h.add("shared", content)

	if len(secrets) == 0 {
		d.step("secrets", "no %s, deploying anyway (the daemon will refuse to run "+
			"an integration whose declared secrets are absent)", SharedEnvFileName)
	} else {
		d.step("secrets", "%d variable(s) from %s", len(secrets), cfg.SharedEnv)
	}

	path := cfg.Target.SharedEnvFilePath()
	// The directory is created here rather than only by the install script:
	// secrets are written before the unit is installed, so nothing else has
	// made the directory yet, and a deploy must not depend on the ordering
	// of two otherwise independent steps.
	script := `set -e
umask 077
install -d -m 0700 ` + ShellQuote(cfg.Target.EnvDir()) + `
cat > ` + ShellQuote(path) + ` <<'OTTER_ENV_EOF'
` + content + `OTTER_ENV_EOF
chmod 0600 ` + ShellQuote(path) + `
`
	if err := d.Runner.RunScript(ctx, script); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return h.sum(), nil
}

// waitForHealth polls the daemon's health endpoint on the host itself, so the
// API stays bound to loopback and still gets verified.
func (d *Deployer) waitForHealth(ctx context.Context) error {
	auth := ""
	if d.Config.Target.APIToken != "" {
		auth = " -H " + ShellQuote("Authorization: Bearer "+d.Config.Target.APIToken)
	}
	command := "curl -fsS --max-time 5" + auth + " " +
		ShellQuote(d.Config.Target.APIURL()+"/health")

	deadline := time.Now().Add(d.healthTimeout())
	var lastErr error
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		out, err := d.Runner.Output(ctx, command)
		if err == nil {
			d.step("verify", "healthy: %s", oneLine(out))
			return nil
		}
		lastErr = err

		if attempt == 1 && strings.Contains(err.Error(), "401") {
			// A wrong token would otherwise look like a slow start for the
			// whole timeout.
			return fmt.Errorf("the deployed daemon rejected the API token: %w", err)
		}
		select {
		case <-ctx.Done():
			// The enclosing timeout expired mid-wait, so ctx.Err() on its own
			// would be a useless "context deadline exceeded".
			return d.healthFailure(lastErr, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	return d.healthFailure(lastErr, nil)
}

// healthTimeout bounds the wait for the daemon to answer after a restart.
func (d *Deployer) healthTimeout() time.Duration {
	const defaultWait = 30 * time.Second
	if d.Config.Timeout > 0 && d.Config.Timeout < defaultWait {
		return d.Config.Timeout
	}
	return defaultWait
}

// healthFailure builds the error for a daemon that never answered, asking the
// host why rather than leaving the operator to guess.
func (d *Deployer) healthFailure(lastErr, ctxErr error) error {
	target := d.Config.Target

	diagnosis := ""
	out, err := d.Runner.Output(context.Background(), ServiceStatusScript(target)+
		"\nsystemctl show -p ExecMainStatus --value "+ShellQuote(target.ServiceUnit())+"\n")
	if err == nil {
		diagnosis = strings.TrimSpace(out)
	}

	cause := "no response"
	if lastErr != nil {
		cause = lastErr.Error()
	} else if ctxErr != nil {
		cause = ctxErr.Error()
	}

	return fmt.Errorf("the daemon did not answer %s within %s: %s\n%s\n"+
		"hint: inspect it with: ssh %s 'journalctl -u %s -n 50'",
		target.APIURL(), d.healthTimeout(), cause, indent(diagnosis), target, target.ServiceUnit())
}

// plan renders the dry run: everything that would be sent, and nothing that
// touches the host beyond the platform handshake.
func (d *Deployer) plan(missing []string, started time.Time) *Result {
	cfg := d.Config
	d.step("plan", "dry run: no files will be written and the service will not be touched")
	d.step("plan", "host:      %s", cfg.Target)
	d.step("plan", "platform:  %s", cfg.Target.Platform)
	d.step("plan", "version:   %s", cfg.Version)
	d.step("plan", "workspace: %s on %s (unit %s)", cfg.Target.WorkspaceName(), cfg.Target, cfg.Target.ServiceUnit())
	d.step("plan", "tree:      %s", cfg.Target.WorkspaceDir())
	d.step("plan", "data:      %s (never written by a deploy)", cfg.Target.DataDir)
	d.step("plan", "api:       %s (loopback; reach it with ssh -L)", cfg.Target.APIURL())
	if src, ok := d.Binaries.(Describer); ok {
		d.step("plan", "binaries:  would %s", src.Describe(cfg.Target.Platform))
	}
	if cfg.DaemonEnv != "" {
		d.step("plan", "daemon:    %s -> %s", cfg.DaemonEnv, cfg.Target.DaemonEnvFilePath())
	} else {
		d.step("plan", "daemon:    no %s; nothing daemon-wide to configure", DaemonEnvFileName)
	}
	if cfg.SharedEnv != "" {
		d.step("plan", "secrets:   %s -> %s (shared by every integration)",
			cfg.SharedEnv, cfg.Target.SharedEnvFilePath())
	} else {
		d.step("plan", "secrets:   no %s; integrations rely on their manifest env", SharedEnvFileName)
	}
	if len(cfg.Integrations) > 0 {
		managed := d.managedIntegrations(cfg)
		line := "release:   would stage and activate " + strings.Join(cfg.IntegrationNames(), ", ")
		if len(managed) > 0 {
			line += " (preparing managed Python for " + strings.Join(managed, ", ") + ")"
		}
		d.step("plan", line)
	} else {
		d.step("plan", "release:   no integrations; nothing to release")
	}
	for _, m := range missing {
		d.step("plan", "warning:   secret %s", m)
	}

	return &Result{
		Host:         cfg.Target.Host,
		Platform:     cfg.Target.Platform,
		Version:      cfg.Version,
		Warnings:     missing,
		Integrations: cfg.IntegrationNames(),
		RemoteDir:    cfg.Target.RemoteDir,
		Workspace:    cfg.Target.WorkspaceName(),
		APIURL:       cfg.Target.APIURL(),
		ServiceUnit:  cfg.Target.ServiceUnit(),
		DryRun:       true,
		Elapsed:      time.Since(started),
	}
}

// managedIntegrations lists the integrations being deployed that opted into a
// managed Python environment. The local manifests are the source of truth;
// they are the same files that were just pushed.
func (d *Deployer) managedIntegrations(cfg Config) []string {
	var managed []string
	for _, integ := range cfg.Integrations {
		if d.managesPython(integ) {
			managed = append(managed, integ.Name)
		}
	}
	return managed
}

// managesPython reports whether one integration uses managed Python.
func (d *Deployer) managesPython(integ Integration) bool {
	path := filepath.Join(integ.Dir, "otter.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var probe struct {
		Python struct {
			Mode string `yaml:"mode"`
		} `yaml:"python"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}
	return probe.Python.Mode == "managed"
}

// uvPath returns the uv executable to use on the host. An explicit flag wins;
// otherwise this workspace's vendored copy. uv is per workspace because its
// version is part of every prepared environment's identity: sharing it would
// let a deploy in one workspace invalidate another's environments.
func (d *Deployer) uvPath(Config) string {
	if d.UV != "" {
		return d.UV
	}
	return filepath.Join(d.Config.Target.ToolsDir(), "uv", "uv")
}

// uvVersion is the uv release a deploy vendors.
func (d *Deployer) uvVersion() string {
	if d.UVVersionOverride != "" {
		return d.UVVersionOverride
	}
	return PinnedUVVersion
}

// Destroy removes the deployment. The data directory is kept unless the caller
// explicitly opts in to deleting it: it holds every integration's watermark,
// and losing it means rescanning the source system from scratch.
func (d *Deployer) Destroy(ctx context.Context, keepData bool) error {
	cfg := d.Config
	if err := cfg.Validate(); err != nil {
		return err
	}

	if cfg.Target.Platform == "" {
		platform, err := d.detectPlatform(ctx)
		if err != nil {
			return err
		}
		cfg.Target.Platform = platform
		d.Config = cfg
	}

	// Which workspace is being removed is read from the host, so a destroy
	// names one workspace and never the rest of them.
	state, _, err := d.Store.Load()
	if err != nil {
		return err
	}
	previous, _, err := state.ForHost(cfg.Target.Host)
	if err != nil {
		return err
	}
	if err := d.resolveWorkspace(ctx, previous); err != nil {
		return err
	}
	cfg = d.Config

	if cfg.DryRun {
		d.step("plan", "dry run: would remove workspace %s (%s), its unit %s and its secrets (data %s)",
			cfg.Target.WorkspaceName(), cfg.Target.WorkspaceDir(), cfg.Target.ServiceUnit(),
			map[bool]string{true: "kept", false: "DELETED"}[keepData])
		return nil
	}

	d.step("destroy", "stopping and removing %s", cfg.Target.ServiceUnit())
	if err := d.Runner.RunScript(ctx, DestroyScript(cfg.Target, keepData)); err != nil {
		return d.hint(err)
	}
	if keepData {
		d.step("destroy", "kept %s: run history and sync watermarks are intact", cfg.Target.DataDir)
	} else {
		d.step("destroy", "deleted %s: the next deploy starts from an empty database", cfg.Target.DataDir)
	}
	// Forget only the host that was just emptied; other hosts this project
	// deploys to keep their records. When that was the last one there is
	// nothing left to remember, so the state files go too.
	state.Delete(cfg.Target.Host)
	if len(state.Deploys) == 0 {
		if err := d.Store.Remove(); err != nil {
			return err
		}
		d.step("destroy", "removed local deploy state")
		return nil
	}
	if err := d.Store.Save(state, "", "", ""); err != nil {
		return err
	}
	d.step("destroy", "forgot %s in the local deploy state", cfg.Target.Host)
	return nil
}

// resolveWorkspace decides which workspace on the host this deploy owns.
//
// It reads the host's workspace records and either adopts the one this project
// already has -- by id, or the one --workspace named -- or takes the next free
// loopback port for a new one. Nothing is written here, so a dry run reports
// exactly the workspace it would create; the record itself is written once the
// directory exists.
func (d *Deployer) resolveWorkspace(ctx context.Context, previous HostDeploy) error {
	target := d.Config.Target

	out, err := d.Runner.Output(ctx, WorkspaceListScript(target))
	if err != nil {
		return fmt.Errorf("read the workspaces on %s: %w", target, err)
	}
	records, listening, err := ParseWorkspaceList(out)
	if err != nil {
		return err
	}

	resolved, created, err := SelectWorkspace(target, records, listening, d.WorkspaceRequest)
	if err != nil {
		return err
	}
	d.WorkspaceCreated = created
	d.Config.Target = resolved

	if !created {
		d.step("workspace", "using %s on %s (%s)", resolved.WorkspaceName(), resolved, resolved.Listen)
		return nil
	}

	d.step("workspace", "creating %s on %s (%s)", resolved.WorkspaceName(), resolved, resolved.Listen)
	// Starting a second workspace is usually a forgotten identity rather than
	// an intent: the project's id lives in otter.deploy.yaml so another machine
	// or checkout lands on the same one.
	if len(records) > 0 {
		d.step("workspace", "hint: commit `workspace: %s` to %s to reuse this workspace from another checkout",
			resolved.WorkspaceID, ConfigFileName)
	}
	return nil
}

// hint adds the most likely explanation to an opaque ssh failure.
func (d *Deployer) hint(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "sudo"):
		return fmt.Errorf("%w\nhint: systemd needs root; deploy as root or grant NOPASSWD sudo", err)
	case strings.Contains(msg, "unit not found"), strings.Contains(msg, "Failed to restart"):
		return fmt.Errorf("%w\nhint: is this a systemd host? otter deploy assumes Linux with systemd", err)
	}
	return err
}

// step writes one progress line. Progress always goes to stderr so that stdout
// stays a clean result stream.
func (d *Deployer) step(label, format string, args ...any) {
	if d.Stderr == nil {
		return
	}
	fmt.Fprintf(d.Stderr, "%-9s %s\n", label+":", fmt.Sprintf(format, args...))
}

// newToken returns a 256-bit random bearer token.
func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate API token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// secretsHasher accumulates a short, stable digest of the rendered environment
// files, so that rotating a credential is visible in the state file.
type secretsHasher struct{ h hash.Hash }

func newHasher() *secretsHasher { return &secretsHasher{h: sha256.New()} }

func (s *secretsHasher) add(name, content string) {
	fmt.Fprintf(s.h, "%s\x00%s\x00", name, content)
}

func (s *secretsHasher) sum() string {
	return hex.EncodeToString(s.h.Sum(nil))[:16]
}
