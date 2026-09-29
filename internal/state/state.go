// Package state implements Otter's durable per-job key/value store.
//
// State is what makes a job resumable: a checkpoint written by one
// run is visible to the next run, and survives job failures, daemon
// restarts and machine reboots. The authoritative copy always lives here in
// SQLite, never in the Python SDK.
package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// Sentinel errors. The API layer maps these onto HTTP status codes.
var (
	ErrNotFound     = errors.New("state key not found")
	ErrInvalidKey   = errors.New("invalid state key")
	ErrInvalidValue = errors.New("invalid state value")
)

// MaxKeyLength bounds state keys.
const MaxKeyLength = 128

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// ValidateKey checks that a key is usable in a URL path segment and as a
// database key.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: key must not be empty", ErrInvalidKey)
	}
	if len(key) > MaxKeyLength {
		return fmt.Errorf("%w: key must be at most %d characters, got %d", ErrInvalidKey, MaxKeyLength, len(key))
	}
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("%w: %q is invalid, use letters, digits, dot, dash, underscore or colon", ErrInvalidKey, key)
	}
	return nil
}

// ValidateValue checks that a value is a well-formed JSON document.
func ValidateValue(value json.RawMessage) error {
	if len(value) == 0 {
		return fmt.Errorf("%w: value must not be empty", ErrInvalidValue)
	}
	if !json.Valid(value) {
		return fmt.Errorf("%w: value is not valid JSON", ErrInvalidValue)
	}
	return nil
}

// Entry is a stored key/value pair.
type Entry struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Store is the SQLite-backed state store.
type Store struct {
	db *sql.DB
}

// NewStore wraps a database handle.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Get returns the raw JSON value for a key.
func (s *Store) Get(ctx context.Context, jobID, key string) (json.RawMessage, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	var (
		value     sql.NullString
		updatedAt database.NullableTime
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT value, updated_at FROM job_state WHERE job_id = ? AND key = ?`,
		jobID, key).Scan(&value, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get %s/%s: %w", jobID, key, err)
	}
	if !value.Valid {
		return json.RawMessage("null"), nil
	}
	return json.RawMessage(value.String), nil
}

// GetEntry returns the value together with its update timestamp.
func (s *Store) GetEntry(ctx context.Context, jobID, key string) (*Entry, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	var (
		value     sql.NullString
		updatedAt database.NullableTime
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT value, updated_at FROM job_state WHERE job_id = ? AND key = ?`,
		jobID, key).Scan(&value, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("state: get %s/%s: %w", jobID, key, err)
	}

	entry := &Entry{Key: key, Value: json.RawMessage("null")}
	if value.Valid {
		entry.Value = json.RawMessage(value.String)
	}
	if updatedAt.Valid {
		entry.UpdatedAt = updatedAt.Time
	}
	return entry, nil
}

// Set writes a value, replacing any previous value for the key.
func (s *Store) Set(ctx context.Context, jobID, key string, value json.RawMessage) (time.Time, error) {
	if err := ValidateKey(key); err != nil {
		return time.Time{}, err
	}
	if err := ValidateValue(value); err != nil {
		return time.Time{}, err
	}

	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO job_state (job_id, key, value, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(job_id, key) DO UPDATE SET
			value = excluded.value,
			updated_at = excluded.updated_at`,
		jobID, key, string(value), database.FormatTime(now))
	if err != nil {
		return time.Time{}, fmt.Errorf("state: set %s/%s: %w", jobID, key, err)
	}
	return now, nil
}

// Delete removes a key. It reports whether the key existed.
func (s *Store) Delete(ctx context.Context, jobID, key string) (bool, error) {
	if err := ValidateKey(key); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM job_state WHERE job_id = ? AND key = ?`,
		jobID, key)
	if err != nil {
		return false, fmt.Errorf("state: delete %s/%s: %w", jobID, key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("state: delete %s/%s: %w", jobID, key, err)
	}
	return n > 0, nil
}

// All returns every key/value pair for a job.
func (s *Store) All(ctx context.Context, jobID string) (map[string]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value FROM job_state WHERE job_id = ? ORDER BY key ASC`,
		jobID)
	if err != nil {
		return nil, fmt.Errorf("state: list %s: %w", jobID, err)
	}
	defer rows.Close()

	out := map[string]json.RawMessage{}
	for rows.Next() {
		var (
			key   string
			value sql.NullString
		)
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("state: list %s: %w", jobID, err)
		}
		if value.Valid {
			out[key] = json.RawMessage(value.String)
		} else {
			out[key] = json.RawMessage("null")
		}
	}
	return out, rows.Err()
}

// DeleteAll clears the state of one job and returns how many keys
// were removed.
func (s *Store) DeleteAll(ctx context.Context, jobID string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM job_state WHERE job_id = ?`, jobID)
	if err != nil {
		return 0, fmt.Errorf("state: clear %s: %w", jobID, err)
	}
	return res.RowsAffected()
}
