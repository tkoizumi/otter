package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func credsWith(token string) *MemoryCredentials {
	c := &MemoryCredentials{}
	c.Put(&Credential{Token: token, RuntimeID: "rt-1", ExpiresAt: time.Now().Add(time.Hour)})
	return c
}

// The credential must reach the control plane, or a release download is refused.
func TestAuthTransportAuthenticatesTheControlPlane(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer srv.Close()

	client := AuthenticatedClient(srv.URL, credsWith("tok-1"))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/agent/v1/releases/x", nil)
	if _, err := client.Do(req); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer tok-1" {
		t.Errorf("Authorization = %q, want the agent credential", got)
	}
}

// THE test for this file. A release may be served by object storage on another
// host. Sending the agent's credential there would hand a third party the ability
// to act as this runtime, so a cross-host request must carry nothing.
func TestAuthTransportNeverLeaksTheCredentialOffHost(t *testing.T) {
	var got string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer other.Close()

	// The control plane is a DIFFERENT server from the one being called.
	control := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer control.Close()

	client := AuthenticatedClient(control.URL, credsWith("tok-secret"))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, other.URL+"/releases/x", nil)
	if _, err := client.Do(req); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("the agent credential was sent to a non-control-plane host: %q", got)
	}
}

// A redirect is the subtle case: Go follows it automatically, and a redirect to
// another host must not carry the credential either.
func TestAuthTransportDoesNotFollowARedirectWithTheCredential(t *testing.T) {
	var leaked string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
	}))
	defer elsewhere.Close()

	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusFound)
	}))
	defer control.Close()

	client := AuthenticatedClient(control.URL, credsWith("tok-secret"))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, control.URL+"/releases/x", nil)
	if _, err := client.Do(req); err != nil {
		t.Fatal(err)
	}
	if leaked != "" {
		t.Errorf("the credential followed a redirect off-host: %q", leaked)
	}
}

// A refreshed credential must be picked up, or every fetch after a rotation
// fails as though the credential were rejected.
func TestAuthTransportReadsTheCredentialPerRequest(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
	}))
	defer srv.Close()

	creds := credsWith("first")
	client := AuthenticatedClient(srv.URL, creds)
	for _, tok := range []string{"first", "second"} {
		creds.Put(&Credential{Token: tok, RuntimeID: "rt-1", ExpiresAt: time.Now().Add(time.Hour)})
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/x", nil)
		if _, err := client.Do(req); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || seen[0] != "Bearer first" || seen[1] != "Bearer second" {
		t.Errorf("the transport did not pick up the refresh: %v", seen)
	}
}

// No credential means an unauthenticated request, not a header with an empty
// token: an empty token is a malformed request and the far side explains the
// wrong thing.
func TestAuthTransportWithNoCredentialSendsNoHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer srv.Close()

	client := AuthenticatedClient(srv.URL, &MemoryCredentials{})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/x", nil)
	if _, err := client.Do(req); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected no Authorization header, got %q", got)
	}
}
