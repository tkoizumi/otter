package agent

import (
	"strings"
	"testing"
	"time"
)

func baseInput() sigV4Input {
	return sigV4Input{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		Region:          "us-east-1",
		Service:         "otter-agent",
		Method:          "POST",
		Host:            "cloud.otter.invalid",
		Path:            "/agent/v1/bootstrap",
		Payload:         []byte("i-0123456789abcdef0"),
		Now:             time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
	}
}

// The signature is what the control plane reproduces. These pin that it is
// deterministic for fixed input, so a change to canonicalisation is a failing
// test rather than an authentication outage discovered in production.
func TestSigV4IsDeterministicForFixedInput(t *testing.T) {
	in := baseInput()
	a, err := signSigV4(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := signSigV4(in)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("same input produced different signatures:\n%s\n%s", a, b)
	}
	for _, want := range []string{"AWS4-HMAC-SHA256", "Credential=AKIDEXAMPLE/20261006/us-east-1/otter-agent/aws4_request", "SignedHeaders=host;x-amz-content-sha256;x-amz-date", "Signature="} {
		if !strings.Contains(a, want) {
			t.Errorf("authorization header missing %q:\n%s", want, a)
		}
	}
}

// Every input that changes what is being signed must change the signature. A
// collision here means two different requests are interchangeable, which is the
// failure SigV4 exists to prevent.
func TestSigV4ChangesWithEverySignedInput(t *testing.T) {
	base, err := signSigV4(baseInput())
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name string
		edit func(*sigV4Input)
	}{
		{"path", func(i *sigV4Input) { i.Path = "/agent/v1/other" }},
		{"method", func(i *sigV4Input) { i.Method = "GET" }},
		{"host", func(i *sigV4Input) { i.Host = "elsewhere.invalid" }},
		{"region", func(i *sigV4Input) { i.Region = "eu-west-1" }},
		{"service", func(i *sigV4Input) { i.Service = "other" }},
		{"payload", func(i *sigV4Input) { i.Payload = []byte("different") }},
		{"time", func(i *sigV4Input) { i.Now = i.Now.Add(time.Second) }},
		{"session token", func(i *sigV4Input) { i.SessionToken = "token" }},
		{"secret", func(i *sigV4Input) { i.SecretAccessKey = "other" }},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			in := baseInput()
			m.edit(&in)
			got, err := signSigV4(in)
			if err != nil {
				t.Fatal(err)
			}
			if got == base {
				t.Errorf("changing %s did not change the signature", m.name)
			}
		})
	}
}

func TestSigV4RefusesIncompleteInput(t *testing.T) {
	cases := []struct {
		name string
		edit func(*sigV4Input)
	}{
		{"no access key", func(i *sigV4Input) { i.AccessKeyID = "" }},
		{"no secret", func(i *sigV4Input) { i.SecretAccessKey = "" }},
		{"no region", func(i *sigV4Input) { i.Region = "" }},
		{"no service", func(i *sigV4Input) { i.Service = "" }},
		{"no time", func(i *sigV4Input) { i.Now = time.Time{} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := baseInput()
			c.edit(&in)
			if _, err := signSigV4(in); err == nil {
				t.Error("expected a refusal for incomplete input")
			}
		})
	}
}

// A session token must be signed as a header, not merely carried: an unsigned
// token could be swapped in transit.
func TestSessionTokenIsPartOfSignedHeaders(t *testing.T) {
	in := baseInput()
	in.SessionToken = "IQoJb3JpZ2luX2VjEA=="
	auth, err := signSigV4(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(auth, "x-amz-security-token") {
		t.Errorf("session token is not in SignedHeaders; it could be swapped:\n%s", auth)
	}
}

// canonicalURI encodes each segment and preserves the separators between them.
// The separator is deliberately not encoded: SigV4 canonicalisation splits on
// "/" first, so encoding it would produce a path the control plane cannot
// reproduce.
func TestCanonicalURIEncodesSegmentsButKeepsSeparators(t *testing.T) {
	cases := map[string]string{
		"":                    "/",
		"/":                   "/",
		"/agent/v1/bootstrap": "/agent/v1/bootstrap",
		"agent/v1":            "/agent/v1",
		"/agent/a b":          "/agent/a%20b",
		"/agent/tilde~name":   "/agent/tilde~name",
		"/agent/plus+sign":    "/agent/plus%2Bsign",
	}
	for in, want := range cases {
		if got := canonicalURI(in); got != want {
			t.Errorf("canonicalURI(%q) = %q, want %q", in, got, want)
		}
	}
}

// The property that matters is that distinct paths cannot sign identically: a
// collision would let one request stand in for another. ".." and an encoded
// "%2E%2E" are different byte sequences in a segment and must stay different.
func TestCanonicalURIDoesNotCollideOnDotSegments(t *testing.T) {
	seen := map[string]string{}
	for _, p := range []string{"/a/b", "/a/../b", "/a/%2E%2E/b", "/a/./b", "/a//b"} {
		c := canonicalURI(p)
		if prev, dup := seen[c]; dup {
			t.Errorf("paths %q and %q canonicalise identically to %q", prev, p, c)
		}
		seen[c] = p
	}
}

// A space must become %20, never "+": form encoding is not URI encoding, and a
// "+" would sign a different value than the server computes.
func TestCanonicalURIUsesPercent20ForSpace(t *testing.T) {
	if got := canonicalURI("/a b"); strings.Contains(got, "+") {
		t.Errorf("space encoded as + rather than %%20: %q", got)
	}
}
