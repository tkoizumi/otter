package inspection

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validBatch(policy Policy, events ...RequestEvent) EventBatch {
	return EventBatch{SchemaVersion: SchemaVersion, Policy: policy, Events: events}
}

func TestValidateBatchAcceptsTheContract(t *testing.T) {
	limits := DefaultLimits()
	status := 400
	batch := validBatch(PolicyFull, RequestEvent{
		Kind:            EventCompleted,
		RequestID:       "req-1",
		ProducerSeq:     3,
		OccurredAt:      time.Now().UTC(),
		Method:          "POST",
		URL:             "https://api.example.com/v1/items",
		CallSite:        "source.py:42 in push",
		StatusCode:      &status,
		DurationTotalMS: int64PtrValue(12),
		RequestHeaders:  []HeaderPair{{Name: "Content-Type", Value: "application/json"}},
		RequestBody:     &BodyDescriptor{State: BodyCaptured, JSON: json.RawMessage(`{"a":1}`)},
		ResponseBody:    &BodyDescriptor{State: BodyOmitted, Reason: ReasonUnsupportedContent},
	})
	if err := ValidateBatch(batch, limits); err != nil {
		t.Fatalf("ValidateBatch rejected a valid batch: %v", err)
	}
}

func TestValidateBatchAcceptsASummaryOnlyBatch(t *testing.T) {
	limits := DefaultLimits()
	// A run that observed nothing still has to be able to say so.
	batch := EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata, Finalization: FinalizationComplete}
	if err := ValidateBatch(batch, limits); err != nil {
		t.Fatalf("a summary-only batch must be accepted: %v", err)
	}
	empty := EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata}
	if err := ValidateBatch(empty, limits); err == nil {
		t.Fatal("a batch with neither events nor a summary update must be rejected")
	}
}

func TestValidateBatchAcceptsAdapterReports(t *testing.T) {
	limits := DefaultLimits()
	// An adapter report is a summary update in its own right: the child may
	// have nothing to send yet but still knows what it instrumented.
	batch := EventBatch{
		SchemaVersion: SchemaVersion,
		Policy:        PolicyFull,
		Adapters:      []string{AdapterURLLib, AdapterHTTPX},
	}
	if err := ValidateBatch(batch, limits); err != nil {
		t.Fatalf("an adapter report must be accepted: %v", err)
	}
}

func TestValidateBatchRejectsUnknownAdapters(t *testing.T) {
	limits := DefaultLimits()

	unknown := validBatch(PolicyFull)
	unknown.Adapters = []string{"carrier-pigeon"}
	if err := ValidateBatch(unknown, limits); err == nil {
		t.Fatal("an unknown adapter must be rejected")
	}

	tooMany := validBatch(PolicyFull)
	tooMany.Adapters = []string{AdapterURLLib, AdapterRequests, AdapterHTTPX, AdapterURLLib}
	if err := ValidateBatch(tooMany, limits); err == nil {
		t.Fatal("reporting more adapters than exist must be rejected")
	}
}

