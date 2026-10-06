package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	// ReleaseDir is where fetched releases are staged. It is the runtime's own
	// release root, so activation finds what the agent verified -- staging
	// elsewhere would verify one thing and activate another.
	ReleaseDir string
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

// Promote makes a verified release active.
//
// It goes through the runtime API -- POST /v1/runtime/releases/activate -- rather
// than touching the data directory. That endpoint was added for this purpose and
// the reason is the property the whole agent design rests on: the agent must not
// be able to do anything an operator could not, and every action must be
// auditable in one place.
//
// The runtime refuses activation while serving, so a caller that skipped
// EnterMaintenance gets an explicit conflict rather than swapping the active
// release under running work.
func (r *RuntimeHTTP) Promote(ctx context.Context, digest string) error {
	if digest == "" {
		return fmt.Errorf("agent: promote needs a digest")
	}
	return r.do(ctx, http.MethodPost, "/v1/runtime/releases/activate",
		map[string]string{"digest": digest}, nil)
}

// Fetch and Validate remain unimplemented, and that is a finding rather than an
// omission.
//
// Fetch   downloading a release and verifying it against its digest
// Validate checking it without executing a customer integration
//
// Neither has a runtime API endpoint. Fetch is genuinely agent-side work -- the
// agent has the release URL and the digest, so it can download and verify
// without the runtime's help, and the verification is the agent's own
// responsibility because it is the agent that must not be deceived. Validate is
// the harder question: checking a release without running a customer integration
// means reading its manifest, paths and dependency metadata, which the runtime
// already does at release time, so it may be that activation's own refusal is the
// validation and a separate call is unnecessary.
//
// Recorded here rather than stubbed: a Validate that compiled but checked nothing
// would be worse than none, because the apply sequence would report it as a step
// that passed.

// Fetch downloads and digest-verifies a release.
//
// It lives on RuntimeHTTP rather than a separate type because the Runtime
// interface requires it, and because the destination is a property of the
// runtime being managed: releases must land where activation looks for them.
//
// It is agent-side work, not an endpoint. The agent has the URL and the digest,
// and it is the agent that must not be deceived, so asking the runtime to fetch
// and verify would move the check to the component being protected.
func (r *RuntimeHTTP) Fetch(ctx context.Context, rel Release) error {
	if r.ReleaseDir == "" {
		// Refusing rather than fetching nowhere: a release that lands outside the
		// runtime's release root can never be activated, and a later validation
		// failure would look like a corrupt release rather than a misconfiguration.
		return fmt.Errorf("agent: runtime client has no release directory; cannot stage %s", shortDigest(rel.Digest))
	}
	f := &Fetcher{Dir: r.ReleaseDir, HTTPClient: r.HTTPClient}
	return f.Fetch(ctx, rel)
}

// Validate checks a staged release before it is activated.
//
// This is deliberately shallow, and the reason is worth stating rather than
// hiding behind a thorough-looking check: the runtime already performs the real
// validation at release time (snapshotting, path resolution, environment
// readiness) and again at activation, where it refuses a release whose metadata
// does not resolve. Re-implementing that here would mean two validators that can
// disagree, and the agent's copy would be the one with less context.
//
// What this DOES catch is the one failure the activation refusal cannot: a
// release the agent believes it staged and that is not present at all. That is
// the gap the apply sequence's validate step exists to close, and pretending it
// is more would be worse than stating its scope.
func (r *RuntimeHTTP) Validate(ctx context.Context, digest string) error {
	if digest == "" {
		return fmt.Errorf("agent: validate needs a digest")
	}
	if r.ReleaseDir == "" {
		return fmt.Errorf("agent: cannot validate %s: no release directory configured", shortDigest(digest))
	}
	want, err := normalizeDigest(digest)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(r.ReleaseDir, want)); err != nil {
		// Not present: either the fetch silently failed or the digest differs.
		// Either way, activating would fail at the runtime with a message about
		// a missing release, which is less specific than saying so here.
		return fmt.Errorf("agent: release %s is not staged in %s: %w", shortDigest(want), r.ReleaseDir, err)
	}
	return nil
}
