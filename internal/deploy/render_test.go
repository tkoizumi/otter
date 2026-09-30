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

// testUnitOptions is the resource policy a plain deploy writes: the built-in
// defaults. Tests that do not care about caps use it so the unit they assert on
// is the one production emits.
func testUnitOptions() UnitOptions {
	return UnitOptions{
		MemoryMax:  DefaultMemoryMax,
		MemoryHigh: DefaultMemoryHigh,
		CPUQuota:   DefaultCPUQuota,
		TasksMax:   DefaultTasksMax,
	}
}

func TestUnitFileRendersEnvironment(t *testing.T) {
	target := testTarget()
	unit := UnitFile(target, testUnitOptions())

	required := []string{
		"User=otter",
		"Group=otter",
		"WorkingDirectory=" + target.WorkspaceDir(),
		"ExecStart=" + target.BinaryPath() + " --jobs " + target.JobsDir() +
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
	// One shared credentials file, not one per job. The daemon's
	// environment is a single process environment, so per-job files
	// never isolated anything -- they only multiplied rotation sites.
	if strings.Contains(unit, "counter.env") {
		t.Errorf("unit file still references a per-job env file:\n%s", unit)
	}
}

func TestUnitFileNeverTouchesTheDataDirectory(t *testing.T) {
	unit := UnitFile(testTarget(), testUnitOptions())
	// The data directory is only ever an argument. Anything that deletes or
	// recreates it on deploy would destroy every job's watermark.
	for _, forbidden := range []string{"ExecStartPre", "ExecStopPost", "rm -rf"} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("unit file contains %q, which could disturb the data directory:\n%s", forbidden, unit)
		}
	}
}

// CA-08: the unit bounds the whole workspace. Every cap is emitted verbatim so
// systemd, not this package, interprets the size.
func TestUnitFileEmitsResourceCaps(t *testing.T) {
	opts := UnitOptions{
		MemoryMax:  "4G",
		MemoryHigh: "3G",
		CPUQuota:   "150%",
		TasksMax:   "256",
	}
	unit := UnitFile(testTarget(), opts)

	for _, want := range []string{
		"MemoryMax=4G",
		"MemoryHigh=3G",
		"CPUQuota=150%",
		"TasksMax=256",
	} {
		if !strings.Contains(unit, "\n"+want+"\n") {
			t.Errorf("unit file is missing %q\n---\n%s", want, unit)
		}
	}
}

// The defaults are what a plain `otter deploy` writes, so they must be present
// without any flag being passed.
func TestUnitFileEmitsTheDefaultCaps(t *testing.T) {
	unit := UnitFile(testTarget(), testUnitOptions())
	for _, want := range []string{
		"\nMemoryMax=" + DefaultMemoryMax + "\n",
		"\nMemoryHigh=" + DefaultMemoryHigh + "\n",
		"\nCPUQuota=" + DefaultCPUQuota + "\n",
		"\nTasksMax=" + DefaultTasksMax + "\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit file is missing the default cap %q\n---\n%s", want, unit)
		}
	}
}

// The unset case: an empty value or the explicit "off" emits no directive at
// all, so systemd's own default applies. It must not emit "MemoryMax=0", which
// systemd reads as a real limit of zero.
func TestUnitFileOmitsCapsThatAreOff(t *testing.T) {
	for _, opts := range []UnitOptions{
		{},
		{MemoryMax: CapOff, MemoryHigh: CapOff, CPUQuota: CapOff, TasksMax: CapOff},
		{MemoryMax: "OFF", MemoryHigh: " off ", CPUQuota: "", TasksMax: CapOff},
	} {
		unit := UnitFile(testTarget(), opts)
		for _, forbidden := range []string{"MemoryMax=", "MemoryHigh=", "CPUQuota=", "TasksMax="} {
			if strings.Contains(unit, forbidden) {
				t.Errorf("cap %q was emitted for %+v:\n%s", forbidden, opts, unit)
			}
		}
	}
}

