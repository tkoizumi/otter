package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/config"
)

// capture runs one Send and returns the decoded body the endpoint received.
func capture(t *testing.T, format string, payload Payload) map[string]any {
	t.Helper()

	var raw []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		if _, err := r.Body.Read(buf); err != nil && err.Error() != "EOF" {
			t.Errorf("read body: %v", err)
		}
		raw = buf
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := New(config.NotifyConfig{URL: server.URL, Format: format}, nil)
	if err := n.Send(context.Background(), payload); err != nil {
		t.Fatalf("Send(%s): %v", format, err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("%s: body is not a JSON object: %v\n%s", format, err, raw)
	}
	return decoded
}

func samplePayload() Payload {
	exit := 1
	return Payload{
		Integration: "shopify-to-salesforce",
		RunID:       "42e84cd5-bd93-4def-ade1",
		Status:      "failed",
		Attempt:     3,
		Error:       "process exited with code 1",
		Detail:      `sync finished {"failed":12,"written":88}`,
		DurationMS:  1840,
		ExitCode:    &exit,
		Release:     "3c850cfa6c9cb5c6",
	}
}

// Slack rejects a body without "text" with 400 invalid_payload, so the key is
// the whole contract.
func TestSlackFormatHasText(t *testing.T) {
	body := capture(t, config.FormatSlack, samplePayload())

	text, ok := body["text"].(string)
	if !ok || text == "" {
		t.Fatalf("slack body has no usable text: %+v", body)
	}
	// The useful information must actually be in the message.
	for _, want := range []string{"shopify-to-salesforce", "failed", "failed\":12", "attempt 3"} {
		if !strings.Contains(text, want) {
			t.Errorf("slack text does not mention %q:\n%s", want, text)
		}
	}
}

func TestDiscordFormatHasContent(t *testing.T) {
	body := capture(t, config.FormatDiscord, samplePayload())

	content, ok := body["content"].(string)
	if !ok || content == "" {
		t.Fatalf("discord body has no usable content: %+v", body)
	}
	if !strings.Contains(content, "shopify-to-salesforce") {
		t.Errorf("discord content is not informative:\n%s", content)
	}
}

// A MessageCard needs its type and context, or Teams ignores the request.
func TestTeamsFormatIsAMessageCard(t *testing.T) {
	body := capture(t, config.FormatTeams, samplePayload())

	if body["@type"] != "MessageCard" {
		t.Errorf("@type = %v, want MessageCard", body["@type"])
	}
	if body["@context"] != "https://schema.org/extensions" {
		t.Errorf("@context = %v", body["@context"])
	}
	if summary, _ := body["summary"].(string); summary == "" {
		t.Error("a MessageCard needs a summary")
	}
	// The integration's own last line should reach the reader.
	if text, _ := body["text"].(string); !strings.Contains(text, "failed\":12") {
		t.Errorf("teams text lost the integration's summary: %q", text)
	}
}

// The default must stay the full, machine-readable payload: it is what a
// custom endpoint, a healthcheck service, or a bridge depends on.
func TestJSONFormatIsTheDefaultAndUnchanged(t *testing.T) {
	body := capture(t, "", samplePayload())

	for _, key := range []string{
		"integration", "run_id", "status", "attempt",
		"error", "detail", "duration_ms", "exit_code", "release",
	} {
		if _, ok := body[key]; !ok {
			t.Errorf("default format dropped %q: %+v", key, body)
		}
	}
	// The chat shapes must not leak into it.
	if _, ok := body["text"]; ok {
		t.Error("the default format gained a chat field")
	}
}

// An unknown format is a configuration error, not a silent fallback.
func TestUnknownFormatFails(t *testing.T) {
	n := New(config.NotifyConfig{URL: "http://127.0.0.1:1/hook", Format: "carrier-pigeon"}, nil,
		WithRetryPolicy(1, 0))
	if _, err := n.body(samplePayload()); err == nil {
		t.Error("an unknown format produced a body")
	}
}

// A timeout should not read like a failure, and a cancel should not read like
// either: the operator action and the crash are different events.
func TestChatMessageDistinguishesStatuses(t *testing.T) {
	failed := chatMessage(Payload{Integration: "i", Status: "failed"})
	timedOut := chatMessage(Payload{Integration: "i", Status: "timed_out"})
	cancelled := chatMessage(Payload{Integration: "i", Status: "cancelled"})

	if failed == timedOut || timedOut == cancelled {
		t.Error("statuses render identically, so the message does not distinguish them")
	}
}

// A single-attempt run should not claim an attempt number it does not have.
func TestChatMessageOmitsTheAttemptWhenThereIsOnlyOne(t *testing.T) {
	one := chatMessage(Payload{Integration: "i", Status: "failed", Attempt: 1})
	three := chatMessage(Payload{Integration: "i", Status: "failed", Attempt: 3})

	if strings.Contains(one, "attempt") {
		t.Errorf("a first attempt mentioned an attempt number: %q", one)
	}
	if !strings.Contains(three, "attempt 3") {
		t.Errorf("a third attempt did not mention it: %q", three)
	}
}
