package inspection

import (
	"encoding/json"
	"strings"
)

// Error summaries are extracted from an already-sanitized response body, so they
// are bounded independently of the message a server chose to send. A server is
// free to return a 40 KiB HTML error page; the summary has to stay a line.
const (
	// MaxErrorCodeLength fits a machine-readable code like
	// FIELD_INTEGRITY_EXCEPTION with room to spare.
	MaxErrorCodeLength = 64
	// MaxErrorMessageLength bounds the human-readable text. It is deliberately
	// larger than a table row: the CLI elides to fit, and a truncated summary
	// cannot be untruncated later.
	MaxErrorMessageLength = 300
)

// ErrorSummary is the reason an exchange failed, reduced to a short, sanitized
// pair that a list or timeline can render without loading a payload.
//
// It is derived at ingestion from the same sanitized body that `otter request`
// prints, which is why it can appear in a metadata-only view without widening
// what that view discloses: redaction has already run, and only a bounded
// summary is kept.
type ErrorSummary struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Empty reports whether there is nothing worth showing.
func (s ErrorSummary) Empty() bool { return s.Code == "" && s.Message == "" }

// Text renders the summary for a single line.
func (s ErrorSummary) Text() string {
	switch {
	case s.Code != "" && s.Message != "":
		return s.Code + ": " + s.Message
	case s.Code != "":
		return s.Code
	default:
		return s.Message
	}
}

// ExtractErrorSummary pulls a reason out of a sanitized response body.
//
// It is deliberately shallow and explicit. Servers disagree about the shape of
// an error — an object at the root, a top-level array of them, a bare string —
// and guessing deeply at arbitrary structures would eventually put unrelated
// content where a reader expects the reason. Unrecognised bodies yield nothing,
// which is honest: no summary means the trace shows the status alone rather than
// a confident wrong answer.
//
// The two fields are kept separate rather than pre-joined because the code is
// the part worth scanning and searching (`FIELD_INTEGRITY_EXCEPTION`), while the
// message is prose. A caller can show one and footnote the other.
func ExtractErrorSummary(body *BodyDescriptor, redactor *Redactor) ErrorSummary {
	if body == nil || body.State != BodyCaptured || len(body.JSON) == 0 {
		return ErrorSummary{}
	}

	var doc any
	if err := json.Unmarshal(body.JSON, &doc); err != nil {
		return ErrorSummary{}
	}

	switch value := doc.(type) {
	case string:
		// A body that is just a sentence is its own message.
		return boundSummary(ErrorSummary{Message: value}, redactor)
	case []any:
		// The first entry is the conventional place for the error. Salesforce
		// answers a rejected write this way.
		if len(value) == 0 {
			return ErrorSummary{}
		}
		return summaryFrom(value[0], redactor)
	default:
		return summaryFrom(doc, redactor)
	}
}

// summaryFrom reads the recognised error keys from one JSON value.
func summaryFrom(value any, redactor *Redactor) ErrorSummary {
	object, ok := value.(map[string]any)
	if !ok {
		if text, ok := value.(string); ok {
			return boundSummary(ErrorSummary{Message: text}, redactor)
		}
		return ErrorSummary{}
	}

	// A nested error object is read before anything else: when it is present it
	// holds both the code and the reason.
	if nested, ok := object["error"].(map[string]any); ok {
		return boundSummary(ErrorSummary{
			Code:    firstString(nested, "code", "errorCode", "type"),
			Message: firstString(nested, "message", "detail", "description"),
		}, redactor)
	}

	code := firstString(object, "errorCode", "code", "error_code", "type")
	// A list of field errors carries its code and reason in the first element.
	if code == "" {
		if list, ok := object["errors"].([]any); ok && len(list) > 0 {
			if first, ok := list[0].(map[string]any); ok {
				code = firstString(first, "code", "errorCode")
			}
		}
	}

	// `error` is ambiguous: OAuth puts an enum-like code there, most other APIs
	// put a sentence. An enum has no spaces, which is the signal used to tell
	// them apart. Getting this wrong is visible rather than silent — the worst
	// case is showing "invalid_grant" as the message instead of the code.
	errValue := firstString(object, "error")
	codeLike := errValue != "" && code == "" && !strings.ContainsAny(errValue, " \t")
	if codeLike {
		code = errValue
	}

	message := firstString(object, "message", "errorMessage", "error_description", "detail")
	if message == "" && !codeLike {
		message = errValue
	}
	if message == "" {
		if list, ok := object["errors"].([]any); ok && len(list) > 0 {
			if first, ok := list[0].(map[string]any); ok {
				message = firstString(first, "message", "detail")
			}
		}
	}

	return boundSummary(ErrorSummary{Code: code, Message: message}, redactor)
}

// boundSummary trims and bounds both fields. Values are flattened to one line
// because a summary is rendered into a table cell; a newline would break it.
//
// Both fields go through SanitizeErrorText as well. The body was already
// sanitized as JSON, which redacts credential-shaped *keys*; a server that puts
// a credential inside a message string still has to be caught, and that is
// exactly what the free-text pass exists for.
func boundSummary(summary ErrorSummary, redactor *Redactor) ErrorSummary {
	if redactor == nil {
		redactor = DefaultRedactor()
	}
	// Flatten first: cleanText *removes* a newline rather than replacing it, so
	// collapsing whitespace afterwards is too late — "one\ntwo" has already
	// become "onetwo". Bounding is last, because the ellipsis a truncation adds
	// has to fall inside the limit rather than push past it.
	return ErrorSummary{
		Code:    sanitizeSummaryField(redactor, summary.Code, MaxErrorCodeLength),
		Message: sanitizeSummaryField(redactor, summary.Message, MaxErrorMessageLength),
	}
}

// oneLineText collapses whitespace so a multi-line message cannot escape its
// cell. It runs before cleanText, which drops control characters outright.
func oneLineText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// sanitizeSummaryField flattens, redacts and bounds one summary field, keeping
// the result within limit runes including the marker a truncation adds.
func sanitizeSummaryField(redactor *Redactor, value string, limit int) string {
	flattened := oneLineText(value)
	if flattened == "" {
		return ""
	}
	// SanitizeErrorText appends an ellipsis when it truncates, which puts the
	// result one rune over the limit. Trimming to one less keeps the marker
	// inside the bound rather than past it.
	bounded := redactor.SanitizeErrorText(flattened, limit)
	if limit > 1 && len([]rune(bounded)) > limit {
		bounded = redactor.SanitizeErrorText(flattened, limit-1)
	}
	return bounded
}

func firstString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := object[key].(string); ok {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