// The sandbox the docs promise. Each directive has to be in the unit body, not
// only in prose.
func TestUnitFileEmitsSandboxHardening(t *testing.T) {
	target := testTarget()
	unit := UnitFile(target, testUnitOptions())

	for _, want := range []string{
		"NoNewPrivileges=true",
		"ProtectSystem=strict",
		"ProtectHome=true",
		"PrivateTmp=true",
		// ProtectSystem=strict makes everything read-only, so the two
		// directories the runtime writes must be reopened explicitly.
		"ReadWritePaths=" + target.WorkspaceDir() + " " + target.DataDir,
	} {
		if !strings.Contains(unit, "\n"+want+"\n") {
			t.Errorf("unit file is missing %q\n---\n%s", want, unit)
		}
	}
}

// systemd's default OOMPolicy=stop stops the whole unit when the OOM killer
// kills any process in it, so a runaway job killed by MemoryMax would take
// otterd and every other in-flight run down with it. The generated unit has to
// opt out of that, or the cap contradicts its own purpose.
func TestUnitFileKeepsTheDaemonAliveOnAnOOMKill(t *testing.T) {
	unit := UnitFile(testTarget(), testUnitOptions())
	if !strings.Contains(unit, "\nOOMPolicy=continue\n") {
		t.Errorf("unit file does not set OOMPolicy=continue:\n%s", unit)
	}
}

// A job that writes scratch outside its workspace needs its path reopened, and
// an opaque mount failure at start is the wrong way to learn the path was
// missing: operator paths are prefixed with "-" so systemd ignores a
// not-yet-existing one. Duplicates are dropped.
func TestUnitFileReadWritePathsAddsOperatorPaths(t *testing.T) {
	target := testTarget()
	unit := UnitFile(target, UnitOptions{ReadWritePaths: []string{"/srv/scratch", "/srv/scratch", target.WorkspaceDir()}})

	want := "ReadWritePaths=" + target.WorkspaceDir() + " " + target.DataDir + " -/srv/scratch"
	if !strings.Contains(unit, want) {
		t.Errorf("unit file ReadWritePaths is wrong:\nwant %q\n---\n%s", want, unit)
	}
	if strings.Count(unit, "/srv/scratch") != 1 {
		t.Errorf("a duplicate ReadWritePaths entry survived:\n%s", unit)
	}
}

// ProtectHome=true makes /home, /root and /run/user inaccessible, and
// ReadWritePaths= cannot reopen a path beneath one. A deploy under any of them
// must fail before the push with a message naming the path.
func TestValidateUnitPathsRejectsProtectedHome(t *testing.T) {
	for _, remoteDir := range []string{"/home/deploy/otter", "/root/otter", "/run/user/1000/otter"} {
		target := Target{RemoteDir: remoteDir, WorkspaceSlug: "demo"}
		target = target.fillWorkspaceDefaults()
		if err := ValidateUnitPaths(target, nil); err == nil {
			t.Errorf("ValidateUnitPaths accepted a workspace under %s", remoteDir)
		}
	}

	// The default install root and an operator's extra path outside a home
	// directory both pass.
	ok := testTarget()
	if err := ValidateUnitPaths(ok, []string{"/srv/scratch"}); err != nil {
		t.Errorf("ValidateUnitPaths rejected /opt/otter with a scratch path: %v", err)
	}
	// An extra writable path under a protected home is rejected too, because
	// it would be just as unreachable.
	if err := ValidateUnitPaths(ok, []string{"/home/deploy/scratch"}); err == nil {
		t.Error("ValidateUnitPaths accepted an extra path under /home")
	}
}

