package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// ErrNotFound reports a missing registry row.
var ErrNotFound = errors.New("identity: not found")

// ErrPathOwned reports that a path already has a current owner.
var ErrPathOwned = errors.New("identity: path already owned")

// Status is the ownership lifecycle of an instance. It is deliberately
// independent of manifest validity and of the execution gate: an instance can
// be active and invalid, or active and temporarily blocked, at the same time.
type Status string

// Ownership statuses.
const (
	StatusPending  Status = "pending"
	StatusActive   Status = "active"
	StatusRetired  Status = "retired"
	StatusDeleting Status = "deleting"
	StatusDeleted  Status = "deleted"
)

// AllStatuses lists every valid status.
func AllStatuses() []Status {
	return []Status{StatusPending, StatusActive, StatusRetired, StatusDeleting, StatusDeleted}
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	for _, known := range AllStatuses() {
		if s == known {
			return true
		}
	}
	return false
}

// AcceptsWork reports whether the instance may receive new work. Only an
// active instance may; pending, retired, deleting and deleted may not.
func (s Status) AcceptsWork() bool { return s == StatusActive }

// Instance is one registered integration instance.
//
// ID is durable; Name is a mutable, non-unique label; CanonicalPath is where
// the source currently lives. Only an explicit move changes the path of an
// existing ID.
type Instance struct {
	ID               ID
	Name             string
	CanonicalPath    string
	Status           Status
	Generation       int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
	RetiredAt        *time.Time
	RetirementReason string
}

// PathRecord is the ownership record for one canonical source path.
type PathRecord struct {
	CanonicalPath     string
	OwnerID           ID
	Suppressed        bool
	SuppressionReason string
	UpdatedAt         time.Time
}

// Operation is one journaled identity mutation. SQL and the filesystem cannot
// be updated in one transaction, so every mutation persists its intent and its
// expected observations first, performs the filesystem work, and only then
// commits the registry change. Recovery replays whatever phase was reached.
type Operation struct {
	ID         string
	Kind       string
	Phase      string
	InstanceID ID
	FromPath   string
	ToPath     string
	Data       json.RawMessage
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Operation kinds.
const (
	OpRegister = "register"
	OpReset    = "reset"
	OpDelete   = "delete"
	OpMove     = "move"
	OpMigrate  = "migrate"
)

// Operation phases.
const (
	PhasePlanned   = "planned"
	PhaseReserved  = "reserved"
	PhaseApplied   = "applied"
	PhaseCommitted = "committed"
	PhaseDone      = "done"
	PhaseBlocked   = "blocked"
)

// Store is the durable identity registry.
type Store struct {
	db *sql.DB
}

// NewStore wraps an open database.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// DB exposes the underlying handle for callers that need to compose the
// registry change into a wider transaction.
func (s *Store) DB() *sql.DB { return s.db }

// ---------------------------------------------------------------- instances

func scanInstance(row interface{ Scan(...any) error }) (Instance, error) {
	var (
		inst    Instance
		id      string
		status  string
		created string
		updated string
		retired database.NullableTime
		gen     int64
	)
	if err := row.Scan(&id, &inst.Name, &inst.CanonicalPath, &status, &gen,
		&created, &updated, &retired, &inst.RetirementReason); err != nil {
		return Instance{}, err
	}
	parsed, err := Parse(id)
	if err != nil {
		return Instance{}, fmt.Errorf("identity: stored id %q is invalid: %w", id, err)
	}
	inst.ID = parsed
	inst.Status = Status(status)
	if !inst.Status.Valid() {
		return Instance{}, fmt.Errorf("identity: stored status %q is invalid", status)
	}
	inst.Generation = gen
	if inst.CreatedAt, err = database.ParseTime(created); err != nil {
		return Instance{}, err
	}
	if inst.UpdatedAt, err = database.ParseTime(updated); err != nil {
		return Instance{}, err
	}
	inst.RetiredAt = retired.Ptr()
	return inst, nil
}

const instanceColumns = `id, name, canonical_path, status, generation, created_at, updated_at, retired_at, retirement_reason`

// Instances returns every registered instance, active or historical.
func (s *Store) Instances(ctx context.Context) ([]Instance, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+instanceColumns+` FROM integration_instances`)
	if err != nil {
		return nil, fmt.Errorf("identity: list instances: %w", err)
	}
	defer rows.Close()

	var out []Instance
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("identity: scan instance: %w", err)
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

// Instance returns one instance by ID.
func (s *Store) Instance(ctx context.Context, id ID) (Instance, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+instanceColumns+` FROM integration_instances WHERE id = ?`, id.String())
	inst, err := scanInstance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Instance{}, fmt.Errorf("%w: instance %s", ErrNotFound, id)
	}
	if err != nil {
		return Instance{}, fmt.Errorf("identity: get instance %s: %w", id, err)
	}
	return inst, nil
}

