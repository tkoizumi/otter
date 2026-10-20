package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeMetadata is a stand-in instance metadata service.
//
// This path had no test coverage before the IMDS flow moved into
// `internal/awscreds`: the agent's bootstrap is the one mechanism that has to
// work on a host nobody is watching, and a silent change here would present as
// "host will not register" with no clue why.
type fakeMetadata struct {
	tokenStatus int
	// responses maps a metadata path to its body. A path with no entry is 404,
	// which is what an instance with no role attached looks like.
	responses map[string]string
	statuses  map[string]int
}

func (f *fakeMetadata) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/latest/api/token" {
			if f.tokenStatus != 0 {
				w.WriteHeader(f.tokenStatus)
				return
			}
			_, _ = w.Write([]byte("imds-token"))
			return
		}
		if r.Header.Get("X-aws-ec2-metadata-token") != "imds-token" {
			http.Error(w, "token required", http.StatusUnauthorized)
			return
		}
		if status, ok := f.statuses[r.URL.Path]; ok {
			w.WriteHeader(status)
			return
		}
		body, ok := f.responses[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}
}

func newRole(t *testing.T, fake *fakeMetadata) *AWSInstanceRole {
	t.Helper()
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	return &AWSInstanceRole{MetadataBase: server.URL}
}

func healthyMetadata() *fakeMetadata {
	return &fakeMetadata{
		responses: map[string]string{
			"/latest/meta-data/instance-id":                          "i-0abcdef",
			"/latest/dynamic/instance-identity/document":             `{"accountId":"426714791664","region":"eu-west-1","instanceId":"i-0abcdef","instanceType":"m7g.large"}`,
			"/latest/meta-data/iam/security-credentials/":            "otter-agent",
			"/latest/meta-data/iam/security-credentials/otter-agent": `{"AccessKeyId":"AKID","SecretAccessKey":"SECRET","Token":"SESSION","Expiration":"2026-10-15T18:00:00Z"}`,
		},
		statuses: map[string]int{},
	}
}

func TestInstanceRoleIdentityCarriesClaimsAndCredentials(t *testing.T) {
	role := newRole(t, healthyMetadata())

	identity, err := role.Identity(context.Background())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if identity.Provider != "aws-instance-role" {
		t.Errorf("provider = %q", identity.Provider)
	}
	for key, want := range map[string]string{
		"instance_id": "i-0abcdef",
		"account_id":  "426714791664",
		"region":      "eu-west-1",
		"role":        "otter-agent",
	} {
		if got := identity.Claims[key]; got != want {
			t.Errorf("claim %s = %q, want %q", key, got, want)
		}
	}
	// The region defaults to what the instance reports, because a hardcoded one
	// would sign for the wrong place on an instance in another region.
	if identity.Region != "eu-west-1" {
		t.Errorf("region = %q, want eu-west-1", identity.Region)
	}
	if identity.AccessKeyID != "AKID" || identity.SecretAccessKey != "SECRET" || identity.SessionToken != "SESSION" {
		t.Errorf("credentials = %+v", identity)
	}
}

// An explicit region still wins, which is what a test or a non-EC2 environment
// needs.
func TestInstanceRoleRegionOverrideWins(t *testing.T) {
	role := newRole(t, healthyMetadata())
	role.Region = "us-east-2"

	identity, err := role.Identity(context.Background())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if identity.Region != "us-east-2" {
		t.Errorf("region = %q, want us-east-2", identity.Region)
	}
}

// IMDSv2 disabled is a refusal the operator has to fix, not something to retry.
func TestInstanceRoleRefusesWhenIMDSv2IsDisabled(t *testing.T) {
	fake := healthyMetadata()
	fake.tokenStatus = http.StatusUnauthorized
	role := newRole(t, fake)

	_, err := role.Identity(context.Background())
	if !errors.Is(err, ErrBootstrapRefused) {
		t.Errorf("error = %v, want ErrBootstrapRefused", err)
	}
	if errors.Is(err, ErrBootstrapUnavailable) {
		t.Errorf("a disabled IMDSv2 must not also read as unavailable: %v", err)
	}
}

// No role attached is a refusal: there is no credential to wait for.
func TestInstanceRoleRefusesWithNoRoleAttached(t *testing.T) {
	fake := healthyMetadata()
	delete(fake.responses, "/latest/meta-data/iam/security-credentials/")
	role := newRole(t, fake)

	_, err := role.Identity(context.Background())
	if !errors.Is(err, ErrBootstrapRefused) {
		t.Errorf("error = %v, want ErrBootstrapRefused", err)
	}
}

// A metadata service that answers badly is worth retrying.
func TestInstanceRoleReportsAMetadataFaultAsUnavailable(t *testing.T) {
	fake := healthyMetadata()
	fake.statuses["/latest/meta-data/instance-id"] = http.StatusInternalServerError
	role := newRole(t, fake)

	_, err := role.Identity(context.Background())
	if !errors.Is(err, ErrBootstrapUnavailable) {
		t.Errorf("error = %v, want ErrBootstrapUnavailable", err)
	}
}
