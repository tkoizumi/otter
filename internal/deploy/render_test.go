package deploy

import (
	"strings"
	"testing"
)

func testTarget() Target {
	t := DefaultTarget()
	t.Host = "droplet"
	t.Platform = "linux/arm64"
	return t
}

func TestUnitFileRendersEnvironment(t *testing.T) {
	target := testTarget()
	unit := UnitFile(target)

	required := []string{
		"User=otter",
		"Group=otter",
		"WorkingDirectory=/opt/otter",
		"ExecStart=/opt/otter/bin/otterd --integrations /opt/otter/integrations --data /opt/otter/data --listen 127.0.0.1:7337",
		"EnvironmentFile=-/etc/otter/daemon.env",
		"EnvironmentFile=-/etc/otter/shared.env",
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
		"ExecStart=/opt/otter/bin/otterd",
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
	if !strings.Contains(script, `mkdir -p "$REMOTE_DIR/bin" "$REMOTE_DIR/integrations"`) {
		t.Errorf("install script must create the destination directories:\n%s", script)
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
	if !strings.Contains(unit, "EnvironmentFile=-/etc/otter/daemon.env") {
		t.Errorf("the daemon environment file is not optional:\n%s", unit)
	}
	if !strings.Contains(unit, "EnvironmentFile=-/etc/otter/shared.env") {
		t.Errorf("the shared environment file is not optional:\n%s", unit)
	}
	// systemd applies a later EnvironmentFile over an earlier one, so the
	// shared credentials must be loaded after the daemon's settings.
	if strings.Index(unit, "daemon.env") > strings.Index(unit, "shared.env") {
		t.Errorf("the shared environment is loaded before the daemon's:\n%s", unit)
	}
}

func TestEnvFilePathsAreStable(t *testing.T) {
	target := testTarget()
	if got := target.DaemonEnvFilePath(); got != "/etc/otter/daemon.env" {
		t.Errorf("DaemonEnvFilePath = %q", got)
	}
	if got := target.SharedEnvFilePath(); got != "/etc/otter/shared.env" {
		t.Errorf("SharedEnvFilePath = %q", got)
	}
	if target.DaemonEnvFilePath() == target.SharedEnvFilePath() {
		t.Error("the daemon and shared environment files resolve to the same path")
	}
}

// The release command names the integrations discovery root explicitly, which
// is what makes the release base the repository root rather than
// /opt/otter/integrations: the shared library is a sibling of that root and no
// release path can spell "../lib/python".
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
	if target.IntegrationsDir() != "/opt/otter/integrations" {
		t.Errorf("IntegrationsDir = %q, want /opt/otter/integrations", target.IntegrationsDir())
	}
	// Each integration is released separately, so a failure is reported
	// against the integration that caused it.
	if strings.Count(script, `"$CLI" release`) != 2 {
		t.Errorf("release script does not release each integration separately:\n%s", script)
	}
}
