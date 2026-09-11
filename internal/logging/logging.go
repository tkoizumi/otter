// Package logging writes Otter's own daemon logs.
//
// The default format is one JSON object per line so that a supervisor or log
// shipper can consume it directly; --log-format pretty produces something a
// human can read during development.
package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Level is a log severity.
type Level int

// Severities in increasing order.
const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// ParseLevel converts a configuration string into a Level.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug, nil
	case "info", "":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	default:
		return LevelInfo, fmt.Errorf("unknown log level %q", s)
	}
}

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "info"
	}
}

// Format is the output encoding.
type Format string

// Supported formats.
const (
	FormatJSON   Format = "json"
	FormatPretty Format = "pretty"
)

// ParseFormat converts a configuration string into a Format.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "json", "":
		return FormatJSON, nil
	case "pretty", "text", "console":
		return FormatPretty, nil
	default:
		return FormatJSON, fmt.Errorf("unknown log format %q", s)
	}
}

// Logger emits structured events. It is safe for concurrent use, and a
// derived logger shares the parent's writer and lock.
type Logger struct {
	mu     *sync.Mutex
	w      io.Writer
	format Format
	level  Level
	fields map[string]any
}

// New creates a logger writing to w.
func New(w io.Writer, format Format, level Level) *Logger {
	return &Logger{
		mu:     &sync.Mutex{},
		w:      w,
		format: format,
		level:  level,
		fields: map[string]any{},
	}
}

// With returns a logger that always includes the supplied key/value pairs.
// Keys and values are given as alternating arguments.
func (l *Logger) With(kv ...any) *Logger {
	fields := make(map[string]any, len(l.fields)+len(kv)/2)
	for k, v := range l.fields {
		fields[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			continue
		}
		fields[key] = normalize(kv[i+1])
	}
	return &Logger{mu: l.mu, w: l.w, format: l.format, level: l.level, fields: fields}
}

// Level reports the configured minimum level.
func (l *Logger) Level() Level { return l.level }

// Enabled reports whether a level would be emitted.
func (l *Logger) Enabled(level Level) bool { return level >= l.level }

// Debug logs at debug level.
func (l *Logger) Debug(event string, kv ...any) { l.log(LevelDebug, event, nil, kv...) }

// Info logs at info level.
func (l *Logger) Info(event string, kv ...any) { l.log(LevelInfo, event, nil, kv...) }

// Warn logs at warn level.
func (l *Logger) Warn(event string, kv ...any) { l.log(LevelWarn, event, nil, kv...) }

// Error logs at error level. If err is non-nil it is recorded under "error".
func (l *Logger) Error(event string, err error, kv ...any) { l.log(LevelError, event, err, kv...) }

func (l *Logger) log(level Level, event string, err error, kv ...any) {
	if !l.Enabled(level) {
		return
	}

	fields := make(map[string]any, len(l.fields)+len(kv)/2+1)
	for k, v := range l.fields {
		fields[k] = v
	}
	if err != nil {
		fields["error"] = err.Error()
	}
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			continue
		}
		fields[key] = normalize(kv[i+1])
	}

	now := time.Now().UTC()

	l.mu.Lock()
	defer l.mu.Unlock()

	switch l.format {
	case FormatPretty:
		l.writePretty(now, level, event, fields)
	default:
		l.writeJSON(now, level, event, fields)
	}
}

func (l *Logger) writeJSON(now time.Time, level Level, event string, fields map[string]any) {
	record := make(map[string]any, len(fields)+3)
	record["level"] = level.String()
	record["event"] = event
	record["timestamp"] = now.Format(time.RFC3339Nano)
	for k, v := range fields {
		if k == "level" || k == "event" || k == "timestamp" {
			continue
		}
		record[k] = v
	}

	// Marshal deterministically by building the object through an ordered
	// encoder: keys are sorted so that log lines diff cleanly.
	keys := make([]string, 0, len(record))
	for k := range record {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	buf := make([]byte, 0, 256)
	buf = append(buf, '{')
	for i, k := range keys {
		if i > 0 {
			buf = append(buf, ',')
		}
		keyJSON, _ := json.Marshal(k)
		valJSON, err := json.Marshal(record[k])
		if err != nil {
			valJSON, _ = json.Marshal(fmt.Sprintf("%v", record[k]))
		}
		buf = append(buf, keyJSON...)
		buf = append(buf, ':')
		buf = append(buf, valJSON...)
	}
	buf = append(buf, '}', '\n')
	_, _ = l.w.Write(buf)
}

func (l *Logger) writePretty(now time.Time, level Level, event string, fields map[string]any) {
	var b strings.Builder
	b.WriteString(now.Format("2006-01-02T15:04:05.000Z"))
	b.WriteString(" ")
	b.WriteString(fmt.Sprintf("%-5s", strings.ToUpper(level.String())))
	b.WriteString(" ")
	b.WriteString(event)

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(" ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(formatValue(fields[k]))
	}
	b.WriteString("\n")
	_, _ = io.WriteString(l.w, b.String())
}

func formatValue(v any) string {
	switch t := v.(type) {
	case string:
		if strings.ContainsAny(t, " \t\"") {
			quoted, err := json.Marshal(t)
			if err == nil {
				return string(quoted)
			}
		}
		return t
	case nil:
		return "-"
	default:
		return fmt.Sprintf("%v", t)
	}
}

// normalize converts values that JSON cannot represent into strings.
func normalize(v any) any {
	switch t := v.(type) {
	case nil, bool, string, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64,
		json.RawMessage, []string, map[string]string:
		return t
	case error:
		return t.Error()
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case time.Duration:
		return t.String()
	case []byte:
		return string(t)
	default:
		return fmt.Sprintf("%v", t)
	}
}
