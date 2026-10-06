// Package agent implements the runtime agent's side of the pull protocol in
// otter-platform/hosting/docs/agent-protocol.md.
//
// Everything here is the *runtime's* half. The control plane answers; the agent
// asks. Nothing in this package accepts an inbound connection from Cloud, which
// is decision D1 and the reason the protocol is shaped the way it is.
package agent

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Bootstrap is the proof a host presents to obtain its first agent credential.
//
// The protocol is deliberately abstract about *how* a host proves its identity,
// because that mechanism is environment-specific and the plan expects a non-AWS
// one later. What is fixed is the shape: a provider name, a signed assertion the
// control plane can verify, and the identity claims the control plane binds the
// registration to.
//
// The security property that matters: an agent credential is issued only to a
// host the control plane can independently identify, and the registration is
// bound to that identity. A stolen credential is useful only from the host it
// was issued to, and it can be revoked when that host is replaced.
type Bootstrap interface {
	// Provider names the mechanism, e.g. "aws-instance-role" or "static-token".
	// The control plane refuses a provider it does not have a verifier for,
	// rather than falling back to something weaker.
	Provider() string

	// Assert produces the signed material the control plane verifies. For AWS
	// this is a SigV4-signed request to the identity endpoint, using the host's
	// instance role: no long-lived secret is stored on disk, which is the point.
	Assert(ctx context.Context) (*Assertion, error)
}

// Assertion is a provider's proof of identity plus the claims it makes about
// itself. The claims are advisory: the control plane verifies them against the
// signature and its own record of what the host should be. A host asserting a
// role it does not have must fail verification, not be trusted because it asked
// nicely.
type Assertion struct {
	Provider string            `json:"provider"`
	Claims   map[string]string `json:"claims"`
	// SignedAt lets the control plane reject a replayed assertion. The window is
	// short: an assertion is a request to authenticate, not a durable token.
	SignedAt time.Time `json:"signed_at"`
	// Material is provider-specific, and is never logged. For AWS it carries the
	// SigV4 headers, which are derived from the instance role's credentials.
	Material string `json:"material"`
}

// Credential is the short-lived agent credential an assertion is exchanged for.
type Credential struct {
	Token     string    `json:"token"`
	RuntimeID string    `json:"runtime_id"`
	ExpiresAt time.Time `json:"expires_at"`
	// Generation is the control plane's current generation for this runtime at
	// the moment of issue, so an agent that has been away can detect staleness
	// without a second round trip.
	Generation int64 `json:"generation"`
}

// ErrBootstrapUnavailable means the host cannot prove its identity right now --
// metadata unreachable, credentials not yet issued, and so on. It is distinct
// from a refusal: unavailable is retried, refused is not.
var ErrBootstrapUnavailable = errors.New("agent: bootstrap identity unavailable")

// ErrBootstrapRefused means the control plane rejected the assertion. Retrying
// the same assertion does not help, so the agent must not hammer it; this is the
// condition an operator sees when a host is not the one the control plane
// expects.
var ErrBootstrapRefused = errors.New("agent: bootstrap refused")

// validate keeps the invariants every provider must satisfy in one place, so a
// new provider cannot quietly omit one.
func (a *Assertion) validate(now time.Time) error {
	if a.Provider == "" {
		return fmt.Errorf("agent: assertion has no provider")
	}
	if a.Material == "" {
		return fmt.Errorf("agent: assertion has no signed material")
	}
	if len(a.Claims) == 0 {
		return fmt.Errorf("agent: assertion makes no claims to bind")
	}
	// An assertion is a fresh proof, not a token. Anything older than a couple of
	// minutes is a replay attempt or a badly skewed clock, and both should fail
	// closed.
	if a.SignedAt.IsZero() {
		return fmt.Errorf("agent: assertion is not timestamped")
	}
	if age := now.Sub(a.SignedAt); age > 2*time.Minute || age < -2*time.Minute {
		return fmt.Errorf("agent: assertion is outside the accepted window (%s)", age.Round(time.Second))
	}
	return nil
}
