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

// HTTPControlPlane is the Cloud side as the agent reaches it: outbound HTTPS,
// authenticated with the credential the bootstrap earned.
//
// Every request carries the credential, and the credential names the runtime, so
// the control plane can refuse a request that does not match. That is why the
// RuntimeID is sent even though the endpoint already knows which agent it is
// talking to -- the check happens on the far side, against the signed binding.
type HTTPControlPlane struct {
	BaseURL string
	// Creds holds the current credential. Read on every request so a refresh is
	// picked up without re-creating the client.
	Creds      CredentialStore
	HTTPClient *http.Client
	// RuntimeID is sent with each request so the control plane can match it
	// against the credential's binding.
	RuntimeID string
}

func (c *HTTPControlPlane) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	// Long enough for a long poll, short enough that a hung control plane does
	// not wedge the agent indefinitely.
	return &http.Client{Timeout: 90 * time.Second}
}

func (c *HTTPControlPlane) post(ctx context.Context, path string, reqBody any, out any) error {
	if c.BaseURL == "" {
		return fmt.Errorf("agent: control plane has no base URL")
	}
	if c.Creds == nil {
		return fmt.Errorf("agent: control plane client has no credential store")
	}
	cred, ok := c.Creds.Get()
	if !ok || cred == nil {
		// Not an error to retry blindly: without a credential every call fails,
		// and the loop re-bootstraps before it gets here.
		return fmt.Errorf("%w: no agent credential", ErrBootstrapUnavailable)
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, trimSlash(c.BaseURL)+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cred.Token)

	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNoContent:
		// "Nothing to deploy" is a 204 rather than an empty state, so an agent
		// never has to guess whether an empty answer means no work or malformed
		// work.
		return errNoDesiredWork
	case resp.StatusCode == http.StatusUnauthorized:
		// The credential is stale or revoked. Re-asserting is the path that
		// exists; nothing else about the runtime changes.
		return fmt.Errorf("%w: control plane rejected the credential", ErrBootstrapUnavailable)
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: status %d", ErrRuntimeUnavailable, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("agent: %s: status %d: %s", path, resp.StatusCode, oneLine(msg))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil && err != io.EOF {
		return fmt.Errorf("agent: %s: malformed response: %w", path, err)
	}
	return nil
}

func (c *HTTPControlPlane) Desired(ctx context.Context, observed Observed) (*Desired, error) {
	req := DesiredRequest{
		// The control plane refuses a request that does not name a runtime, and
		// this independently of the credential: the runtime is what the answer is
		// ABOUT, so it cannot be inferred from who is asking.
		//
		// It was empty here, and the loop only set it on the way back out of step()
		// -- so the request went out nameless and every poll got "request does not
		// name a runtime" while bootstrap succeeded. A live run found it; no unit
		// test did, because the test fakes checked the credential and not this.
		RuntimeID:  c.RuntimeID,
		Observed:   observed,
		Generation: 0,
		// Declared on every request, because it is what selects the SHAPE of the
		// answer. A control plane that does not know the capability replies with
		// the single release, which this agent still understands and reconciles
		// without removing anything -- so a Cloud deploy and an image rebuild can
		// happen in either order.
		Capabilities: []string{CapabilityDesiredSnapshot},
	}
	var out Desired
	if err := c.post(ctx, "/agent/v1/desired", req, &out); err != nil {
		if err == errNoDesiredWork {
			// Nothing to converge on. Reporting the current generation as the
			// desired one would look like a change, so instead the loop treats
			// this as "no work" by returning the observed generation with no
			// release, which Apply refuses as a control-plane bug -- and that
			// refusal is the correct behaviour for a control plane that promised
			// a generation and named no release.
			return nil, errNoDesiredWork
		}
		return nil, err
	}
	return &out, nil
}

func (c *HTTPControlPlane) Report(ctx context.Context, r Reported) error {
	if r.RuntimeID == "" {
		r.RuntimeID = c.RuntimeID
	}
	var out struct {
		Recorded bool `json:"recorded"`
	}
	return c.post(ctx, "/agent/v1/report", r, &out)
}

func (c *HTTPControlPlane) Lease(ctx context.Context) error {
	return c.post(ctx, "/agent/v1/lease", map[string]string{"runtime_id": c.RuntimeID}, nil)
}

// errNoDesiredWork means the control plane has nothing for this agent. It is not
// a failure: the agent keeps serving what it already runs and asks again.
var errNoDesiredWork = fmt.Errorf("agent: no desired work")

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
