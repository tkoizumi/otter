package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/tkoizumi/otter/internal/awscreds"
)

// AWSInstanceRole proves host identity using the EC2 instance role.
//
// This is the bootstrap mechanism the plan chose for the AWS implementation, and
// the property that makes it worth the complexity is negative: **no long-lived
// secret is stored on disk.** The instance role's credentials are short-lived,
// fetched from the instance metadata service, and rotated by the platform. If the
// host is compromised, the operator revokes the agent registration and replaces
// the host; there is no bootstrap secret sitting in a file to hunt down.
//
// The assertion is a SigV4-signed request to the control plane's identity
// endpoint. Signing with the instance role's credentials is what proves the host
// is the instance it claims to be, and the control plane binds the registration
// to the account, role and instance id it verifies.
type AWSInstanceRole struct {
	// MetadataBase is overridable so the flow can be tested without IMDS, and so
	// a non-EC2 environment can be simulated. Production leaves it empty.
	MetadataBase string
	// Service and Region for the SigV4 signature. Region defaults to the value
	// IMDS reports, because a hardcoded region would sign for the wrong place on
	// an instance in another region.
	Service string
	Region  string
	// HTTPClient is injectable for tests.
	HTTPClient *http.Client
}

const defaultMetadataBase = awscreds.DefaultBase

func (p *AWSInstanceRole) Provider() string { return "aws-instance-role" }

func (p *AWSInstanceRole) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return &http.Client{Timeout: 5 * time.Second}
}

func (p *AWSInstanceRole) base() string {
	if p.MetadataBase != "" {
		return p.MetadataBase
	}
	return defaultMetadataBase
}

// imdsGet reads one metadata path.
//
// The IMDSv2 flow itself lives in `internal/awscreds`, which the host broker
// also uses; this is the agent's injection seam and its error translation.
func (p *AWSInstanceRole) imdsGet(ctx context.Context, path string) (string, error) {
	value, err := p.metadata().Get(ctx, path)
	if err != nil {
		return "", mapIMDSError(err)
	}
	return value, nil
}

// metadata is the shared IMDS reader, configured from this type's seams.
func (p *AWSInstanceRole) metadata() *awscreds.IMDS {
	return &awscreds.IMDS{Base: p.base(), HTTP: p.HTTPClient}
}

// mapIMDSError translates the metadata package's sentinels into this package's
// bootstrap ones, which the retry loop already understands.
//
// The distinction is the one that matters operationally: a refusal is a
// configuration an operator must change and retrying cannot fix, while an
// unavailable service is worth another attempt. A path that is not attached to
// the instance (404) is a refusal -- there is no role to wait for.
func mapIMDSError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, awscreds.ErrRefused):
		return fmt.Errorf("%w: %v", ErrBootstrapRefused, err)
	case errors.Is(err, awscreds.ErrUnavailable):
		return fmt.Errorf("%w: %v", ErrBootstrapUnavailable, err)
	default:
		return err
	}
}

// describeInstanceIdentity returns the instance's own view of who it is. The
// control plane does not trust these values; it verifies the signature and then
// compares them against what it expects. They are claims, not proof.
func (p *AWSInstanceRole) describeInstanceIdentity(ctx context.Context) (map[string]string, error) {
	instanceID, err := p.imdsGet(ctx, "/latest/meta-data/instance-id")
	if err != nil {
		return nil, err
	}
	doc, err := p.imdsGet(ctx, "/latest/dynamic/instance-identity/document")
	if err != nil {
		return nil, err
	}
	var parsed struct {
		AccountID  string `json:"accountId"`
		Region     string `json:"region"`
		InstanceID string `json:"instanceId"`
		InstanceTy string `json:"instanceType"`
	}
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		return nil, fmt.Errorf("%w: instance identity document: %v", ErrBootstrapUnavailable, err)
	}
	if parsed.AccountID == "" || parsed.Region == "" {
		return nil, fmt.Errorf("%w: instance identity document is missing account or region", ErrBootstrapUnavailable)
	}
	claims := map[string]string{
		"instance_id": instanceID,
		"account_id":  parsed.AccountID,
		"region":      parsed.Region,
	}
	if parsed.InstanceTy != "" {
		claims["instance_type"] = parsed.InstanceTy
	}
	// The role name identifies which agent role the host is running under, which
	// is the claim the control plane binds the registration to.
	if role, err := p.imdsGet(ctx, "/latest/meta-data/iam/security-credentials/"); err == nil {
		if name := firstLine(role); name != "" {
			claims["role"] = name
		}
	}
	return claims, nil
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			return s[:i]
		}
	}
	return s
}

// Identity fetches the host identity and the instance role's credentials.
//
// The credentials are returned to the caller to sign with and are never sent,
// stored or logged. No long-lived secret exists on disk, which is the property
// the instance-role bootstrap exists to provide.
func (p *AWSInstanceRole) Identity(ctx context.Context) (Identity, error) {
	claims, err := p.describeInstanceIdentity(ctx)
	if err != nil {
		return Identity{}, err
	}
	creds, err := p.imdsGet(ctx, "/latest/meta-data/iam/security-credentials/"+claims["role"])
	if err != nil {
		// `imdsGet` has already classified this as a refusal or an outage.
		// Re-wrapping it as "unavailable" would discard that: a host with no
		// role attached would be retried forever instead of reported as the
		// configuration fault it is.
		return Identity{}, fmt.Errorf("instance role credentials: %w", err)
	}
	var parsed struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		Token           string `json:"Token"`
	}
	if err := json.Unmarshal([]byte(creds), &parsed); err != nil {
		return Identity{}, fmt.Errorf("%w: role credential document: %v", ErrBootstrapUnavailable, err)
	}
	if parsed.AccessKeyID == "" || parsed.SecretAccessKey == "" {
		return Identity{}, fmt.Errorf("%w: instance role returned no usable credentials", ErrBootstrapUnavailable)
	}
	region := p.Region
	if region == "" {
		region = claims["region"]
	}
	service := p.Service
	if service == "" {
		service = "otter-agent"
	}
	return Identity{
		Provider:        p.Provider(),
		Claims:          claims,
		AccessKeyID:     parsed.AccessKeyID,
		SecretAccessKey: parsed.SecretAccessKey,
		SessionToken:    parsed.Token,
		Region:          region,
		Service:         service,
	}, nil
}

// MetadataBaseFromEnv lets an operator point the agent at a proxy for testing,
// and is deliberately the only way that value changes.
func MetadataBaseFromEnv() string { return os.Getenv("OTTER_IMDS_BASE") }