// ActiveInstancesByLabel returns every active instance carrying a label.
func (s *Store) ActiveInstancesByLabel(ctx context.Context, name string) ([]Instance, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+instanceColumns+` FROM integration_instances WHERE name = ? AND status = ? ORDER BY canonical_path`,
		name, string(StatusActive))
	if err != nil {
		return nil, fmt.Errorf("identity: list instances by label %q: %w", name, err)
	}
	defer rows.Close()

	var out []Instance
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("identity: scan instance: %w", err)
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

// CreateInstance inserts a new instance row. It fails with ErrPathOwned when
// another instance already owns the path.
func (s *Store) CreateInstance(ctx context.Context, inst Instance) error {
	if inst.ID.IsZero() {
		return errors.New("identity: create instance requires an id")
	}
	now := time.Now().UTC()
	if inst.CreatedAt.IsZero() {
		inst.CreatedAt = now
	}
	if inst.UpdatedAt.IsZero() {
		inst.UpdatedAt = now
	}
	if inst.Generation == 0 {
		inst.Generation = 1
	}
	if inst.Status == "" {
		inst.Status = StatusActive
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO integration_instances (`+instanceColumns+`)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			inst.ID.String(), inst.Name, inst.CanonicalPath, string(inst.Status), inst.Generation,
			database.FormatTime(inst.CreatedAt), database.FormatTime(inst.UpdatedAt),
			database.FormatNullable(inst.RetiredAt), inst.RetirementReason); err != nil {
			return fmt.Errorf("identity: insert instance %s: %w", inst.ID, err)
		}
		// A reserved legacy identity has no source directory, so there is no
		// path to claim.
		if inst.CanonicalPath == "" {
			return nil
		}
		return s.claimPath(ctx, tx, inst.CanonicalPath, inst.ID)
	})
}

// UpdateObservation refreshes the mutable label of an instance.
func (s *Store) UpdateObservation(ctx context.Context, id ID, name string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE integration_instances SET name = ?, updated_at = ? WHERE id = ?`,
		name, database.FormatTime(time.Now().UTC()), id.String())
	if err != nil {
		return fmt.Errorf("identity: update instance %s: %w", id, err)
	}
	return nil
}

// SetStatus transitions an instance's ownership status. Retirement records a
// timestamp and a reason; any transition away from retired clears them.
func (s *Store) SetStatus(ctx context.Context, id ID, status Status, reason string) error {
	if !status.Valid() {
		return fmt.Errorf("identity: invalid status %q", status)
	}
	now := time.Now().UTC()
	var retiredAt any
	if status == StatusRetired || status == StatusDeleted {
		retiredAt = database.FormatTime(now)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE integration_instances
		    SET status = ?, updated_at = ?, retired_at = ?, retirement_reason = ?
		  WHERE id = ?`,
		string(status), database.FormatTime(now), retiredAt, reason, id.String())
	if err != nil {
		return fmt.Errorf("identity: set status %s for %s: %w", status, id, err)
	}
	return nil
}

