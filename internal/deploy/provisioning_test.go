package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file is the in-repo half of P0-07's evidence bar (CA-06, CA-09, CA-10,
// R-19). The host half is scripts/assert-host-permissions.sh, which reads the
// installed modes, ownership and ingress off the real host. What a host cannot
// check is whether the artifacts a deploy would *install* still carry those
// properties, so that is what these tests pin:
//
//   - the render output names a nologin service account, a 0700 data
//     directory and 0600 environment files;
//   - the unit carries every sandbox directive the security model claims;
//   - the provisioning script exists, is POSIX sh, and does not re-create any
//     of the state `otter deploy` already converges.
//
// Each assertion is a mutation away from failing: the checks are exact
// substrings of output this package generates, so deleting the line that
// carries a mode or a directive fails the test that claims it.

// repoFile reads a file from the repository root, two levels above this
// package. The deploy package is the only place that renders host artifacts,
// so the provisioning script's contract is part of this package's contract.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	path := filepath.Join("..", "..", rel)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// CA-09: the data directory is private to the service account. A deploy that
// stopped emitting 0700 would leave otter.db, every run log and every webhook
// token readable by any local account.
func TestInstallScriptLocksDownTheDataDirectory(t *testing.T) {
	script := InstallScript(testTarget(), testUnitOptions())
	want := `install -d -m 0700 -o "$RUN_AS" -g "$RUN_AS" "$DATA_DIR"`
	if !strings.Contains(script, want) {
		t.Errorf("install script does not create the data directory 0700 for the service account:\nwant %q\n---\n%s", want, script)
	}
}

// CA-09: the environment files hold the API token and every shared secret. A
// deploy that stopped chmod-ing them would leave credentials group- or
// world-readable on the host.
func TestActivateScriptLocksDownTheEnvironmentFiles(t *testing.T) {
	script := ActivateScript(testTarget(), testUnitOptions())
	want := `chmod 0600 "$SHARED_ENV" "$DAEMON_ENV"`
	if !strings.Contains(script, want) {
		t.Errorf("activate script does not chmod the environment files 0600:\nwant %q\n---\n%s", want, script)
	}
}

// CA-09: the service account must not be a login account. `otter deploy`
// already does this (render.go), and this test is what keeps it from
// regressing into `useradd` without a shell.
func TestPrepareScriptCreatesANologinServiceAccount(t *testing.T) {
	script := PrepareScript(testTarget())
	wanted := []string{
		"useradd --system",
		"--shell /usr/sbin/nologin",
		"getent group",
		`id -u "$RUN_AS"`,
	}
	for _, want := range wanted {
		if !strings.Contains(script, want) {
			t.Errorf("prepare script is missing %q:\n%s", want, script)
		}
	}
	// A nologin shell and no home: the account exists to own files and run
	// systemd's ExecStart, not to be logged into.
	if !strings.Contains(script, "--no-create-home") {
		t.Errorf("prepare script creates a home directory for the service account:\n%s", script)
	}
}

// CA-07 / the hardening checklist in docs/security.md: the generated unit
// carries the sandbox. TestUnitFileEmitsSandboxHardening in render_test.go
// introduced these; the full set is asserted again here because P0-07's
// evidence bar names them, and a single deletion should fail one obvious test.
func TestUnitFileCarriesEverySandboxDirective(t *testing.T) {
	target := testTarget()
	unit := UnitFile(target, testUnitOptions())

	for _, directive := range []string{
		"NoNewPrivileges=true",
		"ProtectSystem=strict",
		"ProtectHome=true",
		"PrivateTmp=true",
	} {
		if !strings.Contains(unit, "\n"+directive+"\n") {
			t.Errorf("unit file is missing %q\n---\n%s", directive, unit)
		}
	}

	// ProtectSystem=strict makes everything read-only, so the two directories
	// the runtime owns must be named or the daemon and every job fail with
	// EROFS. The operator's extra paths are covered by render_test.go.
	rw := "ReadWritePaths=" + target.WorkspaceDir() + " " + target.DataDir
	if !strings.Contains(unit, rw) {
		t.Errorf("unit file does not name the writable paths:\nwant %q\n---\n%s", rw, unit)
	}
}

