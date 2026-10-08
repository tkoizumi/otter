package cloud

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/tkoizumi/otter/internal/release"
)

// Client talks to one Otter Cloud control plane with one credential.
//
// It is deliberately small and dumb: every method is one endpoint from the
// frozen protocol, with no retries and no caching, so a failure is reported to
// the operator instead of being papered over.
type Client struct {
	BaseURL string
	Token   string
	// HTTP is the transport. Nil uses a client with a bounded timeout, so a
	// hung control plane fails a command instead of holding a terminal open.
	HTTP *http.Client
}

// NewClient builds a client for a base URL and token.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// APIError is a non-2xx control-plane response. Message is the server's own
// text when it sent one: a refusal is the server's to explain, and rewriting it
// here would lose the reason.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if strings.TrimSpace(e.Message) == "" {
		return fmt.Sprintf("cloud returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("cloud returned HTTP %d: %s", e.StatusCode, e.Message)
}

// IsUnauthorized reports whether err is a 401 or 403: the credential is
// missing, invalid or revoked.
func IsUnauthorized(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden)
}

// IsNotFound reports whether err is a 404.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// IsMethodNotAllowed reports whether err is a 405: the endpoint is real but this
// deployment does not serve the method. A caller offering an optional probe can
// read it as "the control plane cannot answer that question" rather than as a
// failure of the command.
func IsMethodNotAllowed(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusMethodNotAllowed
}

// RefusedError is an admitted=false answer to a deploy. It is separate from
// APIError because the control plane answered 200 and still declined the work:
// the caller must surface Message and exit non-zero, not treat the operation as
// accepted.
type RefusedError struct {
	Message string
}

func (e *RefusedError) Error() string {
	if strings.TrimSpace(e.Message) == "" {
		return "the control plane refused the deploy"
	}
	return e.Message
}

// Organization is the tenant a credential belongs to.
type Organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Runtime is one runtime the organization owns. Lifecycle is the deployment
// state (for example "running") and Placement is where it runs; both are
// opaque to the CLI and printed as-is.
type Runtime struct {
	ID        string `json:"id"`
	Lifecycle string `json:"lifecycle"`
	Placement string `json:"placement"`
}

// Me is the verified identity behind a credential.
type Me struct {
	Organization Organization `json:"organization"`
	Runtimes     []Runtime    `json:"runtimes"`
}

// Operation is one admitted operation on a runtime. Only the fields the CLI
// reports are named; the control plane may send more and they are ignored.
type Operation struct {
	ID     string `json:"id"`
	Status string `json:"status,omitempty"`
}

// OperationsState is the ops state of a runtime. Generation is the value a
// deploy must present as expected_generation, which is how the control plane
// refuses a promotion planned against a state that has moved on.
type OperationsState struct {
	Generation int         `json:"generation"`
	Operations []Operation `json:"operations,omitempty"`
}

// Release is the control plane's record of an uploaded release.
type Release struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

