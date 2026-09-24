package inspection

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Event kinds an SDK may submit. The list is an allowlist: anything else is
// rejected, so a run token cannot use the ingestion endpoint as a general write
// channel.
const (
	// EventStarted is recorded before the original transport call is made, so a
	// request killed mid-flight stays visible as in progress.
	EventStarted = "request.started"
	// EventResponse reports that response headers arrived.
	EventResponse = "request.response"
	// EventCompleted is terminal and carries either a status code or a
	// sanitized transport error.
	EventCompleted = "request.completed"
)

// AllEventKinds lists the allowlisted kinds.
func AllEventKinds() []string { return []string{EventStarted, EventResponse, EventCompleted} }

// ValidEventKind reports whether kind may be ingested.
func ValidEventKind(kind string) bool {
	switch kind {
	case EventStarted, EventResponse, EventCompleted:
		return true
	default:
		return false
	}
}

// Phase is the lifecycle state of a stored exchange.
type Phase string

const (
	// PhaseInProgress means a start was recorded and no terminal event arrived.
	PhaseInProgress Phase = "in_progress"
	// PhaseCompleted means the transport returned a response.
	PhaseCompleted Phase = "completed"
	// PhaseFailed means the transport failed, so there is no response.
	PhaseFailed Phase = "failed"
)

// Finalization is how complete a run's recording is known to be.
type Finalization string

const (
	// FinalizationPending means the run is still executing.
	FinalizationPending Finalization = "pending"
	// FinalizationComplete means the child reported a clean finish.
	FinalizationComplete Finalization = "complete"
	// FinalizationIncomplete means the child finished but some capture was lost,
	// for example because a queue overflowed or delivery failed.
	FinalizationIncomplete Finalization = "incomplete"
	// FinalizationAbrupt means the process died without a chance to report:
	// SIGKILL, a crash, or a timeout kill. Loss is possible, and its exact size
	// is not knowable.
	FinalizationAbrupt Finalization = "abrupt"
)

// ValidFinalization reports whether f is a known finalization state.
func ValidFinalization(f Finalization) bool {
	switch f {
	case FinalizationPending, FinalizationComplete, FinalizationIncomplete, FinalizationAbrupt:
		return true
	default:
		return false
	}
}

// BodyState describes what happened to one message body.
type BodyState string

const (
	// BodyEmpty means the message genuinely had no body. It is deliberately
	// distinct from BodyOmitted: "nothing was sent" is not "we could not show
	// what was sent".
	BodyEmpty BodyState = "empty"
	// BodyCaptured means a sanitized body is present.
	BodyCaptured BodyState = "captured"
	// BodyOmitted means a body existed but was not stored. Reason says why.
	BodyOmitted BodyState = "omitted"
)

// Omission reasons. Each is a specific, reportable explanation; a body is never
// stored as an unsafe truncated prefix with no reason attached.
const (
	// ReasonUnsupportedContent covers text, binary and form encodings that v1
	// does not capture (only JSON is inspectable in this milestone).
	ReasonUnsupportedContent = "unsupported_content"
	// ReasonStreamUnsupported covers request bodies that are streams or
	// iterables, which cannot be read without consuming them.
	ReasonStreamUnsupported = "stream_unsupported"
	// ReasonOversized means the body exceeded MaxBodyBytes.
	ReasonOversized = "oversized"
	// ReasonIncomplete means the application did not consume the body fully, so
	// no complete value exists.
	ReasonIncomplete = "incomplete"
	// ReasonEncoded means a content encoding (gzip, br) made the bytes opaque.
	ReasonEncoded = "encoded"
	// ReasonUnparseable means the bytes were not valid JSON.
	ReasonUnparseable = "unparseable"
	// ReasonRedactionFailed means the body could not be safely sanitized. Such a
	// body is omitted, never stored raw.
	ReasonRedactionFailed = "redaction_failed"
	// ReasonQuotaExceeded means a per-run quota rejected the payload.
	ReasonQuotaExceeded = "quota_exceeded"
	// ReasonDropped means queue pressure dropped the event before delivery.
	ReasonDropped = "dropped"
	// ReasonExpired means retention removed the payload.
	ReasonExpired = "expired"
)

