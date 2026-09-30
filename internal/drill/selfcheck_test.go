package drill

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSelfchecks runs every scripts/drill/selfcheck/*.sh. A selfcheck is a
// drill's no-host half: it asserts the parts of a drill's behavior that can be
// decided without the host the drill exists for — argument enforcement,
// refusing instead of silently falling back, and the preflight assertions a
// two-host run leans on.
//
// Two deliberate properties:
//
//   - a selfcheck that cannot run FAILS. The drills' prerequisites (sqlite3,
//     tar, find) are documented, and a drill that did not run has produced no
//     evidence, so skipping here would be the failure mode the mechanism exists
//     to prevent.
//   - the glob finding nothing is also a failure, so deleting a selfcheck
//     cannot quietly turn this test green.
func TestSelfchecks(t *testing.T) {
	root := repoRoot(t)
	pattern := filepath.Join(root, "scripts", "drill", "selfcheck", "*.sh")
	scripts, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	if len(scripts) == 0 {
		t.Fatalf("no selfchecks found at %s: the drills' hostless checks are gone", pattern)
	}
	registerInputs(t, root)

	for _, script := range scripts {
		script := script
		t.Run(filepath.Base(script), func(t *testing.T) {
			cmd := exec.Command("sh", script)
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			t.Logf("%s", out)
			if err != nil {
				t.Fatalf("%s failed: %v", filepath.Base(script), err)
			}
		})
	}
}

// registerInputs reads the shell the selfchecks are made of, so the Go test
// cache is keyed on it.
//
// The selfchecks run `sh`, which reads the drill scripts in a child process.
// cmd/go tracks the files a test *opens*, not the ones its children open, so
// without this the package would serve a cached PASS after a drill changed —
// exactly the stale green this test exists to prevent. Reading each file here
// makes it a declared input of the test result.
func registerInputs(t *testing.T, root string) {
	t.Helper()
	var files []string
	for _, dir := range []string{
		filepath.Join(root, "scripts", "drill", "selfcheck"),
		filepath.Join(root, "scripts", "drill", "lib"),
		filepath.Join(root, "scripts", "drill", "modes"),
	} {
		found, err := filepath.Glob(filepath.Join(dir, "*.sh"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		files = append(files, found...)
	}
	files = append(files, filepath.Join(root, "scripts", "drill", "backup-restore.sh"))
	for _, file := range files {
		if _, err := os.ReadFile(file); err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
	}
}

// repoRoot walks up from the test's working directory to the module root, so
// the test does not hard-code a depth that a future layout change would break.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
