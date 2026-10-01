// Package schedule owns per-job cadence: the answer to "when should this job
// fire", as runtime state rather than manifest state.
//
// A schedule is keyed by the durable identity id, never the label or the source
// path, for the same reason a pause is: a label need not be unique and a
// directory can move, so either would attach the cadence to the wrong job.
// Keying by identity is also what gives the lifecycle its obvious behaviour --
// `move` carries the schedule, `reset` starts with none, `delete` purges it.
//
// The store is the single source of truth once a job has a row. A manifest's
// `trigger.cron` is imported by Seed on first sight and then ignored, so the
// file can never contradict what the daemon actually does.
package schedule

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// Schedule is one job's cadence.
//
// Cron is empty when the operator has cleared the schedule. That is a
// deliberate "never fire on its own", and it is different from having no row at
// all -- which is what lets a manifest seed the schedule once.
type Schedule struct {
	JobID     string
	Cron      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store is the durable schedule table plus an in-memory mirror of it.
//
// The daemon holds an exclusive lock on its data directory, so it is the only
// writer and the mirror cannot go stale behind its back. The mirror is what
// lets a reload decide every job's cadence without a database read.
type Store struct {
	db *sql.DB

	mu        sync.RWMutex
	schedules map[string]Schedule
}

// NewStore reads the schedule table into memory and returns a Store over db.
func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	s := &Store{db: db, schedules: map[string]Schedule{}}

	rows, err := db.QueryContext(ctx,
		`SELECT job_id, cron, created_at, updated_at FROM job_schedules`)
	if err != nil {
		return nil, fmt.Errorf("schedule: read schedules: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			rec                  Schedule
			createdAt, updatedAt string
		)
		if err := rows.Scan(&rec.JobID, &rec.Cron, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("schedule: scan schedule: %w", err)
		}
		if rec.CreatedAt, err = database.ParseTime(createdAt); err != nil {
			return nil, fmt.Errorf("schedule: %s created_at: %w", rec.JobID, err)
		}
		if rec.UpdatedAt, err = database.ParseTime(updatedAt); err != nil {
			return nil, fmt.Errorf("schedule: %s updated_at: %w", rec.JobID, err)
		}
		s.schedules[rec.JobID] = rec
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schedule: read schedules: %w", err)
	}
	return s, nil
}

// Get returns one job's schedule. The bool is false when the job has no row,
// which is not an error: most jobs have never been given a schedule.
func (s *Store) Get(jobID string) (Schedule, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.schedules[jobID]
	return rec, ok
}

// Len reports how many jobs have a schedule row, cleared or not.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.schedules)
}

// All returns every stored schedule, ordered by job id.
func (s *Store) All() []Schedule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Schedule, 0, len(s.schedules))
	for _, rec := range s.schedules {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobID < out[j].JobID })
	return out
}

// Set writes a job's cadence and reports the resulting schedule plus whether
// anything changed.
//
// An empty cron is a legitimate value: it clears the schedule without deleting
// the row, so a later reload cannot seed it back from the manifest.
func (s *Store) Set(ctx context.Context, jobID, cron string) (Schedule, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	if current, ok := s.schedules[jobID]; ok {
		if current.Cron == cron {
			return current, false, nil
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE job_schedules SET cron = ?, updated_at = ? WHERE job_id = ?`,
			cron, database.FormatTime(now), jobID); err != nil {
			return Schedule{}, false, fmt.Errorf("schedule: set %s: %w", jobID, err)
		}
		current.Cron = cron
		current.UpdatedAt = now
		s.schedules[jobID] = current
		return current, true, nil
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO job_schedules (job_id, cron, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		jobID, cron, database.FormatTime(now), database.FormatTime(now)); err != nil {
		return Schedule{}, false, fmt.Errorf("schedule: set %s: %w", jobID, err)
	}
	rec := Schedule{JobID: jobID, Cron: cron, CreatedAt: now, UpdatedAt: now}
	s.schedules[jobID] = rec
	return rec, true, nil
}

// Seed imports a manifest-declared cadence for a job that has no row yet. It
// reports whether it inserted.
//
// This is the whole of the migration: a job's trigger.cron is imported once, on
// first sight, and the store owns the value from then on. Seed never overwrites
// a row, so a cadence set through the API -- or explicitly cleared -- is never
// undone by a reload.
func (s *Store) Seed(ctx context.Context, jobID, cron string) (bool, error) {
	if cron == "" {
		return false, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.schedules[jobID]; ok {
		return false, nil
	}

	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO job_schedules (job_id, cron, created_at, updated_at)
		 VALUES (?, ?, ?, ?) ON CONFLICT(job_id) DO NOTHING`,
		jobID, cron, database.FormatTime(now), database.FormatTime(now))
	if err != nil {
		return false, fmt.Errorf("schedule: seed %s: %w", jobID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("schedule: seed %s: %w", jobID, err)
	}
	if affected == 0 {
		// Another writer won the race; leave its value alone.
		return false, nil
	}
	s.schedules[jobID] = Schedule{JobID: jobID, Cron: cron, CreatedAt: now, UpdatedAt: now}
	return true, nil
}

// Delete forgets a job's schedule. It is called when an identity is purged, so
// a deleted job leaves nothing behind.
//
// A missing row is not an error: purging a job that never had a schedule is the
// common case.
func (s *Store) Delete(ctx context.Context, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM job_schedules WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("schedule: delete %s: %w", jobID, err)
	}
	delete(s.schedules, jobID)
	return nil
}
