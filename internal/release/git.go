package release

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GitState describes the revision a release was built from.
//
// It is recorded for traceability, never enforced: a developer must be able to
// release uncommitted work to test it, and a build step that demands a clean
// tree would break that loop for a guarantee it cannot actually provide. What
// matters here is that a deployed release can be traced back to a commit, and
// that a dirty tree is visible rather than silent.
type GitState struct {
	// Revision is the short commit hash, empty when the source is not in a git
	// repository or git is unavailable.
	Revision string
	// Dirty reports uncommitted changes at release time.
	Dirty bool
}

// Ready reports whether a revision was identified at all.
func (g GitState) Ready() bool { return g.Revision != "" }

// Describe renders the state for a human, or "" when unknown.
func (g GitState) Describe() string {
	if !g.Ready() {
		return ""
	}
	if g.Dirty {
		return g.Revision + ", working tree dirty"
	}
	return g.Revision
}

// ReadGitState reports the revision of the repository containing dir.
//
// Every failure is soft. A release must not depend on git being installed, on
// the source living in a repository, or on the surrounding layout being
// readable -- this is metadata, not a precondition.
func ReadGitState(ctx context.Context, dir string) GitState {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return GitState{}
	}

	revision := gitOutput(ctx, abs, "rev-parse", "--short", "HEAD")
	if revision == "" {
		return GitState{}
	}

	state := GitState{Revision: revision}
	// A repository with no commits yet still resolves a revision, so this is
	// checked separately rather than inferred from an empty status.
	if status := gitOutput(ctx, abs, "status", "--porcelain"); status != "" {
		state.Dirty = true
	}
	return state
}

func gitOutput(ctx context.Context, dir string, args ...string) string {
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "git", args...)
	cmd.Dir = dir
	// Keep the repository from being relocated or paged by the operator's
	// configuration; this only needs to read two values.
	cmd.Env = append(cmd.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
