package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/datalock"
	"github.com/tkoizumi/otter/internal/identity"
)

// errPathSuppressed reports a source path the runtime refuses to register: its
// job was deleted and the directory deliberately left in place. It is separated
// from "not registered" because the two call for opposite handling in a deploy
// sweep: a new path must be registered and released, while a suppressed one will
// never register and must be skipped and reported rather than failing the run.
var errPathSuppressed = errors.New("path is suppressed")

// suppressedPathError names the path and why the runtime refuses it.
type suppressedPathError struct {
	dir    string
	reason string
}

func (e *suppressedPathError) Error() string {
	reason := e.reason
	if reason == "" {
		reason = "the job was deleted"
	}
	return fmt.Sprintf("%s is suppressed (%s); the runtime will not register it. "+
		"Run `otter register %s` to reuse the path, or remove the directory", e.dir, reason, e.dir)
}

func (e *suppressedPathError) Is(target error) bool { return target == errPathSuppressed }

// This file bridges local, file-writing commands (release, prepare) to the
// durable identity registry.
//
// The daemon owns the registry. When one serves this workspace the CLI asks it,
// reloading first so a directory added since the last scan is registered
// before it is asked about. Without a running daemon the CLI reconciles
// directly while holding the data-directory lock, which is the only way it can
// write identity state without becoming a second writer alongside a live
// daemon.

// mapTargetIdentities rewrites job targets so their ID is the durable
// identity rather than the manifest label.
func (a *App) mapTargetIdentities(ctx context.Context, jobsRoot, dataDir string, targets []jobTarget) ([]jobTarget, int) {
	client := runningWorkspaceClient(ctx)

	out := make([]jobTarget, 0, len(targets))
	for _, target := range targets {
		id, err := resolveIdentityForDir(ctx, client, jobsRoot, dataDir, target.Dir)
		if errors.Is(err, errPathSuppressed) {
			// A retire-but-keep-source deploy discovers the directory like any
			// other, but the runtime will never register it. Skipping is the
			// coherent outcome; failing the whole sweep is not.
			fmt.Fprintf(a.Stdout, "skip: %v\n", err)
			continue
		}
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: %v\n", err)
			return nil, 1
		}
		out = append(out, jobTarget{ID: id, Name: target.ID, Dir: target.Dir})
	}
	return out, 0
}

// runningWorkspaceClient returns a client for the daemon serving this
// workspace, or nil when none is running.
func runningWorkspaceClient(ctx context.Context) *api.Client {
	root, inProject, err := workspaceRoot()
	if err != nil || !inProject {
		return nil
	}
	base, ok := runningURL(serveDir(root, ""))
	if !ok {
		return nil
	}
	client := api.NewClient(base, os.Getenv("OTTER_API_TOKEN"))
	// A reload is how a directory added since the daemon started becomes
	// addressable. A failure here is not fatal: resolution may still succeed
	// for a job the daemon already knows.
	_, _ = client.Reload(ctx)
	return client
}

// resolveIdentityForDir maps a source directory to the identity that owns it.
//
// A suppressed path is reported as such rather than as missing. A running daemon
// answers "not found" for one (it is not registered), so the local registry is
// consulted to tell a refused path from a genuinely new one.
func resolveIdentityForDir(ctx context.Context, client *api.Client, jobsRoot, dataDir, dir string) (string, error) {
	if client != nil {
		view, err := client.ResolveJob(ctx, dir)
		if err == nil {
			return view.ID, nil
		}
		if suppressed, reason := suppressedPath(ctx, dataDir, dir); suppressed {
			return "", &suppressedPathError{dir: dir, reason: reason}
		}
		return "", err
	}
	return reconcileAndResolve(ctx, jobsRoot, dataDir, dir)
}

// suppressedPath asks the durable registry, read-only, whether a source path was
// suppressed by a job deletion. It is the second half of resolveIdentityForDir:
// the positive answer comes from the runtime, and this tells the two negative
// answers apart. A missing or unreadable database is reported as "not
// suppressed" -- the caller's original error is the honest one then.
func suppressedPath(ctx context.Context, dataDir, dir string) (bool, string) {
	if dataDir == "" {
		return false, ""
	}
	if _, err := os.Stat(filepath.Join(dataDir, database.FileName)); err != nil {
		return false, ""
	}
	canonical, err := identity.Canonical(dir)
	if err != nil {
		return false, ""
	}
	db, err := database.Open(ctx, dataDir)
	if err != nil {
		return false, ""
	}
	defer func() { _ = db.Close() }()

	rec, found, err := identity.NewStore(db.DB).PathRecord(ctx, canonical)
	if err != nil || !found {
		return false, ""
	}
	return rec.Suppressed, rec.SuppressionReason
}

// reconcileAndResolve runs the observe-and-reconcile pass locally, under the
// data-directory lock when it is free, and returns the identity owning dir.
func reconcileAndResolve(ctx context.Context, jobsRoot, dataDir, dir string) (string, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("create data directory %s: %w", dataDir, err)
	}

	// The lock is taken when it is free. If a daemon holds it, this process is
	// a reader: SQLite still serialises the write, and the daemon's own
	// reconcile is idempotent, so the worst case is duplicated work rather
	// than a lost update.
	lock, lockErr := datalock.Acquire(dataDir)
	if lockErr == nil {
		defer func() { _ = lock.Close() }()
	}

	db, err := database.Open(ctx, dataDir)
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	if _, err := database.Migrate(ctx, db); err != nil {
		return "", err
	}

	store := identity.NewStore(db.DB)
	service := identity.NewService(store, jobsRoot)

	known, err := store.Paths(ctx)
	if err != nil {
		return "", err
	}
	knownPaths := make([]string, 0, len(known))
	for _, rec := range known {
		if rec.CanonicalPath != "" {
			knownPaths = append(knownPaths, rec.CanonicalPath)
		}
	}
	scan, err := config.Observe(jobsRoot, knownPaths)
	if err != nil {
		return "", fmt.Errorf("observe jobs: %w", err)
	}
	if _, err := service.Recover(ctx); err != nil {
		return "", err
	}
	if _, err := service.Reconcile(ctx, scan); err != nil {
		return "", fmt.Errorf("reconcile job identity: %w", err)
	}

	canonical, err := identity.Canonical(dir)
	if err != nil {
		return "", err
	}
	rec, found, err := store.PathRecord(ctx, canonical)
	if err != nil {
		return "", err
	}
	if found && rec.Suppressed {
		return "", &suppressedPathError{dir: dir, reason: rec.SuppressionReason}
	}
	if !found || rec.OwnerID.IsZero() {
		return "", fmt.Errorf("%s is not a registered job", dir)
	}
	return rec.OwnerID.String(), nil
}
