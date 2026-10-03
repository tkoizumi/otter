package api

import "time"

// Scope is the authority a named API token carries.
//
// There is deliberately no "admin" scope. Full authority stays the static
// OTTER_API_TOKEN, which lives in the daemon's environment rather than in the
// database, so a database compromise cannot mint it.
type Scope string

const (
	// ScopeRead reads runtime metadata: the job and run listings, run detail,
	// run output, the merged timeline, and capture summaries. It commands
	// nothing, and it cannot read job state or capture payloads.
	ScopeRead Scope = "read"

	// ScopeControl is ScopeRead plus the command surface a control plane
	// needs: submit a run, cancel it, pause and resume a job, and set or clear
	// its schedule. It still cannot read job state or capture payloads,
	// register or delete a job, or change the daemon's configuration.
	ScopeControl Scope = "control"
)

// Valid reports whether the scope is one this build issues.
func (s Scope) Valid() bool {
	return s == ScopeRead || s == ScopeControl
}

// APIToken is the authority a presented token resolved to.
type APIToken struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Scope Scope  `json:"scope"`
}

// APITokenView describes a token without revealing it.
type APITokenView struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scope     Scope      `json:"scope"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// APITokenCreated is the single response that carries the token itself. It is
// returned once, at creation, and is not recoverable afterwards.
type APITokenCreated struct {
	APITokenView
	Token string `json:"token"`
}

// CreateAPITokenRequest is the body of POST /v1/tokens.
type CreateAPITokenRequest struct {
	Name  string `json:"name"`
	Scope Scope  `json:"scope"`
}

// RevokeAPITokenResponse is returned by DELETE /v1/tokens/{id}.
type RevokeAPITokenResponse struct {
	ID      string `json:"id"`
	Revoked bool   `json:"revoked"`
}

// APITokenListResponse is returned by GET /v1/tokens.
type APITokenListResponse struct {
	SchemaVersion int            `json:"schema_version"`
	Tokens        []APITokenView `json:"tokens"`
}
