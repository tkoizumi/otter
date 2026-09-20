package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/pyenv"
	"github.com/tkoizumi/otter/internal/release"
)

// cmdRelease stages integrations as immutable releases, prepares their managed
// Python environments, and activates them.
//
// The order is deliberate: stage, validate the snapshot's manifest, prepare
// against the staged snapshot, then switch. A failure at any point leaves the
// previous release active, so a bad candidate can never take down a working
// integration. Activation is the last step, and it is a single atomic rename.
//
// Every integration runs from its active release, so this is the command that
// makes code live, for external and managed Python alike. The difference is
// what else a release pins: managed mode also binds an interpreter and a
// dependency set, which is why only it has a preparation step.
//
// Identity follows `otter run`: no argument means the integration in the
// working directory, a bare word is a manifest name, and anything path-shaped
// is read from disk. --all releases every integration in the workspace.
func (a *App) cmdRelease(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	integrations := fs.String("integrations", config.DefaultIntegrations, "integrations root (default: this workspace)")
	data := fs.String("data", "", "Otter data directory (default: the workspace's, .otter/data)")
	source := fs.String("source", "", "integration directory to snapshot; overrides discovery")
	shared := fs.String("shared", "", "extra shared code directories to snapshot (comma-separated)")
	all := fs.Bool("all", false, "release every integration in the workspace")
	activate := fs.String("activate", "", "activate an already staged release by digest, without staging")
	uv := fs.String("uv", "", "uv executable used only during preparation")
	keep := fs.Int("keep", 0, "retain this many inactive releases (0 keeps every release)")
	list := fs.Bool("list", false, "list staged releases instead of creating one")
	fs.Usage = func() {
		fmt.Fprintln(a.Stderr, "Usage: otter release [flags] [<integration>|.]")
		fmt.Fprintln(a.Stderr)
		fmt.Fprintln(a.Stderr, "Stages an immutable snapshot of an integration, prepares its managed Python")
		fmt.Fprintln(a.Stderr, "environment, and activates it. Runs execute the active release, so an edit is")
		fmt.Fprintln(a.Stderr, "not live until the integration is released again.")
		fmt.Fprintln(a.Stderr)
		fmt.Fprintln(a.Stderr, "With no argument, releases the integration in the working directory.")
		fmt.Fprintln(a.Stderr)
		fmt.Fprintln(a.Stderr, "Flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter release [flags] [<integration>|.]")
		return 2
	}
	if *all && fs.NArg() > 0 {
		fmt.Fprintln(a.Stderr, "otter: --all releases every integration; do not name one as well")
		return 2
	}
	if *all && *source != "" {
		fmt.Fprintln(a.Stderr, "otter: --source names one tree; it cannot be combined with --all")
		return 2
	}
	if *activate != "" && *source != "" {
		fmt.Fprintln(a.Stderr, "otter: --activate switches to a staged release; it cannot stage --source")
		return 2
	}
	if *activate != "" && *all {
		fmt.Fprintln(a.Stderr, "otter: --activate works on one integration at a time")
		return 2
	}
	if *list && (*all || *activate != "" || *source != "") {
		fmt.Fprintln(a.Stderr, "otter: --list cannot be combined with --all, --activate or --source")
		return 2
	}
	named := fs.NArg() == 1
	ref := "."
	if named {
		ref = fs.Arg(0)
	}

	// A release written anywhere but the daemon's own data directory is
	// invisible to it, which surfaces much later as "no active release" while
	// the release sits on disk. Resolve it against the workspace and refuse a
	// disagreement rather than create one.
	dataDir, code := resolveWorkspaceData(a.Stderr, *data, flagWasSet(fs, "data"))
	if code != 0 {
		return code
	}
	*data = dataDir
	integrationsRoot, code := resolveIntegrationsRoot(a.Stderr, *integrations, flagWasSet(fs, "integrations"))
	if code != 0 {
		return code
	}
	*integrations = integrationsRoot

	manager := release.Manager{DataDir: *data}

	targets, code := a.integrationTargets(ref, *integrations, *source, named, *all)
	if code != 0 {
		return code
	}
	if *list {
		return a.printReleases(manager, targets[0].ID)
	}
	if *activate != "" {
		if len(targets) != 1 {
			fmt.Fprintln(a.Stderr, "otter: --activate works on one integration at a time")
			return 2
		}
		return a.activateRelease(ctx, manager, targets[0].ID, *activate, *uv)
	}

	for _, target := range targets {
		if code := a.releaseOne(ctx, manager, integrationsRoot, target, *shared, *uv, *keep); code != 0 {
			return code
		}
	}
	return 0
}

