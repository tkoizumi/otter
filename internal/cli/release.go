package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
// The order is deliberate: stage, then prepare against the staged snapshot,
// then switch. A failure at any point leaves the previous release active, so a
// bad candidate can never take down a working integration. Activation is the
// last step, and it is a single atomic rename.
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
		return a.activateRelease(manager, targets[0].ID, *activate)
	}

	for _, target := range targets {
		if code := a.releaseOne(ctx, manager, target, *shared, *uv, *keep); code != 0 {
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

// releaseOne stages, prepares and activates one integration.
func (a *App) releaseOne(ctx context.Context, manager release.Manager, target integrationTarget, shared, uv string, keep int) int {
	manifest, err := config.LoadAndValidate(filepath.Join(target.Dir, config.ManifestFileName))
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %s: %v\n", target.ID, err)
		return 1
	}
	if manifest.Name != target.ID {
		fmt.Fprintf(a.Stderr, "otter: %s: manifest at %s declares %q\n", target.ID, target.Dir, manifest.Name)
		return 1
	}

	// The manifest's own python.path is the authoritative list of shared code,
	// so a release captures exactly what the integration imports without the
	// operator having to repeat it on the command line. --shared adds trees
	// the manifest cannot express.
	sharedTrees := sharedTreesFor(target.Dir, manifest, shared)
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
	meta, err := manager.StageWithShared(target.ID, target.Dir, sharedTrees, environmentDigest)
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

	// 3. Prepare against the staged tree, not the live one. The snapshot is
	//    what will run, so it is what must be validated.
	if manifest.Python.Mode == "managed" {
		releaseSource := release.SourceDir(mustReleaseDir(manager, target.ID, meta.Digest), target.ID)
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
	if err := manager.Activate(target.ID, meta.Digest); err != nil {
		fmt.Fprintf(a.Stderr, "otter: release %s: %v\n", target.ID, err)
		return 1
	}
	fmt.Fprintf(a.Stdout, "%s: activated %s\n", target.ID, meta.Digest[:12])

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
// A prefix is enough to name one, matching how `--list` prints digests.
func (a *App) activateRelease(manager release.Manager, id, ref string) int {
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

	if err := manager.Activate(id, matches[0].Digest); err != nil {
		fmt.Fprintf(a.Stderr, "otter: release %s: %v\n", id, err)
		return 1
	}
	fmt.Fprintf(a.Stdout, "%s: activated %s\n", id, matches[0].Digest[:12])
	return 0
}

// mustReleaseDir resolves a release directory that was just staged, so a
// failure here means the filesystem changed underneath us.
func mustReleaseDir(m release.Manager, id, digest string) string {
	dir, err := m.Dir(id, digest)
	if err != nil {
		// Dir only fails on an invalid name or digest; both are impossible
		// for a value just produced by Stage.
		panic(fmt.Sprintf("release: %v", err))
	}
	return dir
}

// sharedTreesFor works out which shared trees a release must capture.
//
// The manifest's own python.path is the starting point. Its entries are already
// resolved to absolute directories by the config package, so this does not
// reinterpret them: it only decides where each tree lands inside the release.
//
// The landing place is the shared ancestor that contains both the integration
// and the tree. That is the release root's counterpart in the checkout, so a
// named tree keeps the same relative path it has today and the manifest's
// relative declaration keeps resolving after activation. For the shipped
// layout, `../../lib` from integrations/<name> lands as `lib` at the root.
//
// An entry inside the integration directory needs no separate capture: the
// integration's own copy already carries it. --shared adds trees the manifest
// does not declare.
func sharedTreesFor(sourceDir string, manifest *config.Manifest, flagValue string) []release.SharedTree {
	var trees []release.SharedTree
	seen := map[string]bool{}

	add := func(target string) {
		target = filepath.Clean(target)
		if target == sourceDir || strings.HasPrefix(target, sourceDir+string(filepath.Separator)) {
			return
		}
		name, ok := releaseRelativeName(sourceDir, target)
		if !ok || seen[name] {
			return
		}
		seen[name] = true
		trees = append(trees, release.SharedTree{Source: target, Name: name})
	}

	for _, target := range manifest.PythonPaths() {
		add(target)
	}
	for _, entry := range strings.Split(flagValue, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !filepath.IsAbs(entry) {
			entry = filepath.Join(sourceDir, entry)
		}
		add(entry)
	}
	return trees
}

// releaseRelativeName names a shared directory relative to the release root,
// which is the shallowest ancestor that contains both the integration and the
// tree. Ancestors are tried nearest first, so the tree lands as shallowly as
// the existing layout allows.
//
// Two cases yield no name. A tree inside the integration directory needs no
// separate capture, because the integration's own copy already carries it. A
// tree with no common ancestor short of the filesystem root cannot be
// reproduced at a relative depth, and capturing it would put a directory at the
// root of the release that no manifest path could reach.
func releaseRelativeName(sourceDir, target string) (string, bool) {
	source := filepath.Clean(sourceDir)
	if target == source || strings.HasPrefix(target, source+string(filepath.Separator)) {
		return "", false
	}

	ancestor := filepath.Dir(source)
	for {
		// Stop before the filesystem root. Rel against "/" produces a path with
		// no "..", so accepting that iteration would capture an unrelated tree
		// at a depth no manifest path could reach.
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", false
		}
		if rel, err := filepath.Rel(ancestor, target); err == nil && rel != "." &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel), true
		}
		ancestor = parent
	}
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
		fmt.Fprintf(a.Stdout, "%s %s  %s  env=%s%s  %s\n",
			marker, rel.Digest[:12], rel.CreatedAt.Format("2006-01-02 15:04:05"),
			environment, git, rel.Source)
	}
	if active, ok, err := m.Active(id); err == nil && ok {
		fmt.Fprintf(a.Stdout, "\nactive release: %s\n", active.Digest[:12])
	} else {
		fmt.Fprintf(a.Stderr, "\nno active release; runs of %s are refused until one is activated\n", id)
	}
	return 0
}
