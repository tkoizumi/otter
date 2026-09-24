package deploy

import (
	"strings"
	"testing"
)

func TestNormalizePlatform(t *testing.T) {
	tests := []struct {
		osName  string
		machine string
		want    string
		wantErr bool
	}{
		{"Linux", "x86_64", "linux/amd64", false},
		{"Linux", "aarch64", "linux/arm64", false},
		{"linux", "arm64", "linux/arm64", false},
		{"Darwin", "arm64", "darwin/arm64", false},
		{"FreeBSD", "x86_64", "", true},
		{"Linux", "ppc64le", "", true},
	}

	for _, tc := range tests {
		got, err := normalizePlatform(tc.osName, tc.machine)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizePlatform(%q, %q) = %q, want an error", tc.osName, tc.machine, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizePlatform(%q, %q): %v", tc.osName, tc.machine, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizePlatform(%q, %q) = %q, want %q", tc.osName, tc.machine, got, tc.want)
		}
	}
}

func TestParsePlatform(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"linux/arm64", false},
		{"linux/amd64", false},
		{"darwin/arm64", false},
		{"linux", true},
		{"linux/386", true},
		{"windows/amd64", true},
		{"", true},
		{"/amd64", true},
	}

	for _, tc := range tests {
		_, _, err := ParsePlatform(tc.in)
		if tc.wantErr && err == nil {
			t.Errorf("ParsePlatform(%q) succeeded, want an error", tc.in)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("ParsePlatform(%q): %v", tc.in, err)
		}
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"simple", "'simple'"},
		{"with space", "'with space'"},
		{"it's", `'it'\''s'`},
		{"$(rm -rf /)", "'$(rm -rf /)'"},
		{"a`b`c", "'a`b`c'"},
	}
	for _, tc := range tests {
		if got := ShellQuote(tc.in); got != tc.want {
			t.Errorf("ShellQuote(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestScriptCommandUsesSudoForNonRoot(t *testing.T) {
	root := Target{Host: "h", User: "root"}
	if got := (&SSH{Target: root}).ScriptCommand(); got != "sh -s" {
		t.Errorf("root ScriptCommand = %q, want sh -s", got)
	}

	regular := Target{Host: "h", User: "ubuntu"}
	if got := (&SSH{Target: regular}).ScriptCommand(); got != "sudo -n sh -s" {
		t.Errorf("non-root ScriptCommand = %q, want sudo -n sh -s", got)
	}
}

// Commands travel as an argument, not on stdin: these invocations never write
// stdin, and a shell reading an empty stdin executes nothing at all.
func TestShellCommandPassesTheCommandAsAnArgument(t *testing.T) {
	root := Target{Host: "h", User: "root"}
	got := (&SSH{Target: root}).ShellCommand("uname -s && uname -m")
	if !strings.HasPrefix(got, "sh -c ") {
		t.Errorf("ShellCommand = %q, want a sh -c invocation", got)
	}
	if !strings.Contains(got, "uname -s && uname -m") {
		t.Errorf("ShellCommand dropped the command: %q", got)
	}

	regular := Target{Host: "h", User: "ubuntu"}
	if got := (&SSH{Target: regular}).ShellCommand("true"); !strings.HasPrefix(got, "sudo -n sh -c ") {
		t.Errorf("non-root ShellCommand = %q, want sudo -n sh -c", got)
	}
}

func TestValidateListenRefusesWildcard(t *testing.T) {
	// An empty listen address is legal: a new workspace's port is chosen on
	// the host, where the ports already in use are known.
	for _, listen := range []string{":7337", "0.0.0.0:7337", "0.0.0.0:0"} {
		target := DefaultTarget()
		target.Host = "example.com"
		target.Listen = listen
		if err := target.Validate(); err == nil {
			t.Errorf("Validate accepted listen address %q, want a refusal", listen)
		}
	}

	target := DefaultTarget()
	target.Host = "example.com"
	if err := target.Validate(); err != nil {
		t.Errorf("a default target should be valid: %v", err)
	}
}

// A root login must run remote scripts directly, never through sudo. Getting
// this wrong breaks every deploy to a root-only image.
func TestRemoteCommandsKeyOffTheResolvedUser(t *testing.T) {
	target := Target{Host: "root@10.0.0.1"}
	target.SplitHostUser()
	if got := (&SSH{Target: target}).ScriptCommand(); got != "sh -s" {
		t.Errorf("ScriptCommand for root@host = %q, want sh -s", got)
	}
	if got := (&SSH{Target: target}).ShellCommand("true"); !strings.HasPrefix(got, "sh -c ") {
		t.Errorf("ShellCommand for root@host = %q, want sh -c", got)
	}

	nonRoot := Target{Host: "10.0.0.1", User: "ubuntu"}
	if got := (&SSH{Target: nonRoot}).ScriptCommand(); got != "sudo -n sh -s" {
		t.Errorf("ScriptCommand for a non-root login = %q, want sudo -n sh -s", got)
	}
}

// The host must be checked for the tools a deploy needs, before anything is
// built or pushed. A minimal Amazon Linux image has no rsync, and the raw
// failure surfaces as a confusing rsync protocol error.
func TestRequiredToolsDependOnTheLogin(t *testing.T) {
	root := Target{Host: "h", User: "root"}
	tools := (&SSH{Target: root}).requiredTools()
	for _, must := range []string{"rsync", "systemctl", "find", "install"} {
		if !contains(tools, must) {
			t.Errorf("root deploy does not require %s", must)
		}
	}
	if contains(tools, "sudo") || contains(tools, "runuser") {
		t.Error("a root deploy should not require sudo or runuser")
	}

	nonRoot := Target{Host: "h", User: "ec2-user"}
	tools = (&SSH{Target: nonRoot}).requiredTools()
	for _, must := range []string{"rsync", "sudo", "runuser"} {
		if !contains(tools, must) {
			t.Errorf("non-root deploy does not require %s", must)
		}
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// rsync authenticates as the login user and runs its own remote command, so a
// non-root login has to escalate on the remote side too. Without this, pushing
// into an install root such as /opt fails with "mkdir: Permission denied".
func TestPushEscalatesForANonRootLogin(t *testing.T) {
	target := Target{Host: "10.0.0.1", User: "ec2-user"}
	ssh := &SSH{Target: target, useSudo: true}

	// The argument list is built inside Push, so assert on the helper that
	// decides it.
	if !ssh.needsRemoteSudo() {
		t.Error("a non-root login should escalate the remote rsync")
	}

	root := &SSH{Target: Target{Host: "10.0.0.1", User: "root"}}
	if root.needsRemoteSudo() {
		t.Error("a root login should not escalate the remote rsync")
	}
}
