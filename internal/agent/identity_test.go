package agent

import (
	"strings"
	"testing"
	"time"
)

func validAssertion(now time.Time) *Assertion {
	return &Assertion{
		Provider: "aws-instance-role",
		Claims:   map[string]string{"account": "426714791664", "role": "otter-agent"},
		SignedAt: now,
		Headers:  map[string]string{"authorization": "AWS4-HMAC-SHA256 Credential=stub", "x-amz-date": "20261006T120000Z"},
	}
}

func TestAssertionValidationAcceptsAFreshProof(t *testing.T) {
	now := time.Now()
	if err := validAssertion(now).validate(now); err != nil {
		t.Fatalf("a fresh assertion must be valid: %v", err)
	}
}

// Each of these is a way to present something that looks like a proof but is not
// one. They are tested individually because the failure mode is a host being
// trusted for a claim it did not establish.
func TestAssertionValidationRefusesIncompleteProofs(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name, want string
		mutate     func(*Assertion)
	}{
		{"no provider", "no provider", func(a *Assertion) { a.Provider = "" }},
		{"no headers", "no signed material", func(a *Assertion) { a.Headers = nil }},
		{"headers without Authorization", "no Authorization header", func(a *Assertion) {
			a.Headers = map[string]string{"x-amz-date": "20261006T120000Z"}
		}},
		{"no claims", "no claims to bind", func(a *Assertion) { a.Claims = nil }},
		{"not timestamped", "not timestamped", func(a *Assertion) { a.SignedAt = time.Time{} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := validAssertion(now)
			c.mutate(a)
			err := a.validate(now)
			if err == nil {
				t.Fatal("expected a refusal, got acceptance")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name the reason %q", err, c.want)
			}
		})
	}
}

// A replay is the attack a short window exists to stop. Both directions matter:
// an old assertion and one dated in the future are both signs that what is being
// presented is not a fresh proof.
func TestAssertionValidationRefusesReplayAndClockSkew(t *testing.T) {
	now := time.Now()
	old := validAssertion(now.Add(-5 * time.Minute))
	if err := old.validate(now); err == nil {
		t.Error("an assertion from five minutes ago must be refused: it is a replay")
	}
	future := validAssertion(now.Add(5 * time.Minute))
	if err := future.validate(now); err == nil {
		t.Error("an assertion dated in the future must be refused")
	}
	// Just inside the window is still fine, so the rule is a boundary and not a
	// blanket refusal.
	edge := validAssertion(now.Add(-90 * time.Second))
	if err := edge.validate(now); err != nil {
		t.Errorf("an assertion 90s old is inside the window and must be accepted: %v", err)
	}
}
