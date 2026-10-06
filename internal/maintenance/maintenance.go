// Package maintenance implements the runtime's maintenance gate.
//
// A runtime under maintenance accepts no new work: submissions are refused, the
// worker pool stops claiming, and the state survives a restart. That last part
// is the whole reason this is a table and not a flag -- a pooled runtime is
// started gated, validated and only then activated, so a crash between those
// steps must not bring it back up serving.
//
// The gate is deliberately not a pause. A pause is per job, is an operator's
// opinion about triggers, and leaves the gates that matter (manual runs,
// retries) open. Maintenance is a property of the runtime and closes all of
// them, which is what makes it safe to take a data directory's snapshot.
package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// Mode is the runtime's operational state.
type Mode string

const (
	// ModeServing is the absence of maintenance: the runtime accepts work.
	ModeServing Mode = "serving"
	// ModeStartup is a runtime that has started but has not been activated.
	// It is always the state a fresh process begins in unless an operator
	// explicitly left it serving, because the lifecycle starts a runtime
	// gated and activates it deliberately.
	ModeStartup Mode = "startup"
	// ModeDraining is maintenance with work still finishing. New work is
	// already refused; the mode only records that something is still running.
	ModeDraining Mode = "draining"
	// ModeMaintenance is maintenance with nothing running.
	ModeMaintenance Mode = "maintenance"
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool {
	switch m {
	case ModeServing, ModeStartup, ModeDraining, ModeMaintenance:
		return true
	}
	return false
}

// Gated reports whether the mode refuses new work. Serving is the only mode
// that does not: the difference between draining and maintenance is whether
// something is still running, not whether work is accepted.
func (m Mode) Gated() bool { return m != ModeServing }

// State is the persisted maintenance record.
type State struct {
	Mode Mode
	// Explicit is true when an operator decided this state, as opposed to a
	// start that has not been activated yet. It is what lets a restart resume
	// an operator's maintenance instead of silently clearing it, while still
	// treating an unactivated start as "gated because it just started".
	Explicit  bool
	EnteredAt time.Time
	Reason    string
}

// ErrGated reports that the runtime is under maintenance. The API maps it onto
// 503 with a machine-readable code, the same shape a paused job already uses.
var ErrGated = errors.New("runtime is under maintenance")

// ErrAlreadyServing and ErrAlreadyGated make the transitions idempotent without
// being silent: a caller learns whether it changed anything.
var (
	ErrAlreadyServing = errors.New("runtime is already serving")
	ErrAlreadyGated   = errors.New("runtime is already under maintenance")
)

// Store owns the maintenance row and a cached view of it.
//
// The cache exists so the two hot paths -- a worker deciding whether to claim,
// and a submission deciding whether to accept -- do not each pay a query. The
// database stays authoritative: Enter commits before it publishes, so no caller
// can observe the cache claiming serving after a committed gate, and a
// submission re-checks inside its own transaction rather than trusting the
// cache for the decision that actually admits a run.
type Store struct {
	db *database.DB

	mu    sync.RWMutex
	state State
	// gated mirrors state.Mode.Gated() for the worker loop, which asks on every
	// claim and must not take a lock to find out.
	gated atomic.Bool
}

// Load reads the persisted state.
//
// A missing row means the runtime has never been held back, so it serves. That
// default is load-bearing: an ordinary dedicated deployment has no maintenance
// row and must behave exactly as it did before this feature existed, so
// "absent" cannot mean "gated". Starting a runtime gated is therefore an
// explicit act (StartGated), which is what a pooled lifecycle does -- it starts
// the runtime in maintenance, validates it, and activates it deliberately.
func Load(ctx context.Context, db *database.DB) (*Store, error) {
	s := &Store{db: db}
	state, found, err := s.read(ctx, db)
	if err != nil {
		return nil, err
	}
	if !found {
		state = State{Mode: ModeServing, EnteredAt: time.Now().UTC()}
	}
	s.publish(state)
	return s, nil
}

// StartGated records that this process begins in maintenance, persisting it so
// a crash before activation does not bring the runtime back up serving.
//
// It is a start-time decision rather than a default, and it is idempotent with
// an operator's own maintenance: if the runtime was already gated, the existing
// entry time and reason are kept, because the operator's window started before
// this process did.
func (s *Store) StartGated(ctx context.Context, reason string) (State, bool, error) {
	now := time.Now().UTC()
	state := State{Mode: ModeStartup, Explicit: false, EnteredAt: now, Reason: reason}

	var changed bool
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		// The decision reads the row in this transaction rather than the cache:
		// Enter publishes only after it commits, so a cache read here could see
		// a stale "serving" and overwrite an operator's window with a start.
		current, found, err := s.read(ctx, tx)
		if err != nil {
			return err
		}
		if found && current.Mode.Gated() {
			return nil // already gated; the existing window stands
		}
		changed = true
		return s.write(ctx, tx, state, ModeStartup)
	})
	if err != nil {
		return State{}, false, err
	}
	return s.reload(ctx, changed)
}

