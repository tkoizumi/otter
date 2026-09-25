package inspection

import (
	"encoding/json"
	"strings"
	"testing"
)

func capturedBody(t *testing.T, raw string) *BodyDescriptor {
	t.Helper()
	if !json.Valid([]byte(raw)) {
		t.Fatalf("test body is not valid JSON: %s", raw)
	}
	return &BodyDescriptor{State: BodyCaptured, JSON: json.RawMessage(raw)}
}

// TestExtractErrorSummaryRealSalesforceResponse is the shape that prompted this:
// Salesforce answers a rejected write with a top-level array whose element
// carries both a machine-readable code and the prose reason.
func TestExtractErrorSummaryRealSalesforceResponse(t *testing.T) {
	body := capturedBody(t, `[
	  {
	    "message": "There's a problem with this country, even though it may appear correct. Please select a country/territory from the list of valid countries.: Mailing Country",
	    "errorCode": "FIELD_INTEGRITY_EXCEPTION",
	    "fields": ["MailingCountry"]
	  }
	]`)

	got := ExtractErrorSummary(body, nil)
	if got.Code != "FIELD_INTEGRITY_EXCEPTION" {
		t.Errorf("code = %q, want FIELD_INTEGRITY_EXCEPTION", got.Code)
	}
	if !strings.Contains(got.Message, "problem with this country") {
		t.Errorf("message = %q, want the server's reason", got.Message)
	}
	// The code is what a reader scans for, so Text leads with it.
	if !strings.HasPrefix(got.Text(), "FIELD_INTEGRITY_EXCEPTION: ") {
		t.Errorf("Text() = %q, want it to lead with the code", got.Text())
	}
}

func TestExtractErrorSummaryShapes(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		code    string
		message string
	}{
		{
			name:    "object with error and errorCode",
			body:    `{"error": "invalid cursor", "errorCode": "INVALID_CURSOR"}`,
			code:    "INVALID_CURSOR",
			message: "invalid cursor",
		},
		{
			name:    "oauth style error_description",
			body:    `{"error": "invalid_grant", "error_description": "expired access token"}`,
			code:    "invalid_grant",
			message: "expired access token",
		},
		{
			name:    "nested error object",
			body:    `{"error": {"code": "NOT_FOUND", "message": "no such record"}}`,
			code:    "NOT_FOUND",
			message: "no such record",
		},
		{
			name:    "list of field errors",
			body:    `{"errors": [{"code": "REQUIRED", "message": "Email is required"}]}`,
			code:    "REQUIRED",
			message: "Email is required",
		},
		{
			name:    "bare string body",
			body:    `"upstream is unavailable"`,
			message: "upstream is unavailable",
		},
		{
			name:    "array of strings",
			body:    `["first problem", "second problem"]`,
			message: "first problem",
		},
		{
			name: "unrecognised shape yields nothing",
			body: `{"unexpected": {"nested": [1, 2, 3]}}`,
		},
		{
			name: "empty array yields nothing",
			body: `[]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractErrorSummary(capturedBody(t, tc.body), nil)
			if got.Code != tc.code {
				t.Errorf("code = %q, want %q", got.Code, tc.code)
			}
			if got.Message != tc.message {
				t.Errorf("message = %q, want %q", got.Message, tc.message)
			}
		})
	}
}

// TestExtractErrorSummaryIgnoresNonBodies covers the states that must not
// produce a summary: an omitted or empty body has no reason to read, and a body
// that was never captured must not be invented.
func TestExtractErrorSummaryIgnoresNonBodies(t *testing.T) {
	cases := map[string]*BodyDescriptor{
		"nil":      nil,
		"empty":    {State: BodyEmpty},
		"omitted":  {State: BodyOmitted, Reason: ReasonOversized},
		"no json":  {State: BodyCaptured},
		"bad json": {State: BodyCaptured, JSON: json.RawMessage(`{not json`)},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ExtractErrorSummary(body, nil); !got.Empty() {
				t.Errorf("summary = %+v, want nothing", got)
			}
		})
	}
}

// TestExtractErrorSummaryBoundsAndFlattens keeps a hostile or enormous server
// message from escaping the cell it is rendered into.
func TestExtractErrorSummaryBoundsAndFlattens(t *testing.T) {
	long := strings.Repeat("x", 5000)
	body := capturedBody(t, `{"message": "line one\nline two\tand more", "code": "`+long+`"}`)

	got := ExtractErrorSummary(body, nil)
	if strings.ContainsAny(got.Message, "\n\t") {
		t.Errorf("message carries a control character: %q", got.Message)
	}
	if got.Message != "line one line two and more" {
		t.Errorf("message = %q, want it flattened to one line", got.Message)
	}
	// The limits are columns, so the ellipsis a truncation adds has to be inside
	// them rather than pushing the value over.
	if runes := len([]rune(got.Code)); runes > MaxErrorCodeLength {
		t.Errorf("code is %d runes, want at most %d", runes, MaxErrorCodeLength)
	}
	if runes := len([]rune(got.Message)); runes > MaxErrorMessageLength {
		t.Errorf("message is %d runes, want at most %d", runes, MaxErrorMessageLength)
	}
	if got.Code == "" {
		t.Errorf("a long code should be truncated, not dropped")
	}
	if !strings.HasSuffix(got.Code, "…") {
		t.Errorf("a truncated code should say so: %q", got.Code)
	}
}

// TestExtractErrorSummaryRunsAfterRedaction pins the ordering that makes this
// safe: the summary is taken from the sanitized body, so a credential in the
// error text cannot survive into a view that never showed payloads.
func TestExtractErrorSummaryRunsAfterRedaction(t *testing.T) {
	raw := `[{"message": "token access_token=super-secret-value rejected", "errorCode": "INVALID_TOKEN"}]`

	event := RequestEvent{
		Kind:         EventCompleted,
		RequestID:    "req-1",
		ResponseBody: &BodyDescriptor{State: BodyCaptured, JSON: json.RawMessage(raw)},
	}
	sanitized, _ := SanitizeEvent(event, DefaultRedactor(), DefaultLimits())

	got := ExtractErrorSummary(sanitized.ResponseBody, DefaultRedactor())
	if strings.Contains(got.Message, "super-secret-value") {
		t.Fatalf("a secret survived into the summary: %q", got.Message)
	}
	if !strings.Contains(got.Message, RedactedPlaceholder) {
		t.Errorf("message = %q, want the redaction placeholder", got.Message)
	}
}