// integrationTarget is one integration a local command acts on: the name the
// runtime knows it by, and the source tree it lives in.
type integrationTarget struct {
	ID  string
	Dir string
}

// integrationTargets works out which integrations a command acts on.
//
// A bare name is the only case that needs the workspace, because only then is
// there something to search for; a path reference names its own tree.
func (a *App) integrationTargets(ref, integrationsRoot, source string, named, all bool) ([]integrationTarget, int) {
	if all {
		return a.discoveredIntegrationTargets(integrationsRoot)
	}

	id, dir, err := resolveIntegrationRefDir(ref)
	if err != nil {
		// An unnamed "." that does not resolve is only an error when --source
		// is not there to name the tree instead.
		if named || source == "" {
			fmt.Fprintf(a.Stderr, "otter: %v\n", err)
			return nil, 2
		}
		id, dir = "", ""
	}

	if source != "" {
		absolute, err := absoluteDir(source)
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: %v\n", err)
			return nil, 1
		}
		dir = absolute
		if !named {
			// --source names the tree; with no reference, the tree names the
			// integration.
			m, err := config.Load(filepath.Join(dir, config.ManifestFileName))
			if err != nil {
				fmt.Fprintf(a.Stderr, "otter: %v\n", err)
				return nil, 1
			}
			id = m.Name
		}
		return []integrationTarget{{ID: id, Dir: dir}}, 0
	}

	if dir != "" {
		return []integrationTarget{{ID: id, Dir: dir}}, 0
	}

	items, err := config.Discover(integrationsRoot)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return nil, 1
	}
	for _, item := range items {
		if item.ID != id {
			continue
		}
		if !item.Valid {
			fmt.Fprintf(a.Stderr, "otter: %s: %s\n", id, item.Error)
			return nil, 1
		}
		return []integrationTarget{{ID: id, Dir: item.Dir}}, 0
	}
	fmt.Fprintf(a.Stderr, "otter: integration %s not found under %s\n", id, integrationsRoot)
	return nil, 1
}

// discoveredIntegrationTargets is every valid integration under a root, for --all.
func (a *App) discoveredIntegrationTargets(integrationsRoot string) ([]integrationTarget, int) {
	items, err := config.Discover(integrationsRoot)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return nil, 1
	}
	targets := make([]integrationTarget, 0, len(items))
	for _, item := range items {
		if !item.Valid {
			fmt.Fprintf(a.Stderr, "otter: %s: %s\n", item.ID, item.Error)
			return nil, 1
		}
		targets = append(targets, integrationTarget{ID: item.ID, Dir: item.Dir})
	}
	if len(targets) == 0 {
		fmt.Fprintf(a.Stderr, "otter: no %s found under %s\n", config.ManifestFileName, integrationsRoot)
		return nil, 1
	}
	return targets, 0
}

// absoluteDir resolves a possibly relative directory against the working
// directory.
func absoluteDir(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	wd, err := workingDirForTest()
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(wd, path)), nil
}

