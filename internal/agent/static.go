package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// StaticCredentials is a bootstrap provider for testing and for environments
// without an instance role.
//
// It exists so the authenticated path can be exercised end to end without real
// AWS credentials, and it is the mechanism the protocol document anticipates
// when it says a non-AWS bootstrap "can be added later". It is NOT a bypass: it
// produces the same kind of signed assertion an instance role does, from a key
// the operator supplied, so everything downstream -- binding, credential
// issuance, authorisation -- behaves exactly as in production.
//
// It is opt-in and refuses to be implicit. An agent that quietly preferred a
// static key over the instance role would be a credential on disk pretending to
// be an instance identity, which is the problem the instance-role bootstrap
// exists to avoid.
type StaticCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Claims          map[string]string
	Region          string
	Service         string
	// Now is injectable for tests.
	Now func() time.Time
}

func (s *StaticCredentials) Provider() string { return "static-credentials" }

// StaticCredentialsFromEnv builds a provider from OTTER_AGENT_STATIC_* variables.
//
// Returns nil when they are absent, so the caller falls back to the instance
// role rather than failing. A HALF-configured set is an error: silently using a
// partial key would sign an assertion that cannot verify, and the failure would
// look like a control-plane rejection rather than a misconfiguration.
func StaticCredentialsFromEnv(env func(string) string) (*StaticCredentials, error) {
	key := env("OTTER_AGENT_STATIC_ACCESS_KEY_ID")
	secret := env("OTTER_AGENT_STATIC_SECRET_ACCESS_KEY")
	if key == "" && secret == "" {
		return nil, nil
	}
	if key == "" || secret == "" {
		return nil, fmt.Errorf("agent: static credentials need BOTH OTTER_AGENT_STATIC_ACCESS_KEY_ID and OTTER_AGENT_STATIC_SECRET_ACCESS_KEY")
	}
	claims := map[string]string{}
	for _, pair := range strings.Split(env("OTTER_AGENT_STATIC_CLAIMS"), ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("agent: malformed claim %q; expected key=value", pair)
		}
		claims[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	// The claims the control plane binds on are required, for the same reason it
	// requires them: an assertion that omits the role must not fall through.
	for _, required := range []string{"account_id", "role", "instance_id"} {
		if claims[required] == "" {
			return nil, fmt.Errorf("agent: static credentials are missing the %q claim", required)
		}
	}
	return &StaticCredentials{
		AccessKeyID:     key,
		SecretAccessKey: secret,
		SessionToken:    env("OTTER_AGENT_STATIC_SESSION_TOKEN"),
		Claims:          claims,
		Region:          orDefault(env("OTTER_AGENT_STATIC_REGION"), "us-east-1"),
		Service:         orDefault(env("OTTER_AGENT_STATIC_SERVICE"), "otter-agent"),
	}, nil
}

func (s *StaticCredentials) Identity(ctx context.Context) (Identity, error) {
	_ = ctx
	return Identity{
		Provider:        s.Provider(),
		Claims:          s.Claims,
		AccessKeyID:     s.AccessKeyID,
		SecretAccessKey: s.SecretAccessKey,
		SessionToken:    s.SessionToken,
		Region:          s.Region,
		Service:         s.Service,
	}, nil
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// EnvLookup is os.Getenv, named so call sites read as configuration rather than
// as an ambient dependency.
func EnvLookup(key string) string { return os.Getenv(key) }
