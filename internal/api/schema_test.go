package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/runs"
)

// The frozen JSON shapes. Each list names the fields a consumer may rely on;
// the test asserts they are present, not that nothing else is. Adding an
// optional field is a minor release and must not fail here, while removing or
// renaming one changes or drops a frozen key and does. That is the rule
// [compatibility.md](../../docs/compatibility.md) publishes.
func TestFrozenJSONShapesStillCarryTheirKeys(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name string
		doc  any
		want []string
	}{
		{
			name: "version document",
			doc:  VersionDocumentFor("0.4.0"),
			want: []string{"schema_version", "contract_version", "product_version",
				"manifest_schema", "sdk_version", "supported_platforms"},
		},
		{
			name: "health",
			doc:  HealthResponse{SchemaVersion: SchemaVersion, Status: "ok", Version: "0.4.0"},
			want: []string{"schema_version", "status", "version", "uptime_seconds"},
		},
		{
			name: "error envelope",
			doc:  ErrorResponse{SchemaVersion: SchemaVersion, Error: ErrorBody{Code: CodeNotFound, Message: "nope"}},
			want: []string{"schema_version", "error", "error.code", "error.message"},
		},
		{
			name: "reload result",
			doc: ReloadResult{
				SchemaVersion: SchemaVersion,
				Added:         []string{}, Removed: []string{}, Changed: []string{}, Invalid: []string{},
			},
			want: []string{"schema_version", "added", "removed", "changed", "invalid",
				"total", "valid", "runs_cancelled"},
		},
		{
			name: "schedule list",
			doc:  ScheduleList{SchemaVersion: SchemaVersion, Schedules: []ScheduleView{}},
			want: []string{"schema_version", "schedules"},
		},
		{
			name: "schedule view",
			doc: ScheduleView{
				ID: "sched-1", JobID: "job-1", Name: "job", Cron: "@daily",
				Timezone: "UTC", MissedPolicy: "skip", Origin: "api",
				NextRunAt: &now, Changed: true,
			},
			want: []string{"id", "job_id", "name", "cron", "timezone", "missed_policy",
				"origin", "next_run_at", "changed"},
		},
		{
			name: "job view",
			doc:  JobView{ID: "job-1", Name: "job", Path: "/jobs/job", Valid: true, Generation: 1, Status: "active"},
			want: []string{"id", "name", "path", "entrypoint", "valid", "generation",
				"status", "triggers", "retry", "capture"},
		},
		{
			name: "run view",
			doc: RunView{Run: &runs.Run{
				ID: "run-1", JobID: "job-1", Status: runs.StatusQueued, Attempt: 1,
				CreatedAt: now, TriggerType: runs.TriggerManual,
			}},
			want: []string{"id", "job_id", "status", "attempt", "created_at", "trigger_type"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.doc)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("unmarshal %s: %v", raw, err)
			}
			for _, key := range tc.want {
				if key == "error.code" || key == "error.message" {
					var env struct {
						Error map[string]json.RawMessage `json:"error"`
					}
					_ = json.Unmarshal(raw, &env)
					if _, ok := env.Error[key[len("error."):]]; !ok {
						t.Errorf("%s is missing frozen key %q: %s", tc.name, key, raw)
					}
					continue
				}
				if _, ok := body[key]; !ok {
					t.Errorf("%s is missing frozen key %q: %s", tc.name, key, raw)
				}
			}
		})
	}
}
