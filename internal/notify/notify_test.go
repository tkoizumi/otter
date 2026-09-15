package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/otter-runtime/otter/internal/config"
)

func TestSendPostsThePayload(t *testing.T) {
	var got Payload
	var contentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := New(config.NotifyConfig{URL: server.URL}, nil)
	want := Payload{
		Integration: "shopify-to-salesforce",
		RunID:       "run-1",
		Status:      "failed",
		Attempt:     2,
		Error:       "sync finished with failures",
		Detail:      `sync finished {"failed":100,"written":0}`,
		DurationMS:  1840,
		Release:     "3c850cfa6c9c",
	}
	if err := n.Send(context.Background(), want); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got.RunID != want.RunID || got.Detail != want.Detail || got.Attempt != want.Attempt {
		t.Errorf("payload did not survive the round trip: %+v", got)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
}

// A transient endpoint failure is retried; the delay is compressed so the test
// does not sleep for the production backoff.
func TestSendRetriesThenSucceeds(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 3 {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := New(config.NotifyConfig{URL: server.URL}, nil, WithRetryPolicy(3, time.Millisecond))

	if err := n.Send(context.Background(), Payload{RunID: "r"}); err != nil {
		t.Fatalf("Send should have succeeded on the third attempt: %v", err)
	}
	if attempts != 3 {
		t.Errorf("made %d attempts, want 3", attempts)
	}
}

// A permanently broken endpoint must give up, not hang the worker.
func TestSendGivesUpAfterBoundedAttempts(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		http.Error(w, "always broken", http.StatusBadGateway)
	}))
	defer server.Close()

	n := New(config.NotifyConfig{URL: server.URL}, nil, WithRetryPolicy(defaultAttempts, time.Millisecond))

	if err := n.Send(context.Background(), Payload{RunID: "r"}); err == nil {
		t.Fatal("Send reported success against a broken endpoint")
	}
	if attempts != defaultAttempts {
		t.Errorf("made %d attempts, want %d", attempts, defaultAttempts)
	}
}

// The whole feature must be inert when it is not configured.
func TestDisabledNotifierDoesNothing(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer server.Close()

	n := New(config.NotifyConfig{}, nil)
	if n.Enabled() {
		t.Error("an empty configuration reported itself as enabled")
	}
	if n.Wants("failed") {
		t.Error("a disabled notifier wanted a failure")
	}
	if err := n.Send(context.Background(), Payload{RunID: "r"}); err != nil {
		t.Errorf("Send on a disabled notifier: %v", err)
	}
	if called {
		t.Error("a disabled notifier made a request")
	}

	// A nil notifier is what the daemon holds when nothing is configured.
	var none *Notifier
	if none.Enabled() || none.Wants("failed") {
		t.Error("a nil notifier reported itself as enabled")
	}
}

// Only failures are reported, and only the statuses asked for.
func TestWantsFiltersByStatus(t *testing.T) {
	all := New(config.NotifyConfig{URL: "http://example.test/hook"}, nil)

	for _, status := range []string{"failed", "timed_out", "cancelled"} {
		if !all.Wants(status) {
			t.Errorf("default configuration did not want %q", status)
		}
	}
	// Success is never an alert: it would double the volume and train the
	// reader to ignore it.
	if all.Wants("succeeded") {
		t.Error("success was reported as a failure")
	}

	only := New(config.NotifyConfig{URL: "http://example.test/hook", On: []string{"failed"}}, nil)
	if !only.Wants("failed") {
		t.Error("allow-list dropped the listed status")
	}
	if only.Wants("timed_out") {
		t.Error("allow-list admitted an unlisted status")
	}
}

// The URL can carry a secret (Slack, healthchecks), so anything reporting the
// configuration must redact it.
func TestRedactedURL(t *testing.T) {
	n := New(config.NotifyConfig{URL: "https://hooks.slack.com/services/T000/B000/XXXXSECRET"}, nil)
	got := n.RedactedURL()
	if got != "https://hooks.slack.com/..." {
		t.Errorf("RedactedURL = %q", got)
	}
	if n.cfg.URL == got {
		t.Error("the secret survived redaction")
	}
}
