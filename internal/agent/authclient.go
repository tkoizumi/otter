package agent

import (
	"fmt"
	"net/http"
	"net/url"
)

// AuthTransport attaches the agent's credential to requests aimed at the control
// plane, and to nothing else.
//
// Two reasons this is a RoundTripper rather than a header set at each call site.
//
// First, the credential EXPIRES and is refreshed, so it has to be read at request
// time. A header captured when a client was constructed would go stale silently
// and the failure would look like a rejected credential rather than an old one.
//
// Second, and more important: it must NOT follow a redirect to another host, or
// send the credential to a release URL that happens to be off-origin. Releases
// may be served by Cloud (same origin, authenticated) or by object storage
// (different origin, pre-signed). Sending an agent credential to a third party
// would leak the ability to act as this runtime, so the check is on the host and
// a cross-host request simply carries no Authorization header.
type AuthTransport struct {
	// Base is the control plane's origin. Only requests to this host are
	// authenticated; everything else passes through untouched.
	Base string
	// Creds is read per request, so a refresh is picked up without rebuilding the
	// client.
	Creds CredentialStore
	Next  http.RoundTripper
}

func (t *AuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.Next
	if next == nil {
		next = http.DefaultTransport
	}
	if !t.isControlPlane(req.URL) {
		return next.RoundTrip(req)
	}
	if t.Creds == nil {
		return next.RoundTrip(req)
	}
	cred, ok := t.Creds.Get()
	if !ok || cred == nil || cred.Token == "" {
		// No credential: send unauthenticated rather than a header with an empty
		// token. The far side answers 401 either way, but an empty token is a
		// malformed request and the refusal explains the wrong thing.
		return next.RoundTrip(req)
	}
	// Clone rather than mutate: the caller's request may be retried, and a
	// mutated request is a surprising thing to hand back.
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+cred.Token)
	return next.RoundTrip(cloned)
}

// isControlPlane reports whether a URL is on the control plane's host.
//
// A parse failure is treated as NOT the control plane, so an unparseable URL can
// never cause the credential to be attached.
func (t *AuthTransport) isControlPlane(u *url.URL) bool {
	if u == nil || t.Base == "" {
		return false
	}
	base, err := url.Parse(t.Base)
	if err != nil || base.Host == "" {
		return false
	}
	return u.Host == base.Host
}

// AuthenticatedClient builds an HTTP client that authenticates only to the
// control plane. Used for release downloads, which are served by Cloud under the
// agent's own credential rather than by a publicly readable URL.
func AuthenticatedClient(base string, creds CredentialStore) *http.Client {
	return &http.Client{
		Transport: &AuthTransport{Base: base, Creds: creds},
	}
}

// String keeps a transport from being logged with its credential store in a
// format that might print fields.
func (t *AuthTransport) String() string {
	return fmt.Sprintf("AuthTransport{base=%s}", t.Base)
}
