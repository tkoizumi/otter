package executor

import (
	"strings"
	"testing"
)

func TestRedactReplacesOnlyDeclaredSecretValues(t *testing.T) {
	environment := map[string]string{
		"SHOPIFY_CLIENT_ID": "shp-secret-value",
		"SHOPIFY_STORE":     "acme.myshopify.com", // a setting, not a secret
		"SHORT":             "abc",
	}
	redactor := newSecretRedactor(environment, []string{"SHOPIFY_CLIENT_ID", "SHORT"})

	out := redactor.Redact("id=shp-secret-value store=acme.myshopify.com short=abc")
	if strings.Contains(out, "shp-secret-value") {
		t.Errorf("the declared secret value survived: %q", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Errorf("no marker in %q", out)
	}
	// A non-secret setting is ordinary output and must stay readable.
	if !strings.Contains(out, "acme.myshopify.com") {
		t.Errorf("an undeclared value was removed: %q", out)
	}
	// Too short to redact safely, and it is not swallowed.
	if !strings.Contains(out, "abc") {
		t.Errorf("a short value was redacted, which would mangle unrelated output: %q", out)
	}
}

func TestRedactLongestValueFirst(t *testing.T) {
	// One value contains the other; replacing the short one first would leave a
	// fragment of the long one visible.
	environment := map[string]string{
		"A": "abcdefgh",
		"B": "abcdefgh-ijkl",
	}
	redactor := newSecretRedactor(environment, []string{"A", "B"})
	out := redactor.Redact("value=abcdefgh-ijkl")
	if out != "value=[redacted]" {
		t.Errorf("out = %q, want a single clean replacement", out)
	}
}

func TestRedactIsSilentWhenNothingIsDeclared(t *testing.T) {
	if newSecretRedactor(map[string]string{"X": "a-long-value-here"}, nil) != nil {
		t.Error("a job with no declared secrets must not build a redactor")
	}
	if got := (*secretRedactor)(nil).Redact("unchanged"); got != "unchanged" {
		t.Errorf("nil redactor changed a line: %q", got)
	}
}
