package pool

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

// Client talks to the control plane's host channel with a pool token.
//
// Separate from `internal/cloud.Client` for one reason that matters: that client
// sends `Authorization: Bearer`, which is the *organization* credential, and this
// one sends the host credential in its own header. Presenting one where the other is
// expected must not be possible by accident, so the two are different types with
// different headers rather than one client with a flag.
//
// It reuses the control plane's error envelope shape, so a refusal reads the same
// way here as it does in `otter deploy --cloud`.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient builds a host-channel client.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// APIError is a non-2xx answer from the control plane, carrying the server's own
// message because a refusal is the server's to explain.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if strings.TrimSpace(e.Message) == "" {
		return fmt.Sprintf("control plane returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("control plane returned HTTP %d: %s", e.StatusCode, e.Message)
}

// IsUnauthorized reports whether the pool token was refused.
func IsUnauthorized(err error) bool {
	var apiErr *APIError
	return asAPIError(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden)
}

// IsUnavailable reports whether the control plane cannot serve the host channel at
// all — an unbound registry, or a fault. Distinct from an authorization failure
// because the fix is different: retry, versus install the right credential.
func IsUnavailable(err error) bool {
	var apiErr *APIError
	return asAPIError(err, &apiErr) && apiErr.StatusCode == http.StatusServiceUnavailable
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if apiErr, ok := err.(*APIError); ok {
			*target = apiErr
			return true
		}
		type unwrapper interface{ Unwrap() error }
		next, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = next.Unwrap()
	}
	return false
}

// Work asks for one unit of work, reporting the host's own observations.
//
// The heartbeat travels with the request: a host that is asking is a host that is
// alive, and keeping liveness as one fact with one writer avoids a second loop.
func (c *Client) Work(ctx context.Context, cloudURL string, report Report) (*WorkResponse, error) {
	body := map[string]any{"cloud_url": cloudURL}
	if report.FreeDiskMB != nil {
		body["free_disk_mb"] = *report.FreeDiskMB
	}
	if report.TenantCount != nil {
		body["tenant_count"] = *report.TenantCount
	}
	var out WorkResponse
	if err := c.post(ctx, "/agent/v1/host/work", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Report sends an outcome and the host's observations.
func (c *Client) Report(ctx context.Context, report Report) error {
	return c.post(ctx, "/agent/v1/host/report", report, nil)
}

// Release hands a claim back without an outcome.
//
// Used by a dry run, which must not consume the work it is rehearsing: an agent that
// claims and then skips execution would leave the request in `provisioning` until the
// lease lapsed, and the user would watch a runtime nobody was creating.
func (c *Client) Release(ctx context.Context, runtimeID string) error {
	return c.post(ctx, "/agent/v1/host/release", map[string]string{"runtime_id": runtimeID}, nil)
}

// post sends one JSON body to the host channel.
func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	// The host credential, in its own header. Never `Authorization`.
	req.Header.Set("x-otter-pool-token", c.Token)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("reach %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{StatusCode: resp.StatusCode, Message: errorMessage(payload)}
	}
	if out == nil || len(bytes.TrimSpace(payload)) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// errorMessage reads the control plane's error envelope, falling back to the raw
// body so an unexpected shape is still reported rather than swallowed.
func errorMessage(payload []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	return strings.TrimSpace(string(payload))
}
