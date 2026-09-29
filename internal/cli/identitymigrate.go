package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/datalock"
	"github.com/tkoizumi/otter/internal/identity"
	"github.com/tkoizumi/otter/internal/release"
	"github.com/tkoizumi/otter/internal/runs"
)

// cmdIdentity implements the `otter identity` command group.
func (a *App) cmdIdentity(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter identity migrate [--apply] [--assign <name>=<path>]")
		return 2
	}
	switch args[0] {
	case "migrate":
		return a.cmdIdentityMigrate(ctx, args[1:])
	default:
		fmt.Fprintf(a.Stderr, "otter: unknown identity subcommand %q\n", args[0])
		return 2
	}
}

// stringList collects a repeatable flag value.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// cmdIdentityMigrate moves a name-keyed workspace onto the identity registry.
//
// It is a dry run by default: the plan is printed and nothing is written. A
// legacy name declared by exactly one directory is assigned to it, keeping
// `id = old_name` so no state, history, token or release has to move. A name
// declared by several directories is a collision that needs an operator's
// choice, because there is only one old state namespace and inventing a split
// would attach the wrong data.
func (a *App) cmdIdentityMigrate(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("identity migrate", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jobs := fs.String("jobs", config.DefaultJobs, "jobs root (default: this workspace)")
	data := fs.String("data", "", "Otter data directory (default: the workspace's)")
	apply := fs.Bool("apply", false, "write markers and the registry; without it the command only reports")
	var assigns stringList
	fs.Var(&assigns, "assign", "name=path choosing the directory that keeps a colliding legacy name (repeatable)")
	fs.Usage = func() {
		fmt.Fprintln(a.Stderr, "Usage: otter identity migrate [--apply] [--assign <name>=<path>]")
		fmt.Fprintln(a.Stderr)
		fmt.Fprintln(a.Stderr, "Moves a workspace whose state, history and tokens are keyed by manifest")
		fmt.Fprintln(a.Stderr, "name onto the durable identity registry. A legacy name keeps its own value,")
		fmt.Fprintln(a.Stderr, "so nothing has to be renamed; a name shared by two directories needs --assign.")
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
	if fs.NArg() != 0 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter identity migrate [--apply] [--assign <name>=<path>]")
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

	assignments := map[string]string{}
	for _, raw := range assigns {
		name, path, ok := strings.Cut(raw, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(path) == "" {
			fmt.Fprintf(a.Stderr, "otter: --assign expects <name>=<path>, got %q\n", raw)
			return 2
		}
		assignments[strings.TrimSpace(name)] = strings.TrimSpace(path)
	}

	// Bootstrap writes markers, so it must own the data directory: racing a
	// running daemon is exactly the second-writer problem the registry exists
	// to prevent.
	lock, err := datalock.Acquire(dataDir)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		fmt.Fprintln(a.Stderr, "otter: stop the runtime before migrating its identity registry")
		return 1
	}
	defer func() { _ = lock.Close() }()

	db, err := database.Open(ctx, dataDir)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()
	if err := database.Migrate(ctx, db); err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}

	store := identity.NewStore(db.DB)
	service := identity.NewService(store, jobsRoot)

	done, err := store.BootstrapComplete(ctx)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}
	if done {
		fmt.Fprintln(a.Stdout, "identity registry is already bootstrapped")
		return 0
	}

	legacy, err := identity.LegacyKeys(ctx, db.DB)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}
	if len(legacy) == 0 {
		fmt.Fprintln(a.Stdout, "nothing to migrate: no name-keyed durable data")
		if *apply {
			if err := store.SetBootstrapComplete(ctx, true); err != nil {
				fmt.Fprintf(a.Stderr, "otter: %v\n", err)
				return 1
			}
		}
		return 0
	}

	scan, err := config.Observe(jobsRoot, nil)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}

	result, err := service.BootstrapLegacy(ctx, scan, legacy, assignments, !*apply)
	if err != nil {
		var conflict *identity.BootstrapConflictError
		if errors.As(err, &conflict) {
			fmt.Fprintf(a.Stderr, "otter: %v\n", err)
			fmt.Fprintln(a.Stderr, "otter: choose an owner for each collision, then re-run, for example:")
			for _, c := range conflict.Conflicts {
				if len(c.Paths) > 0 {
					fmt.Fprintf(a.Stderr, "otter:   otter identity migrate --apply --assign %s=%s\n", c.Name, c.Paths[0])
				}
			}
			return 1
		}
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}

	mode := "plan"
	if *apply {
		mode = "applied"
	}
	fmt.Fprintf(a.Stdout, "identity migration %s: %d legacy key(s)\n", mode, len(legacy))
	for _, asg := range result.Assigned {
		fmt.Fprintf(a.Stdout, "  keep     %-32s %s\n", asg.Name, asg.Path)
	}
	for _, id := range result.Reserved {
		fmt.Fprintf(a.Stdout, "  reserve  %-32s (no source directory)\n", id)
	}
	for _, path := range result.Fresh {
		fmt.Fprintf(a.Stdout, "  fresh    %s\n", path)
	}

	if !*apply {
		fmt.Fprintln(a.Stdout, "dry run: re-run with --apply to write markers and the registry")
		return 0
	}
	if err := store.SetBootstrapComplete(ctx, true); err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}

	// Choosing an owner does not make the identity's existing release
	// trustworthy: a release records the tree it was staged from, and if that
	// is the directory the operator decided against, running it would execute
	// the wrong code against the chosen tree's state. Disable it without
	// destroying the snapshot, so the job refuses to run until it is
	// released again.
	if n := a.quarantineMismatchedReleases(ctx, db, dataDir, result.Assigned); n > 0 {
		fmt.Fprintf(a.Stdout, "quarantined %d release(s) whose provenance does not match the chosen owner\n", n)
	}

	fmt.Fprintln(a.Stdout, "bootstrap complete: start the runtime to register the remaining jobs")
	return 0
}

