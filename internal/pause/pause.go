// Package pause owns the per-integration trigger pause: the operator's answer
// to "stop this integration's autonomous triggers, now, without retiring
// anything".
//
// Pausing is neither retirement nor deletion. The integration keeps its
// identity, its state, its run history, its webhook token and its releases.
// What stops is autonomous admission: cron no longer fires and the webhook no
// longer accepts a trigger. An explicit manual run still works, because an
// operator asking for a run is not the thing being paused.
//
// A pause is keyed by the durable identity id, never by the label or the source
// path. A label need not be unique, and a directory can move; either would make
// a pause follow the wrong integration. Keying by identity is also what gives
// the lifecycle its obvious behaviour: `move` preserves the identity and so
// carries the pause, while `reset` mints a fresh identity that starts enabled.
package pause

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// State is one integration's trigger state.
//
// The zero State is enabled: it has Paused false and no time, which is exactly
// what an integration with no row reports.
type State struct {
	Paused bool
	Since  time.Time
}

// Store is the durable pause table plus an in-memory mirror of it.
//
// The daemon holds an exclusive lock on its data directory, so it is the only
// writer and the mirror cannot go stale behind its back. The mirror is what
// lets the hot paths -- a cron tick, a webhook submission, listing every
// integration -- ask whether an integration is paused without a database read.
type Store struct {
	db *sql.DB

	mu     sync.RWMutex
	paused map[string]State
}

// NewStore reads the pause table into memory and returns a Store over db.
func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	s := &Store{db: db, paused: map[string]State{}}

	rows, err := db.QueryContext(ctx, `SELECT integration_id, paused_at FROM integration_pause`)
	if err != nil {
		return nil, fmt.Errorf("pause: read pause state: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id string
			at string
		)
		if err := rows.Scan(&id, &at); err != nil {
			return nil, fmt.Errorf("pause: scan pause state: %w", err)
		}
		since, err := database.ParseTime(at)
		if err != nil {
			return nil, fmt.Errorf("pause: %s: %w", id, err)
		}
		s.paused[id] = State{Paused: true, Since: since}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pause: read pause state: %w", err)
	}
	return s, nil
}

// Get returns one integration's trigger state. An integration with no row is
// enabled, which is the zero State rather than an error.
func (s *Store) Get(integrationID string) State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.paused[integrationID]
}

// Paused reports whether an integration's autonomous triggers are suspended.
func (s *Store) Paused(integrationID string) bool {
	return s.Get(integrationID).Paused
}

// Len reports how many integrations are paused.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.paused)
}

// Set pauses or resumes one integration and reports the resulting state plus
// whether anything changed.
//
// It is deliberately idempotent in both directions: pausing an already-paused
// integration, or resuming one that was never paused, is a no-op that reports
// changed false. That is what lets a deploy script call either unconditionally
// and still tell whether this call was the one that acted.
func (s *Store) Set(ctx context.Context, integrationID string, paused bool) (State, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.paused[integrationID]

	if !paused {
		if !current.Paused {
			return State{}, false, nil
		}
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM integration_pause WHERE integration_id = ?`, integrationID); err != nil {
			return State{}, false, fmt.Errorf("pause: resume %s: %w", integrationID, err)
		}
		delete(s.paused, integrationID)
		return State{}, true, nil
	}

	if current.Paused {
		return current, false, nil
	}

	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO integration_pause (integration_id, paused_at) VALUES (?, ?)`,
		integrationID, database.FormatTime(now)); err != nil {
		return State{}, false, fmt.Errorf("pause: pause %s: %w", integrationID, err)
	}
	state := State{Paused: true, Since: now}
	s.paused[integrationID] = state
	return state, true, nil
}

// Delete forgets an integration's pause. It is called when an identity is
// purged, so a deleted integration leaves nothing behind.
//
// A missing row is not an error: purging an integration that was never paused
// is the common case.
func (s *Store) Delete(ctx context.Context, integrationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM integration_pause WHERE integration_id = ?`, integrationID); err != nil {
		return fmt.Errorf("pause: delete %s: %w", integrationID, err)
	}
	delete(s.paused, integrationID)
	return nil
}
