package cli

import (
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/runs"
)

// A run's row has to answer two questions its status cannot: whether the RUNTIME
// ended it -- the question the pilot gate's reliability row is written in -- and
// whether its output survived. Both columns are `-` for the ordinary run, so each
// draws the eye only on the rows that have something to say.
func TestAttributionColumnAndNote(t *testing.T) {
	cases := []struct {
		name        string
		attribution string
		column      string
		noteHas     string
	}{
		{
			// The one a reliability window counts, and the only value the column
			// shows: everything else is a dash.
			name:        "the runtime's own ending is named and drawn",
			attribution: runs.AttributionRuntime,
			column:      "runtime",
			noteHas:     "reliability window",
		},
		{
			name:        "a job's ending is a dash in the table",
			attribution: runs.AttributionJob,
			column:      "-",
			noteHas:     "job's own execution",
		},
		{
			name:        "an operator's ending is a dash in the table",
			attribution: runs.AttributionOperator,
			column:      "-",
			noteHas:     "an operator",
		},
		{
			// A run that has not finished, a success, or a row written before the
			// column existed. The note is empty so the caller omits the line: an
			// "unknown" would invent a failure.
			name:        "an unrecorded ending says nothing",
			attribution: "",
			column:      "-",
			noteHas:     "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := &runs.Run{Attribution: tc.attribution}
			if got := attributionColumn(run); got != tc.column {
				t.Errorf("column = %q, want %q", got, tc.column)
			}
			note := attributionNote(run)
			if tc.noteHas == "" {
				if note != "" {
					t.Errorf("note = %q, want empty", note)
				}
				return
			}
			if !strings.Contains(note, tc.noteHas) {
				t.Errorf("note = %q, want it to mention %q", note, tc.noteHas)
			}
		})
	}
}

// The pair that predates it, pinned here for the same reason: the column is a
// dash unless something was actually lost, and the note is absent rather than a
// reassuring "none".
func TestLogLossColumnAndNote(t *testing.T) {
	run := &runs.Run{}
	if got := logLossColumn(run); got != "-" {
		t.Errorf("column = %q, want -", got)
	}
	if got := logLossNote(run); got != "" {
		t.Errorf("note = %q, want empty", got)
	}

	run = &runs.Run{LogLost: true, LogLinesDropped: 3}
	if got := logLossColumn(run); got != "lost 3" {
		t.Errorf("column = %q, want %q", got, "lost 3")
	}
	if got := logLossNote(run); !strings.Contains(got, "3 captured line(s)") {
		t.Errorf("note = %q, want the count in it", got)
	}

	// Loss recorded without a count still has to say so.
	run = &runs.Run{LogLost: true}
	if got := logLossColumn(run); got != "lost" {
		t.Errorf("column = %q, want lost", got)
	}
	if got := logLossNote(run); got == "" {
		t.Error("note is empty for a run whose output was lost")
	}
}
