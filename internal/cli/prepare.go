package cli

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/pyenv"
)

// cmdPrepare prepares managed jobs before they can execute.
//
// It is a diagnostic and a warm-up, not a gate: `otter release` prepares the
// environment a run needs, and this builds the same environment without
// staging or activating a release. A job still needs a release before
// anything runs.
//
// Identity follows `otter release` and `otter run`: no argument means every
// job in the workspace, a bare word is a manifest name, and anything
// path-shaped is read from disk.
func (a *App) cmdPrepare(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jobs := fs.String("jobs", config.DefaultJobs, "jobs root (default: this workspace)")
	data := fs.String("data", "", "Otter data directory (default: the workspace's, .otter/data)")
	// Empty means "let the manager choose": a vendored copy under the data
	// directory is preferred, and PATH is the fallback. Defaulting to the bare
	// name "uv" would skip the vendored copy on a host that has no global uv.
	uv := fs.String("uv", "", "uv executable used only during preparation (default: the vendored copy, then PATH)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(a.Stderr, "usage: otter prepare [--jobs DIR] [--data DIR] [<job>|.]")
		return 2
	}

	jobsRoot, code := resolveJobsRoot(a.Stderr, *jobs, flagWasSet(fs, "jobs"))
	if code != 0 {
		return code
	}
	dataDir, code := resolveWorkspaceData(a.Stderr, *data, flagWasSet(fs, "data"))
	if code != 0 {
		return code
	}

	named := fs.NArg() == 1
	ref := "."
	if named {
		ref = fs.Arg(0)
	}
	// No argument means every job: prepare is the warm-up for a whole
	// workspace, and naming one is the narrowing case.
	targets, code := a.jobTargets(ref, jobsRoot, "", named, !named)
	if code != 0 {
		return code
	}
	// Environments are keyed by the durable identity, so resolve before
	// preparing: preparing under a label would build an environment the
	// runtime never looks for.
	targets, code = a.mapTargetIdentities(ctx, jobsRoot, dataDir, targets)
	if code != 0 {
		return code
	}

	manager := pyenv.Manager{DataDir: dataDir}
	count := 0
	for _, target := range targets {
		label := target.Label()
		manifest, err := config.LoadAndValidate(filepath.Join(target.Dir, config.ManifestFileName))
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: %s: %v\n", label, err)
			return 1
		}
		if manifest.Python.Mode != "managed" {
			if named {
				fmt.Fprintf(a.Stderr, "otter: %s uses external Python; nothing to prepare\n", label)
			}
			continue
		}
		ready, err := manager.Prepare(ctx, target.Dir, target.ID, *uv)
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: prepare %s: %v\n", label, err)
			return 1
		}
		fmt.Fprintf(a.Stdout, "%s: Python %s, environment %s\n", label, ready.Python, ready.Digest[:12])
		count++
	}
	if !named && count == 0 {
		fmt.Fprintln(a.Stderr, "otter: no managed jobs found")
	}
	return 0
}
