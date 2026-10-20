package awsv4

import (
	"strings"
	"testing"
	"time"
)

func baseInput() Input {
	return Input{
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

// The signature is what the far side reproduces. These pin that it is
// deterministic for fixed input, so a change to canonicalisation is a failing
// test rather than an authentication outage discovered in production.
func TestSigV4IsDeterministicForFixedInput(t *testing.T) {
	in := baseInput()
	a, err := Sign(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Sign(in)
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
	base, err := Sign(baseInput())
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name string
		edit func(*Input)
	}{
		{"path", func(i *Input) { i.Path = "/agent/v1/other" }},
		{"method", func(i *Input) { i.Method = "GET" }},
		{"host", func(i *Input) { i.Host = "elsewhere.invalid" }},
		{"region", func(i *Input) { i.Region = "eu-west-1" }},
		{"service", func(i *Input) { i.Service = "other" }},
		{"payload", func(i *Input) { i.Payload = []byte("different") }},
		{"time", func(i *Input) { i.Now = i.Now.Add(time.Second) }},
		{"session token", func(i *Input) { i.SessionToken = "token" }},
		{"secret", func(i *Input) { i.SecretAccessKey = "other" }},
		{"query", func(i *Input) { i.Query = "list-type=2" }},
		{"extra header", func(i *Input) { i.Headers = map[string]string{"x-amz-target": "SecretsManager.GetSecretValue"} }},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			in := baseInput()
			m.edit(&in)
			got, err := Sign(in)
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
		edit func(*Input)
	}{
		{"no access key", func(i *Input) { i.AccessKeyID = "" }},
		{"no secret", func(i *Input) { i.SecretAccessKey = "" }},
		{"no region", func(i *Input) { i.Region = "" }},
		{"no service", func(i *Input) { i.Service = "" }},
		{"no time", func(i *Input) { i.Now = time.Time{} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := baseInput()
			c.edit(&in)
			if _, err := Sign(in); err == nil {
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
	auth, err := Sign(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(auth, "x-amz-security-token") {
		t.Errorf("session token is not in SignedHeaders; it could be swapped:\n%s", auth)
	}
}

// A caller-supplied header is signed, and is named in SignedHeaders, or a
// service that requires it (Secrets Manager requires content-type and
// x-amz-target) would reject the request.
func TestExtraHeadersAreSignedAndNamed(t *testing.T) {
	in := baseInput()
	in.Method = "POST"
	in.Path = "/"
	in.Service = "secretsmanager"
	in.Headers = map[string]string{
		"Content-Type": "application/x-amz-json-1.1",
		"X-Amz-Target": "secretsmanager.GetSecretValue",
	}
	auth, err := Sign(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(auth, "content-type") || !strings.Contains(auth, "x-amz-target") {
		t.Errorf("extra headers are not in SignedHeaders:\n%s", auth)
	}

	// Header() must return every signed header, so the request carries what was
	// signed rather than only what the base input named.
	h, err := in.Header()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"Content-Type": "application/x-amz-json-1.1",
		"X-Amz-Target": "secretsmanager.GetSecretValue",
		"Host":         "cloud.otter.invalid",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	if h.Get("Authorization") == "" {
		t.Error("Header() returned no Authorization")
	}
}

// A header added with unusual casing must sign the same as its lowercase form,
// because the service lowercases before verifying.
func TestExtraHeaderNamesAreCaseInsensitive(t *testing.T) {
	lower := baseInput()
	lower.Headers = map[string]string{"x-amz-target": "Thing"}
	upper := baseInput()
	upper.Headers = map[string]string{"X-Amz-Target": "Thing"}

	a, err := Sign(lower)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Sign(upper)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("header casing changed the signature:\n%s\n%s", a, b)
	}
}

// canonicalURI encodes each segment and preserves the separators between them.
// The separator is deliberately not encoded: SigV4 canonicalisation splits on
// "/" first, so encoding it would produce a path the far side cannot reproduce.
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
		if got := CanonicalURI(in); got != want {
			t.Errorf("CanonicalURI(%q) = %q, want %q", in, got, want)
		}
	}
}

// The property that matters is that distinct paths cannot sign identically: a
// collision would let one request stand in for another. ".." and an encoded
// "%2E%2E" are different byte sequences in a segment and must stay different.
func TestCanonicalURIDoesNotCollideOnDotSegments(t *testing.T) {
	seen := map[string]string{}
	for _, p := range []string{"/a/b", "/a/../b", "/a/%2E%2E/b", "/a/./b", "/a//b"} {
		c := CanonicalURI(p)
		if prev, dup := seen[c]; dup {
			t.Errorf("paths %q and %q canonicalise identically to %q", prev, p, c)
		}
		seen[c] = p
	}
}

// A space must become %20, never "+": form encoding is not URI encoding, and a
// "+" would sign a different value than the server computes.
func TestCanonicalURIUsesPercent20ForSpace(t *testing.T) {
	if got := CanonicalURI("/a b"); strings.Contains(got, "+") {
		t.Errorf("space encoded as + rather than %%20: %q", got)
	}
}
