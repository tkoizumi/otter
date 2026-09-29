package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
				ID:          "run-" + strconv.Itoa(i),
				JobID:       "int-1",
				TriggerType: runs.TriggerManual,
				Status:      runs.StatusSucceeded,
				Attempt:     1,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"runs": list})
	}))
	t.Cleanup(server.Close)
	return server
}

// runsFilterRecorder is a run-listing stand-in that remembers the query the CLI
// sent, so the tests can assert what `otter runs` actually asked the daemon for.
type runsFilterRecorder struct {
	query url.Values
	list  []*runs.Run
}

func (rec *runsFilterRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		list := rec.list
		if list == nil {
			list = []*runs.Run{}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"runs": list}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
}

func TestRunsListsHundredByDefault(t *testing.T) {
	server := runsAPI(t, 250)

	code, stdout, stderr := runCLI(t, "--api", server.URL, "runs", "--all")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	rows := strings.Count(stdout, "run-")
	if rows != 100 {
		t.Errorf("printed %d runs, want the 100-run default", rows)
	}
}

func TestRunsLimitIsHonoured(t *testing.T) {
	server := runsAPI(t, 1200)

	for _, tc := range []struct{ limit, want int }{
		{5, 5},
		{50, 50},
		{1000, 1000},
	} {
		code, stdout, stderr := runCLI(t, "--api", server.URL, "runs", "--all", "--limit", strconv.Itoa(tc.limit))
		if code != 0 {
			t.Fatalf("limit %d: exit = %d, stderr = %s", tc.limit, code, stderr)
		}
		if rows := strings.Count(stdout, "run-"); rows != tc.want {
			t.Errorf("limit %d printed %d runs, want %d", tc.limit, rows, tc.want)
		}
	}
}

func TestRunsSaysWhenTheListIsTruncated(t *testing.T) {
	server := runsAPI(t, 200)

	code, stdout, stderr := runCLI(t, "--api", server.URL, "runs", "--all", "--limit", "20")
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
	code, _, stderr = runCLI(t, "--api", server.URL, "runs", "--all", "--limit", "500")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if strings.Contains(stderr, "--limit") {
		t.Errorf("a complete list must not suggest a larger limit:\n%s", stderr)
	}
}

func TestRunsTruncationHintStaysAtTheMaximum(t *testing.T) {
	server := runsAPI(t, 2000)

	code, _, stderr := runCLI(t, "--api", server.URL, "runs", "--all", "--limit", "1000")
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

// TestRunsTakesAPositionalJob is the verb-convention fix: the
// job is an argument, not a flag.
func TestRunsTakesAPositionalJob(t *testing.T) {
	rec := &runsFilterRecorder{}
	server := rec.server(t)
	defer server.Close()

	code, _, stderr := runCLI(t, "--api", server.URL, "runs", "counter")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if got := rec.query.Get("job_id"); got != "counter" {
		t.Errorf("job_id = %q, want counter", got)
	}
}

// TestRunsScopesToTheWorkingDirectoryJob answers "otter runs in an
// job directory": no argument means the job you are standing in.
func TestRunsScopesToTheWorkingDirectoryJob(t *testing.T) {
	withWorkingDir(t, writeManifest(t, "counter"))

	rec := &runsFilterRecorder{}
	server := rec.server(t)
	defer server.Close()

	code, _, stderr := runCLI(t, "--api", server.URL, "runs")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if got := rec.query.Get("job_id"); got != "counter" {
		t.Errorf("job_id = %q, want counter", got)
	}
}

// TestRunsOutsideAnJobRefusesToGuess: a bare `otter runs` elsewhere must
// not silently answer with the whole workspace.
func TestRunsOutsideAnJobRefusesToGuess(t *testing.T) {
	withWorkingDir(t, t.TempDir())

	rec := &runsFilterRecorder{}
	server := rec.server(t)
	defer server.Close()

	code, stdout, stderr := runCLI(t, "--api", server.URL, "runs")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %s", code, stderr)
	}
	if rec.query != nil {
		t.Errorf("the CLI queried the daemon anyway: %v", rec.query)
	}
	if !strings.Contains(stderr, "--all") || !strings.Contains(stderr, "otter runs <job>") {
		t.Errorf("the refusal should name both ways out:\n%s", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("a refusal must not list runs:\n%s", stdout)
	}
}

// TestRunsAllSkipsTheFilter: --all is the explicit workspace-wide listing.
func TestRunsAllSkipsTheFilter(t *testing.T) {
	rec := &runsFilterRecorder{}
	server := rec.server(t)
	defer server.Close()

	code, _, stderr := runCLI(t, "--api", server.URL, "runs", "--all")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if got := rec.query.Get("job_id"); got != "" {
		t.Errorf("job_id = %q, want it unset for --all", got)
	}
}

// TestRunsAllRejectsANamedJob: --all and a name are contradictory.
func TestRunsAllRejectsANamedJob(t *testing.T) {
	code, _, stderr := runCLI(t, "--api", "http://127.0.0.1:1", "runs", "--all", "counter")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "--all") {
		t.Errorf("stderr should explain the conflict:\n%s", stderr)
	}
}