// Me verifies the credential and returns the organization it belongs to.
func (c *Client) Me(ctx context.Context) (*Me, error) {
	var out Me
	if err := c.get(ctx, "/api/cloud/me", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Operations reads a runtime's ops state, including the generation a deploy
// must echo back.
func (c *Client) Operations(ctx context.Context, runtimeID string) (*OperationsState, error) {
	var out OperationsState
	if err := c.get(ctx, "/api/runtimes/"+pathSegment(runtimeID)+"/operations", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Release reads the control plane's record of one uploaded release, which is how
// a promotion answers "does Cloud already hold this digest?" before deciding
// whether it has anything to send.
//
// The probe is an optimisation, not a correctness requirement: uploading is
// content-addressed and therefore idempotent, so a caller that cannot get an
// answer may still upload. A digest Cloud does not hold is a 404, which the
// caller distinguishes with IsNotFound. The digest travels as bare hex in the
// path, matching the x-otter-release-digest header the upload uses, so the two
// calls name the release the same way.
func (c *Client) Release(ctx context.Context, digest string) (*Release, error) {
	bare, err := release.NormalizeDigest(digest)
	if err != nil {
		return nil, fmt.Errorf("release: %w", err)
	}
	var out Release
	if err := c.get(ctx, "/api/releases/"+bare, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UploadRelease sends one package tarball under its canonical release digest.
//
// The digest travels in x-otter-release-digest as BARE hex, and the body is
// the archive bytes: the header is the release identity and the body is its
// transport. The two are different values and the protocol keeps them apart for
// exactly that reason.
func (c *Client) UploadRelease(ctx context.Context, digest string, body io.Reader) (*Release, error) {
	bare, err := release.NormalizeDigest(digest)
	if err != nil {
		return nil, fmt.Errorf("upload release: %w", err)
	}

	req, err := c.request(ctx, http.MethodPost, "/api/releases", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("x-otter-release-digest", bare)
	// A file body carries no length unless it is one of the readers net/http
	// recognizes. Stating it lets the server reject an oversized upload before
	// reading it.
	if sizer, ok := body.(interface {
		Stat() (os.FileInfo, error)
	}); ok {
		if info, statErr := sizer.Stat(); statErr == nil {
			req.ContentLength = info.Size()
		}
	}

	var out Release
	if err := c.roundTrip(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeployRequest is the body of a deploy operation.
type DeployRequest struct {
	Action             string `json:"action"`
	DesiredDigest      string `json:"desired_digest"`
	ExpectedGeneration int    `json:"expected_generation"`
	Operator           string `json:"operator"`
	// JobID names the job the release belongs to. It is required by a control
	// plane that keeps per-job desired state: a deploy that did not name its job
	// could never be matched against what the runtime reports, so the operation
	// would sit unconfirmed forever. An older control plane ignores it.
	JobID string `json:"job_id,omitempty"`
}

// Admission is the answer to a deploy operation.
type Admission struct {
	Admitted  bool       `json:"admitted"`
	Operation *Operation `json:"operation,omitempty"`
	Message   string     `json:"message,omitempty"`
}

// Deploy asks the control plane to move a runtime to a release.
//
// A refused operation is not an HTTP error -- the control plane received and
// understood the request -- so it comes back as *RefusedError with the server's
// own Message, alongside the parsed Admission. The caller prints the message
// and exits non-zero; only a genuine protocol or transport failure is an
// APIError.
//
// jobID names the job the release belongs to and is sent so a control plane
// keeping per-job desired state can address it. A control plane that does not
// know the field ignores it.
func (c *Client) Deploy(ctx context.Context, runtimeID, jobID, digest, operator string, expectedGeneration int) (*Admission, error) {
	bare, err := release.NormalizeDigest(digest)
	if err != nil {
		return nil, fmt.Errorf("deploy: %w", err)
	}
	payload, err := json.Marshal(DeployRequest{
		Action:             "deploy",
		DesiredDigest:      bare,
		ExpectedGeneration: expectedGeneration,
		Operator:           operator,
		JobID:              strings.TrimSpace(jobID),
	})
	if err != nil {
		return nil, fmt.Errorf("encode deploy request: %w", err)
	}

	req, err := c.request(ctx, http.MethodPost, "/api/runtimes/"+pathSegment(runtimeID)+"/operations", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	var out Admission
	if err := c.roundTrip(req, &out); err != nil {
		return &out, err
	}
	if !out.Admitted {
		return &out, &RefusedError{Message: out.Message}
	}
	return &out, nil
}

// Job is the control plane's projection of one runtime job. Only the fields a
// reference needs are named: the id addresses the delete route, and the name is
// the label an operator types.
type Job struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	RuntimeID string `json:"runtimeId"`
}

// Jobs lists the organization's jobs, or one runtime's when runtimeID is given.
//
// It is the read that lets `otter delete --cloud counter` resolve a label to
// the durable id the delete route takes, so a user never has to paste a UUID.
func (c *Client) Jobs(ctx context.Context, runtimeID string) ([]Job, error) {
	path := "/api/jobs"
	if id := strings.TrimSpace(runtimeID); id != "" {
		path += "?runtimeId=" + url.QueryEscape(id)
	}
	var out struct {
		Jobs []Job `json:"jobs"`
	}
	if err := c.get(ctx, path, &out); err != nil {
		return nil, err
	}
	return out.Jobs, nil
}

// JobDeleted is the runtime's own account of a delete: the reference it acted
// on and the note that explains the delete's scope.
//
// The note is not decoration. A job known only from its releases has no
// registry row, so no tombstone is written and releasing the same id brings it
// back; a caller that reported a bare "deleted" would hide that.
type JobDeleted struct {
	JobID   string `json:"jobId"`
	Deleted bool   `json:"deleted"`
	// RemovedFromDesired reports whether Cloud's desired state no longer names
	// the job. It is a POINTER because an older control plane does not send it,
	// and "not reported" must fall back to the delete's own confirmation rather
	// than being read as "not removed".
	RemovedFromDesired *bool `json:"removedFromDesired,omitempty"`
	// Pending reports that Cloud recorded the removal but the runtime has not
	// confirmed it. It is deliberately distinct from `deleted`.
	Pending bool   `json:"pending,omitempty"`
	Note    string `json:"note,omitempty"`
}

// DeleteJobResult is the command envelope DELETE /api/jobs/{id} answers with.
// Job repeats Value, the shape the sibling command routes return, so a poller
// that only knows the command's reference can match the answer; either side may
// carry the runtime's note.
type DeleteJobResult struct {
	Applied bool        `json:"applied"`
	Value   JobDeleted  `json:"value"`
	Job     *JobDeleted `json:"job,omitempty"`
}

// RemovedFromDesired reports whether the job left Cloud's desired state. An
// older control plane does not report the field, so its absence inherits the
// delete's own confirmation -- which for that version is the runtime's answer
// and nothing weaker.
func (r *DeleteJobResult) RemovedFromDesired() bool {
	if r == nil {
		return false
	}
	if r.Value.RemovedFromDesired != nil {
		return *r.Value.RemovedFromDesired
	}
	return r.Value.Deleted
}

// Pending reports that the removal is recorded but unconfirmed by the runtime.
func (r *DeleteJobResult) Pending() bool {
	return r != nil && r.Value.Pending
}

// Deleted reports whether the runtime confirmed the removal.
func (r *DeleteJobResult) Deleted() bool {
	return r != nil && r.Value.Deleted
}

// Note is the runtime's account of what the delete actually removed, when it
// sent one. A caller must surface it rather than report a scope it did not
// verify.
func (r *DeleteJobResult) Note() string {
	if r == nil {
		return ""
	}
	if strings.TrimSpace(r.Value.Note) != "" {
		return r.Value.Note
	}
	if r.Job != nil {
		return r.Job.Note
	}
	return ""
}

// DeleteJob deletes one job and everything it owns through
// DELETE /api/jobs/{id}.
//
// The control plane refuses the call with a 400 without an Idempotency-Key, so
// a fresh key is minted for every deliberate delete: the key is what makes a
// retried request replay the same command instead of deleting twice. jobID must
// be the runtime's own job id -- resolve a label with Jobs first -- and an id
// the fleet does not have comes back as a 404, which IsNotFound distinguishes
// from a transport or admission failure.
func (c *Client) DeleteJob(ctx context.Context, jobID string) (*DeleteJobResult, error) {
	req, err := c.request(ctx, http.MethodDelete, "/api/jobs/"+pathSegment(jobID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Idempotency-Key", NewIdempotencyKey())

	var out DeleteJobResult
	if err := c.roundTrip(req, &out); err != nil {
		return &out, err
	}
	return &out, nil
}

// NewIdempotencyKey mints a key for one mutating control-plane command.
//
// It is random rather than derived from the request: the key identifies one
// deliberate intent, so two deliberate deletes must not share one or the second
// would replay as a no-op. A caller that retries reuses the key it already has;
// the CLI makes no retries, so each call mints a fresh one.
func NewIdempotencyKey() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail in practice. A non-empty fallback keeps the
		// request legal rather than sending a delete with no key at all.
		return fmt.Sprintf("otk_%d", time.Now().UnixNano())
	}
	return "otk_" + hex.EncodeToString(buf[:])
}

// get performs an authenticated GET and decodes the JSON body.
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return c.roundTrip(req, out)
}

// request builds an authenticated request. Every call carries the bearer
// credential: the control plane decides what it may do, and a client that
// selectively omitted the header would fail in a way that looked like a
// protocol bug rather than an authorization one.
func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// roundTrip sends a request and either decodes the JSON response or turns the
// status and the server's message into an APIError.
func (c *Client) roundTrip(req *http.Request, out any) error {
	resp, err := c.transport().Do(req)
	if err != nil {
		return fmt.Errorf("reach %s: %w", c.BaseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{StatusCode: resp.StatusCode, Message: messageFromBody(body)}
	}
	if readErr != nil {
		return fmt.Errorf("read response from %s: %w", c.BaseURL, readErr)
	}
	if out == nil || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response from %s: %w", c.BaseURL, err)
	}
	return nil
}

// transport is the HTTP client, defaulting when the struct was built by hand.
func (c *Client) transport() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// messageFromBody extracts the server's explanation from an error response.
//
// The protocol does not pin the field name, so the common spellings are tried
// and the raw text is the fallback. A JSON body with none of them still
// produces something better than an empty message.
func messageFromBody(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return ""
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &envelope); err == nil {
		for _, key := range []string{"message", "error", "detail", "reason"} {
			raw, ok := envelope[key]
			if !ok {
				continue
			}
			var text string
			if err := json.Unmarshal(raw, &text); err == nil && strings.TrimSpace(text) != "" {
				return text
			}
		}
	}
	return strings.TrimSpace(string(trimmed))
}

// pathSegment escapes an id for use inside a URL path, so an id containing a
// slash cannot silently address a different endpoint.
func pathSegment(id string) string {
	return strings.ReplaceAll(strings.TrimSpace(id), "/", "%2F")
}
