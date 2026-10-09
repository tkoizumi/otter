package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// A dry run must not report an outcome.
//
// The first version of the agent checked the dry run inside the executor, so the
// cycle fell through to the report and told the control plane a runtime was `active`
// when nothing had been created — the user then saw a runtime that did not exist, and
// the workspace stayed in that state until someone looked. The rehearsal path is
// therefore tested for what it must NOT do.
func TestDryRunReleasesItsClaimAndReportsNothing(t *testing.T) {
	var released, reported []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/work"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"work": map[string]any{
					"runtime_id": "rt_dry", "organization_id": "org-1", "tenant": "rtdry",
					"subnet": "100.64.0.0/24", "image": "otter-runtime:local", "isolation": "runsc-v1",
					"limits":           map[string]any{"memory": "320m", "cpus": "1", "pids": 64, "tmpfs_size": "2g", "agent_tmpfs_size": "64m"},
					"bootstrap_secret": "s3cret", "attempts": 1, "lease_expires_at": 0,
				},
				"usage": map[string]int{"assigned": 0, "inFlight": 1, "free": 0}, "capacity": 1,
			})
		case strings.HasSuffix(r.URL.Path, "/release"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			released = append(released, body["runtime_id"])
			_ = json.NewEncoder(w).Encode(map[string]any{"runtime_id": body["runtime_id"], "state": "requested"})
		case strings.HasSuffix(r.URL.Path, "/report"):
			var body Report
			_ = json.NewDecoder(r.Body).Decode(&body)
			reported = append(reported, body.State)
			_ = json.NewEncoder(w).Encode(map[string]any{"recorded": 0, "outcome": nil})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	agent := &Agent{
		Client:   NewClient(server.URL, "otk_test_token"),
		CloudURL: server.URL,
		Options:  Options{DryRun: true, CommandRunner: &RecordingRunner{}},
	}
	if err := agent.Step(context.Background()); err != nil {
		t.Fatalf("dry-run cycle: %v", err)
	}

	if len(released) != 1 || released[0] != "rt_dry" {
		t.Fatalf("a dry run must hand its claim back, got released=%v", released)
	}
	if len(reported) != 0 {
		t.Fatalf("a dry run must report NO outcome, got %v", reported)
	}
}

// A real cycle reports exactly one outcome, and only after the work succeeded.
func TestStepReportsActiveAfterVerifiedWork(t *testing.T) {
	var reports []Report
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/work"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"work": map[string]any{
					"runtime_id": "rt_ok", "organization_id": "org-1", "tenant": "rtok",
					"subnet": "100.64.0.0/24", "image": "otter-runtime:local", "isolation": "runsc-v1",
					"limits":           map[string]any{"memory": "320m", "cpus": "1", "pids": 64, "tmpfs_size": "2g", "agent_tmpfs_size": "64m"},
					"bootstrap_secret": "s3cret", "attempts": 1, "lease_expires_at": 0,
				},
				"usage": map[string]int{"assigned": 0, "inFlight": 1, "free": 0}, "capacity": 1,
			})
		case strings.HasSuffix(r.URL.Path, "/report"):
			var body Report
			_ = json.NewDecoder(r.Body).Decode(&body)
			reports = append(reports, body)
			_ = json.NewEncoder(w).Encode(map[string]any{"recorded": 0, "outcome": nil})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	// A container that appears after the provisioner runs, so verification passes.
	inspects := 0
	runner := &RecordingRunner{}
	runner.Respond = func(name string, args []string) (string, error) {
		if name == "docker" && len(args) > 0 && args[0] == "inspect" {
			inspects++
			if inspects == 1 {
				return "Error: No such object: otter-rtok", errTest("exit status 1")
			}
			return "true|running|rt_ok|org-1|runtime", nil
		}
		return "", nil
	}

	agent := &Agent{
		Client:   NewClient(server.URL, "otk_test_token"),
		CloudURL: server.URL,
		Options:  Options{CommandRunner: runner, StatePath: filepath.Join(t.TempDir(), "state.json")},
	}
	if err := agent.Step(context.Background()); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("want exactly one report, got %d", len(reports))
	}
	if reports[0].State != "active" || reports[0].RuntimeID != "rt_ok" {
		t.Fatalf("want an active report for rt_ok, got %+v", reports[0])
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
