// Package schedule owns per-job cadence: the answer to "when should this job
// fire", as runtime state rather than manifest state.
//
// A schedule is keyed by the durable identity id of its job, never the label or
// the source path, for the same reason a pause is: a label need not be unique
// and a directory can move, so either would attach the cadence to the wrong job.
// Keying by identity is also what gives the lifecycle its obvious behaviour --
// `move` carries the schedule, `reset` starts with none, `delete` purges it.
//
// Every row also carries an origin. A manifest-declared schedule is reconciled
// from the file on every reload; an API-created one is never read, changed or
// deleted by a reload. That one column is what lets a declarative file and an
// imperative control plane coexist without either silently undoing the other.
package schedule

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/scheduler"
)

// MaxPayloadBytes bounds a per-occurrence payload. 64 KiB is generous for a
// dataset identifier and a few overrides, and it keeps a schedule from becoming
// a bulk data store.
const MaxPayloadBytes = 64 * 1024

// DefaultTimezone is the zone a new schedule without one is interpreted in.
// UTC is pinned deliberately: "whatever the host's clock was configured to" is
// not a contract anyone can hold.
const DefaultTimezone = "UTC"

// MaxCatchUp bounds how many missed occurrences a catch_up schedule will replay
// on one wake-up, so a long outage cannot become a thundering herd.
const MaxCatchUp = 100

// MaxCatchUpCeiling is the largest per-schedule catch-up bound an operator may
// set. The default is deliberately modest and this is deliberately not: the
// point is to refuse a bound that is really "no bound", not to second-guess a
// large one. A schedule that asks for more than this is asking for the outage
// to become the herd.
const MaxCatchUpCeiling = 10000

const (
	// LegacyRefMigrated marks a row carried forward from v0.3.0's
	// job_schedules table.
	LegacyRefMigrated = "migrated-v0.3.0"
	// LegacyRefPut marks a row written by the v0.3.0 PUT/DELETE cadence
	// endpoint, which owns a job's single cadence outright.
	LegacyRefPut = "legacy-put"
)

// Origin names what owns a schedule row.
type Origin string

const (
	// OriginManifest is a schedule declared in otter.yaml. Reload reconciles
	// it: the file is the source of truth for its cron, time zone and payload.
	OriginManifest Origin = "manifest"
	// OriginAPI is a schedule created through the API or CLI. A reload never
	// reads, changes or deletes it.
	OriginAPI Origin = "api"
)

// Valid reports whether o is a known origin.
func (o Origin) Valid() bool { return o == OriginManifest || o == OriginAPI }

// MissedPolicy names what happens to occurrences missed while the daemon was
// down.
//
// Only skip is implemented for v0.4.0. coalesce and catch_up are reserved
// names: the column exists so the choice can become data without a migration,
// but a write carrying one is refused rather than accepted and silently
// ignored. Stored rows are still parsed for all three, so a value written by a
// future build does not become unreadable.
type MissedPolicy string

const (
	MissedSkip     MissedPolicy = "skip"
	MissedCoalesce MissedPolicy = "coalesce"
	MissedCatchUp  MissedPolicy = "catch_up"
)

// Valid reports whether p is a known policy name.
func (p MissedPolicy) Valid() bool {
	return p == MissedSkip || p == MissedCoalesce || p == MissedCatchUp
}

// Supported reports whether p is implemented. All three are: skip (the
// default, and the only one a manifest or API caller gets without asking),
// coalesce and catch_up. The write boundary refuses a name that is not a known
// policy; it no longer refuses a known one for being unimplemented.
func (p MissedPolicy) Supported() bool {
	return p == MissedSkip || p == MissedCoalesce || p == MissedCatchUp
}

// Sentinel errors. The daemon maps them onto HTTP status codes without leaking
// HTTP concepts into this package.
var (
	// ErrNotFound reports an unknown schedule id.
	ErrNotFound = errors.New("schedule not found")
	// ErrManifestOwned reports an API attempt to change or delete a schedule
	// the manifest owns. The API cannot fight a reload.
	ErrManifestOwned = errors.New("schedule is owned by the manifest")
	// ErrConflict reports a request that contradicts the schedule's state.
	ErrConflict = errors.New("schedule conflict")
	// ErrInvalid reports a malformed request.
	ErrInvalid = errors.New("invalid schedule")
)

