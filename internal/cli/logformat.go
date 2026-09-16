package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxFieldValue bounds one rendered field value. The point of the human view is
// that a line fits on a screen; the complete value is always available from the
// JSON form.
const maxFieldValue = 160

// isTerminal reports whether w is attached to a terminal.
//
// It exists so `otter logs` can change shape without a flag: a pipe gets
// machine-readable output, a terminal gets the readable form. That is the
// behaviour people already expect from tools that have both, and it removes the
// redirect-to-a-file-and-strip-the-prefix dance entirely.
//
// A writer that is not an *os.File (a test buffer, say) is not a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// logRecord is one line of machine-readable `otter logs` output.
//
// Text and Fields are split rather than merged so a field named "level" or
// "run_id" inside an integration's payload cannot collide with the envelope.
type logRecord struct {
	RunID     string         `json:"run_id"`
	Timestamp time.Time      `json:"timestamp"`
	Stream    string         `json:"stream"`
	Text      string         `json:"text"`
	Fields    map[string]any `json:"fields,omitempty"`
}

// splitStructured separates a stored log line into its human text and, when the
// line carries one, the structured fields the integration logged.
//
// The SDK writes `ctx.log.info("sync starting", store=...)` as
// `sync starting {"level":"info","store":...}`, so the JSON is a *suffix* of the
// line rather than the whole thing. Anything that does not parse as a JSON
// object is left alone, which keeps a traceback or an integration printing raw
// JSONL from being mangled.
func splitStructured(message string) (string, map[string]any) {
	trimmed := strings.TrimRight(message, "\n")

	// The whole line is the object: the SDK logged without a message.
	if strings.HasPrefix(trimmed, "{") {
		if fields := asObject(trimmed); fields != nil {
			return "", fields
		}
	}

	idx := strings.LastIndex(trimmed, " {")
	if idx < 0 {
		return trimmed, nil
	}
	fields := asObject(trimmed[idx+1:])
	if fields == nil {
		return trimmed, nil
	}
	return strings.TrimSpace(trimmed[:idx]), fields
}

func asObject(s string) map[string]any {
	var fields map[string]any
	if err := json.Unmarshal([]byte(s), &fields); err != nil {
		return nil
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// renderLogLine formats one stored line for a terminal.
//
// The trailing JSON blob is replaced by compact key=value pairs. That is the
// difference between reading your logs and piping them through jq to find out
// what happened: `count=17` rather than `{"count":17,"first":{...400 more
// characters...}}` wrapped over three terminal lines.
func renderLogLine(message string) string {
	text, fields := splitStructured(message)
	if len(fields) == 0 {
		return message
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(text)
	for _, key := range keys {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(formatFieldValue(fields[key]))
	}
	return b.String()
}

// formatFieldValue renders one structured value compactly. Nested objects and
// arrays become compact JSON under a length bound, because the alternative is a
// single field that wraps the line several times over.
func formatFieldValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		// JSON numbers decode as float64; print 17 rather than 17.000000.
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprintf("%v", typed)
		}
		return truncateRunes(string(encoded), maxFieldValue)
	}
}

// truncateRunes bounds a string by runes, so a multibyte character is never cut
// in half and rendered as a replacement glyph.
func truncateRunes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}
