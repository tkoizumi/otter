// Package awscreds retrieves AWS credentials from the EC2 instance metadata
// service.
//
// It exists so two callers can share one implementation: the agent's bootstrap
// identity, and the host broker's read of AWS Secrets Manager. Both want the
// same property — **no long-lived secret on disk**. The instance role's
// credentials are short-lived, fetched over IMDSv2, and rotated by the platform.
//
// It deliberately does not import either caller. A credential is a credential;
// what an agent claims about itself, and what a broker is allowed to read, are
// the callers' business.
package awscreds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	// ErrUnavailable is a transient or environmental failure: IMDS did not
	// answer, or answered badly. Retrying may help.
	ErrUnavailable = errors.New("awscreds: metadata service unavailable")

	// ErrRefused is a configuration refusal: IMDSv2 is disabled, or the role is
	// not attached. Retrying will not change it.
	ErrRefused = errors.New("awscreds: metadata service refused")
)

// DefaultBase is the link-local IMDS address.
const DefaultBase = "http://169.254.169.254"

// refreshWindow is how long before expiry a credential is treated as stale. The
// instance role rotates credentials hours apart, so a generous window costs
// nothing and avoids handing a request a credential that expires mid-flight.
const refreshWindow = 5 * time.Minute

// Credential is a resolved AWS credential.
type Credential struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// Usable reports whether the credential carries enough to sign with.
func (c Credential) Usable() bool {
	return c.AccessKeyID != "" && c.SecretAccessKey != ""
}

// Static is a fixed credential, for tests and for an environment that exports
// one directly.
type Static Credential

// Retrieve implements the shape a signing caller wants.
func (s Static) Retrieve(context.Context) (Credential, error) {
	cred := Credential(s)
	if !cred.Usable() {
		return Credential{}, errors.New("awscreds: static credential is incomplete")
	}
	return cred, nil
}

// IMDS reads the instance metadata service over IMDSv2.
type IMDS struct {
	// Base overrides the metadata address, so the flow can be tested without a
	// real instance and a non-EC2 environment can be simulated. Empty uses
	// DefaultBase.
	Base string
	// HTTP is injectable for tests.
	HTTP *http.Client
}

// NewIMDS builds an IMDS reader.
func NewIMDS(base string) *IMDS { return &IMDS{Base: base} }

func (m *IMDS) client() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	return &http.Client{Timeout: 5 * time.Second}
}

func (m *IMDS) base() string {
	if m.Base != "" {
		return m.Base
	}
	return DefaultBase
}

// Get performs an IMDSv2 GET: a token first, then the value.
//
// IMDSv1 is not used even as a fallback, because falling back to the weaker mode
// would make the stronger mode optional, and an attacker able to suppress the
// token request could force the downgrade.
func (m *IMDS) Get(ctx context.Context, path string) (string, error) {
	base := m.base()

	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/latest/api/token", nil)
	if err != nil {
		return "", err
	}
	tokenReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "300")
	tokenResp, err := m.client().Do(tokenReq)
	if err != nil {
		return "", fmt.Errorf("%w: IMDS token: %v", ErrUnavailable, err)
	}
	defer tokenResp.Body.Close()
	if tokenResp.StatusCode == http.StatusUnauthorized || tokenResp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("%w: IMDSv2 is not available (status %d)", ErrRefused, tokenResp.StatusCode)
	}
	if tokenResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: IMDS token status %d", ErrUnavailable, tokenResp.StatusCode)
	}
	token, err := io.ReadAll(io.LimitReader(tokenResp.Body, 4096))
	if err != nil {
		return "", fmt.Errorf("%w: IMDS token read: %v", ErrUnavailable, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-aws-ec2-metadata-token", string(token))
	resp, err := m.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: IMDS %s: %v", ErrUnavailable, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// A missing entry is a refusal, not a transient fault: the role or the
		// path is not attached to this instance.
		return "", fmt.Errorf("%w: IMDS %s is not present", ErrRefused, path)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: IMDS %s status %d", ErrUnavailable, path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return "", fmt.Errorf("%w: IMDS %s read: %v", ErrUnavailable, path, err)
	}
	return string(body), nil
}

// Region returns the instance's region, so a caller does not have to be
// configured with one that could disagree with where the instance actually is.
func (m *IMDS) Region(ctx context.Context) (string, error) {
	document, err := m.Get(ctx, "/latest/dynamic/instance-identity/document")
	if err != nil {
		return "", err
	}
	var parsed struct {
		Region string `json:"region"`
	}
	if err := json.Unmarshal([]byte(document), &parsed); err != nil {
		return "", fmt.Errorf("%w: instance identity document: %v", ErrUnavailable, err)
	}
	if parsed.Region == "" {
		return "", fmt.Errorf("%w: instance identity document has no region", ErrUnavailable)
	}
	return parsed.Region, nil
}

// RoleName returns the name of the IAM role attached to the instance.
func (m *IMDS) RoleName(ctx context.Context) (string, error) {
	raw, err := m.Get(ctx, "/latest/meta-data/iam/security-credentials/")
	if err != nil {
		return "", err
	}
	name := firstLine(raw)
	if name == "" {
		return "", fmt.Errorf("%w: no instance role is attached", ErrRefused)
	}
	return name, nil
}

// Credentials fetches the instance role's current credentials.
func (m *IMDS) Credentials(ctx context.Context) (Credential, error) {
	role, err := m.RoleName(ctx)
	if err != nil {
		return Credential{}, err
	}
	raw, err := m.Get(ctx, "/latest/meta-data/iam/security-credentials/"+role)
	if err != nil {
		return Credential{}, err
	}
	var parsed struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		Token           string `json:"Token"`
		Expiration      string `json:"Expiration"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return Credential{}, fmt.Errorf("%w: role credential document: %v", ErrUnavailable, err)
	}
	cred := Credential{
		AccessKeyID:     parsed.AccessKeyID,
		SecretAccessKey: parsed.SecretAccessKey,
		SessionToken:    parsed.Token,
	}
	if !cred.Usable() {
		return Credential{}, fmt.Errorf("%w: instance role returned no usable credentials", ErrUnavailable)
	}
	if parsed.Expiration != "" {
		if when, err := time.Parse(time.RFC3339, parsed.Expiration); err == nil {
			cred.Expiration = when
		}
	}
	return cred, nil
}

// Source caches an IMDS credential and refreshes it before it expires.
//
// Without a cache every signed request would read the metadata service, which is
// slow and rate-limited. A credential with no reported expiry is refreshed only
// when the caller's own signing fails, so an absent Expiration does not turn
// into a fetch per request.
type Source struct {
	IMDS *IMDS
	Now  func() time.Time

	mu      sync.Mutex
	current Credential
}

// Retrieve implements the shape a signing caller wants.
func (s *Source) Retrieve(ctx context.Context) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	if s.current.Usable() {
		if s.current.Expiration.IsZero() || now().Add(refreshWindow).Before(s.current.Expiration) {
			return s.current, nil
		}
	}

	cred, err := s.IMDS.Credentials(ctx)
	if err != nil {
		// A stale credential beats no credential: if a refresh fails while an
		// unexpired one is still held, the caller keeps signing with it.
		if s.current.Usable() && (s.current.Expiration.IsZero() || now().Before(s.current.Expiration)) {
			return s.current, nil
		}
		return Credential{}, err
	}
	s.current = cred
	return cred, nil
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
