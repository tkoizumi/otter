package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEnvProviderReadsTheDaemonEnvironment(t *testing.T) {
	t.Setenv("OTTER_TEST_SECRET", "s3cr3t")

	provider := NewEnvProvider()

	value, err := provider.Get(context.Background(), "OTTER_TEST_SECRET")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if value != "s3cr3t" {
		t.Errorf("value = %q, want s3cr3t", value)
	}

	if _, err := provider.Get(context.Background(), "OTTER_TEST_ABSENT_SECRET"); !errors.Is(err, ErrNotFound) {
		t.Errorf("absent secret error = %v, want ErrNotFound", err)
	}
}

func TestEnvProviderTreatsBlankAsMissing(t *testing.T) {
	// A blank token is a misconfiguration, not a usable secret.
	t.Setenv("OTTER_TEST_BLANK", "   ")

	if _, err := NewEnvProvider().Get(context.Background(), "OTTER_TEST_BLANK"); !errors.Is(err, ErrNotFound) {
		t.Errorf("blank secret error = %v, want ErrNotFound", err)
	}
}

func TestStaticProviderAndFunc(t *testing.T) {
	static := NewStaticProvider(map[string]string{"A": "1"})
	if value, err := static.Get(context.Background(), "A"); err != nil || value != "1" {
		t.Errorf("get A = %q, %v; want 1, nil", value, err)
	}
	if _, err := static.Get(context.Background(), "B"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing key error = %v, want ErrNotFound", err)
	}

	calls := 0
	fromFunc := ProviderFunc(func(_ context.Context, key string) (string, error) {
		calls++
		if key == "F" {
			return "from-func", nil
		}
		return "", ErrNotFound
	})
	if value, err := fromFunc.Get(context.Background(), "F"); err != nil || value != "from-func" {
		t.Errorf("ProviderFunc get = %q, %v", value, err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestChainPrefersEarlierProviders(t *testing.T) {
	first := NewStaticProvider(map[string]string{"SHARED": "first", "ONLY_FIRST": "a"})
	second := NewStaticProvider(map[string]string{"SHARED": "second", "ONLY_SECOND": "b"})
	chain := NewChain(first, second)

	ctx := context.Background()

	if value, err := chain.Get(ctx, "SHARED"); err != nil || value != "first" {
		t.Errorf("SHARED = %q, %v; want first, nil", value, err)
	}
	if value, err := chain.Get(ctx, "ONLY_SECOND"); err != nil || value != "b" {
		t.Errorf("ONLY_SECOND = %q, %v; want b, nil", value, err)
	}
	if _, err := chain.Get(ctx, "NOWHERE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown key error = %v, want ErrNotFound", err)
	}

	// A nil provider in the chain is skipped rather than panicking.
	withNil := NewChain(nil, first)
	if value, err := withNil.Get(ctx, "ONLY_FIRST"); err != nil || value != "a" {
		t.Errorf("chain with nil provider = %q, %v; want a, nil", value, err)
	}
}

func TestChainPropagatesRealErrors(t *testing.T) {
	boom := ProviderFunc(func(context.Context, string) (string, error) {
		return "", errors.New("backend unavailable")
	})
	chain := NewChain(boom, NewStaticProvider(map[string]string{"K": "v"}))

	// A non-ErrNotFound failure must not silently fall through to the next
	// provider: that would mask a broken secret backend.
	if _, err := chain.Get(context.Background(), "K"); err == nil || !strings.Contains(err.Error(), "backend unavailable") {
		t.Errorf("error = %v, want the provider failure", err)
	}
}

func TestResolveReturnsAllRequestedSecrets(t *testing.T) {
	provider := NewStaticProvider(map[string]string{"A": "1", "B": "2"})

	values, err := Resolve(context.Background(), provider, "svc", []string{"A", "B"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(values) != 2 || values["A"] != "1" || values["B"] != "2" {
		t.Errorf("values = %v, want A=1 B=2", values)
	}

	empty, err := Resolve(context.Background(), provider, "svc", nil)
	if err != nil {
		t.Fatalf("resolve with no keys: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("values = %v, want empty", empty)
	}
}

func TestResolveFailsAtomicallyWhenAnySecretIsMissing(t *testing.T) {
	provider := NewStaticProvider(map[string]string{"PRESENT": "1"})

	// A run must never start with a partially configured environment.
	values, err := Resolve(context.Background(), provider, "shopify-to-erp", []string{"PRESENT", "MISSING_B", "MISSING_A"})
	if err == nil {
		t.Fatalf("expected an error, got values %v", values)
	}
	if values != nil {
		t.Errorf("values = %v, want nil: resolution must be all-or-nothing", values)
	}

	var missing *MissingError
	if !errors.As(err, &missing) {
		t.Fatalf("error type = %T, want *MissingError", err)
	}
	if missing.IntegrationID != "shopify-to-erp" {
		t.Errorf("integration = %q, want shopify-to-erp", missing.IntegrationID)
	}
	if len(missing.Keys) != 2 {
		t.Fatalf("missing keys = %v, want two", missing.Keys)
	}

	// Keys are sorted in the message so the error is stable.
	message := err.Error()
	if !strings.Contains(message, "MISSING_A") || !strings.Contains(message, "MISSING_B") {
		t.Errorf("message = %q, want it to name both missing secrets", message)
	}
	if strings.Index(message, "MISSING_A") > strings.Index(message, "MISSING_B") {
		t.Errorf("message = %q, want the missing keys sorted", message)
	}
	if !strings.Contains(message, "shopify-to-erp") {
		t.Errorf("message = %q, want it to name the integration", message)
	}
}

func TestResolveDefaultsToTheEnvironmentProvider(t *testing.T) {
	t.Setenv("OTTER_TEST_DEFAULT_PROVIDER", "from-env")

	values, err := Resolve(context.Background(), nil, "svc", []string{"OTTER_TEST_DEFAULT_PROVIDER"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if values["OTTER_TEST_DEFAULT_PROVIDER"] != "from-env" {
		t.Errorf("value = %q, want from-env", values["OTTER_TEST_DEFAULT_PROVIDER"])
	}
}

func TestResolveIgnoresBlankKeyNames(t *testing.T) {
	values, err := Resolve(context.Background(), NewStaticProvider(nil), "svc", []string{"", "   "})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("values = %v, want empty", values)
	}
}
