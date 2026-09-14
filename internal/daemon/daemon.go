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
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/otter-runtime/otter/internal/api"
	"github.com/otter-runtime/otter/internal/config"
	"github.com/otter-runtime/otter/internal/database"
	"github.com/otter-runtime/otter/internal/executor"
	"github.com/otter-runtime/otter/internal/logging"
	"github.com/otter-runtime/otter/internal/queue"
	"github.com/otter-runtime/otter/internal/runs"
	"github.com/otter-runtime/otter/internal/scheduler"
	"github.com/otter-runtime/otter/internal/secrets"
	"github.com/otter-runtime/otter/internal/state"
	"github.com/otter-runtime/otter/sdk"
)

// Options configures New.
type Options struct {
	Config config.DaemonConfig
	Logger *logging.Logger

	// Secrets defaults to the environment provider.
	Secrets secrets.Provider

	// Version is reported through /health.
	Version string
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
	owner   *dataLock
	log     *logging.Logger
	version string

	db    *database.DB
	runs  *runs.Store
	logs  *runs.LogStore
	queue *queue.Queue
	state *state.Store

	sched   *scheduler.Scheduler
	exec    *executor.Executor
	secrets secrets.Provider

	reg *registry
	cap *capacity

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

	startedAt    time.Time
	shutdownOnce sync.Once
	shutdownErr  error
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
	owner, err := acquireDataLock(cfg.DataDir)
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

	d := &Daemon{
		cfg:       cfg,
		owner:     owner,
		log:       opts.Logger,
		version:   opts.Version,
		db:        db,
		runs:      runs.NewStore(db.DB),
		logs:      runs.NewLogStore(db.DB),
		queue:     queue.New(db.DB),
		state:     state.NewStore(db.DB),
		sched:     scheduler.New(opts.Logger),
		secrets:   provider,
		reg:       newRegistry(),
		cap:       newCapacity(cfg.Workers),
		runTokens: newRunTokenRegistry(),
		stopCh:    make(chan struct{}),
		wakeCh:    make(chan struct{}, 1),
		runCtl:    map[string]*runControl{},
		startedAt: time.Now().UTC(),
	}

	sdkPath, err := d.resolveSDKPath()
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	d.exec = executor.New(opts.Logger, sdkPath)
	d.log.Info("sdk_ready", "path", sdkPath)

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
		Listen:   cfg.Listen,
		APIToken: cfg.APIToken,
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

// discover loads manifests, generates webhook tokens and registers cron
// triggers. Invalid integrations are reported, never fatal.
func (d *Daemon) discover(ctx context.Context) error {
	items, err := config.Discover(d.cfg.IntegrationsDir)
	if err != nil {
		return fmt.Errorf("discover integrations: %w", err)
	}

	tokens := map[string]string{}
	for _, it := range items {
		if !it.Valid || it.Manifest == nil || !it.Manifest.WebhookEnabled() {
			continue
		}
		token, err := d.ensureWebhookToken(ctx, it.ID)
		if err != nil {
			d.log.Error("webhook_token_failed", err, "integration", it.ID)
			continue
		}
		tokens[it.ID] = token
	}

	d.reg.load(items, tokens)
	d.cap.setLimits(d.reg.limits())

	valid, invalid := d.reg.counts()
	d.log.Info("integrations_discovered",
		"root", d.cfg.IntegrationsDir,
		"total", len(items),
		"valid", valid,
		"invalid", invalid)

	for _, it := range items {
		if !it.Valid {
			d.log.Warn("integration_invalid",
				"integration", it.ID,
				"path", it.ManifestPath,
				"error", it.Error)
			continue
		}
		m := it.Manifest
		d.log.Debug("integration_registered",
			"integration", it.ID,
			"path", it.Dir,
			"cron", m.Cron(),
			"webhook", m.WebhookEnabled(),
			"timeout", m.Timeout.String(),
			"concurrency", m.Concurrency,
			"max_attempts", m.MaxAttempts())
	}

	for _, it := range items {
		if !it.Valid || it.Manifest == nil {
			continue
		}
		spec := it.Manifest.Cron()
		if spec == "" {
			continue
		}
		id := it.ID
		if err := d.sched.Register(id, spec, func() { d.cronTick(id, spec) }); err != nil {
			d.log.Error("cron_register_failed", err, "integration", id, "cron", spec)
		}
	}

	return nil
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
