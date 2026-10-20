// Package awssecrets reads secrets from AWS Secrets Manager.
//
// It exists so the host broker can resolve a job's secrets with the host's
// instance role, without pulling the AWS SDK into a binary that runs on every
// pool host. Requests are signed by `internal/awsv4`, which the agent's
// bootstrap assertion already uses.
//
// The client is write-free on purpose: the host broker only ever READS. Writing
// a secret belongs to the control plane's write-through path, and the control
// plane holds a separate, write-only credential (see D4).
package awssecrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tkoizumi/otter/internal/awscreds"
	"github.com/tkoizumi/otter/internal/awsv4"
)

// ErrNotFound is returned for a secret that does not exist, so a caller can
// distinguish a missing secret from a backend that refused or failed.
var ErrNotFound = errors.New("secret not found")

// batchLimit is the Secrets Manager maximum for BatchGetSecretValue.
const batchLimit = 20

// maxBody bounds a response body. A secret is capped well below this by the
// service; anything larger is a misdirected endpoint.
const maxBody = 1 << 20

// Credentials supplies the credential used to sign. `*awscreds.Source` is the
// host's implementation, reading the instance role; `awscreds.Static` is for
// tests and for an environment that exports one directly.
type Credentials interface {
	Retrieve(ctx context.Context) (awscreds.Credential, error)
}

// Client calls the Secrets Manager JSON API.
type Client struct {
	Region string
	// Endpoint overrides the service URL. Empty uses the regional endpoint.
	// Overridden by tests, and by a deployment reaching Secrets Manager through
	// a private endpoint.
	Endpoint string
	Creds    Credentials
	HTTP     *http.Client
	Now      func() time.Time
}

// New builds a client.
func New(region string, creds Credentials, opts ...func(*Client)) *Client {
	c := &Client{Region: region, Creds: creds}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) serviceURL() string {
	if c.Endpoint != "" {
		return strings.TrimRight(c.Endpoint, "/")
	}
	return "https://secretsmanager." + c.Region + ".amazonaws.com"
}

// GetSecretValue returns one secret's string value.
func (c *Client) GetSecretValue(ctx context.Context, secretID string) (string, error) {
	body, err := json.Marshal(map[string]string{"SecretId": secretID})
	if err != nil {
		return "", fmt.Errorf("awssecrets: marshal request: %w", err)
	}
	var out struct {
		SecretString string `json:"SecretString"`
		SecretBinary string `json:"SecretBinary"`
	}
	if err := c.call(ctx, "secretsmanager.GetSecretValue", body, &out); err != nil {
		return "", err
	}
	if out.SecretString != "" {
		return out.SecretString, nil
	}
	// A binary secret is not something a job secret should be: the runtime
	// injects strings into the child environment.
	if out.SecretBinary != "" {
		return "", fmt.Errorf("awssecrets: %s holds a binary secret, which cannot be injected as an environment value", secretID)
	}
	return "", fmt.Errorf("awssecrets: %s has no secret string", secretID)
}

// ListSecretNames returns every secret whose name begins with prefix, following
// pagination.
func (c *Client) ListSecretNames(ctx context.Context, prefix string) ([]string, error) {
	var names []string
	var next string
	for {
		payload := map[string]any{
			"MaxResults": 100,
			"Filters":    []map[string]any{{"Key": "name", "Values": []string{prefix}}},
		}
		if next != "" {
			payload["NextToken"] = next
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("awssecrets: marshal list request: %w", err)
		}
		var out struct {
			SecretList []struct {
				Name string `json:"Name"`
			} `json:"SecretList"`
			NextToken string `json:"NextToken"`
		}
		if err := c.call(ctx, "secretsmanager.ListSecrets", body, &out); err != nil {
			return nil, err
		}
		for _, entry := range out.SecretList {
			names = append(names, entry.Name)
		}
		if out.NextToken == "" {
			return names, nil
		}
		next = out.NextToken
	}
}

// BatchGetSecretValues returns the values for ids, in chunks the API accepts.
//
// An id that does not exist is reported in the response's Errors list rather
// than failing the call, so a single missing secret does not make the whole job
// unresolvable.
func (c *Client) BatchGetSecretValues(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	for start := 0; start < len(ids); start += batchLimit {
		end := start + batchLimit
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		body, err := json.Marshal(map[string]any{"SecretIdList": chunk})
		if err != nil {
			return nil, fmt.Errorf("awssecrets: marshal batch request: %w", err)
		}
		var payload struct {
			SecretValues []struct {
				Name         string `json:"Name"`
				SecretString string `json:"SecretString"`
			} `json:"SecretValues"`
		}
		if err := c.call(ctx, "secretsmanager.BatchGetSecretValue", body, &payload); err != nil {
			return nil, err
		}
		for _, value := range payload.SecretValues {
			out[value.Name] = value.SecretString
		}
	}
	return out, nil
}

// apiError is the shape Secrets Manager returns for a failure.
type apiError struct {
	Type    string `json:"__type"`
	Message string `json:"message"`
}

func (c *Client) call(ctx context.Context, target string, body []byte, out any) error {
	cred, err := c.Creds.Retrieve(ctx)
	if err != nil {
		return err
	}
	endpoint := c.serviceURL()
	host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")

	headers, err := awsv4.Input{
		AccessKeyID:     cred.AccessKeyID,
		SecretAccessKey: cred.SecretAccessKey,
		SessionToken:    cred.SessionToken,
		Region:          c.Region,
		Service:         "secretsmanager",
		Method:          http.MethodPost,
		Host:            host,
		Path:            "/",
		Payload:         body,
		Now:             c.now(),
		Headers: map[string]string{
			"content-type": "application/x-amz-json-1.1",
			"x-amz-target": target,
		},
	}.Header()
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("awssecrets: build request: %w", err)
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Set(name, value)
		}
	}

	response, err := c.client().Do(request)
	if err != nil {
		return fmt.Errorf("awssecrets: %s: %w", target, err)
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBody))
	if err != nil {
		return fmt.Errorf("awssecrets: read response: %w", err)
	}

	if response.StatusCode != http.StatusOK {
		var apiErr apiError
		_ = json.Unmarshal(raw, &apiErr)
		// The error text is returned for the caller to log; it is never handed
		// to a tenant. A missing secret is separated out because it is a
		// configuration answer, not a failure.
		if strings.Contains(apiErr.Type, "ResourceNotFoundException") {
			return fmt.Errorf("awssecrets: %s: %w", target, ErrNotFound)
		}
		return fmt.Errorf("awssecrets: %s refused with %d: %s", target, response.StatusCode, strings.TrimSpace(string(raw)))
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("awssecrets: decode %s response: %w", target, err)
	}
	return nil
}
