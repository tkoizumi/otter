// Package secrets resolves the secret values injected into integration
// processes.
//
// The MVP ships only an environment-backed provider, but the Provider
// interface is the seam where AWS Secrets Manager, Vault, 1Password or a
// Castor control plane plug in later without touching the executor.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ErrNotFound is returned by a Provider when it does not hold the key.
var ErrNotFound = errors.New("secret not found")

// Provider resolves a single secret by name.
type Provider interface {
	Get(ctx context.Context, key string) (string, error)
}

// ProviderFunc adapts a function to the Provider interface.
type ProviderFunc func(ctx context.Context, key string) (string, error)

// Get implements Provider.
func (f ProviderFunc) Get(ctx context.Context, key string) (string, error) { return f(ctx, key) }

// EnvProvider reads secrets from the daemon's own environment. An empty value
// is treated as absent: a blank token is a misconfiguration, not a secret.
type EnvProvider struct{}

// NewEnvProvider returns the default provider.
func NewEnvProvider() *EnvProvider { return &EnvProvider{} }

// Get implements Provider.
func (p *EnvProvider) Get(_ context.Context, key string) (string, error) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is not set in the daemon environment: %w", key, ErrNotFound)
	}
	return value, nil
}

// StaticProvider serves secrets from a map. It is used by tests.
type StaticProvider struct {
	Values map[string]string
}

// NewStaticProvider builds a provider from a map.
func NewStaticProvider(values map[string]string) *StaticProvider {
	return &StaticProvider{Values: values}
}

// Get implements Provider.
func (p *StaticProvider) Get(_ context.Context, key string) (string, error) {
	v, ok := p.Values[key]
	if !ok {
		return "", fmt.Errorf("%s is not present: %w", key, ErrNotFound)
	}
	return v, nil
}

// Chain tries each provider in order and returns the first value found.
type Chain struct {
	Providers []Provider
}

// NewChain builds a provider chain.
func NewChain(providers ...Provider) *Chain { return &Chain{Providers: providers} }

// Get implements Provider.
func (c *Chain) Get(ctx context.Context, key string) (string, error) {
	for _, p := range c.Providers {
		if p == nil {
			continue
		}
		value, err := p.Get(ctx, key)
		if err == nil {
			return value, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	return "", fmt.Errorf("%s was not found in any configured secret provider: %w", key, ErrNotFound)
}

// MissingError reports secrets that a manifest requires but that could not be
// resolved. This is a configuration failure, so the run is failed before
// Python starts and is not retried.
type MissingError struct {
	IntegrationID string
	Keys          []string
}

func (e *MissingError) Error() string {
	keys := append([]string(nil), e.Keys...)
	sort.Strings(keys)
	return fmt.Sprintf("integration %s requires secrets that are not available: %s",
		e.IntegrationID, strings.Join(keys, ", "))
}

// Resolve fetches every requested key. It fails as a whole when any key is
// missing so that a run never starts with a partially configured environment.
func Resolve(ctx context.Context, provider Provider, integrationID string, keys []string) (map[string]string, error) {
	if len(keys) == 0 {
		return map[string]string{}, nil
	}
	if provider == nil {
		provider = NewEnvProvider()
	}

	out := make(map[string]string, len(keys))
	var missing []string

	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value, err := provider.Get(ctx, key)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				missing = append(missing, key)
				continue
			}
			return nil, fmt.Errorf("resolve secret %s for %s: %w", key, integrationID, err)
		}
		out[key] = value
	}

	if len(missing) > 0 {
		return nil, &MissingError{IntegrationID: integrationID, Keys: missing}
	}
	return out, nil
}
