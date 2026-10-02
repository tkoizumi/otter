package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/runs"
)

// cronTicker is a cron-only job. The descriptor form keeps the schedule
// far enough away that a test never races a real fire.
const cronTicker = `
version: 1
name: ticker
entrypoint: main.py
timeout: 30
trigger:
  cron: "@every 6h"
retry:
  attempts: 0
`

// Pausing takes the trigger out of the scheduler, which is what makes the
// schedule view stop claiming a fire time that will not happen. Resuming puts
// it back, because a resume that waited for the next reload would look broken.
func TestPauseUnarmsCronAndResumeRearms(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "ticker")

	if _, ok := d.sched.Spec(id); !ok {
		t.Fatal("a cron job should start armed")
	}

	view, err := d.SetPaused(ctx, "ticker", true)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if !view.Paused || !view.Changed {
		t.Errorf("pause view = %+v, want paused and changed", view)
	}
	if view.Since == nil {
		t.Error("the pause view should say when the pause began")
	}
	if _, ok := d.sched.Spec(id); ok {
		t.Error("a paused job should not be armed")
	}

	// The view is what `otter inspect` and `otter jobs --schedule`
	// read, so it has to carry both the state and the absence of a next run.
	got, ok := d.GetJob("ticker")
	if !ok {
		t.Fatal("the paused job disappeared from the registry")
	}
	if !got.Triggers.Paused {
		t.Error("the job view does not report the pause")
	}
	if got.Triggers.PausedAt == nil {
		t.Error("the view should report when the pause began")
	}
	if got.NextRunAt != nil {
		t.Errorf("a paused job still offers a next run: %s", got.NextRunAt)
	}

	// A repeat is a no-op, which is what lets a deploy script call it
	// unconditionally.
	again, err := d.SetPaused(ctx, "ticker", true)
	if err != nil {
		t.Fatalf("pause again: %v", err)
	}
	if again.Changed {
		t.Error("re-pausing should not report a change")
	}

	resumed, err := d.SetPaused(ctx, "ticker", false)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Paused || !resumed.Changed {
		t.Errorf("resume view = %+v, want enabled and changed", resumed)
	}
	if _, ok := d.sched.Spec(id); !ok {
		t.Error("resume did not re-arm the cron trigger")
	}
	if got, _ := d.GetJob("ticker"); got.Triggers.Paused {
		t.Error("the view still reports the pause after a resume")
	}
}

// A pause suspends admission, not the job. Cron and webhook triggers
// are refused; a manual run is the operator asking for one, which is exactly
// what pausing did not forbid.
func TestPauseRefusesAutonomousTriggersButNotManualRuns(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	if _, err := d.SetPaused(ctx, "ticker", true); err != nil {
		t.Fatalf("pause: %v", err)
	}

	for _, trigger := range []string{api.TriggerCron, api.TriggerWebhook} {
		_, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: trigger})
		if !errors.Is(err, api.ErrPaused) {
			t.Errorf("%s trigger while paused = %v, want ErrPaused", trigger, err)
		}
	}

	runID, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("a manual run must still be admitted while paused: %v", err)
	}
	if runID == "" {
		t.Fatal("the manual run has no id")
	}

	// An empty type is what the CLI sends for a manual run, and it means the
	// same thing.
	if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{}); err != nil {
		t.Errorf("a run with no declared trigger type should be treated as manual: %v", err)
	}

	// After a resume the trigger is admitted again.
	if _, err := d.SetPaused(ctx, "ticker", false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: api.TriggerWebhook}); err != nil {
		t.Errorf("webhook trigger after resume: %v", err)
	}
}