// CA-10: the deploy refuses a wildcard bind before it touches a host. This is
// the render-side half; the host-side half is the listening-socket check in
// scripts/assert-host-permissions.sh.
func TestValidationRefusesIngressBeforeAnyHostIsTouched(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:7337", "[::]:7337", ":7337", "::7337"} {
		target := testTarget()
		target.Listen = listen
		target.ApplyDefaults()
		if err := target.Validate(); err == nil {
			t.Errorf("Validate accepted the wildcard listen address %q", listen)
		}
	}
	// The loopback default is the only address a deploy offers.
	target := testTarget()
	target.Listen = DefaultListenAddr
	target.ApplyDefaults()
	if err := target.Validate(); err != nil {
		t.Errorf("Validate rejected the loopback default %q: %v", DefaultListenAddr, err)
	}
}

// P0-07's deliverable is one script from an empty VM to a running daemon, and
// it is deliberately not a reimplementation of `otter deploy`. This pins the
// boundary: the script must exist, be a POSIX sh script, and hand off to the
// deployer rather than creating the account, the directories or the unit
// itself.
func TestProvisionScriptHandsOffToDeploy(t *testing.T) {
	script := repoFile(t, "scripts/provision.sh")

	if !strings.HasPrefix(script, "#!/bin/sh") {
		t.Errorf("scripts/provision.sh is not a POSIX sh script:\n%s", firstLines(script, 3))
	}

	// It must actually run the deployer.
	if !strings.Contains(script, " deploy") || !strings.Contains(script, "otter_bin") {
		t.Errorf("scripts/provision.sh does not hand off to `otter deploy`:\n%s", firstLines(script, 40))
	}

	// It must not duplicate what the deployer owns. Each pattern is a command
	// the install script is asserted to contain above; a provisioning script
	// that ran it too would be a second source of truth for the account, the
	// modes or the unit.
	forbidden := []string{
		"useradd --system",
		"groupadd --system",
		`install -d -m 0700`,
		`cat > "$UNIT"`,
		"systemctl enable",
		"systemctl restart",
	}
	for _, want := range forbidden {
		if strings.Contains(script, want) {
			t.Errorf("scripts/provision.sh duplicates `otter deploy` by containing %q\n"+
				"the service account, the unit and the restart belong to internal/deploy", want)
		}
	}

	// The swap check is the loud failure a silently-aborted provisioning
	// script needs; without it the host comes up with zero swap and nothing
	// notices. Its absence must fail here.
	if !strings.Contains(script, "min_swap_mib") || !strings.Contains(script, "/proc/swaps") {
		t.Errorf("scripts/provision.sh does not assert swap; a host that lost its swapfile "+
			"must fail loudly (see the file header):\n%s", firstLines(script, 40))
	}
	// The assertion script is the last step: the deploy's own success is not
	// evidence that the modes and ingress ended up right.
	if !strings.Contains(script, "assert-host-permissions.sh") {
		t.Errorf("scripts/provision.sh does not run scripts/assert-host-permissions.sh after the deploy")
	}

	info, err := os.Stat(filepath.Join("..", "..", "scripts", "provision.sh"))
	if err != nil {
		t.Fatalf("stat scripts/provision.sh: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("scripts/provision.sh is not executable (mode %04o)", info.Mode().Perm())
	}
}

// The host-side assertion script is the executable form of CA-09 and CA-10.
// This checks that it still exists, is a POSIX sh script, and still names each
// of the checks the task list requires, so a rewrite cannot quietly drop one.
func TestHostPermissionScriptAssertsEveryClaim(t *testing.T) {
	script := repoFile(t, "scripts/assert-host-permissions.sh")

	if !strings.HasPrefix(script, "#!/bin/sh") {
		t.Errorf("scripts/assert-host-permissions.sh is not a POSIX sh script")
	}

	required := []string{
		"nologin",
		`"700"`,
		`"600"`,
		"is-enabled",
		"is-active",
		"NoNewPrivileges",
		"ProtectSystem",
		"ProtectHome",
		"PrivateTmp",
		"ReadWritePaths",
		"--listen",
		// The listener check runs ss with -H -tln. The binary itself comes from
		// OTTER_SS_BIN, which is unset on a real host: that hook is what lets
		// the fixture model a host with no ss at all, which PATH shadowing
		// cannot do on a machine that has iproute2.
		"-H -tln",
		"approved_ports",
		"/proc/swaps",
		"otter-provision-report.txt",
		"vm.swappiness",
		// The record `otter deploy` writes has no service_name and no data_dir
		// key: the script must read `unit` and derive `<workspace>/.otter/data`.
		`json_string unit`,
		".otter/data",
		// systemd reads EnvironmentFile= as root before dropping to User=, so
		// the env files are root-owned, not service-owned. Asserting the
		// service-owned shape would make a correct host fail.
		"root:root",
	}
	// Each of those is exercised by a named case in
	// scripts/test-assert-host-permissions.sh; this is the in-repo half that
	// notices the whole check being deleted.
	harness := repoFile(t, "scripts/test-assert-host-permissions.sh")
	if !strings.Contains(harness, "unset_mutations") {
		t.Errorf("scripts/test-assert-host-permissions.sh has no mutation reset, so cases can leak into each other")
	}
	// The fixture must feed the script the record `otter deploy` actually
	// writes, or the derivation above stops being exercised.
	if !strings.Contains(harness, `"unit":"%s"`) || strings.Contains(harness, `"service_name"`) {
		t.Errorf("the fixture's workspace.json is not the record `otter deploy` writes")
	}
	for _, want := range required {
		if !strings.Contains(script, want) {
			t.Errorf("scripts/assert-host-permissions.sh no longer asserts %q", want)
		}
	}

	info, err := os.Stat(filepath.Join("..", "..", "scripts", "assert-host-permissions.sh"))
	if err != nil {
		t.Fatalf("stat scripts/assert-host-permissions.sh: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("scripts/assert-host-permissions.sh is not executable (mode %04o)", info.Mode().Perm())
	}
}

// OTTER_WORKERS=1 is the sizing decision that makes a 1 GiB host survivable:
// the daemon otherwise starts one worker per vCPU, and MemoryMax=75% bounds the
// whole unit cgroup, so two Python processes is how the host OOMs. The
// provisioning script injects the value, and the template documents it.
func TestDaemonEnvTemplatePinsOneWorker(t *testing.T) {
	template := repoFile(t, "otter.daemon.env.example")

	if !strings.Contains(template, "OTTER_WORKERS=1") {
		t.Errorf("otter.daemon.env.example does not set OTTER_WORKERS=1:\n%s", template)
	}
	script := repoFile(t, "scripts/provision.sh")
	if !strings.Contains(script, "OTTER_WORKERS") {
		t.Errorf("scripts/provision.sh does not set OTTER_WORKERS before deploying")
	}
}

// A generated script that cannot be parsed by the host's shell is not a
// script. `sh -n` is the cheapest way to catch a broken heredoc or an
// unbalanced quote in output this package builds by concatenation.
func TestGeneratedScriptsParse(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH to parse the generated scripts")
	}
	target := testTarget()
	for name, script := range map[string]string{
		"install":  InstallScript(target, testUnitOptions()),
		"prepare":  PrepareScript(target),
		"activate": ActivateScript(target, testUnitOptions()),
		"release":  ReleaseScript(target, []string{"counter"}, ReleaseOptions{Keep: DefaultKeep}),
		"destroy":  DestroyScript(target, true),
	} {
		// -n parses without executing, so nothing here touches a host.
		cmd := exec.Command(sh, "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s script does not parse: %v\n%s\n---\n%s", name, err, out, script)
		}
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
