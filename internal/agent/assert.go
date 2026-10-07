package agent

// The assertion is assembled HERE rather than inside each provider, and the
// reason is a bug this fixed: a provider cannot sign the request body, because it
// does not know the serialized body -- that is the exchanger's business. When
// providers signed an extracted field instead, the verifier received a signature
// covering different bytes than the request it inspected, and the only symptom
// was "signature does not match".
//
// So the division is: a provider supplies identity (who this host is, and a key
// to prove it), and this function turns that into a signed assertion over the
// exact bytes that will be sent.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Identity is what a provider knows: a key and the claims it makes. It is not an
// assertion yet -- an assertion is identity PLUS a signature over a specific
// request, which this package attaches.
type Identity struct {
	// Provider names the mechanism, e.g. "aws-instance-role".
	Provider string
	// Claims are the host's own assertions about itself. The control plane
	// verifies them against its record; they are not trusted because they were
	// sent.
	Claims map[string]string
	// Credentials are used to sign and are never sent.
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Region          string
	Service         string
	// BootstrapSecret, when non-empty, is a per-runtime secret presented in the
	// request BODY instead of a signature. It is the one credential a provider
	// supplies that is deliberately sent, because on a pooled sidecar the secret
	// IS the proof and there is no instance role to sign with. It is never
	// placed in a header, logged, or written to disk by the agent.
	BootstrapSecret string
}

// signAssertion produces the headers that authenticate one request.
//
// `body` is the exact serialized request body, because that is what the verifier
// receives and therefore what must be covered. Signing anything else yields a
// signature the far side cannot reproduce.
func signAssertion(id Identity, method, host, path, body string, now time.Time) (map[string]string, error) {
	if id.Provider == "" {
		return nil, fmt.Errorf("agent: identity has no provider")
	}
	if len(id.Claims) == 0 {
		return nil, fmt.Errorf("agent: identity makes no claims")
	}
	service := id.Service
	if service == "" {
		service = "otter-agent"
	}
	h, err := sigV4Input{
		AccessKeyID:     id.AccessKeyID,
		SecretAccessKey: id.SecretAccessKey,
		SessionToken:    id.SessionToken,
		Region:          id.Region,
		Service:         service,
		Method:          method,
		Host:            host,
		Path:            path,
		Payload:         []byte(body),
		Now:             now,
	}.Header()
	if err != nil {
		return nil, err
	}
	return normaliseHeaderNames(h), nil
}

// hostOf extracts the authority a signature must cover. Signing a different host
// than the request names is the classic SigV4 mistake: the control plane
// re-derives over the Host it received and gets a different result.
func hostOf(rawURL string) (string, error) {
	u, err := urlParse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("agent: %q has no host to sign", rawURL)
	}
	return u.Host, nil
}

// signedHeaders returns the names a signature covered, for a diagnostic that
// names the disagreement rather than saying "does not match".
func signedHeaders(h map[string]string) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, strings.ToLower(k))
	}
	return out
}

// AssertionFor builds a signed assertion for one request. Providers use it so
// every mechanism signs the same way.
func AssertionFor(ctx context.Context, id Identity, method, rawURL, body string) (*Assertion, error) {
	_ = ctx
	host, err := hostOf(rawURL)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	headers, err := signAssertion(id, method, host, pathOf(rawURL), body, now)
	if err != nil {
		return nil, err
	}
	_ = http.MethodPost
	a := &Assertion{Provider: id.Provider, Claims: id.Claims, SignedAt: now, Headers: headers}
	if err := a.validate(now); err != nil {
		return nil, err
	}
	return a, nil
}

func urlParse(raw string) (*urlParts, error) {
	// A minimal parse: the agent signs the path and host of a URL it was
	// configured with, so a full net/url dependency would be more surface than
	// the job needs.
	rest := raw
	if i := indexOf(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	host := rest
	path := "/"
	if i := indexOf(rest, "/"); i >= 0 {
		host = rest[:i]
		path = rest[i:]
	}
	if host == "" {
		return nil, fmt.Errorf("agent: cannot parse %q", raw)
	}
	return &urlParts{Host: host, Path: path}, nil
}

type urlParts struct{ Host, Path string }

func pathOf(raw string) string {
	p, err := urlParse(raw)
	if err != nil {
		return "/"
	}
	return p.Path
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
