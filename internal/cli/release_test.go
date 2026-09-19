package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/release"
)

// releaseWorkspace lays out a workspace with one integration and returns the
// workspace root and the integration directory.
func releaseWorkspace(t *testing.T, name string) (root, dir string) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntegrationFixture(t, dir, name, "print('ok')\n")
	return root, dir
}

// writeIntegrationFixture writes a minimal valid external-Python integration.
func writeIntegrationFixture(t *testing.T, dir, name, python string) {
	t.Helper()
	manifest := "version: 1\nname: " + name + "\nentrypoint: main.py\n"
	if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(python), 0o644); err != nil {
		t.Fatal(err)
	}
}

// otterIn runs one CLI invocation with the working directory set to dir.
func otterIn(t *testing.T, dir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	inWorkspace(t, dir, func() {
		var out, errOut bytes.Buffer
		app := New("test", &out, &errOut)
		code = app.Run(context.Background(), args)
		stdout, stderr = out.String(), errOut.String()
	})
	return stdout, stderr, code
}

// `cd counter && otter release` releases counter: identity follows the working
// directory, the way `otter run` already does.
func TestReleaseFromInsideTheIntegrationDirectory(t *testing.T) {
	root, dir := releaseWorkspace(t, "counter")

	stdout, stderr, code := otterIn(t, dir, "release")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "counter: activated") {
		t.Errorf("output does not report the activation:\n%s", stdout)
	}
	// It landed in the workspace's own data directory, not a cwd-relative one
	// that no daemon reads.
	if _, err := os.Stat(filepath.Join(root, stateDirName, "data", release.DirName, "counter")); err != nil {
		t.Errorf("the release did not land in the workspace data directory: %v", err)
	}

	// And --list agrees that something is active.
	stdout, stderr, code = otterIn(t, dir, "release", "--list")
	if code != 0 {
		t.Fatalf("list exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "active release:") {
		t.Errorf("list does not report an active release:\n%s", stdout)
	}
}

// A relative path is read from disk, so `otter release ./counter` works from
// the workspace root as well as the "." form does from inside.
func TestReleaseAcceptsAPathReference(t *testing.T) {
	root, _ := releaseWorkspace(t, "counter")

	stdout, stderr, code := otterIn(t, root, "release", "counter")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "counter: activated") {
		t.Errorf("output does not report the activation:\n%s", stdout)
	}
}

// A bare name works from anywhere in the workspace, and --all covers every
// integration in it.
func TestReleaseByNameAndAll(t *testing.T) {
	root, _ := releaseWorkspace(t, "one")
	if err := os.MkdirAll(filepath.Join(root, "group", "two"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntegrationFixture(t, filepath.Join(root, "group", "two"), "two", "print('two')\n")

	if stdout, stderr, code := otterIn(t, root, "release", "one"); code != 0 {
		t.Fatalf("release one exited %d: %s", code, stderr)
	} else if !strings.Contains(stdout, "one: activated") {
		t.Errorf("output does not report the activation:\n%s", stdout)
	}

	stdout, stderr, code := otterIn(t, root, "release", "--all")
	if code != 0 {
		t.Fatalf("release --all exited %d: %s", code, stderr)
	}
	for _, want := range []string{"one: activated", "two: activated"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("--all did not release everything (%q missing):\n%s", want, stdout)
		}
	}
}

// Rollback: an older staged digest can be activated without staging anything,
// which is the point of keeping releases content-addressed.
func TestReleaseActivateRollsBackToAStagedDigest(t *testing.T) {
	root, dir := releaseWorkspace(t, "counter")
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}

	if _, stderr, code := otterIn(t, dir, "release"); code != 0 {
		t.Fatalf("first release exited %d: %s", code, stderr)
	}
	first, ok, err := manager.Active("counter")
	if err != nil || !ok {
		t.Fatalf("no active release after releasing: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('two')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := otterIn(t, dir, "release"); code != 0 {
		t.Fatalf("second release exited %d: %s", code, stderr)
	}
	second, _, err := manager.Active("counter")
	if err != nil {
		t.Fatalf("read active release: %v", err)
	}
	if second.Digest == first.Digest {
		t.Fatal("editing the source did not produce a new release")
	}

	// A prefix is enough, matching how --list prints digests.
	stdout, stderr, code := otterIn(t, dir, "release", "--activate", first.Digest[:12])
	if code != 0 {
		t.Fatalf("--activate exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, first.Digest[:12]) {
		t.Errorf("output does not name the activated release:\n%s", stdout)
	}
	back, _, err := manager.Active("counter")
	if err != nil {
		t.Fatalf("read active release: %v", err)
	}
	if back.Digest != first.Digest {
		t.Errorf("active release = %s, want the rolled-back %s", back.Digest[:12], first.Digest[:12])
	}
}

// An unknown digest is refused, and the command that shows what is available is
// named rather than leaving the developer to guess.
func TestReleaseActivateRefusesAnUnknownDigest(t *testing.T) {
	_, dir := releaseWorkspace(t, "counter")
	if _, stderr, code := otterIn(t, dir, "release"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}

	_, stderr, code := otterIn(t, dir, "release", "--activate", "deadbeefdead")
	if code == 0 {
		t.Fatal("activating an unknown digest succeeded")
	}
	if !strings.Contains(stderr, "--list") {
		t.Errorf("refusal does not name the list command:\n%s", stderr)
	}
}

// Outside a workspace a release has nowhere to go, and says so instead of
// scanning a relative ./integrations that is not there.
func TestReleaseRefusesOutsideAWorkspace(t *testing.T) {
	dir := t.TempDir()

	_, stderr, code := otterIn(t, dir, "release")
	if code == 0 {
		t.Fatal("release outside a workspace succeeded")
	}
	if !strings.Contains(stderr, "no workspace here") {
		t.Errorf("refusal does not explain itself:\n%s", stderr)
	}
	if strings.Contains(stderr, "is otterd running") {
		t.Errorf("a local path failure blamed the daemon:\n%s", stderr)
	}
}

// Naming one integration and also asking for all of them is a contradiction,
// not a silently ignored flag.
func TestReleaseRejectsAllWithAName(t *testing.T) {
	_, dir := releaseWorkspace(t, "counter")

	_, stderr, code := otterIn(t, dir, "release", "--all", "counter")
	if code == 0 {
		t.Fatal("--all with a name succeeded")
	}
	if !strings.Contains(stderr, "--all") {
		t.Errorf("refusal does not explain the conflict:\n%s", stderr)
	}
}