// ValidOmissionReason reports whether reason is one this contract defines.
func ValidOmissionReason(reason string) bool {
	switch reason {
	case ReasonUnsupportedContent, ReasonStreamUnsupported, ReasonOversized,
		ReasonIncomplete, ReasonEncoded, ReasonUnparseable, ReasonRedactionFailed,
		ReasonQuotaExceeded, ReasonDropped, ReasonExpired:
		return true
	default:
		return false
	}
}

// ValidBodyState reports whether s is a known body state.
func ValidBodyState(s BodyState) bool {
	switch s {
	case BodyEmpty, BodyCaptured, BodyOmitted:
		return true
	default:
		return false
	}
}

// HeaderPair is one header, kept as an ordered pair rather than folded into a
// map so duplicate names and their original order survive.
type HeaderPair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// BodyDescriptor is everything known about one message body. JSON holds the
// sanitized body when State is BodyCaptured, and nothing otherwise.
type BodyDescriptor struct {
	State         BodyState       `json:"state"`
	ContentType   string          `json:"content_type,omitempty"`
	BytesObserved int64           `json:"bytes_observed,omitempty"`
	JSON          json.RawMessage `json:"json,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	Redacted      bool            `json:"redacted,omitempty"`
	RedactedCount int             `json:"redacted_count,omitempty"`
}

// RequestEvent is one update to one HTTP exchange. All three kinds share this
// shape so ingestion has a single validated path.
type RequestEvent struct {
	Kind        string    `json:"kind"`
	RequestID   string    `json:"request_id"`
	ProducerSeq int64     `json:"producer_seq"`
	OccurredAt  time.Time `json:"occurred_at"`

	Method     string `json:"method,omitempty"`
	URL        string `json:"url,omitempty"`
	InitialURL string `json:"initial_url,omitempty"`
	FinalURL   string `json:"final_url,omitempty"`
	CallSite   string `json:"call_site,omitempty"`

	StatusCode     *int   `json:"status_code,omitempty"`
	TransportError string `json:"transport_error,omitempty"`
	TransportClass string `json:"transport_error_class,omitempty"`

	DurationToHeadersMS *int64 `json:"duration_to_headers_ms,omitempty"`
	DurationBodyMS      *int64 `json:"duration_body_ms,omitempty"`
	DurationTotalMS     *int64 `json:"duration_total_ms,omitempty"`

	RequestHeaders  []HeaderPair `json:"request_headers,omitempty"`
	ResponseHeaders []HeaderPair `json:"response_headers,omitempty"`

	RequestBody  *BodyDescriptor `json:"request_body,omitempty"`
	ResponseBody *BodyDescriptor `json:"response_body,omitempty"`
}

// EventBatch is the POST body of one ingestion request.
//
// It carries run-level counters as well as events. That is deliberate: the SDK
// knows about capture it dropped before delivery, and the daemon has no other
// place to learn it. Riding the existing event endpoint keeps the contract to
// the three documented routes instead of adding a fourth.
type EventBatch struct {
	SchemaVersion int    `json:"schema_version"`
	Policy        Policy `json:"policy"`

	// DroppedEvents and DroppedBytes are the SDK's own tally of capture it could
	// not deliver, because queue pressure or a deadline dropped it. They are
	// cumulative per run and are merged by taking the maximum reported value,
	// since a batch may be retried.
	DroppedEvents int64 `json:"dropped_events,omitempty"`
	DroppedBytes  int64 `json:"dropped_bytes,omitempty"`

	// Finalization is the SDK's own last-word on its recording. It is advisory:
	// the daemon decides the recorded finalization from the child's actual fate,
	// because a process that never got to flush cannot report anything.
	Finalization Finalization `json:"finalization,omitempty"`
	Note         string       `json:"note,omitempty"`

	// Adapters is the set of transport adapters the SDK actually installed in
	// the child process. It is optional and cumulative: the daemon takes the
	// union across batches, so coverage describes what was instrumented rather
	// than what the SDK can theoretically instrument.
	Adapters []string `json:"adapters,omitempty"`

	Events []RequestEvent `json:"events"`
}

// ValidateBatch checks an ingestion submission against the contract and limits.
// It runs before storage, so a rejected batch costs no quota and writes nothing.
//
// Policy is part of the check because a metadata run must not be able to submit
// payloads: the SDK is expected to omit them, but the server does not trust that.
func ValidateBatch(batch EventBatch, limits Limits) error {
	if batch.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported capture schema version %d: this daemon accepts %d",
			batch.SchemaVersion, SchemaVersion)
	}
	if !batch.Policy.Valid() {
		return fmt.Errorf("invalid capture policy %q", batch.Policy)
	}
	if !batch.Policy.Enabled() {
		return fmt.Errorf("capture policy %q does not accept events", batch.Policy)
	}
	if len(batch.Events) > limits.MaxBatchEvents {
		return fmt.Errorf("batch contains %d events, more than the limit of %d",
			len(batch.Events), limits.MaxBatchEvents)
	}
	// A batch may carry only a summary update: the final flush of a run that
	// observed nothing still has to say so, or "no calls" could not be told
	// apart from "capture never reported". An adapter report counts as a
	// summary update for the same reason.
	if len(batch.Events) == 0 && batch.Finalization == "" &&
		batch.DroppedEvents == 0 && batch.DroppedBytes == 0 && len(batch.Adapters) == 0 {
		return fmt.Errorf("batch contains no events and no capture summary update")
	}
	if batch.Finalization != "" && !ValidFinalization(batch.Finalization) {
		return fmt.Errorf("invalid finalization %q", batch.Finalization)
	}
	if len(batch.Adapters) > len(AllAdapters()) {
		return fmt.Errorf("batch reports %d capture adapters, but only %d exist",
			len(batch.Adapters), len(AllAdapters()))
	}
	for _, adapter := range batch.Adapters {
		if !ValidAdapter(adapter) {
			return fmt.Errorf("unsupported capture adapter %q", adapter)
		}
	}
	if batch.DroppedEvents < 0 || batch.DroppedBytes < 0 {
		return fmt.Errorf("dropped counters must not be negative")
	}
	if len(batch.Note) > 512 {
		return fmt.Errorf("note exceeds 512 bytes")
	}
	for i, event := range batch.Events {
		if err := event.Validate(batch.Policy, limits); err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
	}
	return nil
}

// Validate checks a single event. The rules are intentionally strict: a rejected
// event is diagnostic loss, which is always preferable to storing something
// unattributable or unsafe.
func (e RequestEvent) Validate(policy Policy, limits Limits) error {
	if !ValidEventKind(e.Kind) {
		return fmt.Errorf("unsupported event kind %q", e.Kind)
	}
	if err := validateRequestID(e.RequestID, limits); err != nil {
		return err
	}
	if e.ProducerSeq < 0 {
		return fmt.Errorf("producer_seq must not be negative")
	}
	if len(e.Method) > 32 {
		return fmt.Errorf("method is too long")
	}
	if len(e.URL) > limits.MaxURLLength {
		return fmt.Errorf("url exceeds %d bytes", limits.MaxURLLength)
	}
	if len(e.InitialURL) > limits.MaxURLLength || len(e.FinalURL) > limits.MaxURLLength {
		return fmt.Errorf("redirect url exceeds %d bytes", limits.MaxURLLength)
	}
	if len(e.CallSite) > limits.MaxCallSiteLength {
		return fmt.Errorf("call_site exceeds %d bytes", limits.MaxCallSiteLength)
	}
	if len(e.TransportError) > limits.MaxErrorTextBytes {
		return fmt.Errorf("transport_error exceeds %d bytes", limits.MaxErrorTextBytes)
	}
	if len(e.TransportClass) > 128 {
		return fmt.Errorf("transport_error_class is too long")
	}
	if e.StatusCode != nil && (*e.StatusCode < 100 || *e.StatusCode > 599) {
		return fmt.Errorf("status_code %d is not a valid HTTP status", *e.StatusCode)
	}
	for name, ms := range map[string]*int64{
		"duration_to_headers_ms": e.DurationToHeadersMS,
		"duration_body_ms":       e.DurationBodyMS,
		"duration_total_ms":      e.DurationTotalMS,
	} {
		if ms != nil && *ms < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
	}
	if err := validateHeaders(e.RequestHeaders, limits); err != nil {
		return fmt.Errorf("request_headers: %w", err)
	}
	if err := validateHeaders(e.ResponseHeaders, limits); err != nil {
		return fmt.Errorf("response_headers: %w", err)
	}
	if err := validateBody(e.RequestBody, policy, limits); err != nil {
		return fmt.Errorf("request_body: %w", err)
	}
	if err := validateBody(e.ResponseBody, policy, limits); err != nil {
		return fmt.Errorf("response_body: %w", err)
	}
	if e.Kind == EventStarted && strings.TrimSpace(e.Method) == "" {
		return fmt.Errorf("a start event must name a method")
	}
	return nil
}

func validateRequestID(id string, limits Limits) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("request_id must not be empty")
	}
	if len(id) > limits.MaxRequestIDLength {
		return fmt.Errorf("request_id exceeds %d bytes", limits.MaxRequestIDLength)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':':
		default:
			return fmt.Errorf("request_id contains an unsupported character %q", r)
		}
	}
	return nil
}

func validateHeaders(pairs []HeaderPair, limits Limits) error {
	if len(pairs) > limits.MaxHeaderPairs {
		return fmt.Errorf("%d pairs exceeds the limit of %d", len(pairs), limits.MaxHeaderPairs)
	}
	total := 0
	for _, pair := range pairs {
		if strings.TrimSpace(pair.Name) == "" {
			return fmt.Errorf("header name must not be empty")
		}
		if strings.ContainsAny(pair.Name, "\r\n") || strings.ContainsAny(pair.Value, "\r\n") {
			return fmt.Errorf("header %q contains a newline", pair.Name)
		}
		total += len(pair.Name) + len(pair.Value)
	}
	if total > limits.MaxHeaderBytes {
		return fmt.Errorf("header block exceeds %d bytes", limits.MaxHeaderBytes)
	}
	return nil
}

func validateBody(body *BodyDescriptor, policy Policy, limits Limits) error {
	if body == nil {
		return nil
	}
	if !ValidBodyState(body.State) {
		return fmt.Errorf("unsupported body state %q", body.State)
	}
	if len(body.ContentType) > 256 {
		return fmt.Errorf("content type is too long")
	}
	if body.BytesObserved < 0 {
		return fmt.Errorf("bytes_observed must not be negative")
	}
	switch body.State {
	case BodyCaptured:
		if !policy.CapturesBodies() {
			return fmt.Errorf("policy %q does not permit captured payloads", policy)
		}
		if len(body.JSON) == 0 {
			return fmt.Errorf("a captured body must carry its JSON value")
		}
		if int64(len(body.JSON)) > limits.MaxBodyBytes {
			return fmt.Errorf("captured body exceeds %d bytes", limits.MaxBodyBytes)
		}
		if !json.Valid(body.JSON) {
			return fmt.Errorf("captured body is not valid JSON")
		}
	case BodyOmitted:
		if !ValidOmissionReason(body.Reason) {
			return fmt.Errorf("omitted body needs a known reason, got %q", body.Reason)
		}
		if len(body.JSON) != 0 {
			return fmt.Errorf("an omitted body must not carry JSON")
		}
	case BodyEmpty:
		if len(body.JSON) != 0 {
			return fmt.Errorf("an empty body must not carry JSON")
		}
	}
	return nil
}

// PhaseFor derives the stored phase from a terminal event.
func PhaseFor(e RequestEvent) Phase {
	if e.Kind != EventCompleted {
		return PhaseInProgress
	}
	if strings.TrimSpace(e.TransportError) != "" || strings.TrimSpace(e.TransportClass) != "" {
		return PhaseFailed
	}
	return PhaseCompleted
}
