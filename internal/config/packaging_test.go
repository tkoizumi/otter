package config

// The pool's tenant image is a security boundary, and its properties are the
// kind that get lost in a refactor: someone removes --read-only to make a test
// pass, or drops tini because it looks redundant. These assert the invariants
// the WP1 launch contract depends on, so losing one is a failing test rather
// than a discovery made in production.
//
// This is not a substitute for qualification. It cannot tell whether the
// sandbox actually enforces anything -- only WP1's effective-limit verification
// can do that, and the README says so.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func packagingDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "packaging", "container")
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
		t.Skipf("packaging/container is not in this checkout (%s)", dir)
	}
	return dir
}

func TestTenantImageKeepsItsBoundarySettings(t *testing.T) {
	dir := packagingDir(t)
	body, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)

	// Each entry is a property the launch contract relies on, and why.
	required := []struct{ snippet, why string }{
		{"tini", "reaps orphaned children and forwards signals, so a cancelled run cannot leave a process counting against pids-limit"},
		{"ca-certificates", "every integration class talks to a vendor HTTPS API"},
		{"FROM debian:bookworm-slim", "the base is pinned by tag rather than floating"},
		// The container runs TWO processes under one supervisor, which is why it
		// needs two unprivileged identities rather than one.
		{"--uid 10001", "the tenant process runs as a fixed uid a host volume can be ownership-checked against"},
		{"--uid 10002", "the agent gets its OWN uid, which is what keeps it out of the tenant's files"},
		{"-m 0700 /workspace", "the tenant volume is unreadable and unwritable to the agent's uid"},
	}
	for _, r := range required {
		if !strings.Contains(src, r.snippet) {
			t.Errorf("Dockerfile no longer contains %q: %s", r.snippet, r.why)
		}
	}

	// And it must NOT pin a single USER. A static USER applies to every process in
	// the container, which would remove the supervisor's ability to drop the agent
	// to a second uid -- and that separation is the isolation.
	if strings.HasPrefix(src, "USER ") || strings.Contains(src, "\nUSER ") {
		t.Error("the image must not pin a single USER; the supervisor drops to uid 10001 and 10002 per process")
	}

	// uv writes the interpreters it installs under $HOME by default, and $HOME is
	// on the read-only root. Without these the image cannot prepare a managed
	// environment at all -- observed as "failed to create directory
	// /home/otter/.local/share/uv/python: Read-only file system".
	for _, env := range []string{"UV_CACHE_DIR=", "UV_PYTHON_INSTALL_DIR=", "UV_TOOL_DIR="} {
		if !strings.Contains(src, env) {
			t.Errorf("Dockerfile does not set %s, so uv will write to the read-only root", env)
		}
	}
	// Cache and tool state are tenant-writable, so they belong in /tmp. The
	// interpreter does NOT: uv installs it where UV_PYTHON_INSTALL_DIR points,
	// and a preparation sandbox has no egress to download one, so it must be
	// provisioned into the image and read at run time from a read-only path.
	for _, env := range []string{"UV_CACHE_DIR=/tmp", "UV_TOOL_DIR=/tmp"} {
		if !strings.Contains(src, env) {
			t.Errorf("%s must point under /tmp, the tenant's bounded temporary storage", env)
		}
	}
	if !strings.Contains(src, "UV_PYTHON_INSTALL_DIR=/opt/") {
		t.Error("UV_PYTHON_INSTALL_DIR must point at a provisioned, read-only path; a preparation sandbox has no egress to download an interpreter")
	}
	if !strings.Contains(src, "uv python install") {
		t.Error("the image does not provision the managed interpreter at build time")
	}

	// The launch arguments themselves now live in the supervisor, because one
	// container runs both processes: otterd as uid 10001 and the agent as uid
	// 10002, over loopback. The boundary properties are asserted there.
	entryBody, err := os.ReadFile(filepath.Join(dir, "entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	entry := string(entryBody)

	// The daemon must not be told to listen anywhere but loopback: a pool where
	// the runtime binds a routable address exposes every tenant on the host.
	if strings.Contains(entry, "0.0.0.0") {
		t.Error("the runtime must not bind a routable address; the agent reaches it over loopback in the same container")
	}
	if !strings.Contains(entry, "127.0.0.1:7337") {
		t.Error("the runtime must listen on loopback")
	}
	// One writable tree for the tenant. A second --data path outside /workspace
	// would be state the backup does not capture.
	if !strings.Contains(entry, "/workspace/.otter/data") {
		t.Error("the data directory must live inside the tenant volume")
	}
	// The agent's own data must NOT live in the tenant volume: that separation,
	// enforced by the two uids and the 0700 mode, is the isolation.
	if !strings.Contains(entry, "/agent/.otter/data") {
		t.Error("the agent's data must live outside the tenant volume")
	}
	// The supervisor must drop to BOTH uids, and must strip the agent-only
	// variables -- the per-runtime bootstrap secret above all -- from otterd's
	// environment, or the tenant process can read the agent's Cloud credential.
	for _, want := range []string{"setpriv --reuid \"$agent_uid\"", "setpriv --reuid \"$tenant_uid\"", "agent_only=", "env $strip"} {
		if !strings.Contains(entry, want) {
			t.Errorf("entrypoint.sh no longer contains %q", want)
		}
	}
	// chmod before chown: after chowning to the agent's uid, root is no longer the
	// owner and chmod would need CAP_FOWNER, which this container deliberately
	// lacks. Getting this backwards stopped the container from starting at all.
	chmodAt := strings.Index(entry, "chmod 0700 \"$agent_root\"")
	chownAt := strings.Index(entry, "chown \"$agent_uid:$agent_uid\" \"$agent_root\"")
	if chmodAt < 0 || chownAt < 0 || chmodAt > chownAt {
		t.Error("the agent data directory must be chmod 0700 BEFORE it is chowned; chown first needs CAP_FOWNER")
	}
}

// The launch flags are a request; the cgroup is the fact. The verification
// script is what turns one into the other, so it has to exist and be executable.
func TestLimitVerificationScriptIsPresentAndExecutable(t *testing.T) {
	dir := packagingDir(t)
	path := filepath.Join(dir, "verify-limits.sh")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("every qualification claim about memory, cpu and pids limits depends on reading them back: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("verify-limits.sh is not executable")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The properties the plan calls out by name.
	for _, want := range []string{"memory.max", "memory.swap.max", "pids.max", "cpu.max", "ReadonlyRootfs"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("verify-limits.sh does not check %q", want)
		}
	}
}

// The egress verifier is the only check on the policy that the qualification
// found violated on the default bridge, so losing it would remove the guard
// without failing anything.
func TestEgressVerificationIsPresentAndConnects(t *testing.T) {
	dir := packagingDir(t)
	path := filepath.Join(dir, "verify-egress.sh")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("verify-egress.sh is missing: nothing would check the egress policy: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("verify-egress.sh is not executable")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)

	// The denied addresses are composed by the shell script, which passes them to
	// the probe as arguments -- the metadata endpoint the prototype was measured
	// reaching on the default bridge, both RFC1918 ranges, and the IPv6
	// loopback, link-local and unique-local forms.
	for _, must := range []string{"169.254.169.254", "AF_INET6", "172.16.0.1", "192.168.0.1", "10.0.0.1", "fe80::1", "fd00::1", "::ffff:169.254.169.254"} {
		if !strings.Contains(src, must) {
			t.Errorf("verify-egress.sh does not probe %q", must)
		}
	}
	// The probe must connect, not merely resolve: a resolve-only check passes
	// while a direct-IP or rebound connection still works.
	egress, err := os.ReadFile(filepath.Join(dir, "egress_probe.py"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(egress), "s.connect(") {
		t.Error("egress_probe.py does not attempt connections; a resolve-only check is not an egress policy")
	}
	// The rebinding case: a name that resolves to a blocked address.
	rebind, err := os.ReadFile(filepath.Join(dir, "rebind_probe.py"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rebind), "nip.io") || !strings.Contains(string(rebind), "s.connect(") {
		t.Error("rebind_probe.py must resolve a name to a blocked address and then attempt to connect")
	}
	// And it must not depend on curl, which the image does not carry.
	if strings.Contains(src, "curl ") {
		t.Error("verify-egress.sh uses curl, which is not in the runtime image")
	}
	// The probes must be real files it copies in. Embedding them in a heredoc
	// piped through docker exec does not reach the process, and silent probes
	// read as a clean pass -- which is exactly what happened.
	for _, probe := range []string{"egress_probe.py", "rebind_probe.py"} {
		if _, err := os.Stat(filepath.Join(dir, probe)); err != nil {
			t.Errorf("%s is missing: verify-egress.sh copies it into the container", probe)
		}
		if !strings.Contains(src, probe) {
			t.Errorf("verify-egress.sh does not install %s", probe)
		}
	}
}

// The build context is the repository root, so without an allowlist the image
// would be built from .git, the test suite and local caches.
func TestDockerignoreRestrictsTheBuildContext(t *testing.T) {
	dir := packagingDir(t)
	body, err := os.ReadFile(filepath.Join(dir, ".dockerignore"))
	if err != nil {
		t.Fatalf(".dockerignore is missing: the build context is the repo root: %v", err)
	}
	// The bin directory is included as a whole. Naming the two binaries
	// individually made the build fail on the real host with "checksum ...
	// /bin/otter: not found" for a file that exists, because "bin/otter" is a
	// prefix of "bin/otterd" and the two negations interact. Asserting the
	// directory form is what keeps a future tidy-up from reintroducing it.
	if !strings.Contains(string(body), "!bin") {
		t.Error(".dockerignore does not include the bin directory, so the image cannot be built")
	}
	if !strings.Contains(string(body), "*") {
		t.Error(".dockerignore does not exclude anything by default")
	}
}
