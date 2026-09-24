package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testTarget is a fixed workspace on a fixed host, so the rendered scripts and
// paths can be asserted exactly. A randomly minted workspace id would make
// every expected path different on every run.
func testTarget() Target {
	t := Target{
		Host:          "droplet",
		Platform:      "linux/arm64",
		RemoteDir:     DefaultRemoteDir,
		RunAsUser:     "otter",
		WorkspaceID:   "24856da9-1111-2222-3333-444444444444",
		WorkspaceSlug: "examples",
		Listen:        DefaultListenAddr,
	}
	return t.fillWorkspaceDefaults()
}

func TestUnitFileRendersEnvironment(t *testing.T) {
	target := testTarget()
	unit := UnitFile(target)

	required := []string{
		"User=otter",
		"Group=otter",
		"WorkingDirectory=" + target.WorkspaceDir(),
		"ExecStart=" + target.BinaryPath() + " --integrations " + target.IntegrationsDir() +
			" --data " + target.DataDir + " --listen " + target.Listen,
		"EnvironmentFile=-" + target.DaemonEnvFilePath(),
		"EnvironmentFile=-" + target.SharedEnvFilePath(),
		"Restart=always",
		"WantedBy=multi-user.target",
	}
	for _, want := range required {
		if !strings.Contains(unit, want) {
			t.Errorf("unit file is missing %q\n---\n%s", want, unit)
		}
	}

	// A wildcard EnvironmentFile would be silently ignored by systemd, which
	// would leave the daemon running with no secrets at all.
	if strings.Contains(unit, "*.env") {
		t.Errorf("unit file uses a wildcard EnvironmentFile:\n%s", unit)
	}
	// One shared credentials file, not one per integration. The daemon's
	// environment is a single process environment, so per-integration files
	// never isolated anything -- they only multiplied rotation sites.
	if strings.Contains(unit, "counter.env") {
		t.Errorf("unit file still references a per-integration env file:\n%s", unit)
	}
}

func TestUnitFileNeverTouchesTheDataDirectory(t *testing.T) {
	unit := UnitFile(testTarget())
	// The data directory is only ever an argument. Anything that deletes or
	// recreates it on deploy would destroy every integration's watermark.
	for _, forbidden := range []string{"ExecStartPre", "ExecStopPost", "rm -rf"} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("unit file contains %q, which could disturb the data directory:\n%s", forbidden, unit)
		}
	}
}

func TestEnvFileSortsAndQuotes(t *testing.T) {
	env := SharedEnvFile("tok123", map[string]string{
		"ZED":   "last",
		"ALPHA": "first",
	})

	alpha := strings.Index(env, "ALPHA=first")
	zed := strings.Index(env, "ZED=last")
	if alpha < 0 || zed < 0 {
		t.Fatalf("env file is missing a variable:\n%s", env)
	}
	if alpha > zed {
		t.Errorf("variables are not sorted:\n%s", env)
	}
	if !strings.Contains(env, "OTTER_API_TOKEN=tok123") {
		t.Errorf("env file is missing the API token:\n%s", env)
	}
}

func TestEnvFileWithoutToken(t *testing.T) {
	env := SharedEnvFile("", map[string]string{"A": "1"})
	if strings.Contains(env, "OTTER_API_TOKEN") {
		t.Errorf("env file should omit an empty token:\n%s", env)
	}
}

func TestInstallScriptBakesTheUnit(t *testing.T) {
	target := testTarget()
	script := InstallScript(target)

	for _, want := range []string{
		"set -e",
		"UNIT=" + ShellQuote(target.UnitPath()),
		`cat > "$UNIT" <<'OTTER_UNIT_EOF'`,
		"OTTER_UNIT_EOF",
		"systemctl daemon-reload",
		`systemctl restart "$SERVICE"`,
		// The unit body must actually be inside the heredoc.
		"ExecStart=" + target.BinaryPath(),
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install script is missing %q\n---\n%s", want, script)
		}
	}

	// The heredoc delimiter must terminate the script's own heredoc and the
	// unit must come before it, or systemd would read a truncated unit.
	if strings.Index(script, "ExecStart=") > strings.Index(script, "\nOTTER_UNIT_EOF") {
		t.Errorf("unit body is outside the heredoc:\n%s", script)
	}
}