func TestValidateBatchRejectsBadSubmissions(t *testing.T) {
	limits := DefaultLimits()
	cases := []struct {
		name  string
		batch EventBatch
		want  string
	}{
		{
			name:  "schema version",
			batch: EventBatch{SchemaVersion: 99, Policy: PolicyMetadata, Events: []RequestEvent{startedEvent("req-1", 1)}},
			want:  "schema version",
		},
		{
			name:  "unknown kind",
			batch: validBatch(PolicyMetadata, RequestEvent{Kind: "request.teleported", RequestID: "req-1"}),
			want:  "event kind",
		},
		{
			name:  "policy off",
			batch: validBatch(PolicyOff, startedEvent("req-1", 1)),
			want:  "does not accept events",
		},
		{
			name:  "empty request id",
			batch: validBatch(PolicyMetadata, RequestEvent{Kind: EventStarted, Method: "GET"}),
			want:  "request_id",
		},
		{
			name:  "request id characters",
			batch: validBatch(PolicyMetadata, RequestEvent{Kind: EventStarted, RequestID: "bad id!", Method: "GET"}),
			want:  "unsupported character",
		},
		{
			name:  "start needs a method",
			batch: validBatch(PolicyMetadata, RequestEvent{Kind: EventStarted, RequestID: "req-1"}),
			want:  "must name a method",
		},
		{
			name:  "status out of range",
			batch: validBatch(PolicyMetadata, RequestEvent{Kind: EventCompleted, RequestID: "req-1", StatusCode: intPtrValue(99)}),
			want:  "not a valid HTTP status",
		},
		{
			name:  "negative duration",
			batch: validBatch(PolicyMetadata, RequestEvent{Kind: EventCompleted, RequestID: "req-1", DurationTotalMS: int64PtrValue(-1)}),
			want:  "must not be negative",
		},
		{
			name: "header newline",
			batch: validBatch(PolicyMetadata, RequestEvent{Kind: EventStarted, RequestID: "req-1", Method: "GET",
				RequestHeaders: []HeaderPair{{Name: "X-Evil", Value: "a\r\nInjected: 1"}}}),
			want: "newline",
		},
		{
			name: "payload under a metadata policy",
			batch: validBatch(PolicyMetadata, RequestEvent{Kind: EventStarted, RequestID: "req-1", Method: "POST",
				RequestBody: &BodyDescriptor{State: BodyCaptured, JSON: json.RawMessage(`{"a":1}`)}}),
			want: "does not permit captured payloads",
		},
		{
			name: "omitted body without a reason",
			batch: validBatch(PolicyFull, RequestEvent{Kind: EventStarted, RequestID: "req-1", Method: "POST",
				RequestBody: &BodyDescriptor{State: BodyOmitted}}),
			want: "known reason",
		},
		{
			name: "omitted body carrying bytes",
			batch: validBatch(PolicyFull, RequestEvent{Kind: EventStarted, RequestID: "req-1", Method: "POST",
				RequestBody: &BodyDescriptor{State: BodyOmitted, Reason: ReasonIncomplete, JSON: json.RawMessage(`{"a":1}`)}}),
			want: "must not carry JSON",
		},
		{
			name: "captured body that is not JSON",
			batch: validBatch(PolicyFull, RequestEvent{Kind: EventStarted, RequestID: "req-1", Method: "POST",
				RequestBody: &BodyDescriptor{State: BodyCaptured, JSON: json.RawMessage(`not json`)}}),
			want: "not valid JSON",
		},
		{
			name:  "invalid finalization",
			batch: EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata, Finalization: "maybe"},
			want:  "invalid finalization",
		},
		{
			name:  "negative dropped counters",
			batch: EventBatch{SchemaVersion: SchemaVersion, Policy: PolicyMetadata, DroppedEvents: -1},
			want:  "must not be negative",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBatch(tc.batch, limits)
			if err == nil {
				t.Fatalf("ValidateBatch accepted an invalid batch")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateBatchRejectsOversizedInputs(t *testing.T) {
	limits := DefaultLimits()

	many := make([]RequestEvent, 0, limits.MaxBatchEvents+1)
	for i := 0; i <= limits.MaxBatchEvents; i++ {
		many = append(many, startedEvent("req-"+string(rune('a'+i%26)), int64(i+1)))
	}
	if err := ValidateBatch(validBatch(PolicyMetadata, many...), limits); err == nil {
		t.Error("an oversized batch must be rejected")
	}

	huge := startedEvent("req-1", 1)
	huge.URL = "https://api.example.com/" + strings.Repeat("x", limits.MaxURLLength)
	if err := ValidateBatch(validBatch(PolicyMetadata, huge), limits); err == nil {
		t.Error("an over-long URL must be rejected")
	}

	manyHeaders := startedEvent("req-1", 1)
	for i := 0; i <= limits.MaxHeaderPairs; i++ {
		manyHeaders.RequestHeaders = append(manyHeaders.RequestHeaders, HeaderPair{Name: "X-N", Value: "v"})
	}
	if err := ValidateBatch(validBatch(PolicyMetadata, manyHeaders), limits); err == nil {
		t.Error("too many headers must be rejected")
	}
}

func TestPolicyParsing(t *testing.T) {
	if got, err := ParsePolicy(""); err != nil || got != DefaultPolicy {
		t.Errorf("ParsePolicy(\"\") = %q, %v; want the default %q", got, err, DefaultPolicy)
	}
	for _, value := range []Policy{PolicyOff, PolicyMetadata, PolicyFull} {
		if !value.Valid() {
			t.Errorf("%q should be valid", value)
		}
	}
	if _, err := ParsePolicy("everything"); err == nil {
		t.Error("an unknown policy must be rejected")
	}
	if Policy("bogus").Enabled() {
		t.Error("an unknown policy must not be considered enabled")
	}
	if PolicyOff.Enabled() || !PolicyMetadata.Enabled() || !PolicyFull.Enabled() {
		t.Error("policy enabled() is wrong")
	}
	if PolicyMetadata.CapturesBodies() || !PolicyFull.CapturesBodies() {
		t.Error("only full may capture bodies")
	}
}

func TestPayloadCoverageIsHonest(t *testing.T) {
	captured := &BodyDescriptor{State: BodyCaptured, JSON: json.RawMessage(`{}`)}
	omitted := &BodyDescriptor{State: BodyOmitted, Reason: ReasonOversized}
	empty := &BodyDescriptor{State: BodyEmpty}

	cases := []struct {
		name              string
		policy            Policy
		request, response *BodyDescriptor
		want              string
	}{
		{"metadata policy never claims payloads", PolicyMetadata, captured, captured, "metadata"},
		{"both captured", PolicyFull, captured, captured, "full"},
		{"an omitted body makes it partial", PolicyFull, captured, omitted, "partial"},
		{"no body information at all", PolicyFull, nil, nil, "metadata"},
		{"an empty body is still information", PolicyFull, empty, empty, "full"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PayloadCoverage(tc.policy, tc.request, tc.response); got != tc.want {
				t.Errorf("PayloadCoverage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSanitizeEventDowngradesUnsafeBodies(t *testing.T) {
	limits := DefaultLimits()
	r := DefaultRedactor()

	trailing := RequestEvent{
		Kind: EventStarted, RequestID: "req-1", Method: "POST",
		RequestBody: &BodyDescriptor{State: BodyCaptured, JSON: json.RawMessage(`{"a":1} trailing`)},
	}
	got, _ := SanitizeEvent(trailing, r, limits)
	if got.RequestBody.State != BodyOmitted || got.RequestBody.Reason != ReasonUnparseable {
		t.Errorf("unparseable body = %+v, want omitted/unparseable", got.RequestBody)
	}
	if len(got.RequestBody.JSON) != 0 {
		t.Error("an unparseable body must not keep its bytes")
	}

	oversized := RequestEvent{
		Kind: EventStarted, RequestID: "req-1", Method: "POST",
		RequestBody: &BodyDescriptor{
			State: BodyCaptured,
			JSON:  json.RawMessage(`{"a":"` + strings.Repeat("x", int(limits.MaxBodyBytes)) + `"}`),
		},
	}
	got, _ = SanitizeEvent(oversized, r, limits)
	if got.RequestBody.State != BodyOmitted || got.RequestBody.Reason != ReasonOversized {
		t.Errorf("oversized body = %+v, want omitted/oversized", got.RequestBody)
	}

	// A captured body that is fine keeps its value and reports its redactions.
	ok := RequestEvent{
		Kind: EventStarted, RequestID: "req-1", Method: "POST",
		RequestBody: &BodyDescriptor{State: BodyCaptured, JSON: json.RawMessage(`{"secret":"s","keep":1}`)},
	}
	got, n := SanitizeEvent(ok, r, limits)
	if n != 1 || !got.RequestBody.Redacted || got.RequestBody.RedactedCount != 1 {
		t.Errorf("redaction accounting = %d/%+v, want 1 redaction recorded", n, got.RequestBody)
	}
	if string(got.RequestBody.JSON) != `{"secret":"REDACTED","keep":1}` {
		t.Errorf("body = %s, want the secret redacted", got.RequestBody.JSON)
	}
}
