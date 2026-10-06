package agent

// Emit a real signed bootstrap request for the CONTROL PLANE's verifier to check.
//
// The signature agreement between the Go signer and the TypeScript verifier is
// the trust boundary: if they disagree about which bytes are covered, every agent
// fails to register. It was verified once by hand, against a live control plane,
// and that is not a thing CI can repeat -- so this writes the agent's actual
// output to a file and the Cloud suite verifies it with the real verifier.
//
// It is a conformance vector, not a mock: the bytes come from signAssertion, the
// same function the running agent uses.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// crossLangVector is the fixture both suites read.
type crossLangVector struct {
	Description string `json:"description"`
	Method      string `json:"method"`
	// Path is what the signature covers; Host is the authority.
	Path string `json:"path"`
	Host string `json:"host"`
	// Body is the exact serialized request body, because THAT is what is signed.
	Body string `json:"body"`
	// AmzDate is the signing time, fixed so the vector is stable.
	AmzDate string `json:"amz_date"`
	// Headers are what the agent sends AND what the verifier must verify against.
	Headers map[string]string `json:"headers"`
	// Credentials let the verifier re-derive the signature. A test key: the real
	// provider's credentials never leave the host.
	Credentials struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SessionToken    string `json:"session_token,omitempty"`
	} `json:"credentials"`
	// Scope the verifier should report back, so a disagreement names itself.
	ExpectedScope struct {
		Date    string `json:"date"`
		Region  string `json:"region"`
		Service string `json:"service"`
	} `json:"expected_scope"`
}

func TestEmitCrossLanguageSignatureVector(t *testing.T) {
	body := `{"assertion":{"provider":"aws-instance-role","claims":{"account_id":"426714791664","instance_id":"i-0abc123","role":"otter-agent"}},"runtime_id":"rt-pilot-1","agent_version":"0.1.0"}`
	signedAt := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	headers, err := signAssertion(Identity{
		Provider:        "aws-instance-role",
		Claims:          map[string]string{"account_id": "426714791664", "role": "otter-agent", "instance_id": "i-0abc123"},
		AccessKeyID:     "AKIDCROSSLANG",
		SecretAccessKey: "crosslang-secret-not-a-real-key",
		Region:          "us-east-1",
		Service:         "otter-agent",
	}, "POST", "cloud.otter.invalid", "/agent/v1/bootstrap", body, signedAt)
	if err != nil {
		t.Fatalf("signing the vector: %v", err)
	}

	v := crossLangVector{
		Description: "Signed by the Go agent signer; verified by the Cloud verifier. A mismatch means every agent fails to register.",
		Method:      "POST",
		Path:        "/agent/v1/bootstrap",
		Host:        "cloud.otter.invalid",
		Body:        body,
		AmzDate:     signedAt.Format("20060102T150405Z"),
		Headers:     headers,
	}
	v.Credentials.AccessKeyID = "AKIDCROSSLANG"
	v.Credentials.SecretAccessKey = "crosslang-secret-not-a-real-key"
	v.ExpectedScope.Date = "20261006"
	v.ExpectedScope.Region = "us-east-1"
	v.ExpectedScope.Service = "otter-agent"

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	out := filepath.Join(filepath.Dir(thisFile), "..", "..", "..",
		"otter-platform", "hosting", "fixtures", "protocol", "signed-request.json")
	body2, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, append(body2, '\n'), 0o644); err != nil {
		t.Skipf("cross-language vector destination is not writable (%v); the Cloud suite reads it", err)
	}

	// Header names are lowercase, which is not cosmetic: a SigV4 signature covers
	// lowercase names, and http.Header canonicalises to "Authorization" while a
	// map literal may use either. Emitting the canonical spelling would make the
	// vector disagree with the signature it carries.
	for name := range headers {
		if name != strings.ToLower(name) {
			t.Errorf("header %q is not lowercase; a signature covers lowercase names", name)
		}
	}

	// The vector must actually carry a signature, or the far side verifies
	// nothing and both suites pass vacuously.
	if headers["authorization"] == "" {
		t.Fatal("the emitted vector has no Authorization header")
	}
	if headers["x-amz-date"] != v.AmzDate {
		t.Errorf("x-amz-date = %q, want %q", headers["x-amz-date"], v.AmzDate)
	}
	if headers["x-amz-content-sha256"] == "" {
		t.Error("the vector carries no body hash; the verifier cannot check the body")
	}
	// The host is signed, so it must be present or the verifier cannot re-derive.
	if headers["host"] != v.Host {
		t.Errorf("host header = %q, want %q", headers["host"], v.Host)
	}
}
