package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/identity"
	"github.com/tkoizumi/otter/internal/release"
)

// ensureIdentityBootstrap decides whether this data directory may run under
// the identity registry.
//
// The SQL migration and the semantic identity bootstrap are deliberately
// separate: the schema can be current while the durable rows are still keyed
// by manifest name. Starting to schedule in that state would recreate
// name-based ownership, which is exactly the bug the registry exists to
// prevent, so a workspace with legacy rows and no registry must be bootstrapped
// first.
//
// An unambiguous workspace is bootstrapped automatically, keeping
// `id = old_name` so nothing has to move. A collision stops startup with the
// operator's next command, because there is only one old state namespace and
// guessing which directory owns it would attach the wrong data.
func (d *Daemon) ensureIdentityBootstrap(ctx context.Context) error {
	store := d.ident.Store()

	done, err := store.BootstrapComplete(ctx)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	legacy, err := d.legacyIdentityIDs(ctx)
	if err != nil {
		return err
	}
	if len(legacy) == 0 {
		// A fresh workspace has nothing to migrate.
		d.log.Info("identity_bootstrap_complete", "mode", "fresh")
		return store.SetBootstrapComplete(ctx, true)
	}

	// Every legacy key must be accounted for. A registry that already holds
	// some of them is not proof the rest were handled: an interrupted or
	// refused bootstrap can leave exactly that state, and treating it as done
	// would strand the remaining keys' state outside the registry.
	pending := make([]string, 0, len(legacy))
	for _, key := range legacy {
		id, err := identity.Parse(key)
		if err != nil {
			pending = append(pending, key)
			continue
		}
		if _, err := store.Instance(ctx, id); errors.Is(err, identity.ErrNotFound) {
			pending = append(pending, key)
		} else if err != nil {
			return err
		}
	}
	if len(pending) == 0 {
		return store.SetBootstrapComplete(ctx, true)
	}
	legacy = pending

	scan, err := config.Observe(d.cfg.IntegrationsDir, nil)
	if err != nil {
		return err
	}
	result, err := d.ident.BootstrapLegacy(ctx, scan, legacy, nil, false)
	if err != nil {
		var conflict *identity.BootstrapConflictError
		if errors.As(err, &conflict) {
			return fmt.Errorf(
				"identity: cannot bootstrap automatically: %v\n"+
					"identity: run `otter identity migrate --dry-run`, then re-run with one `--assign <name>=<path>` per collision",
				err)
		}
		return err
	}

	d.quarantineMismatchedReleases(result.Assigned)

	d.log.Info("identity_bootstrap_complete",
		"mode", "legacy",
		"assigned", len(result.Assigned),
		"reserved", len(result.Reserved))
	return store.SetBootstrapComplete(ctx, true)
}

// quarantineMismatchedReleases disables any active release whose recorded
// source is not the directory that now owns the identity. A release staged
// from a different tree would run the wrong code against the owner's state;
// the staged snapshot is kept, only the activation pointer is removed, so the
// integration refuses to run until it is released again.
func (d *Daemon) quarantineMismatchedReleases(assigned []identity.Assignment) {
	manager := release.Manager{DataDir: d.cfg.DataDir}
	for _, asg := range assigned {
		meta, ok, err := manager.Active(asg.ID.String())
		if err != nil || !ok || meta.Source == "" {
			continue
		}
		source, err := identity.Canonical(meta.Source)
		if err != nil {
			continue
		}
		owner, err := identity.Canonical(asg.Path)
		if err != nil || source == owner {
			continue
		}
		active, err := manager.ActivePath(asg.ID.String())
		if err != nil {
			continue
		}
		if err := os.Remove(active); err != nil {
			continue
		}
		d.log.Warn("release_quarantined",
			"integration", asg.Name,
			"id", asg.ID,
			"digest", meta.Digest,
			"staged_from", source,
			"owner", owner)

		// An attempt already submitted recorded the snapshot it would execute,
		// so disabling the pointer is not enough: it is cancelled rather than
		// run against the identity's newly chosen owner.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cancelled, err := d.runs.CancelPinnedToRelease(ctx, asg.ID.String(), meta.Digest,
			fmt.Sprintf("release %s was quarantined during identity migration: it was staged from a different source than the identity's owner; re-release and resubmit", shortDigest(meta.Digest)))
		cancel()
		if err != nil {
			d.log.Error("release_quarantine_cancel_failed", err, "integration", asg.Name, "id", asg.ID)
			continue
		}
		if cancelled > 0 {
			d.log.Warn("release_quarantine_cancelled_runs", "integration", asg.Name, "id", asg.ID, "count", cancelled)
		}
	}
}

// legacyIdentityIDs inventories the keys durable rows already use, so the
// bootstrap can account for every one of them.
func (d *Daemon) legacyIdentityIDs(ctx context.Context) ([]string, error) {
	return identity.LegacyKeys(ctx, d.db.DB)
}