// releaseOne stages, validates, prepares and activates one integration.
func (a *App) releaseOne(ctx context.Context, manager release.Manager, integrationsRoot string, target integrationTarget, shared, uv string, keep int) int {
	manifest, err := config.LoadAndValidate(filepath.Join(target.Dir, config.ManifestFileName))
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %s: %v\n", target.ID, err)
		return 1
	}
	if manifest.Name != target.ID {
		fmt.Fprintf(a.Stderr, "otter: %s: manifest at %s declares %q\n", target.ID, target.Dir, manifest.Name)
		return 1
	}
	// A release can only carry a tree whose depth is expressed relative to the
	// integration directory. An absolute python.path passes validation on this
	// machine and is a missing directory on the host, so it is refused here.
	if err := manifest.ValidatePythonPathsForRelease(); err != nil {
		fmt.Fprintf(a.Stderr, "otter: %s: %v\n", target.ID, err)
		return 1
	}

	// The manifest's own python.path is the authoritative list of shared code,
	// so a release captures exactly what the integration imports without the
	// operator having to repeat it on the command line. --shared adds trees
	// the manifest cannot express.
	sources, err := sharedSourcesFor(target.Dir, manifest, shared)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %s: %v\n", target.ID, err)
		return 1
	}
	trees := make([]release.SharedTree, 0, len(sources))
	for _, src := range sources {
		trees = append(trees, release.SharedTree{Source: src})
	}
	// ONE placement rule: the base is the closest common ancestor of the
	// discovery root, this integration and every captured tree.
	layout, err := release.Plan(integrationsRoot, target.Dir, trees)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %s: %v\n", target.ID, err)
		return 1
	}

	envManager := pyenv.Manager{DataDir: manager.DataDir}

	// Bound one release: a release that hangs on a package download must not
	// hold a deploy open indefinitely. With --all the bound is per integration,
	// so one slow environment does not starve the rest.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	// 1. Resolve the environment identity for this source tree. The release
	//    digest covers it, so it has to be known before the snapshot is taken.
	//    Only managed Python has one: an external integration runs the
	//    interpreter the manifest names, and what a release pins for it is the
	//    source.
	environmentDigest := ""
	if manifest.Python.Mode == "managed" {
		spec, err := envManager.ResolveCurrentAt(ctx, target.Dir, target.ID, uv)
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: release %s: %v\n", target.ID, err)
			return 1
		}
		environmentDigest = spec.Digest
	}

	// 2. Stage the snapshot. Releasing identical inputs is a no-op, so a
	//    redeploy does not create a second copy of the same tree.
	meta, err := manager.StageWithLayout(target.ID, target.Dir, layout, environmentDigest)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: release %s: %v\n", target.ID, err)
		return 1
	}
	// Traceability, not a gate: a dirty tree is reported because it is
	// otherwise invisible once the release is on the host.
	if git := (release.GitState{Revision: meta.GitRevision, Dirty: meta.GitDirty}); git.Ready() {
		fmt.Fprintf(a.Stdout, "%s: release %s (git %s)\n", target.ID, meta.Digest[:12], git.Describe())
		if git.Dirty {
			fmt.Fprintf(a.Stderr, "note: %s was released from a working tree with uncommitted changes\n", target.ID)
		}
	} else {
		fmt.Fprintf(a.Stdout, "%s: release %s\n", target.ID, meta.Digest[:12])
	}

	// 3. Validate the staged manifest before preparation: the snapshot's own
	//    python.path entries have to resolve inside the snapshot, which is a
	//    different question from whether they resolve on this machine. The
	//    snapshot manifest, not the live one, decides whether preparation runs.
	releaseSource, bound, err := a.snapshotRelease(manager, target.ID, meta)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: release %s: not activating: %v\n", target.ID, err)
		return 1
	}
	if bound.Python.Mode == "managed" {
		ready, err := envManager.Prepare(ctx, releaseSource, target.ID, uv)
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: release %s: not activating: %v\n", target.ID, err)
			return 1
		}
		if ready.Digest != environmentDigest {
			fmt.Fprintf(a.Stderr, "otter: release %s: environment changed during staging (%s -> %s); re-run the release\n",
				target.ID, environmentDigest[:12], ready.Digest[:12])
			return 1
		}
		fmt.Fprintf(a.Stdout, "%s: environment %s (python %s)\n", target.ID, ready.Digest[:12], ready.Python)
	}

	// 4. Activate. Everything above is reversible; this is the commit point.
	//    activateStaged re-validates the snapshot and, for a managed release,
	//    its environment, so a rollback and a fresh release share one gate.
	if code := a.activateStaged(ctx, manager, target.ID, meta.Digest, uv); code != 0 {
		return code
	}

	if keep > 0 {
		removed, err := manager.Retain(target.ID, keep, nil)
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: release %s: retention: %v\n", target.ID, err)
		}
		for _, digest := range removed {
			fmt.Fprintf(a.Stdout, "%s: removed old release %s\n", target.ID, digest[:12])
		}
	}
	return 0
}

