package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Source is WHERE a secret's value came from. It is reported, never the value.
//
// Two things depend on this distinction, which is why it is a type rather than a
// bool:
//
//   - an operator debugging "why did it use the sandbox credential?" needs the
//     answer, and so does the dashboard: a value resolved from the host's
//     environment is not the tenant's platform credential, and saying "configured"
//     without naming the source is a claim neither side can check;
//   - "missing" and "could not be asked" are different failures. A broker that is
//     down must never look like a job with no secrets, because the remedy is
//     completely different.
type Source string

const (
	// SourcePlatform is the shared store reached through the host broker.
	SourcePlatform Source = "platform"
	// SourceEnvironment is the daemon's own environment.
	SourceEnvironment Source = "environment"
	// SourceMissing means every provider was asked and none had it.
	SourceMissing Source = "missing"
	// SourceUnavailable means a provider could not be asked — a timeout, a
	// permission error, a broker that is down. It is not "missing".
	SourceUnavailable Source = "unavailable"
	// SourceUnknown is a provider this package cannot classify.
	SourceUnknown Source = "unknown"
)

// ChainEntry is one provider and the source it represents.
type ChainEntry struct {
	Source   Source
	Provider Provider
}

// NewSourcedChain builds a chain whose answers carry their provenance.
func NewSourcedChain(entries ...ChainEntry) *Chain {
	chain := &Chain{}
	for _, entry := range entries {
		if entry.Provider == nil {
			continue
		}
		chain.Providers = append(chain.Providers, entry.Provider)
		chain.Sources = append(chain.Sources, entry.Source)
	}
	return chain
}

func (c *Chain) sourceAt(index int) Source {
	if index < len(c.Sources) && c.Sources[index] != "" {
		return c.Sources[index]
	}
	return sourceOf(c.Providers[index])
}

// Lookup is Get with provenance. It falls through ONLY on ErrNotFound: a provider
// that failed for any other reason aborts the lookup, so a broker outage cannot
// silently be answered by a stale local value.
func (c *Chain) Lookup(ctx context.Context, key string) (string, Source, error) {
	for index, provider := range c.Providers {
		if provider == nil {
			continue
		}
		value, err := provider.Get(ctx, key)
		if err == nil {
			return value, c.sourceAt(index), nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", SourceUnavailable, err
		}
	}
	return "", SourceMissing, fmt.Errorf("%s was not found in any configured secret provider: %w", key, ErrNotFound)
}

// sourceOf classifies a provider this package knows, for a chain that was built
// without labels (or for a single provider used directly).
func sourceOf(provider Provider) Source {
	switch provider.(type) {
	case *EnvProvider:
		return SourceEnvironment
	case *BrokerProvider:
		return SourcePlatform
	default:
		return SourceUnknown
	}
}

// lookup asks the provider for one key and reports where the value came from.
func lookup(ctx context.Context, provider Provider, key string) (string, Source, error) {
	if chain, ok := provider.(*Chain); ok {
		return chain.Lookup(ctx, key)
	}
	value, err := provider.Get(ctx, key)
	if err != nil {
		return "", sourceOf(provider), err
	}
	return value, sourceOf(provider), nil
}

// ResolveSourced is Resolve plus, per requested name, the source that satisfied it
// (or Missing / Unavailable).
//
// A partial result is returned even when it fails, because the caller records it:
// "these three resolved from the platform, this one is missing" is what an
// operator needs, and the caller must not use the values — the run has already
// failed at that point.
func ResolveSourced(ctx context.Context, provider Provider, jobID string, keys []string) (map[string]string, map[string]Source, error) {
	values := make(map[string]string, len(keys))
	sources := make(map[string]Source, len(keys))
	if len(keys) == 0 {
		return values, sources, nil
	}
	if provider == nil {
		provider = NewEnvProvider()
	}

	var missing []string
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value, source, err := lookup(ctx, provider, key)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				missing = append(missing, key)
				sources[key] = SourceMissing
				continue
			}
			// One name could not be asked, so the run cannot start: mark every
			// unresolved name as unavailable and stop. Anything already resolved
			// keeps its own source, so the report is truthful per name.
			sources[key] = SourceUnavailable
			return values, sources, fmt.Errorf("resolve secret %s for %s: %w", key, jobID, err)
		}
		values[key] = value
		sources[key] = source
	}

	if len(missing) > 0 {
		return values, sources, &MissingError{JobID: jobID, Keys: missing}
	}
	return values, sources, nil
}