// TestRunsDeprecatedJobFlagStillResolves keeps the older spelling
// working; documentation and scripts used it long before the positional form.
func TestRunsDeprecatedJobFlagStillResolves(t *testing.T) {
	rec := &runsFilterRecorder{}
	server := rec.server(t)
	defer server.Close()

	code, _, stderr := runCLI(t, "--api", server.URL, "runs", "--job", "counter")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if got := rec.query.Get("job_id"); got != "counter" {
		t.Errorf("job_id = %q, want counter", got)
	}
}

// TestRunsFlagAndPositionalMustAgree refuses a contradiction rather than
// silently picking one.
func TestRunsFlagAndPositionalMustAgree(t *testing.T) {
	code, _, stderr := runCLI(t, "--api", "http://127.0.0.1:1", "runs", "counter", "--job", "other")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "different jobs") {
		t.Errorf("stderr should name the conflict:\n%s", stderr)
	}
}

// TestRunsPathArgumentResolvesTheManifestName: a directory is accepted wherever
// a name is, exactly as it is for `otter run` and `otter inspect`.
func TestRunsPathArgumentResolvesTheManifestName(t *testing.T) {
	dir := writeManifest(t, "counter")

	rec := &runsFilterRecorder{}
	server := rec.server(t)
	defer server.Close()

	code, _, stderr := runCLI(t, "--api", server.URL, "runs", dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if got := rec.query.Get("job_id"); got != "counter" {
		t.Errorf("job_id = %q, want the manifest name counter", got)
	}
}

// TestRunsTableShowsTheLabelNotTheIdentity: job_id is a durable UUID on
// a migrated workspace; the table must show what the operator can type back.
func TestRunsTableShowsTheLabelNotTheIdentity(t *testing.T) {
	const identity = "986d91e8-dde4-45be-b298-c9332c220498"
	rec := &runsFilterRecorder{list: []*runs.Run{{
		ID:          "run-1",
		JobID:       identity,
		JobName:     "counter",
		TriggerType: runs.TriggerManual,
		Status:      runs.StatusSucceeded,
		Attempt:     1,
	}}}
	server := rec.server(t)
	defer server.Close()

	code, stdout, stderr := runCLI(t, "--api", server.URL, "runs", "counter")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "counter") {
		t.Errorf("the table should name the job:\n%s", stdout)
	}
	if strings.Contains(stdout, identity) {
		t.Errorf("the table should not show the durable identity:\n%s", stdout)
	}
}

// TestRunsTableFallsBackToTheIDForLegacyRows: rows written before identities
// existed have no name, and their id is the label.
func TestRunsTableFallsBackToTheIDForLegacyRows(t *testing.T) {
	rec := &runsFilterRecorder{list: []*runs.Run{{
		ID:          "run-1",
		JobID:       "shopify-to-salesforce",
		TriggerType: runs.TriggerManual,
		Status:      runs.StatusSucceeded,
		Attempt:     1,
	}}}
	server := rec.server(t)
	defer server.Close()

	code, stdout, stderr := runCLI(t, "--api", server.URL, "runs", "shopify-to-salesforce")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "shopify-to-salesforce") {
		t.Errorf("a legacy row should fall back to its id:\n%s", stdout)
	}
}