// BumpGeneration increments and returns the execution generation. A transition
// that revokes authority (reset, move, retirement, deletion) bumps it, so a
// worker or token carrying the old generation can no longer write.
func (s *Store) BumpGeneration(ctx context.Context, id ID) (int64, error) {
	var gen int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			`SELECT generation FROM integration_instances WHERE id = ?`, id.String()).Scan(&gen); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: instance %s", ErrNotFound, id)
			}
			return err
		}
		gen++
		_, err := tx.ExecContext(ctx,
			`UPDATE integration_instances SET generation = ?, updated_at = ? WHERE id = ?`,
			gen, database.FormatTime(time.Now().UTC()), id.String())
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("identity: bump generation for %s: %w", id, err)
	}
	return gen, nil
}

// SetCanonicalPath rebinds an instance to a new source directory. It is only
// used by an explicit move or by migration.
func (s *Store) SetCanonicalPath(ctx context.Context, id ID, path string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE integration_instances SET canonical_path = ?, updated_at = ? WHERE id = ?`,
			path, database.FormatTime(time.Now().UTC()), id.String()); err != nil {
			return err
		}
		return s.claimPath(ctx, tx, path, id)
	})
}

// ---------------------------------------------------------------- paths

func scanPath(row interface{ Scan(...any) error }) (PathRecord, error) {
	var (
		rec     PathRecord
		owner   sql.NullString
		updated string
		sup     int
	)
	if err := row.Scan(&rec.CanonicalPath, &owner, &sup, &rec.SuppressionReason, &updated); err != nil {
		return PathRecord{}, err
	}
	if owner.Valid && owner.String != "" {
		id, err := Parse(owner.String)
		if err != nil {
			return PathRecord{}, fmt.Errorf("identity: stored path owner %q is invalid: %w", owner.String, err)
		}
		rec.OwnerID = id
	}
	rec.Suppressed = sup != 0
	t, err := database.ParseTime(updated)
	if err != nil {
		return PathRecord{}, err
	}
	rec.UpdatedAt = t
	return rec, nil
}

const pathColumns = `canonical_path, owner_id, suppressed, suppression_reason, updated_at`

// PathRecord returns the ownership row for a path.
func (s *Store) PathRecord(ctx context.Context, path string) (PathRecord, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+pathColumns+` FROM integration_paths WHERE canonical_path = ?`, path)
	rec, err := scanPath(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PathRecord{}, false, nil
	}
	if err != nil {
		return PathRecord{}, false, fmt.Errorf("identity: get path %s: %w", path, err)
	}
	return rec, true, nil
}

// Paths returns every ownership row.
func (s *Store) Paths(ctx context.Context) ([]PathRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pathColumns+` FROM integration_paths`)
	if err != nil {
		return nil, fmt.Errorf("identity: list paths: %w", err)
	}
	defer rows.Close()

	var out []PathRecord
	for rows.Next() {
		rec, err := scanPath(rows)
		if err != nil {
			return nil, fmt.Errorf("identity: scan path: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// claimPath atomically gives a path to a new owner, refusing a suppressed path
// or one owned by a different instance.
func (s *Store) claimPath(ctx context.Context, tx *sql.Tx, path string, owner ID) error {
	if path == "" {
		return errors.New("identity: cannot claim an empty path")
	}
	now := database.FormatTime(time.Now().UTC())

	var existingOwner sql.NullString
	var suppressed int
	err := tx.QueryRowContext(ctx,
		`SELECT owner_id, suppressed FROM integration_paths WHERE canonical_path = ?`, path).
		Scan(&existingOwner, &suppressed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx,
			`INSERT INTO integration_paths (canonical_path, owner_id, suppressed, suppression_reason, updated_at)
			 VALUES (?, ?, 0, '', ?)`, path, owner.String(), now)
		return err
	case err != nil:
		return err
	}
	if existingOwner.Valid && existingOwner.String != "" && existingOwner.String != owner.String() {
		return fmt.Errorf("%w: %s is owned by %s", ErrPathOwned, path, existingOwner.String)
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE integration_paths SET owner_id = ?, suppressed = 0, suppression_reason = '', updated_at = ?
		  WHERE canonical_path = ?`, owner.String(), now, path)
	return err
}

