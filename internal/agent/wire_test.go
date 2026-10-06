package agent

// Conformance tests against the shared protocol fixtures.
//
// These exist because the agent and the control plane are written in different
// languages and each side's tests use fakes for the other. A field renamed on one
// side passes its own suite and breaks the protocol in production -- which is
// what happened while writing this: the Go Reported type had no runtime_id, and
// the control plane refuses a report without one, so every report would have
// failed while every local test passed.
//
// The fixtures live in the platform repository beside the protocol document, so
// both languages assert against one file neither owns.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type wireFixtures struct {
	Version        int                        `json:"version"`
	ContentType    string                     `json:"content_type"`
	Messages       map[string]json.RawMessage `json:"messages"`
	Outcomes       []string                   `json:"outcomes"`
	RequiredFields map[string][]string        `json:"required_fields"`
}

func loadFixtures(t *testing.T) wireFixtures {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "..",
		"otter-platform", "hosting", "fixtures", "protocol", "wire.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("protocol fixtures are not checked out at %s", path)
	}
	var f wireFixtures
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatalf("fixtures are not valid JSON: %v", err)
	}
	if f.Version != 1 {
		t.Fatalf("fixture version = %d, want 1", f.Version)
	}
	return f
}

// missingRequired reports which of a message's required fields are absent.
func missingRequired(t *testing.T, raw json.RawMessage, required []string) []string {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("fixture is not a JSON object: %v", err)
	}
	var missing []string
	for _, field := range required {
		if _, ok := obj[field]; !ok {
			missing = append(missing, field)
		}
	}
	return missing
}

// The fixtures must themselves be complete, or every assertion below is weaker
// than it looks.
func TestProtocolFixturesAreComplete(t *testing.T) {
	f := loadFixtures(t)
	for name, required := range f.RequiredFields {
		raw, ok := f.Messages[name]
		if !ok {
			t.Errorf("required_fields names %q but there is no such message", name)
			continue
		}
		if missing := missingRequired(t, raw, required); len(missing) > 0 {
			t.Errorf("fixture %s is missing required field(s) %v", name, missing)
		}
	}
	// Every message should have a required-field entry, or a new message could be
	// added without anyone deciding what is mandatory.
	for name := range f.Messages {
		if _, ok := f.RequiredFields[name]; !ok {
			t.Errorf("message %q has no required_fields entry", name)
		}
	}
}

// The agent must be able to READ what the control plane SENDS. If it cannot, the
// agent silently sees zero values rather than failing, which is worse.
func TestAgentReadsTheControlPlanesMessages(t *testing.T) {
	f := loadFixtures(t)

	t.Run("bootstrap response", func(t *testing.T) {
		var cred Credential
		if err := json.Unmarshal(f.Messages["bootstrap_response"], &cred); err != nil {
			t.Fatalf("the agent cannot read a bootstrap response: %v", err)
		}
		if cred.Token == "" || cred.RuntimeID == "" {
			t.Errorf("agent decoded an empty credential: %+v", cred)
		}
		if cred.Generation != 41 {
			t.Errorf("generation = %d, want 41", cred.Generation)
		}
		if cred.ExpiresAt.IsZero() {
			t.Error("expires_at did not decode; the agent would treat the credential as valid forever")
		}
	})

	t.Run("desired response", func(t *testing.T) {
		var want Desired
		if err := json.Unmarshal(f.Messages["desired_response"], &want); err != nil {
			t.Fatalf("the agent cannot read a desired response: %v", err)
		}
		if want.Generation != 42 {
			t.Errorf("generation = %d, want 42", want.Generation)
		}
		if want.Release.Digest == "" || want.Release.URL == "" {
			t.Errorf("agent decoded an unusable release: %+v", want.Release)
		}
		if want.Maintenance == nil || want.Maintenance.Desired != "maintenance" {
			t.Errorf("agent did not decode the maintenance request: %+v", want.Maintenance)
		}
	})

	t.Run("lease response", func(t *testing.T) {
		var lease struct {
			LeaseSeconds int    `json:"lease_seconds"`
			RenewedAt    string `json:"renewed_at"`
		}
		if err := json.Unmarshal(f.Messages["lease_response"], &lease); err != nil {
			t.Fatalf("the agent cannot read a lease response: %v", err)
		}
		if lease.LeaseSeconds != 60 {
			t.Errorf("lease_seconds = %d, want 60", lease.LeaseSeconds)
		}
	})
}

// The control plane must be able to READ what the agent SENDS, and the agent must
// include every required field. This is the direction that was broken.
func TestAgentWritesTheFieldsTheControlPlaneRequires(t *testing.T) {
	f := loadFixtures(t)

	t.Run("report", func(t *testing.T) {
		// Marshal the agent's actual type, then check the control plane's
		// required fields are present. Marshalling a literal here would test the
		// test rather than the type.
		body, err := json.Marshal(Reported{
			RuntimeID:  "rt-pilot-1",
			Generation: 42,
			Outcome:    OutcomeApplied,
			Observed:   map[string]string{"release_digest": "sha256:bbbb"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if missing := missingRequired(t, body, f.RequiredFields["report_request"]); len(missing) > 0 {
			t.Errorf("the agent's report omits %v, which the control plane requires", missing)
		}
	})

	t.Run("desired request", func(t *testing.T) {
		// A DISTINCT type from the response: the request carries the observed
		// state the control plane reconciles against, and sending the response
		// shape instead would omit it.
		body, err := json.Marshal(DesiredRequest{
			RuntimeID:  "rt-pilot-1",
			Generation: 41,
			Observed:   Observed{ReleaseDigest: "sha256:aaaa", Health: "ok", Maintenance: "serving"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if missing := missingRequired(t, body, f.RequiredFields["desired_request"]); len(missing) > 0 {
			t.Errorf("the agent's desired request omits %v", missing)
		}
	})
}

// The outcome set is closed on both sides. A mismatch here means the agent can
// emit a value the control plane rejects, or the control plane rejects one the
// agent treats as success.
func TestOutcomeConstantsMatchTheFixture(t *testing.T) {
	f := loadFixtures(t)
	got := map[string]bool{
		OutcomeApplied:       true,
		OutcomeFailed:        true,
		OutcomeRefusedFenced: true,
		OutcomeInProgress:    true,
	}
	if len(got) != len(f.Outcomes) {
		t.Fatalf("agent has %d outcomes, fixtures declare %d", len(got), len(f.Outcomes))
	}
	for _, o := range f.Outcomes {
		if !got[o] {
			t.Errorf("fixture outcome %q has no agent constant", o)
		}
	}
}

// Timestamps must be RFC3339 on the wire. A Unix number would decode into a
// zero time on one side and be treated as valid forever.
func TestTimestampsAreRFC3339(t *testing.T) {
	f := loadFixtures(t)
	for _, msg := range []string{"bootstrap_response"} {
		var obj map[string]any
		if err := json.Unmarshal(f.Messages[msg], &obj); err != nil {
			t.Fatal(err)
		}
		for k, v := range obj {
			s, ok := v.(string)
			if !ok {
				continue
			}
			if len(s) < 10 || s[4] != '-' {
				continue // not a timestamp-shaped value
			}
			if _, err := time.Parse(time.RFC3339, s); err != nil {
				t.Errorf("%s.%s = %q is not RFC3339: %v", msg, k, s, err)
			}
		}
	}
}