// querier is the subset of *sql.DB and *sql.Tx the read needs, so the same
// query serves the cached load and the in-transaction check.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// read loads the row. A missing row reports found=false rather than an error.
func (s *Store) read(ctx context.Context, q querier) (State, bool, error) {
	var (
		mode      string
		explicit  int
		enteredAt string
		reason    string
	)
	err := q.QueryRowContext(ctx,
		`SELECT mode, explicit, entered_at, reason FROM runtime_maintenance WHERE id = 1`).
		Scan(&mode, &explicit, &enteredAt, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("maintenance: read state: %w", err)
	}
	at, err := database.ParseTime(enteredAt)
	if err != nil {
		return State{}, false, fmt.Errorf("maintenance: parse entered_at: %w", err)
	}
	state := State{
		Mode:      Mode(mode),
		Explicit:  explicit != 0,
		EnteredAt: at,
		Reason:    reason,
	}
	if !state.Mode.Valid() {
		return State{}, false, fmt.Errorf("maintenance: unknown persisted mode %q", mode)
	}
	return state, true, nil
}

// publish installs a state in memory. Callers must have committed it first.
func (s *Store) publish(state State) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
	s.gated.Store(state.Mode.Gated())
}

// State returns the cached state.
func (s *Store) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Gated reports whether the runtime refuses new work. It is the cheap check for
// hot paths; the authoritative check is CheckTx inside the transaction that
// would admit a run.
func (s *Store) Gated() bool { return s.gated.Load() }

// CheckTx re-reads the persisted state inside a transaction that is about to
// admit work, so the decision to accept a run cannot be made from a cache that
// a concurrent Enter has already invalidated.
//
// An absent row is serving, matching Load: a runtime that never opted into
// maintenance accepts work, which is what keeps an ordinary deployment
// unchanged.
func (s *Store) CheckTx(ctx context.Context, tx *sql.Tx) error {
	state, found, err := s.read(ctx, tx)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if state.Mode.Gated() {
		return s.gatedError(state, true)
	}
	return nil
}

// gatedError describes why work is refused, so the caller can hand the operator
// a reason and a next step rather than only a status code.
func (s *Store) gatedError(state State, found bool) error {
	if !found {
		return fmt.Errorf("%w: this runtime has not been activated yet; "+
			"activate it with DELETE /v1/runtime/maintenance: %w", ErrGated, ErrGated)
	}
	detail := string(state.Mode)
	if state.Reason != "" {
		detail += ": " + state.Reason
	}
	return fmt.Errorf("%w (%s); activate it with DELETE /v1/runtime/maintenance: %w",
		ErrGated, detail, ErrGated)
}

