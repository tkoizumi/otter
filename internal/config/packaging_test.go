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
		{"USER 10001:10001", "runs non-root, with a fixed uid a host volume can be ownership-checked against"},
		{"tini", "reaps orphaned children and forwards signals, so a cancelled run cannot leave a process counting against pids-limit"},
		{"ca-certificates", "every integration class talks to a vendor HTTPS API"},
		{"FROM debian:bookworm-slim", "the base is pinned by tag rather than floating"},
	}
	for _, r := range required {
		if !strings.Contains(src, r.snippet) {
			t.Errorf("Dockerfile no longer contains %q: %s", r.snippet, r.why)
		}
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

	// The daemon must not be told to listen anywhere but loopback: a pool where
	// the runtime binds a routable address exposes every tenant on the host.
	if strings.Contains(src, "--listen\", \"0.0.0.0") {
		t.Error("the image must not bind a routable address; the controlled ingress route is what exposes the API")
	}
	// One writable tree. A second --data path outside /workspace would be state
	// the backup does not capture.
	if !strings.Contains(src, "--data\", \"/workspace") {
		t.Error("the data directory must live inside the tenant volume")
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

	// The addresses the v1 contract denies, and the metadata endpoint the
	// prototype was measured reaching on the default bridge.
	for _, must := range []string{"169.254.169.254", "AF_INET6", "nip.io", "172.16.0.1", "192.168.0.1", "10.0.0.1"} {
		if !strings.Contains(src, must) {
			t.Errorf("verify-egress.sh does not probe %q", must)
		}
	}
	// It must connect, not merely resolve: a resolve-only check passes while a
	// direct-IP or rebound connection still works.
	if !strings.Contains(src, "s.connect(") {
		t.Error("verify-egress.sh does not attempt connections; a resolve-only check is not an egress policy")
	}
	// And it must not depend on curl, which the image does not carry.
	if strings.Contains(src, "curl ") {
		t.Error("verify-egress.sh uses curl, which is not in the runtime image")
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