// quarantineMismatchedReleases removes the activation pointer of any release
// whose recorded source is not the directory that now owns the identity. The
// staged snapshot is left in place: it is evidence, and an operator may still
// inspect or deliberately reactivate it.
func (a *App) quarantineMismatchedReleases(ctx context.Context, db *database.DB, dataDir string, assigned []identity.Assignment) int {
	manager := release.Manager{DataDir: dataDir}
	quarantined := 0
	for _, asg := range assigned {
		meta, ok, err := manager.Active(asg.ID.String())
		if err != nil || !ok || meta.Source == "" {
			continue
		}
		source, err := identity.Canonical(meta.Source)
		if err != nil {
			continue
		}
		chosen, err := identity.Canonical(asg.Path)
		if err != nil || source == chosen {
			continue
		}
		active, err := manager.ActivePath(asg.ID.String())
		if err != nil {
			continue
		}
		if err := os.Remove(active); err != nil {
			continue
		}
		fmt.Fprintf(a.Stderr, "otter: %s: quarantined active release %s: staged from %s, owner is %s\n",
			asg.Name, shortDigest(meta.Digest), source, chosen)
		quarantined++

		// A queued attempt recorded the snapshot it would execute, so it is
		// cancelled rather than run against the owner the operator just chose.
		cancelled, err := runs.NewStore(db.DB).CancelPinnedToRelease(ctx, asg.ID.String(), meta.Digest,
			fmt.Sprintf("release %s was quarantined during identity migration: it was staged from a different source than the identity's owner; re-release and resubmit", shortDigest(meta.Digest)))
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: %v\n", err)
			continue
		}
		if cancelled > 0 {
			fmt.Fprintf(a.Stderr, "otter: %s: cancelled %d queued attempt(s) pinned to the quarantined release\n", asg.Name, cancelled)
		}
	}
	return quarantined
}
