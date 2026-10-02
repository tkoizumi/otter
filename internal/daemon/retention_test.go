package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/logging"
	"github.com/tkoizumi/otter/internal/runs"
)

// newRetentionDaemon builds a daemon with just the stores the retention sweeps
// touch. It deliberately avoids New, which recovers runs, resolves the Python
// SDK and needs a release on disk: retention is about the database, and testing
// it should not depend on python3.
func newRetentionDaemon(t *testing.T, tweak func(*config.DaemonConfig)) *Daemon {
	t.Helper()

	db, err := database.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := database.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := config.DefaultDaemonConfig("test")
	if tweak != nil {
		tweak(&cfg)
	}
	return &Daemon{
		cfg:        cfg,
		db:         db,
		runs:       runs.NewStore(db.DB),
		logs:       runs.NewLogStore(db.DB),
		inspection: inspection.NewStore(db.DB, inspection.NewRedactor(nil, nil, nil), inspection.DefaultLimits()),
		log:        testLogger(),
	}
}

// seedCapturedRun writes a terminal run, one log line, and a capture summary,
// all dated createdAt.
func seedCapturedRun(t *testing.T, d *Daemon, id string, createdAt time.Time) {
	t.Helper()
	ctx := context.Background()

	if err := d.runs.Create(ctx, &runs.Run{
		ID:          id,
		JobID:       "shopify",
		TriggerType: runs.TriggerManual,
		Status:      runs.StatusSucceeded,
		Attempt:     1,
		CreatedAt:   createdAt,
	}); err != nil {
		t.Fatalf("create run %s: %v", id, err)
	}
	if err := d.logs.Append(ctx, runs.LogEntry{
		RunID: id, Stream: runs.StreamStdout, Message: "output", Timestamp: createdAt,
	}); err != nil {
		t.Fatalf("append log for %s: %v", id, err)
	}
	if err := d.inspection.Begin(ctx, inspection.CaptureSettings{
		RunID: id, JobID: "shopify", Policy: inspection.PolicyFull,
	}); err != nil {
		t.Fatalf("begin capture for %s: %v", id, err)
	}
}

func daemonLogCount(t *testing.T, d *Daemon, runID string) int {
	t.Helper()
	entries, err := d.logs.List(context.Background(), runID, 0, 100)
	if err != nil {
		t.Fatalf("list logs for %s: %v", runID, err)
	}
	return len(entries)
}

func daemonRunExists(t *testing.T, d *Daemon, id string) bool {
	t.Helper()
	_, err := d.runs.Get(context.Background(), id)
	if errors.Is(err, runs.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("get run %s: %v", id, err)
	}
	return true
}

func daemonCaptureExists(t *testing.T, d *Daemon, runID string) bool {
	t.Helper()
	_, err := d.inspection.Capture(context.Background(), runID)
	if errors.Is(err, inspection.ErrNotConfigured) {
		return false
	}
	if err != nil {
		t.Fatalf("capture for %s: %v", runID, err)
	}
	return true
}

// The zero default is the promise that an unconfigured daemon deletes nothing.
// This is the regression against accidental data loss.
func TestRetentionDisabledDeletesNothing(t *testing.T) {
	d := newRetentionDaemon(t, nil)

	if d.cfg.LogRetention != 0 || d.cfg.RunRetention != 0 {
		t.Fatalf("defaults are not zero: log=%s run=%s", d.cfg.LogRetention, d.cfg.RunRetention)
	}

	seedCapturedRun(t, d, "old-run", time.Now().UTC().Add(-365*24*time.Hour))
	d.expireRetention(context.Background())

	if !daemonRunExists(t, d, "old-run") {
		t.Error("run deleted with retention disabled")
	}
	if n := daemonLogCount(t, d, "old-run"); n != 1 {
		t.Errorf("run kept %d log rows, want 1", n)
	}
	if !daemonCaptureExists(t, d, "old-run") {
		t.Error("capture deleted with retention disabled")
	}
}

