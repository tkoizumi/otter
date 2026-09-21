package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/datalock"
	"github.com/tkoizumi/otter/internal/identity"
)

// This file bridges local, file-writing commands (release, prepare) to the
// durable identity registry.
//
// The daemon owns the registry. When one serves this workspace the CLI asks it,
// reloading first so a directory added since the last scan is registered
// before it is asked about. Without a running daemon the CLI reconciles
// directly while holding the data-directory lock, which is the only way it can
// write identity state without becoming a second writer alongside a live
// daemon.

// mapTargetIdentities rewrites integration targets so their ID is the durable
// identity rather than the manifest label.
func (a *App) mapTargetIdentities(ctx context.Context, integrationsRoot, dataDir string, targets []integrationTarget) ([]integrationTarget, int) {
	client := runningWorkspaceClient(ctx)

	out := make([]integrationTarget, 0, len(targets))
	for _, target := range targets {
		id, err := resolveIdentityForDir(ctx, client, integrationsRoot, dataDir, target.Dir)
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: %v\n", err)
			return nil, 1
		}
		out = append(out, integrationTarget{ID: id, Name: target.ID, Dir: target.Dir})
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
	// for an integration the daemon already knows.
	_, _ = client.Reload(ctx)
	return client
}

// resolveIdentityForDir maps a source directory to the identity that owns it.
func resolveIdentityForDir(ctx context.Context, client *api.Client, integrationsRoot, dataDir, dir string) (string, error) {
	if client != nil {
		view, err := client.ResolveIntegration(ctx, dir)
		if err != nil {
			return "", err
		}
		return view.ID, nil
	}
	return reconcileAndResolve(ctx, integrationsRoot, dataDir, dir)
}

// reconcileAndResolve runs the observe-and-reconcile pass locally, under the
// data-directory lock when it is free, and returns the identity owning dir.
func reconcileAndResolve(ctx context.Context, integrationsRoot, dataDir, dir string) (string, error) {
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
	if err := database.Migrate(ctx, db); err != nil {
		return "", err
	}

	store := identity.NewStore(db.DB)
	service := identity.NewService(store, integrationsRoot)

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
	scan, err := config.Observe(integrationsRoot, knownPaths)
	if err != nil {
		return "", fmt.Errorf("observe integrations: %w", err)
	}
	if _, err := service.Recover(ctx); err != nil {
		return "", err
	}
	if _, err := service.Reconcile(ctx, scan); err != nil {
		return "", fmt.Errorf("reconcile integration identity: %w", err)
	}

	canonical, err := identity.Canonical(dir)
	if err != nil {
		return "", err
	}
	rec, found, err := store.PathRecord(ctx, canonical)
	if err != nil {
		return "", err
	}
	if !found || rec.OwnerID.IsZero() {
		return "", fmt.Errorf("%s is not a registered integration", dir)
	}
	return rec.OwnerID.String(), nil
}
