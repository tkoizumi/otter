// Package apitoken owns the runtime's named, scoped operator credentials.
//
// The admin token is deliberately not one of these. It stays the static
// OTTER_API_TOKEN the daemon reads from its environment, so a database
// compromise cannot mint full authority. What lives here is the narrower
// credential a gateway holds to command a runtime on a customer's behalf
// (CL-21): read, or read plus the command surface, and nothing else.
//
// Only a SHA-256 of each token is stored. The plaintext is returned once, at
// creation, and is not recoverable afterwards.
//
// Revocation is a timestamp rather than a delete. The row survives for audit,
// and every lookup filters on revoked_at IS NULL, so revocation takes effect on
// the next request with no restart and no cache to invalidate.
package apitoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/database"
)

// A minted token carries a scope-specific prefix. It makes a token recognizable
// in a log line or a paste, and it lets Resolve reject an obviously-foreign
// string before it reaches the database.
const (
	prefixRead    = "otter_ro_"
	prefixControl = "otter_ctl_"
)

// Store is the durable api_tokens table.
type Store struct {
	db *sql.DB
}

// NewStore returns a Store over db. There is no in-memory mirror: a token is
// read on every request precisely so that revocation cannot go stale.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Create mints a named, scoped token and stores only its hash. The plaintext is
// returned here and never again.
func (s *Store) Create(ctx context.Context, name string, scope api.Scope) (api.APITokenCreated, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return api.APITokenCreated{}, fmt.Errorf("%w: name is required", api.ErrInvalid)
	}
	if !scope.Valid() {
		return api.APITokenCreated{}, fmt.Errorf("%w: scope must be %q or %q",
			api.ErrInvalid, api.ScopeRead, api.ScopeControl)
	}

	token, err := mint(scope)
	if err != nil {
		return api.APITokenCreated{}, err
	}

	now := time.Now().UTC()
	created := api.APITokenCreated{
		APITokenView: api.APITokenView{
			ID:        uuid.NewString(),
			Name:      name,
			Scope:     scope,
			CreatedAt: now,
		},
		Token: token,
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO api_tokens (id, name, scope, token_hash, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		created.ID, created.Name, string(created.Scope), hash(token),
		database.FormatTime(now)); err != nil {
		return api.APITokenCreated{}, fmt.Errorf("store api token: %w", err)
	}
	return created, nil
}

// List returns every token, newest first. Revoked tokens are included so an
// operator can see what was withdrawn and when; no row reveals a token.
func (s *Store) List(ctx context.Context) ([]api.APITokenView, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, scope, created_at, revoked_at
		 FROM api_tokens
		 ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	defer rows.Close()

	out := []api.APITokenView{}
	for rows.Next() {
		var (
			id, name, scope, created string
			revoked                  database.NullableTime
		)
		if err := rows.Scan(&id, &name, &scope, &created, &revoked); err != nil {
			return nil, fmt.Errorf("scan api token: %w", err)
		}
		createdAt, err := database.ParseTime(created)
		if err != nil {
			return nil, fmt.Errorf("parse api token created_at: %w", err)
		}
		out = append(out, api.APITokenView{
			ID:        id,
			Name:      name,
			Scope:     api.Scope(scope),
			CreatedAt: createdAt,
			RevokedAt: revoked.Ptr(),
		})
	}
	return out, rows.Err()
}

// Revoke withdraws a token, reporting whether the id is known at all so the API
// can answer 404 for a typo. Revoking an already-revoked token succeeds,
// because the operator asked for it to be unusable and it is.
func (s *Store) Revoke(ctx context.Context, id string) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, fmt.Errorf("%w: token id is required", api.ErrInvalid)
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = ?
		 WHERE id = ? AND revoked_at IS NULL`,
		database.FormatTime(time.Now().UTC()), id)
	if err != nil {
		return false, fmt.Errorf("revoke api token: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return true, nil
	}

	var known int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM api_tokens WHERE id = ?`, id).Scan(&known); err != nil {
		return false, fmt.Errorf("revoke api token: %w", err)
	}
	return known > 0, nil
}

// Resolve validates a presented token. The error is returned rather than folded
// into the boolean so a caller can distinguish "not a valid credential" from
// "the lookup failed", and deny either way.
func (s *Store) Resolve(ctx context.Context, token string) (api.APIToken, bool, error) {
	if !strings.HasPrefix(token, prefixRead) && !strings.HasPrefix(token, prefixControl) {
		return api.APIToken{}, false, nil
	}

	var id, name, scope string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, scope FROM api_tokens
		 WHERE token_hash = ? AND revoked_at IS NULL`,
		hash(token)).Scan(&id, &name, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return api.APIToken{}, false, nil
	}
	if err != nil {
		return api.APIToken{}, false, fmt.Errorf("resolve api token: %w", err)
	}
	return api.APIToken{ID: id, Name: name, Scope: api.Scope(scope)}, true, nil
}

// mint generates a 256-bit token with a scope-specific prefix.
func mint(scope api.Scope) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate api token: %w", err)
	}
	prefix := prefixRead
	if scope == api.ScopeControl {
		prefix = prefixControl
	}
	return prefix + hex.EncodeToString(buf), nil
}

// hash is what the database stores. Tokens are 256-bit random values, so a
// plain SHA-256 is the right primitive: there is no low-entropy secret to
// stretch, and a per-request KDF would cost latency for nothing.
func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
