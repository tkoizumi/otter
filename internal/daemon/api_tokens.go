package daemon

import (
	"context"

	"github.com/tkoizumi/otter/internal/api"
)

// The runtime's named, scoped operator credentials live in the apitoken store.
// These methods are the daemon's edge of the api.Backend interface.
//
// There is no in-memory mirror, deliberately: a token is read on every request
// so that a revocation takes effect immediately rather than at the next restart.

// CreateAPIToken mints a named, scoped token. It is the only moment the token
// itself is readable.
func (d *Daemon) CreateAPIToken(ctx context.Context, name string, scope api.Scope) (api.APITokenCreated, error) {
	return d.apiTokens.Create(ctx, name, scope)
}

// ListAPITokens lists the named tokens, revoked ones included.
func (d *Daemon) ListAPITokens(ctx context.Context) ([]api.APITokenView, error) {
	return d.apiTokens.List(ctx)
}

// RevokeAPIToken withdraws a token, reporting whether the id is known.
func (d *Daemon) RevokeAPIToken(ctx context.Context, id string) (bool, error) {
	return d.apiTokens.Revoke(ctx, id)
}

// ResolveAPIToken denies on a lookup failure as well as on an unknown or
// revoked token, and records which happened, so a failing database never reads
// as a bad credential.
func (d *Daemon) ResolveAPIToken(token string) (api.APIToken, bool) {
	authority, ok, err := d.apiTokens.Resolve(context.Background(), token)
	if err != nil {
		d.log.Error("api_token_lookup_failed", err)
		return api.APIToken{}, false
	}
	return authority, ok
}
