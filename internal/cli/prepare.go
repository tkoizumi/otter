package cli

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/pyenv"
)

// prepareOptions are the preparation-time settings a command passes to
// internal/pyenv. They describe how the host reaches its dependencies, not
// what the environment is, so they are never part of the environment identity.
//
// The zero value is the documented default: uv's own package index and
// interpreter source, the egress preflight enabled, nothing extra checked. A
// command that configures nothing therefore behaves exactly as it did before
// these settings existed.
type prepareOptions struct {
	// UV is the uv executable. Empty lets the manager choose.
	UV string
	// Index replaces the package index dependencies are synced from.
	Index string
	// PythonMirror replaces the root managed interpreter downloads come from.
	PythonMirror string
	// ExtraEndpoints are further endpoints the egress preflight must reach.
	ExtraEndpoints []string
	// SkipEgressCheck turns the preflight off.
	SkipEgressCheck bool
}

// prepareManager assembles the environment manager these options describe.
// Progress, including the preflight's, goes to the app's stderr.
func (a *App) prepareManager(dataDir string, o prepareOptions) pyenv.Manager {
	return pyenv.Manager{
		DataDir:         dataDir,
		Index:           o.Index,
		PythonMirror:    o.PythonMirror,
		ExtraEndpoints:  o.ExtraEndpoints,
		SkipEgressCheck: o.SkipEgressCheck,
		Probe:           a.egressProbe,
		// The preflight is the reason a host with no egress fails as a network
		// problem instead of as a mysterious download error, so say when it
		// ran and what it checked. It is one line per environment that
		// actually had to be built.
		Logf: func(format string, args ...any) { fmt.Fprintf(a.Stderr, "otter: "+format, args...) },
	}
}

// registerPrepareFlags binds the preparation settings shared by `otter
// prepare` and `otter release`.
func (o *prepareOptions) registerPrepareFlags(fs *flag.FlagSet) {
	// Empty means "let the manager choose": a vendored copy under the data
	// directory is preferred, and PATH is the fallback. Defaulting to the bare
	// name "uv" would skip the vendored copy on a host that has no global uv.
	fs.StringVar(&o.UV, "uv", "", "uv executable used only during preparation (default: the vendored copy, then PATH)")
	// The fetch route, for a host whose egress goes through an internal mirror
	// rather than the public endpoints. Both default to empty, which keeps
	// uv's own defaults.
	fs.StringVar(&o.Index, "index", "", "package index for managed dependencies (must be the index uv.lock records; default: the lock's registries)")
	fs.StringVar(&o.PythonMirror, "python-mirror", "", "source for managed interpreter downloads (default uv's python-build-standalone releases)")
	fs.BoolVar(&o.SkipEgressCheck, "skip-egress-check", false, "skip the preflight that checks preparation can reach the endpoints it needs")
	fs.Var((*stringList)(&o.ExtraEndpoints), "egress-endpoint", "extra endpoint the preflight must reach, such as an API a job calls (repeatable)")
}

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
	var opts prepareOptions
	opts.registerPrepareFlags(fs)
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

	manager := a.prepareManager(dataDir, opts)
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
		ready, err := manager.Prepare(ctx, target.Dir, target.ID, opts.UV)
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
