package secrets

import (
	"context"
	"errors"
	"testing"
)

func TestChainFallsThroughOnlyOnNotFound(t *testing.T) {
	platform := ProviderFunc(func(context.Context, string) (string, error) {
		return "", ErrNotFound
	})
	environment := ProviderFunc(func(context.Context, string) (string, error) {
		return "from-the-host", nil
	})

	chain := NewSourcedChain(
		ChainEntry{Source: SourcePlatform, Provider: platform},
		ChainEntry{Source: SourceEnvironment, Provider: environment},
	)
	value, source, err := chain.Lookup(context.Background(), "TOKEN")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if value != "from-the-host" {
		t.Errorf("value = %q, want the environment value", value)
	}
	if source != SourceEnvironment {
		t.Errorf("source = %q, want %q — the report must name where it came from", source, SourceEnvironment)
	}
}

func TestAProviderFailureDoesNotFallBack(t *testing.T) {
	// A broker that is down must not be answered by a stale local value: the run
	// would then execute with a different credential than the operator configured.
	platform := ProviderFunc(func(context.Context, string) (string, error) {
		return "", errors.New("broker: connection refused")
	})
	environment := ProviderFunc(func(context.Context, string) (string, error) {
		return "sandbox-credential", nil
	})
	chain := NewSourcedChain(
		ChainEntry{Source: SourcePlatform, Provider: platform},
		ChainEntry{Source: SourceEnvironment, Provider: environment},
	)

	if _, source, err := chain.Lookup(context.Background(), "TOKEN"); err == nil {
		t.Fatal("Lookup succeeded; a hard provider error must abort the lookup")
	} else if source != SourceUnavailable {
		t.Errorf("source = %q, want %q", source, SourceUnavailable)
	}

	_, sources, err := ResolveSourced(context.Background(), chain, "job", []string{"TOKEN"})
	if err == nil {
		t.Fatal("ResolveSourced succeeded")
	}
	if sources["TOKEN"] != SourceUnavailable {
		t.Errorf("reported %q, want unavailable: an outage must not look like a missing secret", sources["TOKEN"])
	}
}

func TestMissingIsReportedPerNameAndKeepsWhatResolved(t *testing.T) {
	platform := ProviderFunc(func(_ context.Context, key string) (string, error) {
		if key == "PRESENT" {
			return "platform-value", nil
		}
		return "", ErrNotFound
	})
	_, sources, err := ResolveSourced(context.Background(), platform, "job", []string{"PRESENT", "ABSENT"})
	var missing *MissingError
	if !errors.As(err, &missing) {
		t.Fatalf("err = %v, want MissingError", err)
	}
	if sources["PRESENT"] != SourceUnknown {
		// A bare ProviderFunc is unclassifiable; what matters is that it is not
		// reported as missing.
		t.Errorf("PRESENT source = %q, want it not to be missing", sources["PRESENT"])
	}
	if sources["ABSENT"] != SourceMissing {
		t.Errorf("ABSENT source = %q, want %q", sources["ABSENT"], SourceMissing)
	}
}

func TestSourceIsClassifiedForTheShippedProviders(t *testing.T) {
	if got := sourceOf(NewEnvProvider()); got != SourceEnvironment {
		t.Errorf("EnvProvider source = %q, want environment", got)
	}
	if got := sourceOf(NewBrokerProvider("http://broker", "key", WorkloadIdentity{RuntimeID: "rt", JobID: "job"}, BrokerOptions{})); got != SourcePlatform {
		t.Errorf("BrokerProvider source = %q, want platform", got)
	}
}