// activateRelease points an integration at a release it already has.
//
// This is rollback. Every staged digest is still on disk unless retention
// removed it, and switching is the same atomic rename a new release performs.
// A prefix is enough to name one, matching how `--list` prints digests. The
// target's own manifest and environment are re-validated first, so rolling back
// to a broken or unprepared snapshot fails without disturbing the active one.
func (a *App) activateRelease(ctx context.Context, manager release.Manager, id, ref, uv string) int {
	releases, err := manager.List(id)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}
	if len(releases) == 0 {
		fmt.Fprintf(a.Stderr, "otter: %s has no staged releases; run otter release %s\n", id, id)
		return 1
	}

	var matches []release.Release
	for _, rel := range releases {
		if rel.Digest == ref || strings.HasPrefix(rel.Digest, ref) {
			matches = append(matches, rel)
		}
	}
	switch len(matches) {
	case 0:
		fmt.Fprintf(a.Stderr, "otter: %s has no release %s; otter release --list %s\n", id, ref, id)
		return 1
	case 1:
	default:
		fmt.Fprintf(a.Stderr, "otter: %q matches more than one release of %s; name more of the digest\n", ref, id)
		return 1
	}

	return a.activateStaged(ctx, manager, id, matches[0].Digest, uv)
}

// activateStaged validates one staged release and switches to it.
//
// It is the single activation path: a fresh release and a rollback both go
// through it, so both refuse a snapshot whose manifest no longer resolves
// inside the release and a managed release whose environment is not ready.
// Activation is the last step, so every failure here leaves the previous
// active release untouched.
func (a *App) activateStaged(ctx context.Context, manager release.Manager, id, digest, uv string) int {
	meta, err := manager.Metadata(id, digest)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: release %s: %v\n", id, err)
		return 1
	}
	sourceDir, bound, err := a.snapshotRelease(manager, id, meta)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: release %s: not activating: %v\n", id, err)
		return 1
	}
	if bound.Python.Mode == "managed" {
		if err := a.requireReadyEnvironment(ctx, manager, id, sourceDir, uv); err != nil {
			fmt.Fprintf(a.Stderr, "otter: release %s: not activating: %v\n", id, err)
			return 1
		}
	}
	if err := manager.Activate(id, digest); err != nil {
		fmt.Fprintf(a.Stderr, "otter: release %s: %v\n", id, err)
		return 1
	}
	fmt.Fprintf(a.Stdout, "%s: activated %s\n", id, digest[:12])
	return 0
}

// snapshotRelease resolves a staged release's integration directory and loads
// and validates the manifest that travels inside it. Validating the snapshot
// manifest is what tells "the paths exist on the machine that released" apart
// from "the release captured the trees the paths name": a release that names an
// absolute python.path is refused here, because the path it names belongs to
// the machine that staged it and to no other.
func (a *App) snapshotRelease(manager release.Manager, id string, meta release.Metadata) (string, *config.Manifest, error) {
	sourceDir, err := manager.SourceDir(meta)
	if err != nil {
		return "", nil, fmt.Errorf("release %s is unusable: %w", shortDigest(meta.Digest), err)
	}
	bound, err := config.LoadAndValidate(filepath.Join(sourceDir, config.ManifestFileName))
	if err != nil {
		return "", nil, fmt.Errorf("release %s is invalid: %w", shortDigest(meta.Digest), err)
	}
	if err := bound.ValidatePythonPathsForRelease(); err != nil {
		return "", nil, fmt.Errorf("release %s is invalid: %w", shortDigest(meta.Digest), err)
	}
	return sourceDir, bound, nil
}