// CapEnabled is the single decision the renderer and the operators share.
func TestCapEnabled(t *testing.T) {
	for value, want := range map[string]bool{
		"4G": true, "75%": true, "off": false, "OFF": false,
		" off ": false, "": false, "   ": false,
	} {
		if got := CapEnabled(value); got != want {
			t.Errorf("CapEnabled(%q) = %v, want %v", value, got, want)
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
	script := InstallScript(target, testUnitOptions())

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
	script := InstallScript(testTarget(), testUnitOptions())
	for _, want := range []string{
		`install -d -m 0755 -o "$RUN_AS" -g "$RUN_AS" "$WORKSPACE_DIR" "$WORKSPACE_DIR/bin" "$WORKSPACE_DIR/jobs" "$WORKSPACE_DIR/tools"`,
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
	script := InstallScript(testTarget(), testUnitOptions())
	// A vendored uv is provisioned out of band; it must end up owned by the
	// service account that runs preparation.
	if !strings.Contains(script, "for dir in bin jobs lib tools; do") {
		t.Errorf("install script does not take ownership of tools/:\n%s", script)
	}
}

// The unit loads daemon-wide settings and then the shared credentials, and
// tolerates either being absent so a deployment that configures nothing extra
// still starts.
func TestUnitFileLoadsTheEnvironmentFilesInPrecedenceOrder(t *testing.T) {
	target := testTarget()
	unit := UnitFile(target, testUnitOptions())

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

// The release command names the jobs discovery root explicitly, so the
// host never has to guess which directory to scan for manifests.
func TestReleaseScriptNamesTheDiscoveryRoot(t *testing.T) {
	target := testTarget()
	script := ReleaseScript(target, []string{"counter", "invoices"}, ReleaseOptions{UVPath: "/opt/otter/tools/uv/uv", Keep: DefaultKeep})

	for _, want := range []string{
		`"$CLI" release`,
		"--jobs " + ShellQuote(target.JobsDir()),
		"--data " + ShellQuote(target.DataDir),
		"--uv " + ShellQuote("/opt/otter/tools/uv/uv"),
		// Each job is released by its destination path, so a failure
		// is reported against the job that caused it and identity
		// resolution never mistakes a directory basename for a label.
		ShellQuote(target.JobsDir() + "/counter"),
		ShellQuote(target.JobsDir() + "/invoices"),
	} {
		if !strings.Contains(script, want) {
			t.Errorf("release script is missing %q\n---\n%s", want, script)
		}
	}
	if want := target.WorkspaceDir() + "/jobs"; target.JobsDir() != want {
		t.Errorf("JobsDir = %q, want %q", target.JobsDir(), want)
	}
	// Each job is released separately, so a failure is reported
	// against the job that caused it.
	if strings.Count(script, `"$CLI" release`) != 2 {
		t.Errorf("release script does not release each job separately:\n%s", script)
	}
}

// A deploy is where convergence is expected: the release step prunes old
// releases to the configured keep window rather than leaving every snapshot on
// the host. --keep 0 is the explicit opt-out and must not emit the flag, so the
// CLI keeps its "retain everything" default.
func TestReleaseScriptPrunesToTheConfiguredKeep(t *testing.T) {
	target := testTarget()

	script := ReleaseScript(target, []string{"counter"}, ReleaseOptions{Keep: 3})
	if !strings.Contains(script, " --keep 3") {
		t.Errorf("release script does not prune to the configured keep:\n%s", script)
	}
	if strings.Count(script, "--keep") != 1 {
		t.Errorf("release script passed --keep more than once per job:\n%s", script)
	}

	keepAll := ReleaseScript(target, []string{"counter"}, ReleaseOptions{})
	if strings.Contains(keepAll, "--keep") {
		t.Errorf("keep 0 still passed a --keep flag:\n%s", keepAll)
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
	release := ReleaseScript(target, []string{"counter"}, ReleaseOptions{Keep: DefaultKeep})
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
	script := ActivateScript(target, testUnitOptions())

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

	sub := filepath.Join(ws, "jobs", "one")
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
