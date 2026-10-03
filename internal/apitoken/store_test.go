package apitoken_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/apitoken"
	"github.com/tkoizumi/otter/internal/database"
)

func newStore(t *testing.T) (*apitoken.Store, *database.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return apitoken.NewStore(db.DB), db
}

func TestCreateResolveAndRevoke(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)

	created, err := store.Create(ctx, "cloud-gateway", api.ScopeControl)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Token == "" || created.ID == "" {
		t.Fatalf("created token is incomplete: %+v", created)
	}

	authority, ok, err := store.Resolve(ctx, created.Token)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !ok {
		t.Fatal("a freshly minted token does not resolve")
	}
	if authority.Scope != api.ScopeControl || authority.Name != "cloud-gateway" || authority.ID != created.ID {
		t.Fatalf("authority = %+v, want the control token", authority)
	}

	known, err := store.Revoke(ctx, created.ID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !known {
		t.Fatal("revoking a known id reported it unknown")
	}

	// The "immediate" half: no restart, no cache, the next read is refused.
	if _, ok, err := store.Resolve(ctx, created.Token); err != nil {
		t.Fatalf("resolve after revoke: %v", err)
	} else if ok {
		t.Fatal("a revoked token still resolves")
	}

	// Revoking again is a success, not a 404: the operator asked for it to be
	// unusable and it is.
	if known, err := store.Revoke(ctx, created.ID); err != nil {
		t.Fatalf("re-revoke: %v", err)
	} else if !known {
		t.Fatal("re-revoking a known id reported it unknown")
	}

	if known, err := store.Revoke(ctx, "no-such-id"); err != nil {
		t.Fatalf("revoke unknown: %v", err)
	} else if known {
		t.Fatal("revoking an unknown id reported it known")
	}
}

// TestResolveRejectsARevokedTokenAcrossReopen proves the revocation is durable
// rather than a property of one process's memory.
func TestResolveRejectsARevokedTokenAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	db, err := database.Open(ctx, dir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := apitoken.NewStore(db.DB)
	created, err := store.Create(ctx, "gateway", api.ScopeControl)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Revoke(ctx, created.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := database.Open(ctx, dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if _, ok, err := apitoken.NewStore(reopened.DB).Resolve(ctx, created.Token); err != nil {
		t.Fatalf("resolve after reopen: %v", err)
	} else if ok {
		t.Fatal("a token revoked before the reopen resolves after it")
	}
}

// TestOnlyTheHashIsStored is the claim that makes a database leak survivable.
func TestOnlyTheHashIsStored(t *testing.T) {
	ctx := context.Background()
	store, db := newStore(t)

	created, err := store.Create(ctx, "gateway", api.ScopeRead)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var stored string
	if err := db.QueryRowContext(ctx,
		`SELECT token_hash FROM api_tokens WHERE id = ?`, created.ID).Scan(&stored); err != nil {
		t.Fatalf("read stored hash: %v", err)
	}

	if stored == created.Token {
		t.Fatal("the plaintext token is stored verbatim")
	}
	sum := sha256.Sum256([]byte(created.Token))
	if want := hex.EncodeToString(sum[:]); stored != want {
		t.Fatalf("stored hash = %q, want sha256 of the token (%q)", stored, want)
	}
}

func TestListReportsRevocationAndNeverTheToken(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)

	keep, err := store.Create(ctx, "castor-backend", api.ScopeRead)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	gone, err := store.Create(ctx, "old-gateway", api.ScopeControl)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Revoke(ctx, gone.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	views, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("list returned %d tokens, want 2", len(views))
	}

	byID := map[string]api.APITokenView{}
	for _, v := range views {
		byID[v.ID] = v
		if v.Name == "" || v.Scope == "" {
			t.Fatalf("token view is incomplete: %+v", v)
		}
	}

	if v := byID[keep.ID]; v.RevokedAt != nil {
		t.Fatalf("a live token reports revoked_at = %v", v.RevokedAt)
	}
	if v := byID[gone.ID]; v.RevokedAt == nil {
		t.Fatal("a revoked token reports no revoked_at")
	}
}

func TestCreateValidatesNameAndScope(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)

	cases := []struct {
		name  string
		label string
		scope api.Scope
	}{
		{"empty name", "", api.ScopeControl},
		{"blank name", "   ", api.ScopeRead},
		{"unknown scope", "gateway", api.Scope("admin")},
		{"empty scope", "gateway", api.Scope("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.Create(ctx, tc.label, tc.scope); err == nil {
				t.Fatalf("Create(%q, %q) succeeded, want an error", tc.label, tc.scope)
			}
		})
	}
}

func TestResolveRejectsAForeignString(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)

	for _, token := range []string{"", "nonsense", "otter_ctl_", "otter_ro_not-a-real-token", "otter_cap_"} {
		if _, ok, err := store.Resolve(ctx, token); err != nil {
			t.Fatalf("resolve %q: %v", token, err)
		} else if ok {
			t.Fatalf("resolve %q returned a principal", token)
		}
	}
}

// TestCaptureScopeRoundTrips is the evidence that the third scope is issued,
// stored and resolved rather than merely accepted by Valid(): each scope mints
// its own recognizable prefix, and the prefix guard admits it.
func TestCaptureScopeRoundTrips(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)

	cases := []struct {
		scope  api.Scope
		prefix string
	}{
		{api.ScopeRead, "otter_ro_"},
		{api.ScopeControl, "otter_ctl_"},
		{api.ScopeCapture, "otter_cap_"},
	}
	for _, tc := range cases {
		t.Run(string(tc.scope), func(t *testing.T) {
			created, err := store.Create(ctx, "cloud-"+string(tc.scope), tc.scope)
			if err != nil {
				t.Fatalf("create %s: %v", tc.scope, err)
			}
			if !strings.HasPrefix(created.Token, tc.prefix) {
				t.Errorf("%s token = %q, want the %q prefix", tc.scope, created.Token, tc.prefix)
			}

			authority, ok, err := store.Resolve(ctx, created.Token)
			if err != nil {
				t.Fatalf("resolve %s: %v", tc.scope, err)
			}
			if !ok {
				t.Fatalf("a freshly minted %s token does not resolve", tc.scope)
			}
			if authority.Scope != tc.scope {
				t.Errorf("resolved scope = %q, want %q", authority.Scope, tc.scope)
			}
		})
	}
}
