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
	unit := UnitFile(target, []string{"counter", "shopify-to-salesforce"})

	required := []string{
		"User=otter",
		"Group=otter",
		"WorkingDirectory=/opt/otter",
		"ExecStart=/opt/otter/bin/otterd --integrations /opt/otter/integrations --data /opt/otter/data --listen 127.0.0.1:7337",
		"EnvironmentFile=/etc/otter/counter.env",
		"EnvironmentFile=/etc/otter/shopify-to-salesforce.env",
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
}

func TestUnitFileNeverTouchesTheDataDirectory(t *testing.T) {
	unit := UnitFile(testTarget(), []string{"counter"})
	// The data directory is only ever an argument. Anything that deletes or
	// recreates it on deploy would destroy every integration's watermark.
	for _, forbidden := range []string{"ExecStartPre", "ExecStopPost", "rm -rf"} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("unit file contains %q, which could disturb the data directory:\n%s", forbidden, unit)
		}
	}
}

func TestEnvFileSortsAndQuotes(t *testing.T) {
	env := EnvFile("counter", "tok123", map[string]string{
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
	env := EnvFile("counter", "", map[string]string{"A": "1"})
	if strings.Contains(env, "OTTER_API_TOKEN") {
		t.Errorf("env file should omit an empty token:\n%s", env)
	}
}

func TestInstallScriptBakesTheUnit(t *testing.T) {
	target := testTarget()
	script := InstallScript(target, []string{"counter"})

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
	script := InstallScript(testTarget(), []string{"counter"})
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
	script := InstallScript(testTarget(), []string{"counter"})
	// A vendored uv is provisioned out of band; it must end up owned by the
	// service account that runs preparation.
	if !strings.Contains(script, "for dir in bin integrations lib tools; do") {
		t.Errorf("install script does not take ownership of tools/:\n%s", script)
	}
}