// Schedule is one cadence for one job.
type Schedule struct {
	ID       string
	JobID    string
	Cron     string
	Timezone string
	// Payload becomes ctx.trigger.body for a run this schedule starts. It is
	// always a JSON object.
	Payload json.RawMessage
	// ConfigVersion pins the job configuration a run uses. It is stored now so
	// the configuration layer can land without a second migration.
	ConfigVersion string
	MissedPolicy  MissedPolicy
	// MaxCatchUp bounds how many missed occurrences this schedule replays on
	// one wake-up. Zero means the daemon default (MaxCatchUp); it is stored as
	// an explicit number only when an operator asked for one, so a release can
	// move the default without rewriting rows.
	MaxCatchUp     int
	Origin         Origin
	OriginRef      string
	PausedAt       *time.Time
	LastFiredAt    *time.Time
	IdempotencyKey string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// CatchUpLimit resolves the schedule's catch-up bound, falling back to the
// package default when the row carries none.
func (s Schedule) CatchUpLimit() int {
	if s.MaxCatchUp <= 0 {
		return MaxCatchUp
	}
	return s.MaxCatchUp
}

// Paused reports whether this schedule is held back. A paused schedule is not
// armed at all, so it has no next occurrence.
func (s Schedule) Paused() bool { return s.PausedAt != nil }

// LegacyCadence reports whether this row is the single job-owned cadence that
// v0.3.0's migration or PUT endpoint wrote. Such a row keeps the old promise --
// the store owns the cadence and a reload never reconciles a manifest over it --
// and it is why reconciliation must not add a second, duplicate manifest row to
// a job that already carries one.
func (s Schedule) LegacyCadence() bool {
	return s.Origin == OriginAPI &&
		(s.OriginRef == LegacyRefMigrated || s.OriginRef == LegacyRefPut)
}

// Location resolves the schedule's time zone.
func (s Schedule) Location() (*time.Location, error) {
	return time.LoadLocation(s.Timezone)
}

// CreateInput describes a new schedule.
type CreateInput struct {
	JobID        string
	Cron         string
	Timezone     string
	Payload      json.RawMessage
	MissedPolicy MissedPolicy
	// MaxCatchUp is the schedule's own catch-up bound. Zero means the daemon
	// default, which is what a caller that does not ask for one gets.
	MaxCatchUp     int
	Origin         Origin
	OriginRef      string
	IdempotencyKey string
	// ID lets a reconciliation supply a stable id. An empty value mints one.
	ID string
}

// UpdateInput is a partial change. A nil field is left alone.
type UpdateInput struct {
	Cron         *string
	Timezone     *string
	Payload      *json.RawMessage
	MissedPolicy *MissedPolicy
	MaxCatchUp   *int
}

// ManifestSpec is one schedule a manifest declares for a job.
type ManifestSpec struct {
	// Ref is the schedule's stable identifier inside the manifest. It is how a
	// reordered list does not churn rows; empty falls back to position.
	Ref      string
	Cron     string
	Timezone string
	Payload  json.RawMessage
	// MissedPolicy and MaxCatchUp are optional in the manifest. Their absence
	// means the same as an API create that omitted them: skip, daemon default.
	MissedPolicy MissedPolicy
	MaxCatchUp   int
}

// ReconcileResult reports what a manifest reconciliation changed.
type ReconcileResult struct {
	Added   int
	Updated int
	Removed int
}

// Store is the durable schedule table plus an in-memory mirror of it.
//
// The daemon holds an exclusive lock on its data directory, so it is the only
// writer and the mirror cannot go stale behind its back. The mirror is what
// lets a reload decide every job's cadence without a database read.
type Store struct {
	db *sql.DB

	mu    sync.RWMutex
	byID  map[string]Schedule
	byJob map[string][]string
	byKey map[string]string // idempotency key -> schedule id
}

const scheduleColumns = `id, job_id, cron, timezone, payload, config_version,
	missed_policy, max_catch_up, origin, origin_ref, paused_at, last_fired_at,
	idempotency_key, created_at, updated_at`

// NewStore reads the schedule table into memory and returns a Store over db.
func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	s := &Store{
		db:    db,
		byID:  map[string]Schedule{},
		byJob: map[string][]string{},
		byKey: map[string]string{},
	}

	rows, err := db.QueryContext(ctx, `SELECT `+scheduleColumns+` FROM schedules`)
	if err != nil {
		return nil, fmt.Errorf("schedule: read schedules: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		rec, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("schedule: scan schedule: %w", err)
		}
		s.putLocked(rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schedule: read schedules: %w", err)
	}
	return s, nil
}

// Get returns one schedule by id.
func (s *Store) Get(id string) (Schedule, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.byID[id]
	return rec, ok
}

// ForJob returns every schedule for a job, ordered by creation time and then
// id, so the order is stable across reads.
func (s *Store) ForJob(jobID string) []Schedule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.forJobLocked(jobID)
}

