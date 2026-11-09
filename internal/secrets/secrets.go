// Package secrets resolves the secret values injected into job
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
//
// Sources runs parallel to Providers and is what Lookup reports; it may be empty
// for a chain built with NewChain, in which case the source is inferred from the
// provider's type.
type Chain struct {
	Providers []Provider
	Sources   []Source
}

// NewChain builds a provider chain.
func NewChain(providers ...Provider) *Chain { return &Chain{Providers: providers} }

// Get implements Provider. The fallback rule lives in Lookup so a chain cannot
// behave one way when asked and another way when asked with provenance.
func (c *Chain) Get(ctx context.Context, key string) (string, error) {
	value, _, err := c.Lookup(ctx, key)
	return value, err
}

// MissingError reports secrets that a manifest requires but that could not be
// resolved. This is a configuration failure, so the run is failed before
// Python starts and is not retried.
type MissingError struct {
	JobID string
	Keys  []string
}

func (e *MissingError) Error() string {
	keys := append([]string(nil), e.Keys...)
	sort.Strings(keys)
	return fmt.Sprintf("job %s requires secrets that are not available: %s",
		e.JobID, strings.Join(keys, ", "))
}

// Resolve fetches every requested key. It fails as a whole when any key is
// missing so that a run never starts with a partially configured environment.
func Resolve(ctx context.Context, provider Provider, jobID string, keys []string) (map[string]string, error) {
	values, _, err := ResolveSourced(ctx, provider, jobID, keys)
	if err != nil {
		return nil, err
	}
	return values, nil
}
