package daemon

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/database"
)

// runTokenRegistry hands out short-lived bearer tokens to child processes.
//
// A child gets a token scoped to exactly one run and one job: enough
// to read its trigger, read and write its job's state, and append its
// own logs. It cannot list jobs, trigger other runs or cancel
// anything. Tokens live only in memory, which is correct: child processes do
// not survive a daemon restart either.
type runTokenRegistry struct {
	mu      sync.RWMutex
	byToken map[string]api.RunToken
	byRun   map[string]string
}

func newRunTokenRegistry() *runTokenRegistry {
	return &runTokenRegistry{
		byToken: map[string]api.RunToken{},
		byRun:   map[string]string{},
	}
}

// Issue creates a token for a run, replacing any previous token for that run.
// Issue creates a token for a run, replacing any previous token for that run.
// Generation is the identity generation the run was authorized against; it is
// checked again when the token is used to mutate state.
func (r *runTokenRegistry) Issue(runID, jobID string, generation int64) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate run token: %w", err)
	}
	token := hex.EncodeToString(buf)

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byRun[runID]; ok {
		delete(r.byToken, existing)
	}
	r.byToken[token] = api.RunToken{RunID: runID, JobID: jobID, Generation: generation}
	r.byRun[runID] = token
	return token, nil
}

// Lookup resolves a presented token.
func (r *runTokenRegistry) Lookup(token string) (api.RunToken, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	scope, ok := r.byToken[token]
	return scope, ok
}

// Revoke invalidates a token when its run finishes.
func (r *runTokenRegistry) Revoke(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	scope, ok := r.byToken[token]
	if !ok {
		return
	}
	delete(r.byToken, token)
	if current, ok := r.byRun[scope.RunID]; ok && current == token {
		delete(r.byRun, scope.RunID)
	}
}

// Len reports how many live tokens exist, for tests.
func (r *runTokenRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byToken)
}

// ensureWebhookToken returns the persisted webhook token for a job,
// generating one on first use.
func (d *Daemon) ensureWebhookToken(ctx context.Context, jobID string) (string, error) {
	var token string
	err := d.db.QueryRowContext(ctx,
		`SELECT token FROM webhook_tokens WHERE job_id = ?`, jobID).Scan(&token)
	if err == nil {
		return token, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read webhook token for %s: %w", jobID, err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate webhook token for %s: %w", jobID, err)
	}
	token = hex.EncodeToString(buf)

	// DO NOTHING keeps a concurrent writer's token rather than replacing it.
	if _, err := d.db.ExecContext(ctx,
		`INSERT INTO webhook_tokens (job_id, token, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(job_id) DO NOTHING`,
		jobID, token, database.FormatTime(time.Now().UTC())); err != nil {
		return "", fmt.Errorf("store webhook token for %s: %w", jobID, err)
	}

	if err := d.db.QueryRowContext(ctx,
		`SELECT token FROM webhook_tokens WHERE job_id = ?`, jobID).Scan(&token); err != nil {
		return "", fmt.Errorf("reread webhook token for %s: %w", jobID, err)
	}
	return token, nil
}

// rotateWebhookToken forces a new webhook token, invalidating the old one.
func (d *Daemon) rotateWebhookToken(ctx context.Context, jobID string) (string, error) {
	if _, err := d.db.ExecContext(ctx,
		`DELETE FROM webhook_tokens WHERE job_id = ?`, jobID); err != nil {
		return "", fmt.Errorf("clear webhook token for %s: %w", jobID, err)
	}
	return d.ensureWebhookToken(ctx, jobID)
}
