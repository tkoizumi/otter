package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// parseRecords splits output into one JSON object per line and fails if any
// line is not a single, well-formed JSON object.
func parseRecords(t *testing.T, out string) []map[string]any {
	t.Helper()

	trimmed := strings.TrimRight(out, "\n")
	if trimmed == "" {
		return nil
	}
	lines := strings.Split(trimmed, "\n")
	records := make([]map[string]any, 0, len(lines))
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d is not a JSON object: %q: %v", i, line, err)
		}
		records = append(records, rec)
	}
	return records
}

func stringField(t *testing.T, rec map[string]any, key string) string {
	t.Helper()
	v, ok := rec[key]
	if !ok {
		t.Fatalf("record %v has no %q field", rec, key)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("record field %q = %v (%T), want string", key, v, v)
	}
	return s
}

func TestJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, FormatJSON, LevelDebug)

	l.Info("first_event", "answer", 42, "name", "otter")
	l.Warn("second_event", "ok", true)
	l.Debug("third_event")

	if got := strings.Count(buf.String(), "\n"); got != 3 {
		t.Fatalf("newline count = %d, want one line per record (3)", got)
	}

	records := parseRecords(t, buf.String())
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}

	first := records[0]
	if got := stringField(t, first, "level"); got != "info" {
		t.Fatalf("level = %q, want info", got)
	}
	if got := stringField(t, first, "event"); got != "first_event" {
		t.Fatalf("event = %q", got)
	}
	if got := stringField(t, first, "name"); got != "otter" {
		t.Fatalf("name = %q", got)
	}
	if n, ok := first["answer"].(float64); !ok || n != 42 {
		t.Fatalf("answer = %v (%T), want 42", first["answer"], first["answer"])
	}

	ts := stringField(t, first, "timestamp")
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatalf("timestamp %q is not RFC3339Nano: %v", ts, err)
	}
	if parsed.IsZero() {
		t.Fatalf("timestamp parsed to the zero time")
	}

	if got := stringField(t, records[2], "level"); got != "debug" {
		t.Fatalf("third level = %q, want debug", got)
	}
}

func TestWithMergesFieldsWithoutMutatingParent(t *testing.T) {
	var buf bytes.Buffer
	base := New(&buf, FormatJSON, LevelDebug)

	withA := base.With("a", 1)
	withAB := withA.With("b", 2)

	withA.Info("first")
	withAB.Info("second")
	withA.Info("third")
	base.Info("fourth")

	records := parseRecords(t, buf.String())
	if len(records) != 4 {
		t.Fatalf("records = %d, want 4", len(records))
	}

	if _, ok := records[0]["a"]; !ok {
		t.Fatalf("first record missing merged field a: %v", records[0])
	}
	if _, ok := records[0]["b"]; ok {
		t.Fatalf("first record leaked field b from a later With: %v", records[0])
	}

	if _, ok := records[1]["a"]; !ok {
		t.Fatalf("second record missing inherited field a: %v", records[1])
	}
	if _, ok := records[1]["b"]; !ok {
		t.Fatalf("second record missing own field b: %v", records[1])
	}

	if _, ok := records[2]["b"]; ok {
		t.Fatalf("third record was mutated by the sibling With: %v", records[2])
	}

	if _, ok := records[3]["a"]; ok {
		t.Fatalf("base logger was mutated by With: %v", records[3])
	}

	// A non-string key is skipped rather than panicking.
	withBadKey := base.With(123, "value", "good", "yes")
	before := buf.Len()
	withBadKey.Info("fifth")
	last := parseRecords(t, buf.String()[before:])
	if len(last) != 1 {
		t.Fatalf("records = %d, want 1", len(last))
	}
	if _, ok := last[0]["123"]; ok {
		t.Fatalf("non-string key was recorded: %v", last[0])
	}
	if got := stringField(t, last[0], "good"); got != "yes" {
		t.Fatalf("good = %q", got)
	}
}

