package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/api"
)

func TestCaptureSummary(t *testing.T) {
	tests := []struct {
		policy string
		want   string
	}{
		{"full", "full (request and response headers and JSON bodies are stored)"},
		{"metadata", "metadata (request summaries only; no headers or bodies)"},
		{"off", "off (nothing is recorded)"},
	}
	for _, tc := range tests {
		if got := captureSummary(tc.policy); got != tc.want {
			t.Errorf("captureSummary(%q) = %q, want %q", tc.policy, got, tc.want)
		}
	}
}

// `otter inspect` must say whether payloads are being stored before a failure,
// since full is now the default and an operator may not expect it.
func TestInspectReportsCapturePolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/v1/runs") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_ = json.NewEncoder(w).Encode(api.IntegrationView{
			ID:               "billing-sync",
			Name:             "billing-sync",
			Path:             "/tmp/billing-sync",
			Entrypoint:       "main.py",
			PythonExecutable: "python3",
			TimeoutSeconds:   300,
			Concurrency:      1,
			Valid:            true,
			Capture:          "off",
			Retry:            api.RetryView{Backoff: "none", InitialDelay: "2s", MaxDelay: "1m0s"},
		})
	}))
	defer server.Close()

	var out, errOut bytes.Buffer
	app := New("test", &out, &errOut)
	code := app.cmdInspect(context.Background(), globals{api: server.URL}, []string{"billing-sync"})
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut.String())
	}
	if got := out.String(); !strings.Contains(got, "capture:       off (nothing is recorded)") {
		t.Errorf("inspect output does not report the capture policy:\n%s", got)
	}
}
