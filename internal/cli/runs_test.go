package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/runs"
)

// runsAPI serves a fixed set of runs, honouring the limit and offset the CLI
// sends, so the tests exercise the real query plumbing rather than a canned body.
func runsAPI(t *testing.T, total int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/runs") {
			_, _ = w.Write([]byte(`[]`))
			return
		}

		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 50
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

		list := []*runs.Run{}
		for i := offset; i < total && len(list) < limit; i++ {
			list = append(list, &runs.Run{
				ID:            "run-" + strconv.Itoa(i),
				IntegrationID: "int-1",
				TriggerType:   runs.TriggerManual,
				Status:        runs.StatusSucceeded,
				Attempt:       1,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"runs": list})
	}))
	t.Cleanup(server.Close)
	return server
}

// TestRunsListsHundredByDefault documents the default page size: the default was
// raised from 20 because a run history is exactly the thing an operator scans.
func TestRunsListsHundredByDefault(t *testing.T) {
	server := runsAPI(t, 250)

	code, stdout, stderr := runCLI(t, "--api", server.URL, "runs")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	rows := strings.Count(stdout, "run-")
	if rows != 100 {
		t.Errorf("printed %d runs, want the 100-run default", rows)
	}
}

// TestRunsLimitIsHonoured covers the flag itself, including the 1000 maximum the
// API accepts in one request.
func TestRunsLimitIsHonoured(t *testing.T) {
	server := runsAPI(t, 1200)

	for _, tc := range []struct{ limit, want int }{
		{5, 5},
		{50, 50},
		{1000, 1000},
	} {
		code, stdout, stderr := runCLI(t, "--api", server.URL, "runs", "--limit", strconv.Itoa(tc.limit))
		if code != 0 {
			t.Fatalf("limit %d: exit = %d, stderr = %s", tc.limit, code, stderr)
		}
		if rows := strings.Count(stdout, "run-"); rows != tc.want {
			t.Errorf("limit %d printed %d runs, want %d", tc.limit, rows, tc.want)
		}
	}
}

// TestRunsSaysWhenTheListIsTruncated is the point of the feature request: a full
// page must not look like the whole history, and the hint must name a limit the
// API will actually accept.
func TestRunsSaysWhenTheListIsTruncated(t *testing.T) {
	server := runsAPI(t, 200)

	code, stdout, stderr := runCLI(t, "--api", server.URL, "runs", "--limit", "20")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if rows := strings.Count(stdout, "run-"); rows != 20 {
		t.Fatalf("printed %d runs, want 20", rows)
	}
	if !strings.Contains(stderr, "--limit 100") {
		t.Errorf("a truncated list should suggest a larger limit:\n%s", stderr)
	}

	// A short page is the whole history and must stay quiet.
	code, _, stderr = runCLI(t, "--api", server.URL, "runs", "--limit", "500")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if strings.Contains(stderr, "--limit") {
		t.Errorf("a complete list must not suggest a larger limit:\n%s", stderr)
	}
}

// TestRunsTruncationHintStaysAtTheMaximum keeps the hint from naming an
// out-of-range limit, which the API would reject.
func TestRunsTruncationHintStaysAtTheMaximum(t *testing.T) {
	server := runsAPI(t, 2000)

	code, _, stderr := runCLI(t, "--api", server.URL, "runs", "--limit", "1000")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "--limit 1000") {
		t.Errorf("at the maximum the hint should still say 1000:\n%s", stderr)
	}
	if strings.Contains(stderr, "--limit 5000") {
		t.Errorf("the hint must never name a limit above the API maximum:\n%s", stderr)
	}
}
