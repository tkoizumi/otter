package agent

import (
	"context"
	"fmt"
)

// RuntimeSecret is the bootstrap provider for a pooled sidecar: it presents a
// per-runtime secret in the body instead of a SigV4 assertion.
//
// WHY THIS EXISTS. On a pooled host the agent runs as a sidecar sharing the
// tenant's network namespace, and the tenant's egress chain drops
// 169.254.0.0/16. The EC2 instance-role bootstrap reaches IMDS at
// 169.254.169.254, so it is unreachable from the agent, and widening the tenant
// policy to reach it would expose instance metadata to the tenant -- the exact
// hole the pool closes.
//
// It is NOT a static claim. A static SigV4 claim is self-asserted: any host
// holding the shared key could replay the recorded account/role/instance trio.
// This secret is scoped to ONE runtime and stored only as `sha256(secret)` in
// the durable assignment, so only the sidecar the operator gave it to can
// present it, and a sibling on the same host cannot.
//
// The secret is supplied out of band -- by the provisioner, to the sidecar's
// environment only -- and is never written to disk by the agent.
type RuntimeSecret struct {
	// Secret is the plaintext per-runtime bootstrap secret.
	Secret string
}

func (s *RuntimeSecret) Provider() string { return "runtime-secret" }

// Identity returns the secret as the provider's proof.
//
// It makes NO claims and carries no signing key: the control plane binds the
// issued credential to its own durable assignment, so claims from the host would
// only be something to distrust. The exchanger sees the non-empty BootstrapSecret
// and skips SigV4 entirely.
func (s *RuntimeSecret) Identity(ctx context.Context) (Identity, error) {
	_ = ctx
	if s.Secret == "" {
		// Fail closed rather than sending an empty secret, which the control
		// plane would reject anyway -- and with a less useful message.
		return Identity{}, fmt.Errorf("agent: runtime-secret bootstrap has no secret")
	}
	return Identity{Provider: s.Provider(), BootstrapSecret: s.Secret}, nil
}
