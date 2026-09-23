package inspection

import (
	"encoding/json"
)

// SanitizeEvent applies the redaction policy to an event for storage.
//
// The SDK redacts before it sends, and this runs again on the server. That is
// not redundant defensiveness for its own sake: the server is the last place
// that can guarantee a stored value is safe, and it cannot assume the client is
// current, correct, or even ours. Operator rules can add redaction here; nothing
// a client sends can weaken it.
//
// It returns the sanitized event and how many values were redacted.
func SanitizeEvent(e RequestEvent, r *Redactor, limits Limits) (RequestEvent, int) {
	if r == nil {
		r = DefaultRedactor()
	}
	out := e
	redactions := 0

	var n int
	out.Method = bound(cleanText(e.Method), 32)
	out.URL, n = r.SanitizeURL(e.URL)
	redactions += n
	out.InitialURL, n = r.SanitizeURL(e.InitialURL)
	redactions += n
	out.FinalURL, n = r.SanitizeURL(e.FinalURL)
	redactions += n

	out.CallSite = bound(cleanText(e.CallSite), limits.MaxCallSiteLength)
	out.TransportClass = bound(cleanText(e.TransportClass), 128)
	out.TransportError = r.SanitizeErrorText(e.TransportError, limits.MaxErrorTextBytes)

	out.RequestHeaders, n = r.SanitizeHeaders(e.RequestHeaders)
	redactions += n
	out.ResponseHeaders, n = r.SanitizeHeaders(e.ResponseHeaders)
	redactions += n

	out.RequestBody, n = sanitizeBody(e.RequestBody, r, limits)
	redactions += n
	out.ResponseBody, n = sanitizeBody(e.ResponseBody, r, limits)
	redactions += n

	return out, redactions
}

// sanitizeBody sanitizes one body descriptor. A body that cannot be safely
// processed is replaced by an omission with a specific reason; it is never
// stored as a raw or truncated prefix.
func sanitizeBody(body *BodyDescriptor, r *Redactor, limits Limits) (*BodyDescriptor, int) {
	if body == nil {
		return nil, 0
	}
	out := *body
	out.ContentType = bound(cleanText(body.ContentType), 256)

	switch body.State {
	case BodyCaptured:
		if int64(len(body.JSON)) > limits.MaxBodyBytes {
			out.State = BodyOmitted
			out.Reason = ReasonOversized
			out.JSON = nil
			out.Redacted = false
			out.RedactedCount = 0
			return &out, 0
		}
		sanitized, n, err := r.SanitizeJSON(body.JSON)
		if err != nil {
			out.State = BodyOmitted
			out.Reason = ReasonUnparseable
			out.JSON = nil
			out.Redacted = false
			out.RedactedCount = 0
			return &out, 0
		}
		out.JSON = sanitized
		out.Redacted = n > 0
		out.RedactedCount = n
		return &out, n

	case BodyOmitted:
		// An omitted body must not smuggle bytes past the omission.
		out.JSON = nil
		out.Redacted = false
		out.RedactedCount = 0
		if !ValidOmissionReason(out.Reason) {
			out.Reason = ReasonIncomplete
		}
		return &out, 0

	case BodyEmpty:
		out.JSON = nil
		out.Redacted = false
		out.RedactedCount = 0
		out.Reason = ""
		return &out, 0

	default:
		out.State = BodyOmitted
		out.Reason = ReasonUnsupportedContent
		out.JSON = nil
		out.Redacted = false
		out.RedactedCount = 0
		return &out, 0
	}
}

// PayloadCoverage describes how much of an exchange's bodies is visible, so the
// list view can report completeness without loading any payload.
func PayloadCoverage(policy Policy, request, response *BodyDescriptor) string {
	if !policy.CapturesBodies() {
		return "metadata"
	}
	omitted := false
	reported := false
	for _, body := range []*BodyDescriptor{request, response} {
		if body == nil {
			continue
		}
		reported = true
		if body.State == BodyOmitted {
			omitted = true
		}
	}
	switch {
	case omitted:
		return "partial"
	case reported:
		return "full"
	default:
		// The policy permitted payloads but the exchange never described one, so
		// claiming full coverage would overstate what was captured.
		return "metadata"
	}
}

func bound(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return truncateRunes(s, limit) + "…"
}

func eventRowBytes(e RequestEvent) int64 {
	total := int64(len(e.URL) + len(e.InitialURL) + len(e.FinalURL) +
		len(e.CallSite) + len(e.TransportError) + len(e.Method))
	for _, pair := range e.RequestHeaders {
		total += int64(len(pair.Name) + len(pair.Value))
	}
	for _, pair := range e.ResponseHeaders {
		total += int64(len(pair.Name) + len(pair.Value))
	}
	total += bodyBytes(e.RequestBody)
	total += bodyBytes(e.ResponseBody)
	return total
}

func bodyBytes(body *BodyDescriptor) int64 {
	if body == nil {
		return 0
	}
	return int64(len(body.JSON) + len(body.ContentType) + len(body.Reason))
}

// headerJSON and bodyJSON render a stored column. They marshal values that were
// already validated, so an error here means a programming mistake, not bad input.
func headerJSON(pairs []HeaderPair) (string, error) {
	if len(pairs) == 0 {
		return "[]", nil
	}
	encoded, err := json.Marshal(pairs)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func bodyJSON(body *BodyDescriptor) (string, error) {
	if body == nil {
		return "", nil
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
