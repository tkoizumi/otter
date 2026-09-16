package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestSplitStructuredSeparatesTheSuffix(t *testing.T) {
	text, fields := splitStructured(`sync starting {"dry_run":true,"level":"info","page_size":100}`)

	if text != "sync starting" {
		t.Errorf("text = %q, want the human prefix", text)
	}
	if fields["dry_run"] != true {
		t.Errorf("dry_run = %v, want true", fields["dry_run"])
	}
	if fields["level"] != "info" {
		t.Errorf("level = %v, want info", fields["level"])
	}
}

// A whole-line object is what the SDK writes when ctx.log is called with no
// message.
func TestSplitStructuredHandlesAWholeLineObject(t *testing.T) {
	text, fields := splitStructured(`{"count":17}`)

	if text != "" {
		t.Errorf("text = %q, want empty", text)
	}
	if fields["count"] != float64(17) {
		t.Errorf("count = %v, want 17", fields["count"])
	}
}

// Anything that is not a JSON object suffix must survive untouched. A traceback
// is the case that matters: mangling it would hide the reason a run failed.
func TestSplitStructuredLeavesOtherLinesAlone(t *testing.T) {
	for _, line := range []string{
		"run queued (trigger manual)",
		"run started (attempt 1 of 3, trigger manual)",
		"Traceback (most recent call last):\n  File \"main.py\", line 1\n    }",
		"a { b",
		"shopify page {not json}",
		"records {}",
	} {
		text, fields := splitStructured(line)
		if fields != nil {
			t.Errorf("%q was parsed as structured: %v", line, fields)
		}
		if text != line {
			t.Errorf("%q: text = %q, want it unchanged", line, text)
		}
	}
}

func TestRenderLogLineCompactsFields(t *testing.T) {
	got := renderLogLine(`shopify page {"count":17,"level":"info","page":0}`)
	want := "shopify page count=17 level=info page=0"

	if got != want {
		t.Errorf("renderLogLine =\n  %q\nwant\n  %q", got, want)
	}
}

func TestRenderLogLineKeepsPlainLines(t *testing.T) {
	line := "run queued (trigger manual)"
	if got := renderLogLine(line); got != line {
		t.Errorf("renderLogLine = %q, want it unchanged", got)
	}
}

// Numbers decode from JSON as float64. Printing 17.000000 in a log line is the
// kind of noise that makes people reach for jq in the first place.
func TestRenderLogLinePrintsNumbersWithoutDecimals(t *testing.T) {
	got := renderLogLine(`page {"count":17,"page":0,"ok":true}`)
	if strings.Contains(got, "17.0") || strings.Contains(got, "0.0") {
		t.Errorf("number rendered as a float: %q", got)
	}
	if !strings.Contains(got, "count=17") || !strings.Contains(got, "page=0") {
		t.Errorf("numbers not rendered plainly: %q", got)
	}
}

// A nested record is what made lines wrap across the terminal. The compact form
// has to bound it.
func TestRenderLogLineTruncatesLargeValues(t *testing.T) {
	big := `{"first":{"id":"` + strings.Repeat("x", 400) + `"}}`
	got := renderLogLine("shopify page " + big)

	if len(got) > maxFieldValue+32 {
		t.Errorf("line is not bounded: %d chars", len(got))
	}
	if !strings.Contains(got, "…") {
		t.Errorf("truncation marker is missing: %q", got)
	}
}

func TestTruncateRunesKeepsMultibyteIntact(t *testing.T) {
	got := truncateRunes("ααααα", 3)
	if got != "ααα…" {
		t.Errorf("truncateRunes = %q, want %q", got, "ααα…")
	}
	if strings.Contains(got, "\ufffd") {
		t.Errorf("truncation split a character: %q", got)
	}
}

// The whole point of the JSON form is that a multi-line message cannot break
// the line-oriented contract: one record, one physical line, newlines escaped.
func TestLogRecordKeepsAMultilineMessageOnOneLine(t *testing.T) {
	encoded, err := json.Marshal(logRecord{
		RunID:  "run-1",
		Stream: "stderr",
		Text:   "Traceback (most recent call last):\n  File \"main.py\", line 1\n    boom",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "\n") {
		t.Errorf("encoded record contains a raw newline: %q", encoded)
	}
	if !strings.Contains(string(encoded), `\n`) {
		t.Errorf("newlines were not escaped: %q", encoded)
	}
}

// A writer that is not a terminal must take the machine-readable path, which is
// what makes `otter logs <id> | jq` work without a flag.
func TestIsTerminalRejectsNonFiles(t *testing.T) {
	if isTerminal(&bytes.Buffer{}) {
		t.Error("a buffer must not be treated as a terminal")
	}
}
