// Package daemon wires the Otter runtime together.
//
// It owns discovery, the durable queue, the worker pool, concurrency limits,
// retries, crash recovery, triggers, the HTTP API and graceful shutdown. The
// packages it composes stay independent: the daemon is the only place that
// knows how they fit together.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/datalock"
	"github.com/tkoizumi/otter/internal/executor"
	"github.com/tkoizumi/otter/internal/identity"
	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/logging"
	"github.com/tkoizumi/otter/internal/notify"
	"github.com/tkoizumi/otter/internal/queue"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/scheduler"
	"github.com/tkoizumi/otter/internal/secrets"
	"github.com/tkoizumi/otter/internal/state"
	"github.com/tkoizumi/otter/sdk"
)

// Options configures New.
type Options struct {
	Config config.DaemonConfig
	Logger *logging.Logger

	// Secrets defaults to the environment provider.
	Secrets secrets.Provider

	// Version is reported through /health.
	Version string

	// OnReady is called once the API listener is bound, with the resolved
	// address. `otter serve` uses it to record where this daemon can be
	// reached, so a developer's next command is `otter run`, not a
	// copy-pasted --api URL.
	OnReady func(addr string)
}

// cancelReason records why a running process was signalled. It decides the
// status a run ends up with, and therefore whether it is retried.
type cancelReason int

const (
	reasonNone cancelReason = iota
	// reasonUser is an explicit POST /v1/runs/{id}/cancel. Cancelled runs are
	// never retried.
	reasonUser
	// reasonShutdown is the daemon going down. These runs are marked failed
	// and follow the manifest retry policy, so a deploy does not silently
	// drop work.
	reasonShutdown
)

// runControl lets the API cancel a specific running process and lets the
// worker learn why it was cancelled.
type runControl struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	reason cancelReason
}

func (c *runControl) setReason(r cancelReason) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reason == reasonNone {
		c.reason = r
	}
}

func (c *runControl) getReason() cancelReason {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reason
}

// Daemon is the Otter runtime.
type Daemon struct {
	cfg     config.DaemonConfig
	owner   *datalock.Lock
	log     *logging.Logger
	version string

	db    *database.DB
	runs  *runs.Store
	logs  *runs.LogStore
	queue *queue.Queue
	state *state.Store

	// inspection holds bounded HTTP capture: per-run summaries and request
	// records. It is diagnostic, so nothing recorded through it may change what
	// a run does.
	inspection *inspection.Store

	sched    *scheduler.Scheduler
	exec     *executor.Executor
	secrets  secrets.Provider
	notifier *notify.Notifier

	reg *registry
	cap *capacity

	// ident is the durable identity authority: it owns the registry, the
	// source markers and the reconciliation that assigns identities.
	ident *identity.Service

	runTokens *runTokenRegistry

	apiServer *api.Server
	apiCancel context.CancelFunc

	baseCtx    context.Context
	baseCancel context.CancelFunc

	stopCh   chan struct{}
	wakeCh   chan struct{}
	wg       sync.WaitGroup
	draining atomic.Bool

	runCtlMu sync.Mutex
	runCtl   map[string]*runControl

	// reloadMu serialises reloads. Two concurrent passes would race on cron
	// registration and each report the other's work as a change.
	reloadMu sync.Mutex

	startedAt    time.Time
	shutdownOnce sync.Once
	shutdownErr  error

	// hostname identifies this machine in failure notifications. With more
	// than one daemon syncing the same destination -- a laptop and a server,
	// say -- an alert without it does not say which one is broken.
	hostname string
}

// Compile-time proof that the daemon satisfies the API's backend contract.
var _ api.Backend = (*Daemon)(nil)

