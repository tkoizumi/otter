package cli

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/otter-runtime/otter/internal/config"
	"github.com/otter-runtime/otter/internal/pyenv"
	"github.com/otter-runtime/otter/internal/release"
)

// cmdRelease stages an integration as an immutable release, prepares its
// environment, and activates it.
//
// The order is deliberate: stage, then prepare against the staged snapshot,
// then switch. A failure at any point leaves the previous release active, so a
// bad candidate can never take down a working integration. Activation is the
// last step, and it is a single atomic rename.
func (a *App) cmdRelease(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	integrations := fs.String("integrations", config.DefaultIntegrations, "integrations root")
	data := fs.String("data", config.DefaultDataDir, "Otter data directory")
	source := fs.String("source", "", "integration directory to snapshot; defaults to discovering <integration> under --integrations")
	shared := fs.String("shared", "", "comma-separated shared code directories to snapshot with the integration")
	uv := fs.String("uv", "", "uv executable used only during preparation")
	keep := fs.Int("keep", 0, "retain this many inactive releases (0 keeps every release)")
	list := fs.Bool("list", false, "list staged releases instead of creating one")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Stderr, "usage: otter release [--integrations DIR] [--data DIR] <integration>")
		return 2
	}
	id := fs.Arg(0)

	manager := release.Manager{DataDir: *data}
	if *list {
		return a.printReleases(manager, id)
	}

	sourceDir := *source
	if sourceDir == "" {
		items, err := config.Discover(*integrations)
		if err != nil {
			return a.fail(err)
		}
		for _, item := range items {
			if item.ID != id {
				continue
			}
			if !item.Valid {
				fmt.Fprintf(a.Stderr, "otter: %s: %s\n", id, item.Error)
				return 1
			}
			sourceDir = item.Dir
			break
		}
		if sourceDir == "" {
			fmt.Fprintf(a.Stderr, "otter: integration %s not found under %s\n", id, *integrations)
			return 1
		}
	}

	manifest, err := config.LoadAndValidate(filepath.Join(sourceDir, "otter.yaml"))
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %s: %v\n", id, err)
		return 1
	}
	if manifest.Name != id {
		fmt.Fprintf(a.Stderr, "otter: %s: manifest at %s declares %q\n", id, sourceDir, manifest.Name)
		return 1
	}

	// The manifest's own python.path is the authoritative list of shared code,
	// so a release captures exactly what the integration imports without the
	// operator having to repeat it on the command line. --shared adds trees
	// the manifest cannot express.
	sharedTrees := sharedTreesFor(sourceDir, manifest, *shared)
	envManager := pyenv.Manager{DataDir: *data}

	// Bound the whole operation: a release that hangs on a package download
	// must not hold a deploy open indefinitely.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	// 1. Resolve the environment identity for this source tree. The release
	//    digest covers it, so it has to be known before the snapshot is taken.
	environmentDigest := ""
	if manifest.Python.Mode == "managed" {
		spec, err := envManager.ResolveCurrentAt(ctx, sourceDir, id, *uv)
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: release %s: %v\n", id, err)
			return 1
		}
		environmentDigest = spec.Digest
	}

	// 2. Stage the snapshot. Releasing identical inputs is a no-op, so a
	//    redeploy does not create a second copy of the same tree.
	meta, err := manager.StageWithShared(id, sourceDir, sharedTrees, environmentDigest)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: release %s: %v\n", id, err)
		return 1
	}
	// Traceability, not a gate: a dirty tree is reported because it is
	// otherwise invisible once the release is on the host.
	if git := (release.GitState{Revision: meta.GitRevision, Dirty: meta.GitDirty}); git.Ready() {
		fmt.Fprintf(a.Stdout, "%s: release %s (git %s)\n", id, meta.Digest[:12], git.Describe())
		if git.Dirty {
			fmt.Fprintf(a.Stderr, "note: %s was released from a working tree with uncommitted changes\n", id)
		}
	} else {
		fmt.Fprintf(a.Stdout, "%s: release %s\n", id, meta.Digest[:12])
	}

	// 3. Prepare against the staged tree, not the live one. The snapshot is
	//    what will run, so it is what must be validated.
	if manifest.Python.Mode == "managed" {
		releaseSource := release.SourceDir(mustReleaseDir(manager, id, meta.Digest), id)
		ready, err := envManager.Prepare(ctx, releaseSource, id, *uv)
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: release %s: not activating: %v\n", id, err)
			return 1
		}
		if ready.Digest != environmentDigest {
			fmt.Fprintf(a.Stderr, "otter: release %s: environment changed during staging (%s -> %s); re-run the release\n",
				id, environmentDigest[:12], ready.Digest[:12])
			return 1
		}
		fmt.Fprintf(a.Stdout, "%s: environment %s (python %s)\n", id, ready.Digest[:12], ready.Python)
	}

	// 4. Activate. Everything above is reversible; this is the commit point.
	if err := manager.Activate(id, meta.Digest); err != nil {
		fmt.Fprintf(a.Stderr, "otter: release %s: %v\n", id, err)
		return 1
	}
	fmt.Fprintf(a.Stdout, "%s: activated %s\n", id, meta.Digest[:12])

	if *keep > 0 {
		removed, err := manager.Retain(id, *keep, nil)
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: release %s: retention: %v\n", id, err)
		}
		for _, digest := range removed {
			fmt.Fprintf(a.Stdout, "%s: removed old release %s\n", id, digest[:12])
		}
	}
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
		return a.fail(err)
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
		fmt.Fprintf(a.Stderr, "\nno active release; the integration is running from its source tree\n")
	}
	return 0
}
