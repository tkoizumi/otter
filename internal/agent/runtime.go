package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RuntimeHTTP is the agent's view of the local runtime over its own HTTP API.
//
// It implements the parts of Runtime that the API actually exposes. Two methods
// the agent needs are NOT reachable over HTTP -- see Promote and the note on
// Fetch -- and rather than stub them into something that looks finished, they
// are implemented by LocalRuntime below, which is explicit that it touches the
// filesystem.
//
// The split matters because it records a real constraint discovered while
// building this: the runtime API gained a maintenance gate in WP2 but has no
// release-activation endpoint, so a pull-based deploy cannot be expressed
// entirely as API calls today.
type RuntimeHTTP struct {
	// BaseURL is the runtime's API, normally http://127.0.0.1:7337.
	BaseURL string
	// Token is a runtime-scoped control credential. The agent holds an admin
	// token only because it is the runtime's own operator; the pilot's
	// provisioning should issue it a scoped one.
	Token      string
	HTTPClient *http.Client
}

func (r *RuntimeHTTP) client() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (r *RuntimeHTTP) do(ctx context.Context, method, path string, body any, out any) error {
	if r.BaseURL == "" {
		return fmt.Errorf("agent: runtime client has no base URL")
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(r.BaseURL, "/")+path, reader)
	if err != nil {
		return err
	}
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		// 5xx is the runtime failing; 4xx is the agent asking wrongly. The
		// distinction decides whether retrying is sensible.
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w: %s %s: status %d: %s", ErrRuntimeUnavailable, method, path, resp.StatusCode, oneLine(msg))
		}
		return fmt.Errorf("agent: %s %s: status %d: %s", method, path, resp.StatusCode, oneLine(msg))
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil && err != io.EOF {
			return fmt.Errorf("agent: %s %s: malformed response: %w", method, path, err)
		}
	}
	return nil
}

// ErrRuntimeUnavailable means the runtime could not be reached or failed
// server-side. Kept distinct so the loop can retry it without treating an
// operator error as transient.
var ErrRuntimeUnavailable = fmt.Errorf("agent: runtime unavailable")

func (r *RuntimeHTTP) Observe(ctx context.Context) (Observed, error) {
	var health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := r.do(ctx, http.MethodGet, "/health", nil, &health); err != nil {
		return Observed{}, err
	}
	// The release the runtime is actually serving is not in /health, so it comes
	// from the local view. Health answers liveness and maintenance state; the
	// release identity is a filesystem fact.
	var maint struct {
		Mode string `json:"mode"`
	}
	if err := r.do(ctx, http.MethodGet, "/v1/runtime/maintenance", nil, &maint); err != nil {
		return Observed{}, err
	}
	return Observed{
		RuntimeVersion: health.Version,
		Health:         health.Status,
		Maintenance:    maint.Mode,
	}, nil
}

// EnterMaintenance gates the runtime and waits until nothing is running.
//
// The wait is what makes this a drain rather than a gate: WP2's contract refuses
// new work immediately but lets in-flight runs finish, and the deadline is where
// the agent gives up rather than killing them.
func (r *RuntimeHTTP) EnterMaintenance(ctx context.Context, reason string, deadline time.Duration) error {
	body := map[string]string{}
	if reason != "" {
		body["reason"] = reason
	}
	if err := r.do(ctx, http.MethodPost, "/v1/runtime/maintenance", body, nil); err != nil {
		return err
	}

	// Poll until the active count reaches zero. Bounded by the deadline, after
	// which the caller aborts -- and aborting is the point: the protocol does not
	// trade a customer's in-flight work for a deploy.
	stop := time.Now().Add(deadline)
	for {
		var view struct {
			Mode    string `json:"mode"`
			Running int    `json:"running"`
		}
		if err := r.do(ctx, http.MethodGet, "/v1/runtime/maintenance", nil, &view); err != nil {
			return err
		}
		if view.Running == 0 {
			return nil
		}
		if time.Now().After(stop) {
			return fmt.Errorf("%w: %d run(s) still active after %s", ErrDrainTimeout, view.Running, deadline)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (r *RuntimeHTTP) ExitMaintenance(ctx context.Context) error {
	return r.do(ctx, http.MethodDelete, "/v1/runtime/maintenance", nil, nil)
}

// What is NOT here, and why
//
// Runtime has five methods. RuntimeHTTP implements three of them, and the other
// two are deliberately absent rather than stubbed:
//
//	Fetch    downloading and digest-verifying a release
//	Promote  making a verified release active
//	Validate checking it without executing a customer integration
//
// Building this surfaced a real constraint. The runtime API has a maintenance
// gate, because WP2 added one, but it has **no release-activation endpoint**, and
// the release package exposes no public activate method either -- activation
// happens through the CLI's own path (internal/release/stage.go creates the
// `active` symlink). So a pull-based deploy cannot be expressed as runtime API
// calls today.
//
// The fix is a small addition rather than a workaround, and there are two
// options worth deciding between before writing it:
//
//	1. add `POST /v1/runtime/releases/{digest}/activate` to the runtime API, so
//	   the agent stays a client of the API and inherits its authorisation and
//	   audit; or
//	2. export an `Activate` on internal/release and let the agent call it
//	   in-process, which is simpler but makes the agent reach past the API.
//
// Option 1 is the one consistent with the rest of this design -- the agent uses
// the same API an operator would, so it cannot do anything an operator could not
// and every action is auditable in one place. This is recorded here rather than
// implemented as a stub, because a Promote that compiled but did nothing would
// be mistaken for a working deploy path.