// Enter persists maintenance and only then publishes it, so a successful return
// means no later submission can be admitted. It is idempotent: entering
// maintenance twice reports changed=false, which is what lets a deploy script
// call it unconditionally.
func (s *Store) Enter(ctx context.Context, reason string) (State, bool, error) {
	now := time.Now().UTC()
	// Draining is the honest starting mode: whether anything is running is
	// decided by the caller that can see the active count, and claiming
	// "maintenance" while a child is alive would be a lie an operator might
	// act on.
	state := State{Mode: ModeDraining, Explicit: true, EnteredAt: now, Reason: reason}

	var changed bool
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		current, found, err := s.read(ctx, tx)
		if err != nil {
			return err
		}
		// Only an operator's own earlier decision makes this a no-op. A runtime
		// gated by its start is *not* a window -- nobody asked for it -- so an
		// operator entering maintenance takes ownership of it and their reason
		// and entry time replace the start's. Treating the two as the same fact
		// would report a window that began when the process happened to boot.
		if found && current.Mode.Gated() && current.Explicit {
			return nil
		}
		changed = true
		return s.write(ctx, tx, state, ModeMaintenance)
	})
	if err != nil {
		return State{}, false, err
	}
	return s.reload(ctx, changed)
}

// Exit clears maintenance. Like Enter it persists first, so a caller that sees
// success knows the runtime will accept work from that point.
func (s *Store) Exit(ctx context.Context) (State, bool, error) {
	now := time.Now().UTC()
	state := State{Mode: ModeServing, Explicit: true, EnteredAt: now}

	var changed bool
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		current, found, err := s.read(ctx, tx)
		if err != nil {
			return err
		}
		if found && !current.Mode.Gated() && current.Explicit {
			return nil // already serving, and an operator already said so
		}
		changed = true
		return s.write(ctx, tx, state, ModeServing)
	})
	if err != nil {
		return State{}, false, err
	}
	return s.reload(ctx, changed)
}

// reload re-reads the committed row and publishes it. Reading after the commit
// rather than assuming what was written means the in-memory view can never
// disagree with the table about the state that admits work.
func (s *Store) reload(ctx context.Context, changed bool) (State, bool, error) {
	final, found, err := s.read(ctx, s.db)
	if err != nil {
		return State{}, false, err
	}
	if !found {
		final = State{Mode: ModeServing, Explicit: true, EnteredAt: time.Now().UTC()}
	}
	s.publish(final)
	return final, changed, nil
}

// SetMode records a mode transition that is not an enter/exit, used when the
// drain finishes so the state stops claiming work is running.
func (s *Store) SetMode(ctx context.Context, mode Mode, reason string) (State, error) {
	if !mode.Valid() {
		return State{}, fmt.Errorf("maintenance: unknown mode %q", mode)
	}
	state := State{Mode: mode, Explicit: true, EnteredAt: time.Now().UTC(), Reason: reason}
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		current, _, err := s.read(ctx, tx)
		if err != nil {
			return err
		}
		// Preserve the original entry time: the operator wants to know how long
		// the runtime has been held back, not when the drain happened to finish.
		state.EnteredAt = current.EnteredAt
		if state.EnteredAt.IsZero() {
			state.EnteredAt = time.Now().UTC()
		}
		if current.Reason != "" && reason == "" {
			state.Reason = current.Reason
		}
		return s.write(ctx, tx, state, mode)
	})
	if err != nil {
		return State{}, err
	}
	final, _, err := s.read(ctx, s.db)
	if err != nil {
		return State{}, err
	}
	s.publish(final)
	return final, nil
}

// write upserts the singleton row. requested records what the operator asked
// for, which is what a restart consults to decide whether to resume.
func (s *Store) write(ctx context.Context, tx *sql.Tx, state State, requested Mode) error {
	explicit := 0
	if state.Explicit {
		explicit = 1
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO runtime_maintenance (id, mode, explicit, entered_at, reason, requested)
		 VALUES (1, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		     mode = excluded.mode,
		     explicit = excluded.explicit,
		     entered_at = excluded.entered_at,
		     reason = excluded.reason,
		     requested = excluded.requested`,
		string(state.Mode), explicit, database.FormatTime(state.EnteredAt), state.Reason, string(requested))
	if err != nil {
		return fmt.Errorf("maintenance: write state: %w", err)
	}
	return nil
}
