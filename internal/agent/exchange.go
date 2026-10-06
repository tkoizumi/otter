package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Exchanger turns a bootstrap assertion into a short-lived agent credential.
//
// This is the one place where a host becomes an authenticated agent, so it is
// deliberately narrow: it sends the assertion, requires a credential back, and
// refuses to invent one if the control plane does not provide it. There is no
// cache and no fallback path -- a host that cannot prove who it is does not
// become an agent, rather than continuing with a credential it happens to hold.
type Exchanger struct {
	// BaseURL is the control plane, e.g. https://cloud.example.
	BaseURL string
	// HTTPClient is injectable for tests.
	HTTPClient *http.Client
	// Now is injectable so expiry can be tested without sleeping.
	Now func() time.Time
}

func (e *Exchanger) client() *http.Client {
	if e.HTTPClient != nil {
		return e.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (e *Exchanger) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// bootstrapRequest is what travels. The claims travel with the assertion so the
// control plane can compare them against its own record, but they are not what
// authenticates: the signature is.
type bootstrapRequest struct {
	Assertion *Assertion `json:"assertion"`
	// RuntimeID is the runtime the agent believes it is. Cloud must reject a
	// mismatch rather than create a second registration, or a reimaged host would
	// silently become a new runtime with the old one still registered.
	RuntimeID    string `json:"runtime_id"`
	AgentVersion string `json:"agent_version,omitempty"`
}

// Exchange performs the bootstrap. It returns a credential or an error that is
// either ErrBootstrapUnavailable (retry) or ErrBootstrapRefused (do not).
func (e *Exchanger) Exchange(ctx context.Context, b Bootstrap, runtimeID, agentVersion string) (*Credential, error) {
	if e.BaseURL == "" {
		return nil, fmt.Errorf("agent: exchanger has no base URL")
	}
	if b == nil {
		return nil, fmt.Errorf("agent: exchanger has no bootstrap provider")
	}
	assertion, err := b.Assert(ctx)
	if err != nil {
		return nil, err
	}
	// Re-validate here rather than trusting the provider: a provider that
	// constructed a malformed assertion would otherwise put it on the wire.
	if err := assertion.validate(e.now().UTC()); err != nil {
		return nil, err
	}

	body, err := json.Marshal(bootstrapRequest{Assertion: assertion, RuntimeID: runtimeID, AgentVersion: agentVersion})
	if err != nil {
		return nil, fmt.Errorf("agent: bootstrap request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/agent/v1/bootstrap", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// The provider's signed material authenticates the request. It is a header
	// rather than a field so it is never written to a log line that prints the
	// JSON body.
	req.Header.Set("X-Otter-Bootstrap", assertion.Provider)
	req.Header.Set("X-Otter-Assertion", assertion.Material)

	resp, err := e.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBootstrapUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		// fall through to decode
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// Refused: the host is not the one Cloud expects, or the registration was
		// revoked. Retrying the same assertion cannot help, so the caller must
		// back off rather than hammer.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%w: status %d: %s", ErrBootstrapRefused, resp.StatusCode, oneLine(msg))
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: status %d", ErrBootstrapUnavailable, resp.StatusCode)
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%w: status %d: %s", ErrBootstrapRefused, resp.StatusCode, oneLine(msg))
	}

	var cred Credential
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&cred); err != nil {
		return nil, fmt.Errorf("%w: malformed credential response: %v", ErrBootstrapUnavailable, err)
	}
	// A credential with no token or no runtime binding is not a credential.
	if cred.Token == "" {
		return nil, fmt.Errorf("%w: control plane returned no token", ErrBootstrapRefused)
	}
	if cred.RuntimeID == "" {
		return nil, fmt.Errorf("%w: credential is not bound to a runtime", ErrBootstrapRefused)
	}
	if runtimeID != "" && cred.RuntimeID != runtimeID {
		// The credential is for a different runtime than the one this agent is
		// managing. Accepting it would manage another tenant's runtime.
		return nil, fmt.Errorf("%w: credential is for runtime %q, not %q", ErrBootstrapRefused, cred.RuntimeID, runtimeID)
	}
	if !cred.ExpiresAt.After(e.now()) {
		return nil, fmt.Errorf("%w: control plane returned an already-expired credential", ErrBootstrapRefused)
	}
	return &cred, nil
}

// oneLine keeps an error response from a remote server out of multi-line output,
// where a newline could forge what looks like a log record.
func oneLine(b []byte) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == '\n' || c == '\r' {
			c = ' '
		}
		out = append(out, c)
	}
	return string(bytes.TrimSpace(out))
}

// CredentialStore holds the current agent credential. Kept as an interface so
// the in-memory implementation used by the tests is obviously not durable: the
// credential is short-lived by design, and persisting it would recreate the
// long-lived-secret problem the instance-role bootstrap exists to avoid.
type CredentialStore interface {
	Get() (*Credential, bool)
	Put(*Credential)
	// Expired reports whether the held credential is past its expiry, with a
	// margin so a credential does not expire mid-request.
	Expired(now time.Time) bool
}

// MemoryCredentials is the pilot's store: in-memory, lost on restart, and
// re-obtained by asserting the instance role again. That is the intended
// behaviour rather than a limitation.
type MemoryCredentials struct {
	cred *Credential
	// Margin is how long before expiry a credential is treated as expired, so a
	// request does not start with a credential that dies in flight.
	Margin time.Duration
}

func (m *MemoryCredentials) Get() (*Credential, bool) {
	if m.cred == nil {
		return nil, false
	}
	return m.cred, true
}

func (m *MemoryCredentials) Put(c *Credential) { m.cred = c }

func (m *MemoryCredentials) Expired(now time.Time) bool {
	if m.cred == nil {
		return true
	}
	margin := m.Margin
	if margin == 0 {
		margin = 30 * time.Second
	}
	return !m.cred.ExpiresAt.Add(-margin).After(now)
}