// requireReadyEnvironment confirms that a managed release's environment was
// prepared. It is checked at activation, not only at preparation time, so
// `--activate` cannot switch to a release whose environment is missing. The
// error names `otter prepare`, which is the command that fixes it.
func (a *App) requireReadyEnvironment(ctx context.Context, manager release.Manager, id, sourceDir, uv string) error {
	envManager := pyenv.Manager{DataDir: manager.DataDir}
	spec, err := envManager.ResolveCurrentAt(ctx, sourceDir, id, uv)
	if err != nil {
		return fmt.Errorf("resolve managed Python for %s: %w", id, err)
	}
	if _, err := envManager.GetReady(spec); err != nil {
		return err
	}
	return nil
}

// sharedSourcesFor works out which live directories a release must capture.
//
// The manifest's own python.path is the starting point. Its entries are already
// resolved to absolute directories by the config package, so this does not
// reinterpret them: it only confirms each one exists as a directory. An entry
// inside the integration directory is returned too and dropped later by
// release.Plan, because the integration's own copy already carries it. --shared
// adds trees the manifest cannot express and may be absolute.
//
// A declared tree that is missing is an error rather than a skip: silently
// dropping it would produce a release that runs against the live tree here and
// fails on the host.
func sharedSourcesFor(sourceDir string, manifest *config.Manifest, flagValue string) ([]string, error) {
	var out []string
	seen := map[string]bool{}

	add := func(target, label string) error {
		target = filepath.Clean(target)
		info, err := os.Stat(target)
		if err != nil {
			return fmt.Errorf("%s does not exist (%s)", label, target)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", label)
		}
		if seen[target] {
			return nil
		}
		seen[target] = true
		out = append(out, target)
		return nil
	}

	for _, spec := range manifest.PythonPathEntries() {
		if err := add(spec.Resolved, fmt.Sprintf("python.path %q", spec.Declared)); err != nil {
			return nil, err
		}
	}
	for _, raw := range strings.Split(flagValue, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		target := raw
		if !filepath.IsAbs(target) {
			target = filepath.Join(sourceDir, filepath.FromSlash(raw))
		}
		if err := add(target, fmt.Sprintf("--shared %q", raw)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// shortDigest abbreviates a digest for a message, tolerating an empty value.
func shortDigest(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	if digest == "" {
		return "unknown"
	}
	return digest
}

func (a *App) printReleases(m release.Manager, id string) int {
	releases, err := m.List(id)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}
	if len(releases) == 0 {
		fmt.Fprintf(a.Stderr, "otter: %s has no staged releases\n", id)
		return 1
	}
	code := 0
	for _, rel := range releases {
		marker := " "
		if rel.Active {
			marker = "*"
		}
		environment := rel.Environment
		if environment != "" {
			environment = environment[:12]
		}
		git := ""
		if rel.GitRevision != "" {
			git = " git=" + rel.GitRevision
			if rel.GitDirty {
				git += "+dirty"
			}
		}
		// The placement is read back through the one resolver, so a hand-edited
		// release is reported as broken instead of looking healthy.
		placement := "invalid"
		if src, err := m.SourceDir(rel.Metadata); err == nil {
			if shown, relErr := filepath.Rel(mustReleaseDir(m, id, rel.Digest), src); relErr == nil {
				placement = filepath.ToSlash(shown)
			}
		} else {
			fmt.Fprintf(a.Stderr, "otter: release %s has an invalid integration path: %v\n", rel.Digest[:12], err)
			code = 1
		}
		fmt.Fprintf(a.Stdout, "%s %s  %s  env=%s%s  at=%s  %s\n",
			marker, rel.Digest[:12], rel.CreatedAt.Format("2006-01-02 15:04:05"),
			environment, git, placement, rel.Source)
	}
	if active, ok, err := m.Active(id); err == nil && ok {
		fmt.Fprintf(a.Stdout, "\nactive release: %s\n", active.Digest[:12])
	} else {
		fmt.Fprintf(a.Stderr, "\nno active release; runs of %s are refused until one is activated\n", id)
	}
	return code
}

// mustReleaseDir resolves a release directory that has already been listed, so
// a failure here means the filesystem changed underneath us.
func mustReleaseDir(m release.Manager, id, digest string) string {
	dir, err := m.Dir(id, digest)
	if err != nil {
		// Dir only fails on an invalid name or digest; both are impossible for
		// a value that just came from List.
		panic(fmt.Sprintf("release: %v", err))
	}
	return dir
}
