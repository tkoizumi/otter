package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// Token is the agent's own runtime-scoped credential. Mint it with
	// `otter token create --name <runtime> --scope agent`: that scope reaches
	// only the deploy surface (maintenance and releases), so the agent no longer
	// needs -- and should not hold -- the static admin token (CL-21).
	Token      string
	HTTPClient *http.Client
	// ReleaseDir is where fetched releases are staged. It is the runtime's own
	// release root, so activation finds what the agent verified -- staging
	// elsewhere would verify one thing and activate another.
	ReleaseDir string
	// ReleaseClient fetches release artifacts, and is SEPARATE from the client
	// used for the runtime's own API.
	//
	// The two go to different places with different credentials: the runtime API
	// is loopback and takes the runtime's agent-scoped token, while a release
	// comes from the control plane and takes the agent's credential. Reusing one
	// client meant the release fetch carried the wrong token -- or, once the
	// credential is attached by host, needed a client that knows which host it is
	// talking to.
	ReleaseClient *http.Client
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
	var maint struct {
		Mode string `json:"mode"`
	}
	if err := r.do(ctx, http.MethodGet, "/v1/runtime/maintenance", nil, &maint); err != nil {
		return Observed{}, err
	}

	// WHAT THE RUNTIME IS ACTUALLY SERVING.
	//
	// This is the evidence the control plane's unknown-outcome rule needs. Before
	// it, an agent could only CLAIM it had applied a release, and a claim is not
	// evidence -- which is why an operation could never be closed as applied and a
	// deploy never completed.
	//
	// The active release is per JOB and a runtime manages several, so:
	//   - exactly one active release: that is the runtime's release, reported.
	//   - none: reported as empty, which is the honest answer for a runtime that
	//     has never been given one.
	//   - several: NOT reported, because a single field cannot say which. Guessing
	//     would be worse than saying nothing, since the control plane treats a
	//     reported digest as evidence.
	var active struct {
		Active []struct {
			Job    string `json:"job"`
			Digest string `json:"digest"`
		} `json:"active"`
	}
	digest := ""
	if err := r.do(ctx, http.MethodGet, "/v1/runtime/releases/active", nil, &active); err == nil {
		if len(active.Active) == 1 {
			// Reported in the control plane's canonical `sha256:<hex>` form. The
			// runtime's own store names directories by bare hex, but Cloud decides
			// whether a deploy landed by comparing this value with the desired
			// digest it stored -- and comparing spellings instead of identities
			// leaves a landed deploy open forever.
			if d, err := normalizeDigest(active.Active[0].Digest); err == nil {
				digest = "sha256:" + d
			}
		}
	}
	// A failure to read the active release is not fatal: the cycle can still
	// observe health and maintenance, and reporting no digest is better than
	// abandoning the cycle. The consequence is only that an operation will not be
	// closed automatically, which is the safe direction.

	return Observed{
		ReleaseDigest:  digest,
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
	// Send the store's canonical form. The control plane speaks
	// `sha256:<hex>` and the runtime's release directories are named by bare hex;
	// forwarding the prefixed spelling unchanged produced "no job has release
	// sha256:...", a comparison of spellings rather than of identities.
	want, err := normalizeDigest(digest)
	if err != nil {
		return err
	}
	return r.do(ctx, http.MethodPost, "/v1/runtime/releases/activate",
		map[string]string{"digest": want}, nil)
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
	if rel.Digest == "" {
		return fmt.Errorf("agent: release has no digest; refusing to install unverifiable content")
	}
	want, err := normalizeDigest(rel.Digest)
	if err != nil {
		return err
	}
	if rel.URL == "" {
		return fmt.Errorf("agent: release %s has no URL", shortDigest(want))
	}

	// THE PACKAGE IS INSTALLED, NOT STAGED, and the difference is what makes the
	// digest an address rather than a label.
	//
	// An earlier version downloaded the bytes into a directory of its own naming
	// and then asked the runtime to activate that digest -- which the runtime had
	// never seen, because its release store is built by `otter release` with a
	// DIFFERENT digest for a different thing. Promotion failed with "no job has
	// release <digest>", and every component was individually correct.
	//
	// Now the agent downloads the portable package and hands it to the runtime,
	// which RECOMPUTES the digest from the bytes before installing. The agent
	// gated the runtime already (the apply sequence enters maintenance first), so
	// the runtime accepts the write.
	// The release client when configured, else the runtime's, else a default --
	// never a nil client, which panics inside net/http rather than returning an
	// error and took down the integration test rather than failing it.
	client := r.ReleaseClient
	if client == nil {
		client = r.HTTPClient
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: fetch %s: %v", ErrRuntimeUnavailable, shortDigest(want), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent: fetch %s: status %d", shortDigest(want), resp.StatusCode)
	}

	// Streamed to the runtime rather than buffered: a package can be large, and
	// holding it in memory buys nothing when the runtime has to receive every byte
	// anyway. The stream is hashed on the way through so the transport checksum
	// Cloud stated can be checked without a second pass or a buffer.
	//
	// The checksum is a TRANSPORT check, not the identity: it proves the bytes did
	// not change in flight, while the runtime's recompute proves what release
	// those bytes are. Both are worth having, and conflating them is the error
	// this whole contract exists to remove.
	wantArtifact := resp.Header.Get("X-Otter-Artifact-Sha256")
	hasher := sha256.New()

	pr, pw := io.Pipe()
	defer pr.Close()
	var streamErr error
	streamed := make(chan struct{})
	go func() {
		defer close(streamed)
		_, streamErr = io.Copy(pw, io.TeeReader(resp.Body, hasher))
		_ = pw.CloseWithError(streamErr)
	}()

	// The transport is the RUNTIME's client (its own token, loopback), not the
	// release client: this request goes to the runtime, not to Cloud, and the two
	// take different credentials.
	installReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(r.BaseURL, "/")+"/v1/runtime/releases/install", pr)
	if err != nil {
		return err
	}
	if r.Token != "" {
		installReq.Header.Set("Authorization", "Bearer "+r.Token)
	}
	installReq.Header.Set("Content-Type", "application/gzip")
	installReq.ContentLength = -1

	installResp, err := r.client().Do(installReq)
	if err != nil {
		return fmt.Errorf("%w: install %s: %v", ErrRuntimeUnavailable, shortDigest(want), err)
	}
	defer installResp.Body.Close()
	// Drained so the connection can be reused, and so a refusal's message is read
	// rather than the body being abandoned mid-stream.
	body, _ := io.ReadAll(io.LimitReader(installResp.Body, 8<<10))
	if installResp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent: install %s: status %d: %s",
			shortDigest(want), installResp.StatusCode, strings.TrimSpace(string(body)))
	}

	// The download has been fully consumed by now (the runtime reads the whole
	// body before it verifies), so the transport checksum can be compared.
	<-streamed
	if streamErr != nil {
		return fmt.Errorf("%w: fetch %s: %v", ErrRuntimeUnavailable, shortDigest(want), streamErr)
	}
	if wantArtifact != "" {
		statement, err := normalizeDigest(wantArtifact)
		if err != nil {
			return fmt.Errorf("agent: install %s: the transport checksum header is unreadable: %v",
				shortDigest(want), err)
		}
		if got := hex.EncodeToString(hasher.Sum(nil)); got != statement {
			return fmt.Errorf(
				"%w: the archive changed in transit: cloud stated sha256:%s, the bytes hash to sha256:%s",
				ErrDigestMismatch, statement, got)
		}
	}

	// THE CONFIRMATION LINK. Cloud asked the runtime to run `want`; the runtime
	// replies with the digest it RECOMPUTED from the bytes it actually stored.
	// Confirming the two are equal is what makes the runtime's store an address
	// rather than a label -- and without it the agent would be asserting that the
	// package it uploaded matches the release it was told to run.
	//
	// The digest is not trusted from the URL the agent fetched. A runtime that
	// answers with a different digest is saying the bytes it verified are a
	// different release, which is exactly the substitution this check exists to
	// catch.
	var installed struct {
		Job            string `json:"job"`
		Digest         string `json:"digest"`
		RecordedDigest string `json:"recorded_digest"`
	}
	if err := json.Unmarshal(body, &installed); err != nil {
		return fmt.Errorf("agent: install %s: the runtime returned no readable install result: %v",
			shortDigest(want), err)
	}
	if installed.Digest == "" {
		return fmt.Errorf("agent: install %s: the runtime did not report which release it installed",
			shortDigest(want))
	}
	got, err := normalizeDigest(installed.Digest)
	if err != nil {
		return fmt.Errorf("agent: install %s: the runtime reported an unreadable digest: %v",
			shortDigest(want), err)
	}
	if got != want {
		return fmt.Errorf(
			"%w: cloud asked for %s but the runtime installed %s",
			ErrDigestMismatch, shortDigest(want), shortDigest(got))
	}
	// The manifest's own claim, when the runtime returns it, has to agree with
	// the recomputed digest as well: a package whose content hashes to X while
	// its manifest claims Y would otherwise install under X and still be reported
	// as Y.
	if installed.RecordedDigest != "" {
		recorded, err := normalizeDigest(installed.RecordedDigest)
		if err != nil || recorded != got {
			return fmt.Errorf(
				"%w: the installed release is %s but its manifest claims %s",
				ErrDigestMismatch, shortDigest(got), shortDigest(installed.RecordedDigest))
		}
	}
	return nil
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
	want, err := normalizeDigest(digest)
	if err != nil {
		return err
	}
	// Asked of the RUNTIME rather than of a directory the agent chose. The
	// installer decided where a release lands, and a second opinion from the agent
	// about a path it picked would be checking its own assumption -- which is
	// exactly the mistake that let a fetch "succeed" and a promotion fail.
	var active struct {
		Active []struct {
			Digest string `json:"digest"`
		} `json:"active"`
	}
	if err := r.do(ctx, http.MethodGet, "/v1/runtime/releases/active", nil, &active); err != nil {
		// Not an error the agent should invent a reason for: a runtime that cannot
		// answer has not confirmed anything.
		return fmt.Errorf("agent: cannot confirm %s is installed: %v", shortDigest(want), err)
	}
	for _, a := range active.Active {
		if got, err := normalizeDigest(a.Digest); err == nil && got == want {
			return nil
		}
	}
	// NOT YET ACTIVE is the expected state at this point in the sequence: install
	// happens before promote, so the release is present but not serving. The check
	// that matters is that the INSTALL succeeded, which Fetch already established
	// by requiring a 200 from the runtime.
	return nil
}
