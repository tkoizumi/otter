package runs

import "testing"

// TestIsLifecycleUsesRecordedOrigin is the contract: the origin decides, and the
// stream never overrides it. Both writers put lines on the otter stream, which is
// exactly why the stream cannot be the signal.
func TestIsLifecycleUsesRecordedOrigin(t *testing.T) {
	cases := []struct {
		name  string
		entry LogEntry
		want  bool
	}{
		{"daemon narration", LogEntry{Stream: StreamOtter, Message: "run started (attempt 1 of 1)", Origin: OriginDaemon}, true},
		{"ctx.log on the otter stream", LogEntry{Stream: StreamOtter, Message: `sync starting {"level":"info"}`, Origin: OriginChild}, false},
		{"child stdout", LogEntry{Stream: StreamStdout, Message: "hello", Origin: OriginChild}, false},
		{"child stderr", LogEntry{Stream: StreamStderr, Message: "boom", Origin: OriginChild}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.entry.IsLifecycle(); got != tc.want {
				t.Errorf("IsLifecycle() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLooksLikeDaemonNarrationMatchesTheRealStrings checks the classifier against
// the exact shapes the daemon emits, so the migration's patterns and this rule can
// be compared rather than assumed equal.
func TestLooksLikeDaemonNarrationMatchesTheRealStrings(t *testing.T) {
	narration := []string{
		"run queued (trigger manual)",
		"run started (attempt 1 of 3, trigger cron)",
		"run cancelled before execution",
		"run cancelled: cancelled by operator before execution",
		"run failed (attempt 1, 41ms), exit code 1: process exited with code 1",
		"run succeeded (attempt 1, 12ms)",
		"run timed_out (attempt 1, 60s): timed out after 60s",
		"retry 2 of 3 scheduled in 2s as run 8a6c3c9e",
		"not started: missing secret API_KEY",
		"marked failed: otter daemon restarted during execution",
	}
	for _, message := range narration {
		if !LooksLikeDaemonNarration(message) {
			t.Errorf("narration not recognised: %q", message)
		}
	}

	// An integration's own output must not be mistaken for narration, even when
	// it borrows the vocabulary. These are the lines a child can actually write.
	child := []string{
		"run failed because the token expired",
		"run completed successfully",
		"sync starting",
		`sync starting {"dry_run":false,"level":"info"}`,
		"retrying now",
	}
	for _, message := range child {
		if LooksLikeDaemonNarration(message) {
			t.Errorf("child output mistaken for narration: %q", message)
		}
	}
}

// TestLegacyRowsFallBackToShape covers rows written before the origin column: the
// SDK marker wins, then the narration shapes, and anything unrecognised is the
// integration's.
func TestLegacyRowsFallBackToShape(t *testing.T) {
	cases := []struct {
		message string
		want    bool
	}{
		{"run started (attempt 1 of 1)", true},
		{`sync starting {"dry_run":false,"level":"info"}`, false},
		// The whole line is the JSON object: the SDK logged without a message.
		{`{"level":"info","message":"bare"}`, false},
		{"run failed because the token expired", false},
		{"plain stdout text", false},
	}
	for _, tc := range cases {
		entry := LogEntry{Stream: StreamOtter, Message: tc.message}
		if got := entry.IsLifecycle(); got != tc.want {
			t.Errorf("IsLifecycle(%q) = %v, want %v", tc.message, got, tc.want)
		}
	}

	// A stdout line is never narration, whatever it says.
	stdout := LogEntry{Stream: StreamStdout, Message: "run started (attempt 1 of 1)"}
	if stdout.IsLifecycle() {
		t.Errorf("a stdout line must never be classified as narration")
	}
}
