package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/api"
)

func TestPrintReloadResultNamesEveryChange(t *testing.T) {
	var out bytes.Buffer
	app := New("test", &out, &out)

	app.printReloadResult(&api.ReloadResult{
		Added:         []string{"fresh"},
		Removed:       []string{"gone"},
		Changed:       []string{"edited"},
		Invalid:       []string{"broken"},
		Total:         4,
		Valid:         3,
		RunsCancelled: 2,
	})

	got := out.String()
	for _, want := range []string{
		"added        fresh",
		"removed      gone",
		"changed      edited",
		"invalid      broken",
		"cancelled    2 queued run(s)",
		"otter release",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output is missing %q:\n%s", want, got)
		}
	}
}

// A reload that changed nothing is a normal answer, not an empty screen: it is
// what "my edit did not land" looks like, and it must be distinguishable from
// a command that failed.
func TestPrintReloadResultSaysWhenNothingChanged(t *testing.T) {
	var out bytes.Buffer
	app := New("test", &out, &out)

	app.printReloadResult(&api.ReloadResult{Total: 3, Valid: 3})

	got := out.String()
	if !strings.Contains(got, "no changes") || !strings.Contains(got, "3 integration(s)") {
		t.Errorf("output = %q, want a no-changes line naming 3 integrations", got)
	}
	if strings.Contains(got, "otter release") {
		t.Errorf("a reload with nothing added should not suggest a release:\n%s", got)
	}
}

func TestReloadCommandPostsAndPrintsTheResult(t *testing.T) {
	var (
		method string
		path   string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"added":["fresh"],"removed":[],"changed":[],
			"invalid":[],"total":2,"valid":2,"runs_cancelled":0}`)
	}))
	defer server.Close()

	var out, errOut bytes.Buffer
	app := New("test", &out, &errOut)
	if code := app.Run(context.Background(), []string{"--api", server.URL, "reload"}); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut.String())
	}

	if method != http.MethodPost || path != "/v1/reload" {
		t.Errorf("request = %s %s, want POST /v1/reload", method, path)
	}
	if got := out.String(); !strings.Contains(got, "added        fresh") {
		t.Errorf("output = %q, want the added integration named", got)
	}
}

// The daemon answers 409 when a reload is already running, and 403 when the
// caller is not an admin. Both must surface as a failure with the daemon's own
// message rather than as success.
func TestReloadCommandReportsARefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"already running", http.StatusConflict, "conflict"},
		{"not an admin", http.StatusForbidden, "forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":{"code":"`+tc.code+`","message":"a reload is already in progress"}}`)
			}))
			defer server.Close()

			var out, errOut bytes.Buffer
			app := New("test", &out, &errOut)
			if code := app.Run(context.Background(), []string{"--api", server.URL, "reload"}); code == 0 {
				t.Fatalf("exit code = 0, want a failure (stdout: %s)", out.String())
			}
			if got := errOut.String(); !strings.Contains(got, "a reload is already in progress") {
				t.Errorf("stderr = %q, want the daemon's message", got)
			}
		})
	}
}