func TestPrettyFormat(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, FormatPretty, LevelDebug)

	l.Warn("disk_full", "path", "/var/log", "msg", "has space", "n", 7)

	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("pretty output should end with a newline: %q", out)
	}
	line := strings.TrimRight(out, "\n")
	if strings.Contains(line, "\n") {
		t.Fatalf("pretty output should be one line per record: %q", out)
	}
	for _, want := range []string{"WARN", "disk_full", "path=/var/log", `msg="has space"`, "n=7"} {
		if !strings.Contains(line, want) {
			t.Fatalf("pretty line %q does not contain %q", line, want)
		}
	}
	if json.Valid([]byte(line)) {
		t.Fatalf("pretty output must not be valid JSON: %q", line)
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, FormatJSON, LevelWarn)

	l.Debug("debug_event")
	l.Info("info_event")
	if buf.Len() != 0 {
		t.Fatalf("debug/info must be suppressed at warn level, got %q", buf.String())
	}

	if l.Enabled(LevelDebug) || l.Enabled(LevelInfo) {
		t.Fatalf("Enabled should be false below the configured level")
	}
	if !l.Enabled(LevelWarn) || !l.Enabled(LevelError) {
		t.Fatalf("Enabled should be true at or above the configured level")
	}
	if l.Level() != LevelWarn {
		t.Fatalf("Level() = %v, want warn", l.Level())
	}

	l.Warn("warn_event")
	l.Error("error_event", nil)

	records := parseRecords(t, buf.String())
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if got := stringField(t, records[0], "level"); got != "warn" {
		t.Fatalf("level = %q", got)
	}
	if got := stringField(t, records[1], "level"); got != "error" {
		t.Fatalf("level = %q", got)
	}
}

func TestErrorField(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, FormatJSON, LevelDebug)

	l.Error("failed_event", errors.New("kaboom"))
	l.Error("fine_event", nil)

	records := parseRecords(t, buf.String())
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if got := stringField(t, records[0], "error"); got != "kaboom" {
		t.Fatalf("error = %q, want kaboom", got)
	}
	if _, ok := records[1]["error"]; ok {
		t.Fatalf("nil error must not add an error field: %v", records[1])
	}
}

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in      string
		want    Level
		wantErr bool
	}{
		{"debug", LevelDebug, false},
		{"DEBUG", LevelDebug, false},
		{" info ", LevelInfo, false},
		{"", LevelInfo, false},
		{"warn", LevelWarn, false},
		{"warning", LevelWarn, false},
		{"error", LevelError, false},
		{"verbose", LevelInfo, true},
		{"trace", LevelInfo, true},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseLevel(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseLevel(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLevel(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseFormat(t *testing.T) {
	cases := []struct {
		in      string
		want    Format
		wantErr bool
	}{
		{"json", FormatJSON, false},
		{"JSON", FormatJSON, false},
		{"", FormatJSON, false},
		{"pretty", FormatPretty, false},
		{"text", FormatPretty, false},
		{"console", FormatPretty, false},
		{"xml", FormatJSON, true},
		{"yaml", FormatJSON, true},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseFormat(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseFormat(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFormat(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseFormat(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestUnsupportedValuesAreStringified(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, FormatJSON, LevelDebug)

	when := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	l.Info("vals",
		"dur", 1500*time.Millisecond,
		"when", when,
		"err", errors.New("kaboom"),
		"raw", json.RawMessage(`{"a":1}`),
		"bytes", []byte("hi"),
	)

	records := parseRecords(t, buf.String())
	if len(records) != 1 {
		t.Fatalf("record was dropped: %q", buf.String())
	}
	rec := records[0]

	if got := stringField(t, rec, "dur"); got != "1.5s" {
		t.Fatalf("dur = %q, want 1.5s", got)
	}
	if got := stringField(t, rec, "when"); got != when.Format(time.RFC3339Nano) {
		t.Fatalf("when = %q, want %q", got, when.Format(time.RFC3339Nano))
	}
	if got := stringField(t, rec, "err"); got != "kaboom" {
		t.Fatalf("err = %q", got)
	}
	if got := stringField(t, rec, "bytes"); got != "hi" {
		t.Fatalf("bytes = %q", got)
	}
	raw, ok := rec["raw"].(map[string]any)
	if !ok {
		t.Fatalf("raw = %v (%T), want an inline JSON object", rec["raw"], rec["raw"])
	}
	if n, ok := raw["a"].(float64); !ok || n != 1 {
		t.Fatalf("raw = %v", raw)
	}
}

func TestConcurrentLoggingProducesWellFormedLines(t *testing.T) {
	const goroutines = 50

	var buf bytes.Buffer
	l := New(&buf, FormatJSON, LevelDebug)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.Info("concurrent_event", "worker", i, "message", strings.Repeat("x", 64))
		}(i)
	}
	wg.Wait()

	records := parseRecords(t, buf.String())
	if len(records) != goroutines {
		t.Fatalf("records = %d, want %d; output was corrupted or dropped", len(records), goroutines)
	}

	seen := make(map[int]bool, goroutines)
	for _, rec := range records {
		worker, ok := rec["worker"].(float64)
		if !ok {
			t.Fatalf("record missing worker field: %v", rec)
		}
		seen[int(worker)] = true
	}
	if len(seen) != goroutines {
		t.Fatalf("distinct workers = %d, want %d", len(seen), goroutines)
	}
	for i := 0; i < goroutines; i++ {
		if !seen[i] {
			t.Fatalf("worker %d never logged", i)
		}
	}
}
