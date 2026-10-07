package cli

import (
	"testing"

	"github.com/tkoizumi/otter/internal/agent"
)

// agentBootstrap is the ONE place a provider is selected. The table pins every
// branch, including the pooled sidecar's `runtime-secret` mode: an unknown name
// must be refused rather than falling back, and `runtime-secret` with no secret
// must fail at start-up rather than bootstrapping with an empty proof.
func TestAgentBootstrapSelectsTheProvider(t *testing.T) {
	// The static provider is selected by its env presence, which would otherwise
	// leak in from the surrounding environment and make the table flaky.
	t.Setenv("OTTER_AGENT_STATIC_ACCESS_KEY_ID", "")
	t.Setenv("OTTER_AGENT_STATIC_SECRET_ACCESS_KEY", "")

	cases := []struct {
		name         string
		provider     string
		imdsBase     string
		secret       string
		wantProvider string
		wantErr      bool
	}{
		{name: "default falls back to the instance role", provider: "", wantProvider: "aws-instance-role"},
		{name: "aws-instance-role is explicit", provider: "aws-instance-role", wantProvider: "aws-instance-role"},
		{name: "runtime-secret with a secret", provider: "runtime-secret", secret: "shh", wantProvider: "runtime-secret"},
		{name: "runtime-secret without a secret is refused", provider: "runtime-secret", wantErr: true},
		{name: "static-credentials without a key is refused", provider: "static-credentials", wantErr: true},
		{name: "an unknown provider is refused", provider: "not-a-provider", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := agentBootstrap(tc.provider, tc.imdsBase, tc.secret)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got provider %q", b.Provider())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := b.Provider(); got != tc.wantProvider {
				t.Errorf("provider = %q, want %q", got, tc.wantProvider)
			}
		})
	}
}

// The command must carry the secret from the environment into the provider, or
// the pooled sidecar starts with an empty proof.
func TestAgentBootstrapWiresTheRuntimeSecret(t *testing.T) {
	t.Setenv("OTTER_AGENT_STATIC_ACCESS_KEY_ID", "")
	t.Setenv("OTTER_AGENT_STATIC_SECRET_ACCESS_KEY", "")

	b, err := agentBootstrap("runtime-secret", "", "from-env")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rs, ok := b.(*agent.RuntimeSecret)
	if !ok {
		t.Fatalf("provider is %T, want *agent.RuntimeSecret", b)
	}
	if rs.Secret != "from-env" {
		t.Errorf("Secret = %q, want from-env", rs.Secret)
	}
}
