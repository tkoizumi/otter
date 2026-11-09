package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/secrets"
)

func TestPooledDefaultIsPlatformOnly(t *testing.T) {
	// The default must not consult the environment at all: on a shared host that
	// is how one tenant's stray variable would satisfy another tenant's secret.
	platform := secrets.ProviderFunc(func(context.Context, string) (string, error) {
		return "", secrets.ErrNotFound
	})
	environment := secrets.ProviderFunc(func(context.Context, string) (string, error) {
		return "host-value", nil
	})

	provider := secretProviderFor(config.SecretBrokerConfig{}, platform, environment)
	if _, err := provider.Get(context.Background(), "SHOPIFY_CLIENT_ID"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound — the environment must not be consulted by default", err)
	}
}

func TestFallbackAppliesOnlyWhenEnabledAndOnlyOnMissing(t *testing.T) {
	missingPlatform := secrets.ProviderFunc(func(context.Context, string) (string, error) {
		return "", secrets.ErrNotFound
	})
	environment := secrets.ProviderFunc(func(context.Context, string) (string, error) {
		return "host-value", nil
	})

	enabled := config.SecretBrokerConfig{AllowEnvironmentFallback: true}
	value, source, err := secrets.ResolveSourced(
		context.Background(),
		secretProviderFor(enabled, missingPlatform, environment),
		"job",
		[]string{"SHOPIFY_CLIENT_ID"},
	)
	if err != nil {
		t.Fatalf("ResolveSourced: %v", err)
	}
	if value["SHOPIFY_CLIENT_ID"] != "host-value" || source["SHOPIFY_CLIENT_ID"] != secrets.SourceEnvironment {
		t.Errorf("value=%q source=%q, want the environment value and its source", value["SHOPIFY_CLIENT_ID"], source["SHOPIFY_CLIENT_ID"])
	}

	// A platform that cannot be asked is NOT a miss: the run must fail as
	// unavailable rather than quietly use the host credential.
	brokenPlatform := secrets.ProviderFunc(func(context.Context, string) (string, error) {
		return "", errors.New("broker: 503")
	})
	_, sources, err := secrets.ResolveSourced(
		context.Background(),
		secretProviderFor(enabled, brokenPlatform, environment),
		"job",
		[]string{"SHOPIFY_CLIENT_ID"},
	)
	if err == nil {
		t.Fatal("resolved a value through an outage")
	}
	if sources["SHOPIFY_CLIENT_ID"] != secrets.SourceUnavailable {
		t.Errorf("source = %q, want unavailable", sources["SHOPIFY_CLIENT_ID"])
	}
}
