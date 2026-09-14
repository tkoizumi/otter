package release

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitOrSkip returns a usable git binary, or skips.
func gitOrSkip(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	return path
}

// runGit runs git with a minimal identity so the test does not depend on the
// machine's configuration.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestReadGitState(t *testing.T) {
	gitOrSkip(t)

	t.Run("a clean repository reports its revision", func(t *testing.T) {
		dir := t.TempDir()
		runGit(t, dir, "init", "-q")
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", ".")
		runGit(t, dir, "commit", "-q", "-m", "initial")

		state := ReadGitState(context.Background(), dir)
		if !state.Ready() {
			t.Fatal("no revision was read from a committed repository")
		}
		if state.Dirty {
			t.Errorf("a freshly committed tree reported dirty: %s", state.Describe())
		}
		if got := state.Describe(); got == "" {
			t.Error("Describe returned nothing for a known revision")
		}
	})

	t.Run("uncommitted changes are reported", func(t *testing.T) {
		dir := t.TempDir()
		runGit(t, dir, "init", "-q")
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", ".")
		runGit(t, dir, "commit", "-q", "-m", "initial")

		// A revision still resolves, and the dirtiness rides along with it.
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("two\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		state := ReadGitState(context.Background(), dir)
		if !state.Ready() {
			t.Fatal("no revision was read from a dirty repository")
		}
		if !state.Dirty {
			t.Error("a modified file was not reported as dirty")
		}
		if got := state.Describe(); got == "" {
			t.Error("Describe returned nothing")
		}
	})

	// A release must never fail because git is missing or the source is not in
	// a repository. This is metadata, not a precondition.
	t.Run("a directory outside a repository is not an error", func(t *testing.T) {
		dir := t.TempDir()
		state := ReadGitState(context.Background(), dir)
		if state.Ready() {
			t.Errorf("a non-repository produced a revision: %v", state)
		}
		if state.Describe() != "" {
			t.Errorf("Describe = %q, want empty for an unknown revision", state.Describe())
		}
	})

	t.Run("a missing directory is not an error", func(t *testing.T) {
		state := ReadGitState(context.Background(), filepath.Join(t.TempDir(), "nope"))
		if state.Ready() {
			t.Errorf("a missing directory produced a revision: %v", state)
		}
	})
}