// ReleasePath clears a path's owner, retaining the row as history.
func (s *Store) ReleasePath(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE integration_paths SET owner_id = NULL, updated_at = ? WHERE canonical_path = ?`,
		database.FormatTime(time.Now().UTC()), path)
	if err != nil {
		return fmt.Errorf("identity: release path %s: %w", path, err)
	}
	return nil
}

// SuppressPath marks a path as deleted so discovery does not immediately
// re-register it. Only a path still owned by the deleted instance is touched.
func (s *Store) SuppressPath(ctx context.Context, path string, owner ID, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO integration_paths (canonical_path, owner_id, suppressed, suppression_reason, updated_at)
		 VALUES (?, NULL, 1, ?, ?)
		 ON CONFLICT(canonical_path) DO UPDATE SET
		   suppressed = 1,
		   suppression_reason = excluded.suppression_reason,
		   owner_id = NULL,
		   updated_at = excluded.updated_at
		 WHERE integration_paths.owner_id IS NULL OR integration_paths.owner_id = ?`,
		path, reason, database.FormatTime(time.Now().UTC()), owner.String())
	if err != nil {
		return fmt.Errorf("identity: suppress path %s: %w", path, err)
	}
	return nil
}

// ClearSuppression removes a deletion suppression, which is what an explicit
// register does.
func (s *Store) ClearSuppression(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE integration_paths SET suppressed = 0, suppression_reason = '', updated_at = ?
		  WHERE canonical_path = ?`,
		database.FormatTime(time.Now().UTC()), path)
	if err != nil {
		return fmt.Errorf("identity: clear suppression for %s: %w", path, err)
	}
	return nil
}

// ReservePath takes exclusive ownership of a path for a journaled operation.
func (s *Store) ReservePath(ctx context.Context, path string, owner ID, reason string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		rec, found, err := pathInTx(ctx, tx, path)
		if err != nil {
			return err
		}
		if found && rec.Suppressed {
			return fmt.Errorf("%w: %s is suppressed (%s)", ErrPathOwned, path, rec.SuppressionReason)
		}
		if found && !rec.OwnerID.IsZero() && rec.OwnerID != owner {
			return fmt.Errorf("%w: %s is owned by %s", ErrPathOwned, path, rec.OwnerID)
		}
		return s.claimPath(ctx, tx, path, owner)
	})
}

func pathInTx(ctx context.Context, tx *sql.Tx, path string) (PathRecord, bool, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+pathColumns+` FROM integration_paths WHERE canonical_path = ?`, path)
	rec, err := scanPath(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PathRecord{}, false, nil
	}
	if err != nil {
		return PathRecord{}, false, err
	}
	return rec, true, nil
}

// ---------------------------------------------------------------- operations

const operationColumns = `id, kind, phase, instance_id, from_path, to_path, data, created_at, updated_at`

func scanOperation(row interface{ Scan(...any) error }) (Operation, error) {
	var (
		op      Operation
		inst    string
		data    string
		created string
		updated string
	)
	if err := row.Scan(&op.ID, &op.Kind, &op.Phase, &inst, &op.FromPath, &op.ToPath, &data, &created, &updated); err != nil {
		return Operation{}, err
	}
	if inst != "" {
		id, err := Parse(inst)
		if err != nil {
			return Operation{}, fmt.Errorf("identity: stored operation instance %q is invalid: %w", inst, err)
		}
		op.InstanceID = id
	}
	if data != "" {
		op.Data = json.RawMessage(data)
	}
	var err error
	if op.CreatedAt, err = database.ParseTime(created); err != nil {
		return Operation{}, err
	}
	if op.UpdatedAt, err = database.ParseTime(updated); err != nil {
		return Operation{}, err
	}
	return op, nil
}

// CreateOperation records a journaled mutation.
func (s *Store) CreateOperation(ctx context.Context, op Operation) error {
	now := time.Now().UTC()
	if op.CreatedAt.IsZero() {
		op.CreatedAt = now
	}
	op.UpdatedAt = now
	if len(op.Data) == 0 {
		op.Data = json.RawMessage("{}")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO identity_operations (`+operationColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID, op.Kind, op.Phase, op.InstanceID.String(), op.FromPath, op.ToPath, string(op.Data),
		database.FormatTime(op.CreatedAt), database.FormatTime(op.UpdatedAt))
	if err != nil {
		return fmt.Errorf("identity: create operation %s: %w", op.ID, err)
	}
	return nil
}

