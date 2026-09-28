package daemon

import (
	"context"
	"fmt"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
)

// SetPaused implements api.Backend. It suspends or re-arms an integration's
// autonomous triggers.
//
// The durable record is written first: a daemon that restarts between the write
// and the in-memory scheduler change comes back with the pause already in
// force, because bootstrap arms only the integrations that are not paused. The
// reverse order would lose a pause to a crash.
//
// Resuming re-arms the cron trigger from the live manifest. A resume on an
// integration that was never paused still arms it, so the command is a
// statement about the state the operator wants rather than a delta, and
// `sched.Replace` leaves an unchanged expression's next fire time alone.
func (d *Daemon) SetPaused(ctx context.Context, ref string, paused bool) (api.PauseView, error) {
	entry, err := d.resolveRef(ref)
	if err != nil {
		return api.PauseView{}, err
	}

	inst := entry.Instance
	label := entry.Integration.Name
	if label == "" {
		label = entry.Integration.ID
	}
	if inst.ID.IsZero() {
		return api.PauseView{}, fmt.Errorf("integration %q has no registry identity: %w", label, api.ErrNotFound)
	}
	// A retired or deleted identity accepts no work at all, so pausing it
	// would record a control that can never be observed or usefully cleared.
	if !inst.Status.AcceptsWork() {
		return api.PauseView{}, fmt.Errorf("integration %q is %s and cannot be paused or resumed: %w",
			label, inst.Status, api.ErrConflict)
	}

	integrationID := inst.ID.String()

	state, changed, err := d.paused.Set(ctx, integrationID, paused)
	if err != nil {
		return api.PauseView{}, err
	}

	if paused {
		if changed {
			// Unregistering is what makes the next-run column stop reporting a
			// fire time that will not happen. The tick itself re-checks the
			// pause, so this is about honesty in the view, not safety.
			d.sched.Unregister(integrationID)
			d.log.Info("integration_paused", "integration", label, "id", integrationID)
		}
	} else {
		d.armCron(integrationID, entry.Manifest)
		if changed {
			d.log.Info("integration_resumed", "integration", label, "id", integrationID)
		}
	}

	view := api.PauseView{
		IntegrationID: integrationID,
		Name:          label,
		Paused:        state.Paused,
		Changed:       changed,
	}
	if !state.Since.IsZero() {
		at := state.Since
		view.Since = &at
	}
	return view, nil
}

// armCron registers an integration's cron trigger if it declares one. It is the
// per-integration counterpart of the reconciliation in syncSchedules, and it
// exists because a resume must take effect immediately rather than at the next
// reload.
func (d *Daemon) armCron(integrationID string, m *config.Manifest) {
	if m == nil {
		// An invalid integration has no usable trigger to arm. A later reload
		// arms it once the manifest validates, because it is no longer paused.
		return
	}
	spec := m.Cron()
	if spec == "" {
		return
	}
	if err := d.sched.Replace(integrationID, spec, d.cronJob(integrationID, spec)); err != nil {
		d.log.Error("cron_register_failed", err, "integration", integrationID, "cron", spec)
	}
}