func TestInstallScriptCreatesDirectoriesBeforePushing(t *testing.T) {
	script := InstallScript(testTarget())
	for _, want := range []string{
		`install -d -m 0755 -o "$RUN_AS" -g "$RUN_AS" "$WORKSPACE_DIR" "$WORKSPACE_DIR/bin" "$WORKSPACE_DIR/integrations" "$WORKSPACE_DIR/tools"`,
		`install -d -m 0700 -o "$RUN_AS" -g "$RUN_AS" "$DATA_DIR"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install script must create the workspace layout (%q):\n%s", want, script)
		}
	}
}

// The data directory holds the live SQLite database and the sync watermarks.
// Derived state inside it may be handed to the service account, but the
// database itself must never be walked or rewritten by a deploy.
func TestPrepareScriptDoesNotRewriteTheDatabase(t *testing.T) {
	target := testTarget()
	script := PrepareScript(target)

	if strings.Contains(script, `chown -R "$RUN_AS:$RUN_AS" "$REMOTE_DIR"`) {
		t.Errorf("script recursively chowns the whole install root, which includes data:\n%s", script)
	}
	if strings.Contains(script, `chown -R "$RUN_AS:$RUN_AS" "$DATA_DIR"`) {
		t.Errorf("script recursively chowns the whole data directory:\n%s", script)
	}
	for _, line := range strings.Split(script, "\n") {
		if !strings.Contains(line, "chown") {
			continue
		}
		for _, forbidden := range []string{"otter.db", `"$DATA_DIR"/*`} {
			if strings.Contains(line, forbidden) {
				t.Errorf("prepare touches database files (%s): %s", forbidden, line)
			}
		}
	}
	// Creating it once, with the right owner and mode, is the one exception.
	if !strings.Contains(script, `install -d -m 0700 -o "$RUN_AS" -g "$RUN_AS" "$DATA_DIR"`) {
		t.Errorf("prepare script does not create the data directory:\n%s", script)
	}
}

func TestInstallScriptOwnsTheVendoredToolchain(t *testing.T) {
	script := InstallScript(testTarget())
	// A vendored uv is provisioned out of band; it must end up owned by the
	// service account that runs preparation.
	if !strings.Contains(script, "for dir in bin integrations lib tools; do") {
		t.Errorf("install script does not take ownership of tools/:\n%s", script)
	}
}

// The unit loads daemon-wide settings and then the shared credentials, and
// tolerates either being absent so a deployment that configures nothing extra
// still starts.
func TestUnitFileLoadsTheEnvironmentFilesInPrecedenceOrder(t *testing.T) {
	target := testTarget()
	unit := UnitFile(target)

	for _, want := range []string{
		"EnvironmentFile=-" + target.DaemonEnvFilePath(),
		"EnvironmentFile=-" + target.SharedEnvFilePath(),
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit is missing %q\n---\n%s", want, unit)
		}
	}
	// The leading dash is what makes an absent file non-fatal.
	if !strings.Contains(unit, "EnvironmentFile=-"+target.DaemonEnvFilePath()) {
		t.Errorf("the daemon environment file is not optional:\n%s", unit)
	}
	// systemd applies a later EnvironmentFile over an earlier one, so the
	// shared credentials must be loaded after the daemon's settings.
	if strings.Index(unit, target.DaemonEnvFilePath()) > strings.Index(unit, target.SharedEnvFilePath()) {
		t.Errorf("the shared environment is loaded before the daemon's:\n%s", unit)
	}
}

func TestEnvFilePathsAreStable(t *testing.T) {
	target := testTarget()
	name := target.WorkspaceName()
	if got, want := target.DaemonEnvFilePath(), "/etc/otter/workspaces/"+name+".daemon.env"; got != want {
		t.Errorf("DaemonEnvFilePath = %q, want %q", got, want)
	}
	if got, want := target.SharedEnvFilePath(), "/etc/otter/workspaces/"+name+".env"; got != want {
		t.Errorf("SharedEnvFilePath = %q, want %q", got, want)
	}
	if target.DaemonEnvFilePath() == target.SharedEnvFilePath() {
		t.Error("the daemon and shared environment files resolve to the same path")
	}
}

// The release command names the integrations discovery root explicitly, so the
// host never has to guess which directory to scan for manifests.
func TestReleaseScriptNamesTheDiscoveryRoot(t *testing.T) {
	target := testTarget()
	script := ReleaseScript(target, []string{"counter", "invoices"}, "/opt/otter/tools/uv/uv")

	for _, want := range []string{
		`"$CLI" release`,
		"--integrations " + ShellQuote(target.IntegrationsDir()),
		"--data " + ShellQuote(target.DataDir),
		"--uv " + ShellQuote("/opt/otter/tools/uv/uv"),
		// Each integration is released by its destination path, so a failure
		// is reported against the integration that caused it and identity
		// resolution never mistakes a directory basename for a label.
		ShellQuote(target.IntegrationsDir() + "/counter"),
		ShellQuote(target.IntegrationsDir() + "/invoices"),
	} {
		if !strings.Contains(script, want) {
			t.Errorf("release script is missing %q\n---\n%s", want, script)
		}
	}
	if want := target.WorkspaceDir() + "/integrations"; target.IntegrationsDir() != want {
		t.Errorf("IntegrationsDir = %q, want %q", target.IntegrationsDir(), want)
	}
	// Each integration is released separately, so a failure is reported
	// against the integration that caused it.
	if strings.Count(script, `"$CLI" release`) != 2 {
		t.Errorf("release script does not release each integration separately:\n%s", script)
	}
}

// A non-login ssh command starts in the login user's home, and on a stock image
// that is /root, mode 0700. The release runs the CLI as the service account,
// which cannot traverse it, and the CLI resolves its workspace from the working
// directory even when every path it was handed is absolute -- so the script has
// to move somewhere reachable before dropping privileges.
func TestServiceScriptsRunFromAReachableDirectory(t *testing.T) {
	target := testTarget()

	// The release script works in variables, so it moves to $REMOTE_DIR.
	release := ReleaseScript(target, []string{"counter"}, "")
	cdAt := strings.Index(release, `cd "$WORKSPACE_DIR"`)
	cliAt := strings.Index(release, `"$CLI" release`)
	if cdAt < 0 {
		t.Errorf("release script never changes directory:\n%s", release)
	} else if cliAt >= 0 && cdAt > cliAt {
		t.Error("release script drops privileges before moving to a reachable directory")
	}

	// The bindings one is a single command, so it names the directory inline.
	bindings := BindingsScript(target)
	if !strings.HasPrefix(bindings, "cd "+ShellQuote(target.WorkspaceDir())+" && ") {
		t.Errorf("bindings script does not change directory first: %s", bindings)
	}
}

// The host is where an operator lands when a sync is failing, so `otter` has to
// exist there -- but one symlink can only name one workspace. The host-wide
// entry point is therefore a dispatcher that resolves the workspace from the
// working directory, the same rule every other command uses.
func TestActivateScriptInstallsAHostWideDispatcher(t *testing.T) {
	target := testTarget()
	script := ActivateScript(target)

	if !strings.Contains(script, "cat > "+ShellQuote(CLIDispatcherPath)) {
		t.Errorf("activate script does not install %s:\n%s", CLIDispatcherPath, script)
	}
	if !strings.Contains(script, "OTTER_DISPATCH_EOF") {
		t.Error("the dispatcher heredoc is never terminated")
	}

	dispatcher := DispatcherFile(target)
	for _, want := range []string{
		"#!/bin/sh",
		dispatcherMarker,
		ShellQuote(target.WorkspacesRoot()),
		`while [ "$dir" != "/" ]`,
		`exec "$ws/bin/otter" "$@"`,
		// The daemon requires its token even on loopback, so a bare `otter runs`
		// only works if the dispatcher supplies it.
		"OTTER_API_TOKEN",
	} {
		if !strings.Contains(dispatcher, want) {
			t.Errorf("dispatcher is missing %q:\n%s", want, dispatcher)
		}
	}
}

// The dispatcher is shared by every workspace on the host, so destroying one
// workspace must not remove it while others remain.
func TestDestroyKeepsTheDispatcherWhileWorkspacesRemain(t *testing.T) {
	target := testTarget()
	script := DestroyScript(target, false)

	if !strings.Contains(script, dispatcherMarker) {
		t.Error("destroy does not check whether the dispatcher is one this tool wrote")
	}
	if !strings.Contains(script, `remaining=$(ls -d "$WORKSPACES_ROOT"/*/ 2>/dev/null | wc -l)`) {
		t.Errorf("destroy removes the shared dispatcher without counting the remaining workspaces:\n%s", script)
	}
}

// The dispatcher must find the workspace from anywhere inside it -- and from a
// host with several workspaces it must refuse rather than guess.
func TestDispatcherResolvesTheWorkspaceFromTheWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	target := Target{
		RemoteDir:     root,
		WorkspaceID:   "1234abcd-0000-0000-0000-000000000000",
		WorkspaceSlug: "demo",
	}
	target = target.fillWorkspaceDefaults()
	ws := target.WorkspaceDir()

	sub := filepath.Join(ws, "integrations", "one")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".otter"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(ws, "bin", "otter")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\necho \"cli $*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(t.TempDir(), "otter")
	if err := os.WriteFile(script, []byte(DispatcherFile(target)), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", script, "runs", "--limit", "3")
	cmd.Dir = sub
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("dispatcher from a subdirectory: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "cli runs --limit 3" {
		t.Errorf("dispatcher ran %q, want it to forward to the workspace CLI", got)
	}
}