// A webhook is an autonomous trigger, so a paused job answers 503 --
// "not accepting triggers right now" -- rather than 409 or 404. The caller
// holds a valid token, so telling it the job is paused leaks nothing,
// while 404 would look like a configuration error.
func TestPausedWebhookReturnsServiceUnavailable(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "hook", `
version: 1
name: hook
entrypoint: main.py
timeout: 30
trigger:
  webhook:
    enabled: true
retry:
  attempts: 0
`, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	token, ok := d.WebhookTokenFor("hook")
	if !ok || token == "" {
		t.Fatal("a webhook-enabled job should have a token")
	}
	url := "http://" + d.cfg.Listen + "/v1/hooks/hook"

	post := func() *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(`{"event":"push"}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("X-Otter-Token", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post webhook: %v", err)
		}
		return resp
	}

	if _, err := d.SetPaused(context.Background(), "hook", true); err != nil {
		t.Fatalf("pause: %v", err)
	}

	resp := post()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("webhook while paused = %d, want 503", resp.StatusCode)
	}
	var envelope api.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if envelope.Error.Code != api.CodeUnavailable {
		t.Errorf("error code = %q, want %q", envelope.Error.Code, api.CodeUnavailable)
	}

	// The same endpoint accepts the trigger again once resumed.
	if _, err := d.SetPaused(context.Background(), "hook", false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	okResp := post()
	defer okResp.Body.Close()
	if okResp.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook after resume = %d, want 202", okResp.StatusCode)
	}
}

// The pause is durable and keyed by identity, so a daemon that comes back up
// must not arm a trigger the operator paused before the restart.
func TestPausedJobStaysUnarmedAcrossARestart(t *testing.T) {
	root := t.TempDir()
	dataDir := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	// Learn the identity the way the daemon will, then write the pause the way
	// a previous daemon would have left it.
	id := identityIDFor(t, root, dataDir, "ticker")
	ctx := context.Background()
	seed, err := database.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open %s: %v", dataDir, err)
	}
	if _, err := database.Migrate(ctx, seed); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := seed.ExecContext(ctx,
		`INSERT INTO job_pause (job_id, paused_at) VALUES (?, ?)`,
		id, database.FormatTime(time.Now().UTC())); err != nil {
		t.Fatalf("seed pause: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d := newDaemon(t, root, dataDir, nil, nil)

	if _, ok := d.sched.Spec(id); ok {
		t.Error("a paused job was armed at startup")
	}
	view, ok := d.GetJob("ticker")
	if !ok {
		t.Fatal("the paused job is missing from the registry")
	}
	if !view.Triggers.Paused {
		t.Errorf("trigger state after restart = %+v, want the seeded pause", view.Triggers)
	}
}

// A move preserves the identity, so it must preserve the pause. A reset mints a
// fresh identity, so it must not inherit one: the operator is looking at a new
// job and it starts enabled.
func TestPauseFollowsAMoveAndNotAReset(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "a", cronTicker, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "ticker")

	if _, err := d.SetPaused(ctx, "id:"+id, true); err != nil {
		t.Fatalf("pause: %v", err)
	}

	moved, err := d.MoveJob(ctx, "id:"+id, filepath.Join(root, "moved"))
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if moved.ID != id {
		t.Fatalf("move changed the identity: %q -> %q", id, moved.ID)
	}
	if !moved.Triggers.Paused {
		t.Error("the pause did not follow the identity across the move")
	}
	if _, ok := d.sched.Spec(id); ok {
		t.Error("a moved job that is paused should stay unarmed")
	}

	reset, err := d.ResetJob(ctx, "id:"+id)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if reset.NewID == id {
		t.Fatal("reset should mint a fresh identity")
	}
	after, ok := d.GetJob("id:" + reset.NewID)
	if !ok {
		t.Fatal("the fresh identity is missing from the registry")
	}
	if after.Triggers.Paused {
		t.Error("a fresh identity must start enabled, not inherit the old pause")
	}
	if _, ok := d.sched.Spec(reset.NewID); !ok {
		t.Error("the fresh identity's cron trigger should be armed")
	}
}

// Purging an identity must not leave its pause behind, or a later identity that
// happened to reuse the row would come back paused for no visible reason.
func TestDeletePurgesThePause(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "ticker")

	if _, err := d.SetPaused(ctx, "id:"+id, true); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := d.DeleteJob(ctx, "id:"+id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if d.paused.Paused(id) {
		t.Error("the deleted identity still holds a pause")
	}

	var rows int
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM job_pause WHERE job_id = ?`, id).Scan(&rows); err != nil {
		t.Fatalf("count pause rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("pause rows for the deleted identity = %d, want 0", rows)
	}
}

// A trigger that was already in flight when the pause landed must not become a
// run. Unregistering the schedule is not the same instant as the operator's
// decision, so the tick re-checks.
func TestCronTickSkipsAPausedJob(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "ticker")

	if _, err := d.SetPaused(ctx, "id:"+id, true); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Call the job the scheduler would have called, as if this tick had been
	// queued before the pause.
	d.cronTick(id, "@every 6h")

	list, err := d.ListRuns(ctx, runs.Filter{JobID: id})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("a paused tick created %d run(s), want 0", len(list))
	}
}