func TestExpireRunsDeletesRunLogsAndCapture(t *testing.T) {
	d := newRetentionDaemon(t, func(cfg *config.DaemonConfig) {
		cfg.RunRetention = 24 * time.Hour
	})
	ctx := context.Background()
	now := time.Now().UTC()

	seedCapturedRun(t, d, "expired", now.Add(-72*time.Hour))
	seedCapturedRun(t, d, "recent", now.Add(-time.Hour))

	d.expireRetention(ctx)

	if daemonRunExists(t, d, "expired") {
		t.Error("expired run survived")
	}
	if n := daemonLogCount(t, d, "expired"); n != 0 {
		t.Errorf("expired run left %d log rows, want 0", n)
	}
	if daemonCaptureExists(t, d, "expired") {
		t.Error("expired run left its capture behind")
	}
	if !daemonRunExists(t, d, "recent") {
		t.Error("recent run was deleted")
	}
	if n := daemonLogCount(t, d, "recent"); n != 1 {
		t.Errorf("recent run kept %d log rows, want 1", n)
	}
}

// Log retention prunes output but keeps the run row, so `otter runs` history
// survives a short log window.
func TestExpireLogsKeepsRunRows(t *testing.T) {
	d := newRetentionDaemon(t, func(cfg *config.DaemonConfig) {
		cfg.LogRetention = 24 * time.Hour
	})
	now := time.Now().UTC()

	seedCapturedRun(t, d, "expired", now.Add(-72*time.Hour))
	seedCapturedRun(t, d, "recent", now.Add(-time.Hour))

	d.expireRetention(context.Background())

	if n := daemonLogCount(t, d, "expired"); n != 0 {
		t.Errorf("expired logs kept %d rows, want 0", n)
	}
	if !daemonRunExists(t, d, "expired") {
		t.Error("log retention deleted the run row it must keep")
	}
	if n := daemonLogCount(t, d, "recent"); n != 1 {
		t.Errorf("recent logs kept %d rows, want 1", n)
	}
}

// Each sweep reports what it removed, with the cutoff, so an operator can see
// the window is live rather than guess.
func TestRetentionLogsOneLinePerSweep(t *testing.T) {
	d := newRetentionDaemon(t, func(cfg *config.DaemonConfig) {
		cfg.LogRetention = 48 * time.Hour
		cfg.RunRetention = 240 * time.Hour
	})
	var buf bytes.Buffer
	d.log = logging.New(&buf, logging.FormatJSON, logging.LevelInfo)

	now := time.Now().UTC()
	// Old enough for the run window: the run and its logs both go.
	seedCapturedRun(t, d, "run-expired", now.Add(-300*time.Hour))
	// Inside the run window, past the log window: the logs go, the run stays.
	seedCapturedRun(t, d, "logs-expired", now.Add(-72*time.Hour))

	d.expireRetention(context.Background())

	events := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("parse log line %q: %v", line, err)
		}
		name, _ := record["event"].(string)
		events[name] = record
	}

	runsEvent, ok := events["runs_retained"]
	if !ok {
		t.Fatalf("no runs_retained line in:\n%s", buf.String())
	}
	if n, _ := runsEvent["runs"].(float64); n != 1 {
		t.Errorf("runs_retained runs = %v, want 1", runsEvent["runs"])
	}
	assertCutoff(t, "runs_retained", runsEvent)

	logsEvent, ok := events["logs_retained"]
	if !ok {
		t.Fatalf("no logs_retained line in:\n%s", buf.String())
	}
	if n, _ := logsEvent["logs"].(float64); n != 1 {
		t.Errorf("logs_retained logs = %v, want 1", logsEvent["logs"])
	}
	assertCutoff(t, "logs_retained", logsEvent)
}

func assertCutoff(t *testing.T, event string, record map[string]any) {
	t.Helper()
	value, ok := record["cutoff"].(string)
	if !ok {
		t.Fatalf("%s cutoff = %v, want a string", event, record["cutoff"])
	}
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		t.Errorf("%s cutoff = %q, want an RFC3339 time: %v", event, value, err)
	}
}