// New opens the database, discovers integrations, recovers interrupted runs
// and prepares (but does not start) the runtime.
func New(ctx context.Context, opts Options) (*Daemon, error) {
	if opts.Logger == nil {
		return nil, errors.New("daemon: a logger is required")
	}

	cfg := opts.Config
	provider := opts.Secrets
	if provider == nil {
		provider = secrets.NewEnvProvider()
	}
	owner, err := datalock.Acquire(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	keepOwner := false
	defer func() {
		if !keepOwner {
			_ = owner.Close()
		}
	}()

	db, err := database.Open(ctx, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if err := database.Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	identStore := identity.NewStore(db.DB)
	d := &Daemon{
		cfg:        cfg,
		owner:      owner,
		log:        opts.Logger,
		version:    opts.Version,
		db:         db,
		runs:       runs.NewStore(db.DB),
		logs:       runs.NewLogStore(db.DB),
		queue:      queue.New(db.DB),
		state:      state.NewStore(db.DB),
		inspection: inspection.NewStore(db.DB, inspection.DefaultRedactor(), inspection.DefaultLimits()),
		sched:      scheduler.New(opts.Logger),
		secrets:    provider,
		reg:        newRegistry(),
		cap:        newCapacity(cfg.Workers),
		ident:      identity.NewService(identStore, cfg.IntegrationsDir),
		runTokens:  newRunTokenRegistry(),
		stopCh:     make(chan struct{}),
		wakeCh:     make(chan struct{}, 1),
		runCtl:     map[string]*runControl{},
		startedAt:  time.Now().UTC(),
	}

	if err := d.ensureIdentityBootstrap(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	sdkPath, err := d.resolveSDKPath()
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	d.exec = executor.New(opts.Logger, sdkPath)
	d.log.Info("sdk_ready", "path", sdkPath)

	if host, err := os.Hostname(); err == nil {
		d.hostname = host
	}
	d.notifier = notify.New(cfg.Notify, opts.Logger)
	if d.notifier.Enabled() {
		// The URL is redacted: providers such as Slack embed a secret in the
		// path, and this line goes to the daemon log.
		d.log.Info("failure_notification_enabled",
			"endpoint", d.notifier.RedactedURL(),
			"on", strings.Join(cfg.Notify.On, ","))
	}

	if err := d.discover(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	// Recover before any trigger can fire so an interrupted run is never
	// executed twice.
	if err := d.recoverRuns(ctx); err != nil {
		d.log.Error("recovery_failed", err)
	}
	if err := d.reconcileQueue(ctx); err != nil {
		d.log.Error("queue_reconcile_failed", err)
	}

	d.apiServer = api.NewServer(api.ServerConfig{
		Listen:        cfg.Listen,
		APIToken:      cfg.APIToken,
		CaptureLimits: inspection.DefaultLimits(),
		OnReady:       opts.OnReady,
	}, d, opts.Logger)

	keepOwner = true
	return d, nil
}

// resolveSDKPath decides which directory is prepended to the child
// PYTHONPATH: an explicit --sdk-path, or the SDK embedded in this binary
// extracted into the data directory. The result is always absolute because
// children run with their working directory set to the integration directory.
func (d *Daemon) resolveSDKPath() (string, error) {
	if d.cfg.SDKPath != "" {
		abs, err := filepath.Abs(d.cfg.SDKPath)
		if err != nil {
			return "", fmt.Errorf("resolve --sdk-path %s: %w", d.cfg.SDKPath, err)
		}
		return abs, nil
	}
	return sdk.Extract(d.cfg.DataDir)
}

// discover reconciles the registry against the source tree, then loads the
// resulting instances into the runtime registry. Invalid integrations are
// reported, never fatal.
func (d *Daemon) discover(ctx context.Context) error {
	set, err := d.loadIntegrations(ctx)
	if err != nil {
		return err
	}

	d.reg.load(set)
	d.cap.setLimits(d.reg.limits())

	valid, invalid := d.reg.counts()
	d.log.Info("integrations_discovered",
		"root", d.cfg.IntegrationsDir,
		"total", len(set.items),
		"valid", valid,
		"invalid", invalid)

	d.logIntegrations(set.items)
	d.syncSchedules(set.items)

	return nil
}

// loadIntegrations observes the integrations directory, reconciles the durable
// identity registry against it, and joins the two into the runtime view.
//
// Identity comes from the registry, never from the manifest: a manifest names
// a label, and a directory is a location. That is what makes a copied
// directory a fresh instance, and a deleted-and-recreated one fresh too.
//
// It touches no in-memory shared state. That is what lets a reload do all of
// its slow work -- walking the tree, parsing manifests, writing markers --
// before taking any lock, so a large integrations directory is invisible to
// everything already running.
func (d *Daemon) loadIntegrations(ctx context.Context) (discovered, error) {
	known := d.knownSourcePaths(ctx)
	scan, err := config.Observe(d.cfg.IntegrationsDir, known)
	if err != nil {
		return discovered{}, fmt.Errorf("observe integrations: %w", err)
	}

	// Recovery runs before reconciliation so a crash midway through an earlier
	// mutation converges on one identity instead of minting a second one.
	if _, err := d.ident.Recover(ctx); err != nil {
		d.log.Error("identity_recovery_failed", err)
	}
	if _, err := d.ident.Reconcile(ctx, scan); err != nil {
		return discovered{}, fmt.Errorf("reconcile integration identity: %w", err)
	}

	instances, err := d.ident.Store().Instances(ctx)
	if err != nil {
		return discovered{}, err
	}

	set := discovered{
		tokens:    map[string]string{},
		instances: map[string]identity.Instance{},
	}
	for _, inst := range instances {
		if inst.Status != identity.StatusActive {
			continue
		}
		it := &config.Integration{
			ID:   inst.ID.String(),
			Name: inst.Name,
			Dir:  inst.CanonicalPath,
		}
		if inst.CanonicalPath == "" {
			it.Error = "instance has no source path"
		} else {
			it.ManifestPath = filepath.Join(inst.CanonicalPath, config.ManifestFileName)
			m, err := config.LoadAndValidate(it.ManifestPath)
			if err != nil {
				it.Error = err.Error()
			} else {
				it.Manifest = m
				it.Valid = true
			}
		}
		set.items = append(set.items, it)
		set.instances[it.ID] = inst
	}

	// A directory the registry does not own is surfaced anyway when it is
	// present but unusable, so an operator still sees a broken manifest or an
	// untrustworthy marker in `otter integrations --all`. These entries carry
	// no identity: they cannot run, and they never claim state.
	owned := map[string]bool{}
	for _, inst := range instances {
		if inst.Status == identity.StatusActive || inst.Status == identity.StatusDeleting {
			owned[inst.CanonicalPath] = true
		}
	}
	for _, obs := range scan.Observations {
		if !obs.Exists || owned[obs.Path] {
			continue
		}
		if rec, found, err := d.ident.Store().PathRecord(ctx, obs.Path); err == nil && found && rec.Suppressed {
			continue
		}
		label := obs.Name
		if label == "" {
			label = filepath.Base(obs.Path)
		}
		set.items = append(set.items, &config.Integration{
			ID:           label,
			Name:         label,
			Dir:          obs.Path,
			ManifestPath: filepath.Join(obs.Path, config.ManifestFileName),
			Valid:        false,
			Error:        observationError(obs),
		})
	}
	sort.Slice(set.items, func(i, j int) bool { return set.items[i].ID < set.items[j].ID })

	for _, it := range set.items {
		if !it.Valid || it.Manifest == nil || !it.Manifest.WebhookEnabled() {
			continue
		}
		token, err := d.ensureWebhookToken(ctx, it.ID)
		if err != nil {
			d.log.Error("webhook_token_failed", err, "integration", it.ID)
			continue
		}
		set.tokens[it.ID] = token
	}

	return set, nil
}

// knownSourcePaths returns every canonical source path the registry knows, so
// the scan describes them explicitly instead of inferring deletion from their
// absence in the manifest walk.
func (d *Daemon) knownSourcePaths(ctx context.Context) []string {
	paths, err := d.ident.Store().Paths(ctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p.CanonicalPath != "" {
			out = append(out, p.CanonicalPath)
		}
	}
	return out
}

// logIntegrations reports what discovery found, one integration at a time.
func (d *Daemon) logIntegrations(items []*config.Integration) {
	for _, it := range items {
		if !it.Valid {
			d.log.Warn("integration_invalid",
				"integration", it.Name,
				"id", it.ID,
				"path", it.ManifestPath,
				"error", it.Error)
			continue
		}
		m := it.Manifest
		d.log.Debug("integration_registered",
			"integration", it.Name,
			"id", it.ID,
			"path", it.Dir,
			"cron", m.Cron(),
			"webhook", m.WebhookEnabled(),
			"timeout", m.Timeout.String(),
			"concurrency", m.Concurrency,
			"max_attempts", m.MaxAttempts())
	}
}

// syncSchedules makes the cron runner match items: it reconciles the trigger of
// every integration that declares one and drops the triggers of integrations
// that no longer do, or are no longer valid.
//
// Replace leaves an unchanged expression's entry exactly as it is, so an
// integration that did not change keeps its next fire time across a reload.
func (d *Daemon) syncSchedules(items []*config.Integration) {
	want := map[string]string{}
	for _, it := range items {
		if !it.Valid || it.Manifest == nil {
			continue
		}
		if spec := it.Manifest.Cron(); spec != "" {
			want[it.ID] = spec
		}
	}

	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		spec := want[id]
		if err := d.sched.Replace(id, spec, d.cronJob(id, spec)); err != nil {
			d.log.Error("cron_register_failed", err, "integration", id, "cron", spec)
		}
	}

	stale := d.sched.IDs()
	sort.Strings(stale)
	for _, id := range stale {
		if _, ok := want[id]; ok {
			continue
		}
		d.sched.Unregister(id)
		d.log.Info("cron_unregistered", "integration", id)
	}
}

// cronJob returns the job registered for an integration's cron trigger. The
// job does no work itself, so a slow integration never blocks the cron runner.
func (d *Daemon) cronJob(integrationID, spec string) func() {
	return func() { d.cronTick(integrationID, spec) }
}

// Reload re-reads the integrations directory and applies what it finds to the
// running daemon.
//
// It is deliberately not a restart. The process, the API listener, the worker
// pool and every executing child are left alone: only the registry, the
// per-integration concurrency limits and the cron triggers are replaced. That
// is what lets an integration be added without interrupting the ones already
// running.
//
// Discovery and token resolution run before any shared state is touched, so
// the swap is brief, and a failed walk leaves the previous registry exactly as
// it was rather than half-applied.
func (d *Daemon) Reload(ctx context.Context) (api.ReloadResult, error) {
	// One reload at a time: two concurrent passes would race on cron
	// registration and each report the other's work as a change.
	if !d.reloadMu.TryLock() {
		return api.ReloadResult{}, fmt.Errorf("a reload is already in progress: %w", api.ErrConflict)
	}
	defer d.reloadMu.Unlock()

	set, err := d.loadIntegrations(ctx)
	if err != nil {
		return api.ReloadResult{}, err
	}

	before := d.reg.snapshot()
	result := diffRegistry(before, set.items)

	// Work belonging to an integration that is about to disappear can never
	// run -- a worker refuses to claim it -- so it is ended here, with a reason
	// that names the cause, before the swap that makes it unrunnable. Doing it
	// first also closes the gap in which a worker could claim one and fail it
	// with a less useful message.
	cancelled, err := d.cancelRunsOfRemoved(ctx, removedIdentityIDs(before, set.items))
	if err != nil {
		d.log.Error("reload_cancel_removed_failed", err)
	}
	result.RunsCancelled = cancelled

	// Discovery already succeeded, so nothing below can fail: the new set is
	// applied whole.
	d.reg.load(set)
	d.cap.setLimits(d.reg.limits())
	d.syncSchedules(set.items)

	valid, invalid := d.reg.counts()
	d.log.Info("integrations_reloaded",
		"root", d.cfg.IntegrationsDir,
		"total", len(set.items),
		"valid", valid,
		"invalid", invalid,
		"added", len(result.Added),
		"removed", len(result.Removed),
		"changed", len(result.Changed),
		"runs_cancelled", cancelled)

	// An invalid manifest is worth naming on a reload for the same reason it is
	// at startup: it is the difference between "not there" and "there but
	// broken", and only one of those is fixed by editing the file.
	for _, it := range set.items {
		if !it.Valid {
			d.log.Warn("integration_invalid",
				"integration", it.Name,
				"id", it.ID,
				"path", it.ManifestPath,
				"error", it.Error)
		}
	}

	return result, nil
}

// diffRegistry reports what a reload changed, relative to what the daemon knew
// before it. Changed means the manifest itself differs, which is what an
// operator who edited otter.yaml expects to see reported.
//
// Entries are reported by label: the operator edited a manifest with a name,
// and an opaque identity would make the report unreadable. The identity is
// still the key; the label is only what is printed.
func diffRegistry(before map[string]*registered, items []*config.Integration) api.ReloadResult {
	result := api.ReloadResult{}
	present := make(map[string]bool, len(items))

	for _, it := range items {
		present[it.ID] = true
		label := displayLabel(it)

		old, existed := before[it.ID]
		switch {
		case !existed:
			result.Added = append(result.Added, label)
		case old.Integration.Valid != it.Valid ||
			old.Integration.Error != it.Error ||
			!reflect.DeepEqual(old.Manifest, it.Manifest):
			result.Changed = append(result.Changed, label)
		}

		if !it.Valid {
			result.Invalid = append(result.Invalid, label)
		}
	}

	for id, entry := range before {
		if !present[id] {
			result.Removed = append(result.Removed, displayLabel(entry.Integration))
		}
	}

	sort.Strings(result.Added)
	sort.Strings(result.Removed)
	sort.Strings(result.Changed)
	sort.Strings(result.Invalid)

	result.Total = len(items)
	result.Valid = result.Total - len(result.Invalid)
	return result
}

// displayLabel renders an integration for a human-facing report.
func displayLabel(it *config.Integration) string {
	if it == nil {
		return ""
	}
	if it.Name != "" {
		return it.Name
	}
	return it.ID
}

// removedIdentityIDs returns the durable identities that disappeared, which is
// what cancelling their queued runs needs. The display result reports labels;
// run records are keyed by identity, so the two must not be confused.
func removedIdentityIDs(before map[string]*registered, items []*config.Integration) []string {
	present := make(map[string]bool, len(items))
	for _, it := range items {
		present[it.ID] = true
	}
	var out []string
	for id := range before {
		if !present[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// cancelRunsOfRemoved ends the queued and retrying runs of integrations that a
// reload removed.
//
// Cancelled, rather than failed, is the honest status: an operator removed the
// integration on purpose, and a failure alert for work they deliberately
// discarded is noise. Runs that are already executing are left alone -- they no
// longer depend on the registry, and interrupting them is exactly what reload
// exists to avoid.
func (d *Daemon) cancelRunsOfRemoved(ctx context.Context, removed []string) (int, error) {
	if len(removed) == 0 {
		return 0, nil
	}

	gone := make(map[string]bool, len(removed))
	for _, id := range removed {
		gone[id] = true
	}

	cancelled := 0
	for _, status := range []runs.Status{runs.StatusQueued, runs.StatusRetrying} {
		list, err := d.runs.ListByStatus(ctx, status, 10000)
		if err != nil {
			return cancelled, err
		}

		for _, run := range list {
			if !gone[run.IntegrationID] {
				continue
			}
			message := fmt.Sprintf("integration %s was removed from the integrations directory", run.IntegrationID)

			if _, err := d.queue.Remove(ctx, run.ID); err != nil {
				return cancelled, err
			}
			if err := d.runs.Finish(ctx, run.ID, runs.Finish{
				Status:     runs.StatusCancelled,
				Error:      message,
				FinishedAt: time.Now().UTC(),
			}); err != nil {
				return cancelled, err
			}

			d.appendOtterLog(run.ID, "run cancelled: "+message)
			d.log.Warn("run_cancelled",
				"integration", run.IntegrationID,
				"run_id", run.ID,
				"reason", "integration_removed")
			cancelled++
		}
	}

	return cancelled, nil
}

// cronTick enqueues a run for a cron trigger. The job does no work itself, so
// a slow integration never blocks the cron runner.
func (d *Daemon) cronTick(integrationID, spec string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	scheduled := time.Now().UTC()
	d.log.Info("cron_fired", "integration", integrationID, "cron", spec)

	if _, err := d.SubmitRun(ctx, integrationID, api.TriggerPayload{
		Type:        api.TriggerCron,
		ScheduledAt: &scheduled,
	}); err != nil {
		d.log.Error("cron_submit_failed", err, "integration", integrationID)
	}
}

// Run starts the API, the scheduler and the workers, then blocks until ctx is
// cancelled (SIGINT/SIGTERM) or the API fails, and shuts down gracefully.
func (d *Daemon) Run(ctx context.Context) error {
	d.baseCtx, d.baseCancel = context.WithCancel(context.Background())
	defer d.baseCancel()

	apiCtx, apiCancel := context.WithCancel(context.Background())
	d.apiCancel = apiCancel

	apiErr := make(chan error, 1)
	go func() { apiErr <- d.apiServer.Run(apiCtx) }()

	d.sched.Start()
	d.startWorkers()
	go d.runCaptureRetention(apiCtx)

	d.log.Info("daemon_started",
		"version", d.version,
		"workers", d.cfg.Workers,
		"cron_triggers", d.sched.Count(),
		"data", d.cfg.DataDir,
		"database", d.db.Path)

	select {
	case <-ctx.Done():
		d.log.Info("shutdown_requested", "reason", ctx.Err().Error())
	case err := <-apiErr:
		if err != nil {
			d.log.Error("api_failed", err)
			_ = d.Shutdown(context.Background())
			return err
		}
	}

	return d.Shutdown(context.Background())
}

// Shutdown stops the runtime in a fixed order: stop accepting work, stop the
// scheduler, stop claiming, let running integrations finish, terminate what is
// left, then close SQLite.
func (d *Daemon) Shutdown(ctx context.Context) error {
	d.shutdownOnce.Do(func() {
		d.shutdownErr = d.shutdown(ctx)
	})
	return d.shutdownErr
}

func (d *Daemon) shutdown(ctx context.Context) error {
	d.draining.Store(true)
	d.cap.close()

	depth, _ := d.queue.Depth(context.Background())
	d.log.Info("shutdown_started",
		"running", d.cap.totalRunning(),
		"queued", depth,
		"grace", d.cfg.ShutdownGrace.String())

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := d.sched.Stop(stopCtx); err != nil {
		d.log.Warn("scheduler_stop_slow", "error", err.Error())
	}
	cancel()

	// Workers stop claiming but finish whatever they already claimed.
	close(d.stopCh)

	if d.cfg.ShutdownGrace > 0 {
		if !d.waitForRunning(d.cfg.ShutdownGrace) {
			d.log.Warn("shutdown_grace_expired",
				"running", d.cap.totalRunning(),
				"grace", d.cfg.ShutdownGrace.String())
		}
	}

	// Terminate the stragglers; their runs are failed and retried.
	d.cancelAllRuns(reasonShutdown)
	d.waitForWorkers(30 * time.Second)
	// Children may still be checkpointing during the grace period. Keep the
	// state/log API reachable until every worker has finished its child.
	if d.apiCancel != nil {
		d.apiCancel()
	}

	if d.baseCancel != nil {
		d.baseCancel()
	}

	var err error
	if cerr := d.db.Close(); cerr != nil {
		err = cerr
		d.log.Error("database_close_failed", cerr)
	}
	if d.owner != nil {
		_ = d.owner.Close()
	}

	d.log.Info("shutdown_complete")
	return err
}

// waitForRunning blocks until no integration is executing, or the timeout
// expires.
func (d *Daemon) waitForRunning(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if d.cap.totalRunning() == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (d *Daemon) waitForWorkers(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		d.log.Warn("shutdown_workers_timeout", "running", d.cap.totalRunning())
	}
}

func (d *Daemon) cancelAllRuns(reason cancelReason) {
	d.runCtlMu.Lock()
	controls := make([]*runControl, 0, len(d.runCtl))
	for _, ctl := range d.runCtl {
		controls = append(controls, ctl)
	}
	d.runCtlMu.Unlock()

	for _, ctl := range controls {
		ctl.setReason(reason)
		ctl.cancel()
	}
}

// notifyWorkers wakes an idle worker without blocking.
func (d *Daemon) notifyWorkers() {
	select {
	case d.wakeCh <- struct{}{}:
	default:
	}
}

// Version implements api.Backend.
func (d *Daemon) Version() string { return d.version }

// StartedAt implements api.Backend.
func (d *Daemon) StartedAt() time.Time { return d.startedAt }
