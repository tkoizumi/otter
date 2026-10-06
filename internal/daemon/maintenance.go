package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/maintenance"
)

// MaintenanceView renders the daemon's maintenance state for the API.
func (d *Daemon) Maintenance(context.Context) (api.MaintenanceView, error) {
	return d.maintenanceView(), nil
}

// maintenanceView builds the view from the store and the live running count.
//
// The running count comes from capacity rather than from the database because
// it is the count of children this process is actually executing; a run row
// that says "running" after a crash is recovery's problem, not a drain's.
func (d *Daemon) maintenanceView() api.MaintenanceView {
	state := d.maint.State()
	mode := string(state.Mode)
	view := api.MaintenanceView{
		Mode:          mode,
		AcceptingWork: !state.Mode.Gated(),
		Explicit:      state.Explicit,
		Reason:        state.Reason,
	}
	if !state.EnteredAt.IsZero() {
		view.Since = state.EnteredAt.UTC().Format(time.RFC3339)
	}
	// A runtime that claims to be draining with nothing left to drain is in
	// maintenance, however it got there: the mode exists to tell an operator
	// whether it is safe to snapshot, and only the live count can answer that.
	if state.Mode == maintenance.ModeDraining && d.cap.totalRunning() == 0 {
		view.Mode = string(maintenance.ModeMaintenance)
	}
	if state.Mode.Gated() {
		view.Running = d.cap.totalRunning()
	}
	return view
}

// EnterMaintenance closes every path that admits work.
//
// Order matters and is the whole contract: the state is persisted before this
// returns, so a caller that saw success cannot observe a later run being
// admitted. The running count is deliberately not consulted to pick the mode --
// entering always reports draining, and the transition to maintenance happens
// when the last child finishes, so no caller is told "nothing is running" by a
// function that did not check.
func (d *Daemon) EnterMaintenance(ctx context.Context, reason string) (api.MaintenanceView, error) {
	state, changed, err := d.maint.Enter(ctx, reason)
	if err != nil {
		return api.MaintenanceView{}, fmt.Errorf("enter maintenance: %w", err)
	}
	if changed {
		d.log.Warn("maintenance_entered",
			"mode", string(state.Mode),
			"reason", reason,
			"running", d.cap.totalRunning())
	} else {
		d.log.Info("maintenance_enter_repeated", "mode", string(state.Mode))
	}
	return d.maintenanceView(), nil
}

// ExitMaintenance activates the runtime. It is the only way out of maintenance
// and it is deliberately the explicit, audited step rather than an automatic
// consequence of health: a runtime that activated itself would defeat starting
// gated in the first place.
func (d *Daemon) ExitMaintenance(ctx context.Context) (api.MaintenanceView, error) {
	before := d.maint.State()
	state, changed, err := d.maint.Exit(ctx)
	if err != nil {
		return api.MaintenanceView{}, fmt.Errorf("exit maintenance: %w", err)
	}
	if changed {
		d.log.Warn("maintenance_exited",
			"from", string(before.Mode),
			"held_for", time.Since(before.EnteredAt).Round(time.Second).String(),
			"reason", before.Reason)
		// A worker that stopped claiming during maintenance is parked on its
		// poll timer. Wake one so activation takes effect immediately rather
		// than up to a poll interval later -- the operator has just told the
		// runtime to start serving.
		d.notifyWorkers()
	} else {
		d.log.Info("maintenance_exit_repeated", "mode", string(state.Mode))
	}
	return d.maintenanceView(), nil
}

// noteDrainProgress records that the runtime is genuinely holding no work, so
// the state stops claiming a drain is in progress.
//
// Only a draining state is advanced: a runtime in ModeMaintenance is already
// there, and one in ModeStartup must stay gated-and-unactivated until an
// operator says otherwise, not be quietly promoted because nothing happened to
// be running.
func (d *Daemon) noteDrainProgress() {
	state := d.maint.State()
	if state.Mode != maintenance.ModeDraining {
		return
	}
	if d.cap.totalRunning() > 0 {
		return
	}
	if _, err := d.maint.SetMode(context.Background(), maintenance.ModeMaintenance, ""); err != nil {
		d.log.Error("maintenance_drain_transition_failed", err)
		return
	}
	d.log.Info("maintenance_drained", "held_for", time.Since(state.EnteredAt).Round(time.Second).String())
}