func (s *Store) forJobLocked(jobID string) []Schedule {
	ids := s.byJob[jobID]
	out := make([]Schedule, 0, len(ids))
	for _, id := range ids {
		if rec, ok := s.byID[id]; ok {
			out = append(out, rec)
		}
	}
	sortSchedules(out)
	return out
}

// All returns every stored schedule.
func (s *Store) All() []Schedule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Schedule, 0, len(s.byID))
	for _, rec := range s.byID {
		out = append(out, rec)
	}
	sortSchedules(out)
	return out
}

// Len reports how many schedules exist.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byID)
}

// LegacyCadenceForJob reports whether a job still carries the single
// job-owned cadence v0.3.0 wrote. Such a job is not reconciled from its
// manifest, so the migration cannot leave it with two rows firing the same
// trigger.
func (s *Store) LegacyCadenceForJob(jobID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rec := range s.forJobLocked(jobID) {
		if rec.LegacyCadence() {
			return true
		}
	}
	return false
}

// Create inserts a schedule and returns it. When the input carries an
// idempotency key that already exists, it returns the existing row and false --
// a retried command does not create a second schedule.
func (s *Store) Create(ctx context.Context, in CreateInput) (Schedule, bool, error) {
	if strings.TrimSpace(in.JobID) == "" {
		return Schedule{}, false, fmt.Errorf("%w: job id is required", ErrInvalid)
	}
	if _, err := scheduler.Parse(in.Cron); err != nil {
		return Schedule{}, false, fmt.Errorf("%w: invalid cron expression %q: %v", ErrInvalid, in.Cron, err)
	}
	tz, err := normalizeTimezone(in.Timezone)
	if err != nil {
		return Schedule{}, false, err
	}
	payload, err := normalizePayload(in.Payload)
	if err != nil {
		return Schedule{}, false, err
	}
	policy := in.MissedPolicy
	if policy == "" {
		policy = MissedSkip
	}
	if !policy.Valid() {
		return Schedule{}, false, fmt.Errorf("%w: unknown missed_policy %q", ErrInvalid, policy)
	}
	// A catch-up bound is only meaningful for catch_up. Refusing it elsewhere
	// would make a policy change and a bound cleanup two edits; ignoring it
	// would store a number that does nothing. It is normalized away instead, so
	// the row says what it means.
	maxCatchUp, err := normalizeMaxCatchUp(policy, in.MaxCatchUp)
	if err != nil {
		return Schedule{}, false, err
	}
	origin := in.Origin
	if origin == "" {
		origin = OriginAPI
	}
	if !origin.Valid() {
		return Schedule{}, false, fmt.Errorf("%w: unknown origin %q", ErrInvalid, origin)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if in.IdempotencyKey != "" {
		if id, ok := s.byKey[in.IdempotencyKey]; ok {
			if rec, ok := s.byID[id]; ok {
				return rec, false, nil
			}
		}
	}

	now := time.Now().UTC()
	rec := Schedule{
		ID:             in.ID,
		JobID:          in.JobID,
		Cron:           in.Cron,
		Timezone:       tz,
		Payload:        payload,
		MissedPolicy:   policy,
		MaxCatchUp:     maxCatchUp,
		Origin:         origin,
		OriginRef:      in.OriginRef,
		IdempotencyKey: in.IdempotencyKey,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if rec.ID == "" {
		rec.ID = uuid.NewString()
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO schedules (`+scheduleColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.JobID, rec.Cron, rec.Timezone, string(rec.Payload),
		database.NullableString(rec.ConfigVersion), string(rec.MissedPolicy), rec.MaxCatchUp,
		string(rec.Origin), rec.OriginRef,
		database.FormatNullable(rec.PausedAt), database.FormatNullable(rec.LastFiredAt),
		database.NullableString(rec.IdempotencyKey),
		database.FormatTime(rec.CreatedAt), database.FormatTime(rec.UpdatedAt))
	if err != nil {
		if in.IdempotencyKey != "" {
			// A concurrent insert won the key. Read its row rather than
			// refusing: the caller's retry has already succeeded.
			if existing, ok := s.lookupKeyLocked(ctx, in.IdempotencyKey); ok {
				return existing, false, nil
			}
		}
		return Schedule{}, false, fmt.Errorf("schedule: create %s: %w", rec.JobID, err)
	}
	s.putLocked(rec)
	return rec, true, nil
}

// lookupKeyLocked resolves an idempotency key against the table, for the narrow
// race a unique-index conflict reveals. It is called with s.mu held.
func (s *Store) lookupKeyLocked(ctx context.Context, key string) (Schedule, bool) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+scheduleColumns+` FROM schedules WHERE idempotency_key = ?`, key)
	rec, err := scanSchedule(row)
	if err != nil {
		return Schedule{}, false
	}
	s.putLocked(rec)
	return rec, true
}

// Update applies a partial change to an API-owned schedule. A manifest-owned
// row refuses the change: the file is its owner.
func (s *Store) Update(ctx context.Context, id string, in UpdateInput) (Schedule, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.byID[id]
	if !ok {
		return Schedule{}, false, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if rec.Origin == OriginManifest {
		return Schedule{}, false, fmt.Errorf("%w: %s", ErrManifestOwned, id)
	}

	changed := false
	if in.Cron != nil {
		if _, err := scheduler.Parse(*in.Cron); err != nil {
			return Schedule{}, false, fmt.Errorf("%w: invalid cron expression %q: %v", ErrInvalid, *in.Cron, err)
		}
		if rec.Cron != *in.Cron {
			rec.Cron = *in.Cron
			changed = true
		}
	}
	if in.Timezone != nil {
		tz, err := normalizeTimezone(*in.Timezone)
		if err != nil {
			return Schedule{}, false, err
		}
		if rec.Timezone != tz {
			rec.Timezone = tz
			changed = true
		}
	}
	if in.Payload != nil {
		payload, err := normalizePayload(*in.Payload)
		if err != nil {
			return Schedule{}, false, err
		}
		if string(rec.Payload) != string(payload) {
			rec.Payload = payload
			changed = true
		}
	}
	if in.MissedPolicy != nil {
		if !in.MissedPolicy.Valid() {
			return Schedule{}, false, fmt.Errorf("%w: unknown missed_policy %q", ErrInvalid, *in.MissedPolicy)
		}
		if rec.MissedPolicy != *in.MissedPolicy {
			rec.MissedPolicy = *in.MissedPolicy
			changed = true
		}
	}
	if in.MaxCatchUp != nil {
		// Validated against the policy the row will end up with, not the one it
		// had: a request that sets catch_up and its bound in one call must be
		// judged on the result.
		limit, err := normalizeMaxCatchUp(rec.MissedPolicy, *in.MaxCatchUp)
		if err != nil {
			return Schedule{}, false, err
		}
		if rec.MaxCatchUp != limit {
			rec.MaxCatchUp = limit
			changed = true
		}
	} else if in.MissedPolicy != nil && rec.MissedPolicy != MissedCatchUp && rec.MaxCatchUp != 0 {
		// Moving away from catch_up drops a bound that no longer applies, so a
		// later return to catch_up starts from the default rather than
		// resurrecting a stale number.
		rec.MaxCatchUp = 0
		changed = true
	}
	if !changed {
		return rec, false, nil
	}

	rec.UpdatedAt = time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE schedules SET cron = ?, timezone = ?, payload = ?, missed_policy = ?, max_catch_up = ?, updated_at = ?
		 WHERE id = ?`,
		rec.Cron, rec.Timezone, string(rec.Payload), string(rec.MissedPolicy), rec.MaxCatchUp,
		database.FormatTime(rec.UpdatedAt), rec.ID); err != nil {
		return Schedule{}, false, fmt.Errorf("schedule: update %s: %w", id, err)
	}
	s.putLocked(rec)
	return rec, true, nil
}

// SetPaused holds a schedule back or releases it. Pausing is an operator
// control, not a lifecycle change, so it is allowed on a manifest-owned row:
// reconciliation preserves paused_at across a reload, which is what keeps the
// two owners from fighting.
func (s *Store) SetPaused(ctx context.Context, id string, paused bool) (Schedule, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.byID[id]
	if !ok {
		return Schedule{}, false, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if rec.Paused() == paused {
		return rec, false, nil
	}

	now := time.Now().UTC()
	if paused {
		rec.PausedAt = &now
	} else {
		rec.PausedAt = nil
	}
	rec.UpdatedAt = now
	if _, err := s.db.ExecContext(ctx,
		`UPDATE schedules SET paused_at = ?, updated_at = ? WHERE id = ?`,
		database.FormatNullable(rec.PausedAt), database.FormatTime(rec.UpdatedAt), rec.ID); err != nil {
		return Schedule{}, false, fmt.Errorf("schedule: pause %s: %w", id, err)
	}
	s.putLocked(rec)
	return rec, true, nil
}

// SetLegacyCadence reads or replaces the single job-owned cadence that v0.3.0's
// `PUT/DELETE /v1/jobs/{id}/schedule` and `otter schedule set|clear` address.
//
// An empty cron is legitimate here and is the whole point: it records "the
// operator cleared this job's cadence" so a later reload cannot reseed it from
// the manifest. A new-API caller never produces an empty cron; the endpoint that
// does is the deprecated one, and it is the reason this method exists beside the
// stricter Create and Update.
func (s *Store) SetLegacyCadence(ctx context.Context, jobID, cron string) (Schedule, bool, error) {
	if cron != "" {
		if _, err := scheduler.Parse(cron); err != nil {
			return Schedule{}, false, fmt.Errorf("%w: invalid cron expression %q: %v", ErrInvalid, cron, err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var target *Schedule
	for _, rec := range s.forJobLocked(jobID) {
		if rec.Origin != OriginAPI {
			continue
		}
		// Prefer the row the legacy endpoint owns; otherwise adopt the job's
		// first API row, which is the migrated v0.3.0 cadence.
		if rec.OriginRef == LegacyRefPut {
			chosen := rec
			target = &chosen
			break
		}
		if target == nil {
			chosen := rec
			target = &chosen
		}
	}

	now := time.Now().UTC()
	if target != nil {
		if target.Cron == cron {
			return *target, false, nil
		}
		rec := *target
		rec.Cron = cron
		rec.UpdatedAt = now
		if _, err := s.db.ExecContext(ctx,
			`UPDATE schedules SET cron = ?, updated_at = ? WHERE id = ?`,
			rec.Cron, database.FormatTime(rec.UpdatedAt), rec.ID); err != nil {
			return Schedule{}, false, fmt.Errorf("schedule: set cadence %s: %w", jobID, err)
		}
		s.putLocked(rec)
		return rec, true, nil
	}

	rec := Schedule{
		ID:           uuid.NewString(),
		JobID:        jobID,
		Cron:         cron,
		Timezone:     DefaultTimezone,
		Payload:      json.RawMessage("{}"),
		MissedPolicy: MissedSkip,
		Origin:       OriginAPI,
		OriginRef:    LegacyRefPut,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO schedules (`+scheduleColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.JobID, rec.Cron, rec.Timezone, string(rec.Payload),
		nil, string(rec.MissedPolicy), rec.MaxCatchUp, string(rec.Origin), rec.OriginRef,
		nil, nil, nil,
		database.FormatTime(rec.CreatedAt), database.FormatTime(rec.UpdatedAt)); err != nil {
		return Schedule{}, false, fmt.Errorf("schedule: create cadence %s: %w", jobID, err)
	}
	s.putLocked(rec)
	return rec, true, nil
}

// Delete removes one API-owned schedule. A manifest-owned row is refused.
func (s *Store) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if rec.Origin == OriginManifest {
		return fmt.Errorf("%w: %s", ErrManifestOwned, id)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM schedule_fires WHERE schedule_id = ?`, id); err != nil {
		return fmt.Errorf("schedule: delete fires %s: %w", id, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id); err != nil {
		return fmt.Errorf("schedule: delete %s: %w", id, err)
	}
	s.removeLocked(id)
	return nil
}

// DeleteForJob forgets every schedule of a job, whatever its origin. It is
// called when an identity is purged, so a deleted job leaves nothing behind.
func (s *Store) DeleteForJob(ctx context.Context, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM schedule_fires WHERE schedule_id IN (SELECT id FROM schedules WHERE job_id = ?)`,
		jobID); err != nil {
		return fmt.Errorf("schedule: delete fires for job %s: %w", jobID, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("schedule: delete job %s: %w", jobID, err)
	}
	for _, id := range append([]string(nil), s.byJob[jobID]...) {
		s.removeLocked(id)
	}
	return nil
}

// ReconcileManifest makes a job's manifest-owned rows match specs exactly. Rows
// with origin = 'api' are never read, changed or deleted here.
//
// A spec matches an existing row by its stable Ref first. Only when a spec has
// no Ref does position decide, and that is a weaker identity: reordering a list
// without Refs resets last_fired_at and can skip or repeat an occurrence.
func (s *Store) ReconcileManifest(ctx context.Context, jobID string, specs []ManifestSpec) (ReconcileResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing := make([]Schedule, 0)
	for _, rec := range s.forJobLocked(jobID) {
		if rec.Origin == OriginManifest {
			existing = append(existing, rec)
		}
	}

	var result ReconcileResult
	matched := map[string]bool{}
	var toPut []Schedule
	var toRemove []string

	if err := s.withTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		for i, spec := range specs {
			tz, err := normalizeTimezone(spec.Timezone)
			if err != nil {
				return err
			}
			payload, err := normalizePayload(spec.Payload)
			if err != nil {
				return err
			}
			// A manifest schedule carries its policy too. Absence means skip and
			// the daemon default, exactly as an API create that omitted them.
			policy := spec.MissedPolicy
			if policy == "" {
				policy = MissedSkip
			}
			if !policy.Valid() {
				return fmt.Errorf("%w: unknown missed_policy %q", ErrInvalid, policy)
			}
			maxCatchUp, err := normalizeMaxCatchUp(policy, spec.MaxCatchUp)
			if err != nil {
				return err
			}

			var current *Schedule
			if spec.Ref != "" {
				for j := range existing {
					if existing[j].OriginRef == spec.Ref && !matched[existing[j].ID] {
						current = &existing[j]
						break
					}
				}
			}
			if current == nil && i < len(existing) && !matched[existing[i].ID] {
				current = &existing[i]
			}

			ref := spec.Ref
			if ref == "" {
				ref = fmt.Sprintf("position:%d", i)
			}

			if current == nil {
				rec := Schedule{
					ID:           uuid.NewString(),
					JobID:        jobID,
					Cron:         spec.Cron,
					Timezone:     tz,
					Payload:      payload,
					MissedPolicy: policy,
					MaxCatchUp:   maxCatchUp,
					Origin:       OriginManifest,
					OriginRef:    ref,
					CreatedAt:    now,
					UpdatedAt:    now,
				}
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO schedules (`+scheduleColumns+`)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					rec.ID, rec.JobID, rec.Cron, rec.Timezone, string(rec.Payload),
					nil, string(rec.MissedPolicy), rec.MaxCatchUp, string(rec.Origin), rec.OriginRef,
					nil, nil, nil,
					database.FormatTime(rec.CreatedAt), database.FormatTime(rec.UpdatedAt)); err != nil {
					return fmt.Errorf("schedule: reconcile insert %s: %w", jobID, err)
				}
				result.Added++
				toPut = append(toPut, rec)
				continue
			}

			matched[current.ID] = true

			if current.Cron == spec.Cron && current.Timezone == tz &&
				string(current.Payload) == string(payload) && current.OriginRef == ref &&
				current.MissedPolicy == policy && current.MaxCatchUp == maxCatchUp {
				continue
			}
			updated := *current
			updated.Cron = spec.Cron
			updated.Timezone = tz
			updated.Payload = payload
			updated.OriginRef = ref
			updated.MissedPolicy = policy
			updated.MaxCatchUp = maxCatchUp
			updated.UpdatedAt = now
			if _, err := tx.ExecContext(ctx,
				`UPDATE schedules SET cron = ?, timezone = ?, payload = ?, origin_ref = ?,
				     missed_policy = ?, max_catch_up = ?, updated_at = ?
				 WHERE id = ?`,
				updated.Cron, updated.Timezone, string(updated.Payload), updated.OriginRef,
				string(updated.MissedPolicy), updated.MaxCatchUp,
				database.FormatTime(updated.UpdatedAt), updated.ID); err != nil {
				return fmt.Errorf("schedule: reconcile update %s: %w", updated.ID, err)
			}
			result.Updated++
			toPut = append(toPut, updated)
		}

		for i := range existing {
			if matched[existing[i].ID] {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, existing[i].ID); err != nil {
				return fmt.Errorf("schedule: reconcile delete %s: %w", existing[i].ID, err)
			}
			result.Removed++
			toRemove = append(toRemove, existing[i].ID)
		}
		return nil
	}); err != nil {
		return ReconcileResult{}, err
	}

	// The mirror moves only after every statement committed, so a failed
	// reconciliation leaves the in-memory view exactly as the database is.
	for _, rec := range toPut {
		s.putLocked(rec)
	}
	for _, id := range toRemove {
		s.removeLocked(id)
	}

	return result, nil
}

// RecordFireTx writes one occurrence to the ledger inside the caller's
// transaction -- the same transaction that creates the run. It reports false
// when the occurrence already fired, in which case the caller must abort: the
// duplicate loses to the primary key rather than producing a second run.
func (s *Store) RecordFireTx(ctx context.Context, tx *sql.Tx, scheduleID string, occurrence time.Time, runID string) (bool, error) {
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO schedule_fires (schedule_id, occurrence_at, run_id, fired_at)
		 VALUES (?, ?, ?, ?) ON CONFLICT(schedule_id, occurrence_at) DO NOTHING`,
		scheduleID, database.FormatTime(occurrence),
		runID, database.FormatTime(now))
	if err != nil {
		return false, fmt.Errorf("schedule: record fire %s: %w", scheduleID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("schedule: record fire %s: %w", scheduleID, err)
	}
	if affected == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE schedules SET last_fired_at = ? WHERE id = ?`,
		database.FormatTime(now), scheduleID); err != nil {
		return false, fmt.Errorf("schedule: mark fired %s: %w", scheduleID, err)
	}
	return true, nil
}

// NoteFired updates the in-memory mirror after a fire transaction commits, so a
// reader sees the new last_fired_at without a database round trip.
func (s *Store) NoteFired(scheduleID string, occurrence time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[scheduleID]
	if !ok {
		return
	}
	at := occurrence.UTC()
	rec.LastFiredAt = &at
	s.byID[scheduleID] = rec
}

// FireRecorded reports whether an occurrence is already in the ledger. It
// exists for tests and diagnostics rather than for the fire path, which relies
// on the primary key.
func (s *Store) FireRecorded(ctx context.Context, scheduleID string, occurrence time.Time) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schedule_fires WHERE schedule_id = ? AND occurrence_at = ?`,
		scheduleID, database.FormatTime(occurrence)).Scan(&n); err != nil {
		return false, fmt.Errorf("schedule: read fire %s: %w", scheduleID, err)
	}
	return n > 0, nil
}

// PruneFires drops ledger rows whose occurrence is older than before, keeping
// dedup working for anything still live. It returns how many rows went.
func (s *Store) PruneFires(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM schedule_fires WHERE occurrence_at < ?`, database.FormatTime(before))
	if err != nil {
		return 0, fmt.Errorf("schedule: prune fires: %w", err)
	}
	return res.RowsAffected()
}

// withTx runs fn in a transaction, rolling back on error.
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("schedule: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("schedule: commit: %w", err)
	}
	return nil
}

// putLocked inserts or replaces a row in the mirror. Called with s.mu held.
func (s *Store) putLocked(rec Schedule) {
	if old, ok := s.byID[rec.ID]; ok {
		if old.IdempotencyKey != "" && old.IdempotencyKey != rec.IdempotencyKey {
			delete(s.byKey, old.IdempotencyKey)
		}
		// The job may have changed (it never does today, but a future move
		// could), so drop it from the old job's index first.
		if old.JobID != rec.JobID {
			s.unindexJobLocked(old.JobID, rec.ID)
		}
	}
	if _, ok := s.byID[rec.ID]; !ok {
		s.byJob[rec.JobID] = append(s.byJob[rec.JobID], rec.ID)
	}
	s.byID[rec.ID] = rec
	if rec.IdempotencyKey != "" {
		s.byKey[rec.IdempotencyKey] = rec.ID
	}
}

// removeLocked forgets a row in the mirror. Called with s.mu held.
func (s *Store) removeLocked(id string) {
	rec, ok := s.byID[id]
	if !ok {
		return
	}
	delete(s.byID, id)
	if rec.IdempotencyKey != "" {
		delete(s.byKey, rec.IdempotencyKey)
	}
	s.unindexJobLocked(rec.JobID, id)
}

func (s *Store) unindexJobLocked(jobID, id string) {
	ids := s.byJob[jobID]
	for i, existing := range ids {
		if existing == id {
			s.byJob[jobID] = append(ids[:i], ids[i+1:]...)
			break
		}
	}
	if len(s.byJob[jobID]) == 0 {
		delete(s.byJob, jobID)
	}
}

// sortSchedules orders by job, then creation time, then id. The id tiebreak
// keeps the order total, so two reads of an unchanged set agree.
func sortSchedules(out []Schedule) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].JobID != out[j].JobID {
			return out[i].JobID < out[j].JobID
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
}

// scanSchedule reads one row in scheduleColumns order.
func scanSchedule(sc interface{ Scan(...any) error }) (Schedule, error) {
	var (
		rec                     Schedule
		payload, policy, origin string
		originRef               string
		configVer, idem         sql.NullString
		createdAt, updatedAt    string
		pausedAt, lastFired     database.NullableTime
	)
	if err := sc.Scan(&rec.ID, &rec.JobID, &rec.Cron, &rec.Timezone, &payload,
		&configVer, &policy, &rec.MaxCatchUp, &origin, &originRef, &pausedAt, &lastFired,
		&idem, &createdAt, &updatedAt); err != nil {
		return Schedule{}, err
	}
	rec.Payload = json.RawMessage(payload)
	rec.ConfigVersion = configVer.String
	rec.MissedPolicy = MissedPolicy(policy)
	rec.Origin = Origin(origin)
	rec.OriginRef = originRef
	rec.PausedAt = pausedAt.Ptr()
	rec.LastFiredAt = lastFired.Ptr()
	rec.IdempotencyKey = idem.String
	var err error
	if rec.CreatedAt, err = database.ParseTime(createdAt); err != nil {
		return Schedule{}, fmt.Errorf("%s created_at: %w", rec.ID, err)
	}
	if rec.UpdatedAt, err = database.ParseTime(updatedAt); err != nil {
		return Schedule{}, fmt.Errorf("%s updated_at: %w", rec.ID, err)
	}
	return rec, nil
}

// normalizeTimezone resolves a zone name, defaulting an empty value to UTC.
func normalizeTimezone(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return DefaultTimezone, nil
	}
	if _, err := time.LoadLocation(name); err != nil {
		return "", fmt.Errorf("%w: unknown timezone %q", ErrInvalid, name)
	}
	return name, nil
}

// normalizeMaxCatchUp validates a requested catch-up bound against the policy it
// would apply to.
//
// The bound is only meaningful for catch_up, so a value supplied for any other
// policy is normalized to zero -- "use the daemon default" -- rather than
// stored, where it would look like configuration that does something. A
// negative or absurd bound is refused rather than silently clamped: the caller
// asked for something the runtime will not do.
func normalizeMaxCatchUp(policy MissedPolicy, requested int) (int, error) {
	if requested < 0 {
		return 0, fmt.Errorf("%w: max_catch_up must not be negative", ErrInvalid)
	}
	if policy != MissedCatchUp {
		return 0, nil
	}
	if requested > MaxCatchUpCeiling {
		return 0, fmt.Errorf("%w: max_catch_up %d exceeds the ceiling of %d",
			ErrInvalid, requested, MaxCatchUpCeiling)
	}
	return requested, nil
}

// normalizePayload checks that a payload is a JSON object within the size
// bound. A schedule payload is not a secret and must not hold one; it is
// printed by otter inspect and returned by the API.
func normalizePayload(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	if len(raw) > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: payload is %d bytes; the maximum is %d", ErrInvalid, len(raw), MaxPayloadBytes)
	}
	trimmed := strings.TrimSpace(string(raw))
	if !json.Valid([]byte(trimmed)) {
		return nil, fmt.Errorf("%w: payload is not valid JSON", ErrInvalid)
	}
	if !strings.HasPrefix(trimmed, "{") {
		return nil, fmt.Errorf("%w: payload must be a JSON object", ErrInvalid)
	}
	return json.RawMessage(trimmed), nil
}