// UpdateOperation persists a new phase and payload.
func (s *Store) UpdateOperation(ctx context.Context, op Operation) error {
	if len(op.Data) == 0 {
		op.Data = json.RawMessage("{}")
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE identity_operations SET phase = ?, instance_id = ?, from_path = ?, to_path = ?, data = ?, updated_at = ?
		  WHERE id = ?`,
		op.Phase, op.InstanceID.String(), op.FromPath, op.ToPath, string(op.Data),
		database.FormatTime(time.Now().UTC()), op.ID)
	if err != nil {
		return fmt.Errorf("identity: update operation %s: %w", op.ID, err)
	}
	return nil
}

// Operation returns one journaled operation.
func (s *Store) Operation(ctx context.Context, id string) (Operation, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM identity_operations WHERE id = ?`, id)
	op, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, false, nil
	}
	if err != nil {
		return Operation{}, false, fmt.Errorf("identity: get operation %s: %w", id, err)
	}
	return op, true, nil
}

// Operations returns every journaled operation in creation order.
func (s *Store) Operations(ctx context.Context) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+operationColumns+` FROM identity_operations ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("identity: list operations: %w", err)
	}
	defer rows.Close()

	var out []Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, fmt.Errorf("identity: scan operation: %w", err)
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// UnfinishedOperations returns operations that still need recovery.
func (s *Store) UnfinishedOperations(ctx context.Context) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+operationColumns+` FROM identity_operations
		  WHERE phase NOT IN (?, ?) ORDER BY created_at, id`,
		PhaseDone, PhaseBlocked)
	if err != nil {
		return nil, fmt.Errorf("identity: list unfinished operations: %w", err)
	}
	defer rows.Close()

	var out []Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, fmt.Errorf("identity: scan operation: %w", err)
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// DeleteOperation removes a completed journal entry.
func (s *Store) DeleteOperation(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM identity_operations WHERE id = ?`, id); err != nil {
		return fmt.Errorf("identity: delete operation %s: %w", id, err)
	}
	return nil
}

// ---------------------------------------------------------------- metadata

const (
	metaBootstrapComplete = "bootstrap_complete"
)

// BootstrapComplete reports whether semantic identity migration has finished.
// SQL schema version and semantic bootstrap are deliberately separate: the
// daemon must not schedule work merely because the SQL migration advanced.
func (s *Store) BootstrapComplete(ctx context.Context) (bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM identity_meta WHERE key = ?`, metaBootstrapComplete).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("identity: read bootstrap flag: %w", err)
	}
	return value == "true", nil
}

// SetBootstrapComplete records the bootstrap outcome.
func (s *Store) SetBootstrapComplete(ctx context.Context, done bool) error {
	value := "false"
	if done {
		value = "true"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO identity_meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		metaBootstrapComplete, value)
	if err != nil {
		return fmt.Errorf("identity: set bootstrap flag: %w", err)
	}
	return nil
}

// withTx runs fn in a transaction against the store's handle.
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
