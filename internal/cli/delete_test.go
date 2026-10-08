package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tkoizumi/otter/internal/cloud"
)

// deleteCall is one request a delete reached a fake daemon or control plane
// with. The header is cloned because the recorder keeps it after the handler
// returns.
type deleteCall struct {
	method string
	path   string
	query  string
	header http.Header
}

// deleteRecorder is a stand-in for both the local daemon and Otter Cloud: it
// records every call so a test can assert that a declined prompt made none, and
// answers from a routing function.
type deleteRecorder struct {
	mu    sync.Mutex
	calls []deleteCall
	reply func(w http.ResponseWriter, r *http.Request)
}

func (d *deleteRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.calls = append(d.calls, deleteCall{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			header: r.Header.Clone(),
		})
		d.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if d.reply != nil {
			d.reply(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"no route"}`)
	}))
}

func (d *deleteRecorder) recorded() []deleteCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]deleteCall(nil), d.calls...)
}

// runDelete runs one CLI invocation with a chosen stdin, so a test can answer
// the prompt (or prove it was never read).
func runDelete(t *testing.T, stdin io.Reader, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	app := New("test", &out, &errOut)
	app.Stdin = stdin
	code = app.Run(context.Background(), args)
	return out.String(), errOut.String(), code
}

// stubTerminalInput decides interactiveInput without a pty. The real detection
// (a character device) is exercised by the non-interactive tests, which use
// readers that are not terminals.
func stubTerminalInput(t *testing.T, interactive bool) {
	t.Helper()
	previous := interactiveInput
	interactiveInput = func(io.Reader) bool { return interactive }
	t.Cleanup(func() { interactiveInput = previous })
}

// unreadableStdin fails the test if anything tries to read the prompt, which is
// how `--yes` is proven to skip it rather than answer it.
type unreadableStdin struct{ t *testing.T }

func (u unreadableStdin) Read([]byte) (int, error) {
	u.t.Error("stdin was read even though --yes was passed")
	return 0, io.EOF
}

// daemonDeleteReply is the local daemon's answer to a workspace delete.
func daemonDeleteReply(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, `{"deleted":true,"id":"job_1","name":"counter",`+
		`"note":"source files were left in place; the path is suppressed until an explicit register."}`)
}

// A declined prompt must delete nothing: no request is sent, the output says so
// and the exit code is 1. This is the local path's whole safety property.
func TestDeleteLocalDeclineAbortsWithoutTouchingTheDaemon(t *testing.T) {
	rec := &deleteRecorder{reply: daemonDeleteReply}
	srv := rec.server(t)
	defer srv.Close()
	stubTerminalInput(t, true)

	stdout, stderr, code := runDelete(t, strings.NewReader("no thanks\n"), "--api", srv.URL, "delete", "counter")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
	}
	if calls := rec.recorded(); len(calls) != 0 {
		t.Fatalf("a declined delete sent %d requests: %+v", len(calls), calls)
	}
	if !strings.Contains(stderr, "aborted; nothing was deleted") {
		t.Errorf("abort message missing:\n%s", stderr)
	}
	if !strings.Contains(stderr, "counter") {
		t.Errorf("the prompt does not name the job:\n%s", stderr)
	}
	if strings.Contains(stdout, "deleted") {
		t.Errorf("a declined delete reported a deletion:\n%s", stdout)
	}
}

// The prompt accepts y/yes case-insensitively and then performs exactly one
// delete of the reference the operator typed.
func TestDeleteLocalConfirmationProceeds(t *testing.T) {
	rec := &deleteRecorder{reply: daemonDeleteReply}
	srv := rec.server(t)
	defer srv.Close()
	stubTerminalInput(t, true)

	stdout, stderr, code := runDelete(t, strings.NewReader("YES\n"), "--api", srv.URL, "delete", "counter")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	calls := rec.recorded()
	if len(calls) != 1 {
		t.Fatalf("requests = %d, want 1: %+v", len(calls), calls)
	}
	if calls[0].method != http.MethodDelete || calls[0].path != "/v1/jobs/counter" {
		t.Errorf("request = %s %s, want DELETE /v1/jobs/counter", calls[0].method, calls[0].path)
	}
	if !strings.Contains(stdout, "deleted     counter (job_1)") {
		t.Errorf("output does not report the delete:\n%s", stdout)
	}
}

// --yes is the scripted spelling: it must skip the prompt entirely, without
// reading stdin at all.
func TestDeleteLocalYesSkipsThePrompt(t *testing.T) {
	rec := &deleteRecorder{reply: daemonDeleteReply}
	srv := rec.server(t)
	defer srv.Close()

	stdout, stderr, code := runDelete(t, unreadableStdin{t: t}, "--api", srv.URL, "delete", "counter", "--yes")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	if strings.Contains(stderr, "[y/N]") {
		t.Errorf("--yes still prompted:\n%s", stderr)
	}
	if calls := rec.recorded(); len(calls) != 1 {
		t.Fatalf("requests = %d, want 1: %+v", len(calls), calls)
	}
	if !strings.Contains(stdout, "deleted     counter (job_1)") {
		t.Errorf("output does not report the delete:\n%s", stdout)
	}
}

// A short -y is the same skip as --yes, and a flag written after the job still
// parses.
func TestDeleteLocalShortYesAfterTheJobSkipsThePrompt(t *testing.T) {
	rec := &deleteRecorder{reply: daemonDeleteReply}
	srv := rec.server(t)
	defer srv.Close()

	_, stderr, code := runDelete(t, unreadableStdin{t: t}, "--api", srv.URL, "delete", "counter", "-y")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	if calls := rec.recorded(); len(calls) != 1 {
		t.Fatalf("requests = %d, want 1: %+v", len(calls), calls)
	}
}

// Non-interactive stdin without --yes refuses with a message naming --yes and
// performs no side effect. The reader is a strings.Reader, which interactiveInput
// rejects exactly as it rejects a pipe.
func TestDeleteNonInteractiveWithoutYesRefuses(t *testing.T) {
	rec := &deleteRecorder{reply: daemonDeleteReply}
	srv := rec.server(t)
	defer srv.Close()

	stdout, stderr, code := runDelete(t, strings.NewReader("y\n"), "--api", srv.URL, "delete", "counter")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
	}
	if calls := rec.recorded(); len(calls) != 0 {
		t.Fatalf("a refused delete sent %d requests: %+v", len(calls), calls)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("the refusal does not name --yes:\n%s", stderr)
	}
	if !strings.Contains(stderr, "aborted; nothing was deleted") {
		t.Errorf("the refusal is not an abort:\n%s", stderr)
	}
	if strings.Contains(stdout, "deleted") {
		t.Errorf("a refused delete reported a deletion:\n%s", stdout)
	}
}

// A real pipe is not a terminal, so the answer it carries is never trusted and
// nothing blocks on it.
func TestDeleteNonInteractivePipeWithoutYesRefuses(t *testing.T) {
	rec := &deleteRecorder{reply: daemonDeleteReply}
	srv := rec.server(t)
	defer srv.Close()

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer read.Close()
	if _, err := io.WriteString(write, "y\n"); err != nil {
		t.Fatalf("write to pipe: %v", err)
	}
	_ = write.Close()

	_, stderr, code := runDelete(t, read, "--api", srv.URL, "delete", "counter")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
	}
	if calls := rec.recorded(); len(calls) != 0 {
		t.Fatalf("a refused delete sent %d requests: %+v", len(calls), calls)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("the refusal does not name --yes:\n%s", stderr)
	}
}

// The cloud path sends DELETE /api/jobs/{id} with an idempotency key, using the
// id resolved from the label, and prints the runtime's own note.
func TestDeleteCloudUsesTheResolvedIDAndPrintsTheNote(t *testing.T) {
	cloudHome(t)
	const note = "released-only job: no tombstone was written, so releasing this id brings it back"
	rec := &deleteRecorder{reply: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/cloud/me":
			io.WriteString(w, meJSON(`{"id":"rt_1","lifecycle":"running","placement":"aws"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/jobs":
			io.WriteString(w, `{"jobs":[{"id":"job_abc","name":"counter","runtimeId":"rt_1"}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/jobs/job_abc":
			io.WriteString(w, `{"applied":true,`+
				`"value":{"jobId":"job_abc","deleted":true,"note":"`+note+`"},`+
				`"job":{"jobId":"job_abc","deleted":true,"note":"`+note+`"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"no route"}`)
		}
	}}
	srv := rec.server(t)
	defer srv.Close()
	if err := cloud.SaveConfig(cloud.Config{CloudURL: srv.URL, Token: "otk_1_secret"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	stdout, stderr, code := runDelete(t, unreadableStdin{t: t}, "delete", "--cloud", "counter", "--yes")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	if strings.Contains(stderr, "[y/N]") {
		t.Errorf("--yes still prompted:\n%s", stderr)
	}

	var deleted *deleteCall
	resolved := false
	for _, call := range rec.recorded() {
		switch {
		case call.method == http.MethodGet && call.path == "/api/jobs":
			resolved = true
			if call.query != "runtimeId=rt_1" {
				t.Errorf("job list query = %q, want runtimeId=rt_1", call.query)
			}
		case call.method == http.MethodDelete:
			c := call
			deleted = &c
		}
	}
	if !resolved {
		t.Error("the cloud delete never resolved the label through the job list")
	}
	if deleted == nil {
		t.Fatal("the cloud delete sent no DELETE")
	}
	if deleted.path != "/api/jobs/job_abc" {
		t.Errorf("DELETE path = %q, want the resolved id /api/jobs/job_abc", deleted.path)
	}
	if key := deleted.header.Get("Idempotency-Key"); strings.TrimSpace(key) == "" {
		t.Error("the cloud delete sent no Idempotency-Key")
	}
	if !strings.Contains(stdout, "deleted     counter (job_abc)") {
		t.Errorf("output does not report the delete:\n%s", stdout)
	}
	if !strings.Contains(stdout, note) {
		t.Errorf("output swallowed the runtime's note:\n%s", stdout)
	}
}

// The cloud path resolves by id too, without needing a label match.
func TestDeleteCloudResolvesById(t *testing.T) {
	cloudHome(t)
	rec := &deleteRecorder{reply: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/cloud/me":
			io.WriteString(w, meJSON(`{"id":"rt_1","lifecycle":"running","placement":"aws"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/jobs":
			io.WriteString(w, `{"jobs":[{"id":"job_zzz","name":"other","runtimeId":"rt_1"}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/jobs/job_zzz":
			io.WriteString(w, `{"applied":true,"value":{"jobId":"job_zzz","deleted":true,"note":"purged"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"no route"}`)
		}
	}}
	srv := rec.server(t)
	defer srv.Close()
	if err := cloud.SaveConfig(cloud.Config{CloudURL: srv.URL, Token: "otk_1_secret"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	_, stderr, code := runDelete(t, nil, "delete", "--cloud", "job_zzz", "--yes")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	if calls := rec.recorded(); len(calls) != 3 {
		t.Fatalf("requests = %d, want me + list + delete: %+v", len(calls), calls)
	}
}

// An unknown job is a clean not-found: exit 1, no DELETE queued, and no server
// error surfaced as a crash.
func TestDeleteCloudUnknownJobIsANotFound(t *testing.T) {
	cloudHome(t)
	rec := &deleteRecorder{reply: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/cloud/me":
			io.WriteString(w, meJSON(`{"id":"rt_1","lifecycle":"running","placement":"aws"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/jobs":
			io.WriteString(w, `{"jobs":[{"id":"job_abc","name":"counter","runtimeId":"rt_1"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"no route"}`)
		}
	}}
	srv := rec.server(t)
	defer srv.Close()
	if err := cloud.SaveConfig(cloud.Config{CloudURL: srv.URL, Token: "otk_1_secret"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	_, stderr, code := runDelete(t, nil, "delete", "--cloud", "ghost", "--yes")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
	}
	for _, call := range rec.recorded() {
		if call.method == http.MethodDelete {
			t.Fatalf("an unknown job still sent a DELETE: %+v", call)
		}
	}
	if !strings.Contains(stderr, `no job "ghost"`) {
		t.Errorf("the not-found does not name the job:\n%s", stderr)
	}
}

// The cloud gate precedes the first request: declining makes no API call at all,
// not even a read of the organization.
func TestDeleteCloudDeclineMakesNoAPICall(t *testing.T) {
	cloudHome(t)
	stubTerminalInput(t, true)
	rec := &deleteRecorder{}
	srv := rec.server(t)
	defer srv.Close()
	if err := cloud.SaveConfig(cloud.Config{CloudURL: srv.URL, Token: "otk_1_secret"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	stdout, stderr, code := runDelete(t, strings.NewReader("n\n"), "delete", "--cloud", "counter")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
	}
	if calls := rec.recorded(); len(calls) != 0 {
		t.Fatalf("a declined cloud delete sent %d requests: %+v", len(calls), calls)
	}
	if !strings.Contains(stderr, "aborted; nothing was deleted") {
		t.Errorf("abort message missing:\n%s", stderr)
	}
	if strings.Contains(stdout, "deleted") {
		t.Errorf("a declined delete reported a deletion:\n%s", stdout)
	}
}

// Without a stored credential the cloud path refuses before prompting and names
// `otter login` as the fix.
func TestDeleteCloudRefusesWithoutAStoredCredential(t *testing.T) {
	cloudHome(t)

	_, stderr, code := runDelete(t, nil, "delete", "--cloud", "counter", "--yes")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "otter login") {
		t.Errorf("the refusal does not point at otter login:\n%s", stderr)
	}
}

// deletePathRefs are the references a delete must refuse: a directory (the
// working directory, its parent, or a relative or absolute path) or the
// manifest file itself. Every one of them is a reference the other commands
// would happily read as a path, which is exactly why a destructive command
// must not accept it.
var deletePathRefs = []string{
	".",
	"..",
	"./counter",
	"../counter",
	"jobs/counter",
	`jobs\counter`,
	"/abs/path",
	"otter.yaml",
}

// deletePathGuidance is the one sentence both delete paths refuse a path with.
const deletePathGuidance = "otter: delete takes a job name or id, not a path:"

// A path reference is a usage error on the local path, and it is refused before
// anything happens: exit 2, guidance naming the reference, no prompt, no
// daemon request and no reported deletion.
func TestDeleteLocalRefusesPathReferencesWithoutTouchingTheDaemon(t *testing.T) {
	for _, ref := range deletePathRefs {
		t.Run(ref, func(t *testing.T) {
			rec := &deleteRecorder{reply: daemonDeleteReply}
			srv := rec.server(t)
			defer srv.Close()
			// A terminal, so a prompt would be printed and answered if the
			// guard had not run first.
			stubTerminalInput(t, true)

			stdout, stderr, code := runDelete(t, strings.NewReader("y\n"), "--api", srv.URL, "delete", ref)
			if code != 2 {
				t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
			}
			if !strings.Contains(stderr, deletePathGuidance+" "+strconv.Quote(ref)) {
				t.Errorf("the refusal does not quote %q:\n%s", ref, stderr)
			}
			if !strings.Contains(stderr, "pass the name otter jobs lists") {
				t.Errorf("the refusal does not say what to do instead:\n%s", stderr)
			}
			if strings.Contains(stderr, "deleting ") || strings.Contains(stderr, "[y/N]") {
				t.Errorf("a path reference reached the confirmation prompt:\n%s", stderr)
			}
			if strings.Contains(stdout, "deleted") {
				t.Errorf("a refused delete reported a deletion:\n%s", stdout)
			}
			if calls := rec.recorded(); len(calls) != 0 {
				t.Fatalf("a refused delete made %d daemon requests: %+v", len(calls), calls)
			}
		})
	}
}

// The same references are refused on the cloud path, and before the credential
// is even read: exit 2, guidance, no prompt and no API call at all.
func TestDeleteCloudRefusesPathReferencesWithoutAnAPICall(t *testing.T) {
	cloudHome(t)
	for _, ref := range deletePathRefs {
		t.Run(ref, func(t *testing.T) {
			rec := &deleteRecorder{}
			srv := rec.server(t)
			defer srv.Close()
			if err := cloud.SaveConfig(cloud.Config{CloudURL: srv.URL, Token: "otk_1_secret"}); err != nil {
				t.Fatalf("SaveConfig: %v", err)
			}
			stubTerminalInput(t, true)

			stdout, stderr, code := runDelete(t, strings.NewReader("y\n"), "delete", "--cloud", ref)
			if code != 2 {
				t.Fatalf("exit = %d, want 2; stderr:\n%s", code, stderr)
			}
			if !strings.Contains(stderr, deletePathGuidance+" "+strconv.Quote(ref)) {
				t.Errorf("the refusal does not quote %q:\n%s", ref, stderr)
			}
			if strings.Contains(stderr, "deleting ") || strings.Contains(stderr, "[y/N]") {
				t.Errorf("a path reference reached the confirmation prompt:\n%s", stderr)
			}
			if strings.Contains(stdout, "deleted") {
				t.Errorf("a refused delete reported a deletion:\n%s", stdout)
			}
			if calls := rec.recorded(); len(calls) != 0 {
				t.Fatalf("a refused cloud delete made %d API calls: %+v", len(calls), calls)
			}
		})
	}
}

// promptReference extracts the reference the confirmation echoed, so a test can
// assert on the thing the operator is asked to authorise.
func promptReference(t *testing.T, stderr string) string {
	t.Helper()
	_, rest, ok := strings.Cut(stderr, "otter: deleting ")
	if !ok {
		t.Fatalf("no confirmation prompt in:\n%s", stderr)
	}
	ref, _, ok := strings.Cut(rest, " destroys, irreversibly:")
	if !ok {
		t.Fatalf("malformed confirmation prompt in:\n%s", stderr)
	}
	return ref
}

// The confirmation can no longer be pointed at a path. A reference that reaches
// the prompt is the operator's name or id, and the prompt echoes it verbatim --
// this pins that property on both delete paths, so a future refactor cannot
// reintroduce "deleting . destroys, irreversibly:".
func TestDeleteConfirmationNeverNamesAPath(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		rec := &deleteRecorder{reply: daemonDeleteReply}
		srv := rec.server(t)
		defer srv.Close()
		stubTerminalInput(t, true)

		_, stderr, code := runDelete(t, strings.NewReader("no\n"), "--api", srv.URL, "delete", "counter")
		if code != 1 {
			t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
		}
		ref := promptReference(t, stderr)
		if ref != "counter" {
			t.Errorf("the prompt named %q, want the reference typed", ref)
		}
		if looksLikePath(ref) {
			t.Errorf("the confirmation named a path: %q", ref)
		}
		if calls := rec.recorded(); len(calls) != 0 {
			t.Fatalf("a declined delete sent %d requests: %+v", len(calls), calls)
		}
	})

	t.Run("cloud", func(t *testing.T) {
		cloudHome(t)
		rec := &deleteRecorder{}
		srv := rec.server(t)
		defer srv.Close()
		if err := cloud.SaveConfig(cloud.Config{CloudURL: srv.URL, Token: "otk_1_secret"}); err != nil {
			t.Fatalf("SaveConfig: %v", err)
		}
		stubTerminalInput(t, true)

		_, stderr, code := runDelete(t, strings.NewReader("no\n"), "delete", "--cloud", "counter")
		if code != 1 {
			t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
		}
		ref := promptReference(t, stderr)
		if ref != "counter" {
			t.Errorf("the prompt named %q, want the reference typed", ref)
		}
		if looksLikePath(ref) {
			t.Errorf("the confirmation named a path: %q", ref)
		}
		if calls := rec.recorded(); len(calls) != 0 {
			t.Fatalf("a declined cloud delete sent %d requests: %+v", len(calls), calls)
		}
	})
}

// A valid name and a valid id still delete on the local path, exactly as before
// the guard: --yes skips the prompt and the daemon receives the reference.
func TestDeleteLocalStillDeletesByNameAndIdWithYes(t *testing.T) {
	cases := []struct {
		ref      string
		wantPath string
	}{
		{"counter", "/v1/jobs/counter"},
		{"job_1", "/v1/jobs/job_1"},
	}
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			rec := &deleteRecorder{reply: daemonDeleteReply}
			srv := rec.server(t)
			defer srv.Close()

			stdout, stderr, code := runDelete(t, unreadableStdin{t: t}, "--api", srv.URL, "delete", tc.ref, "--yes")
			if code != 0 {
				t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
			}
			calls := rec.recorded()
			if len(calls) != 1 {
				t.Fatalf("requests = %d, want 1: %+v", len(calls), calls)
			}
			if calls[0].method != http.MethodDelete || calls[0].path != tc.wantPath {
				t.Errorf("request = %s %s, want DELETE %s", calls[0].method, calls[0].path, tc.wantPath)
			}
			if !strings.Contains(stdout, "deleted     counter (job_1)") {
				t.Errorf("output does not report the delete:\n%s", stdout)
			}
		})
	}
}

// The cloud path still resolves by case-insensitive label and exact id after
// the guard, and still sends the DELETE for the resolved id.
func TestDeleteCloudStillResolvesNameAndId(t *testing.T) {
	cases := []struct {
		ref    string
		wantID string
	}{
		{"counter", "job_name"},
		{"COUNTER", "job_name"},
		{"job_wanted", "job_wanted"},
	}
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			cloudHome(t)
			rec := &deleteRecorder{reply: func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/cloud/me":
					io.WriteString(w, meJSON(`{"id":"rt_1","lifecycle":"running","placement":"aws"}`))
				case r.Method == http.MethodGet && r.URL.Path == "/api/jobs":
					io.WriteString(w, `{"jobs":[`+
						`{"id":"job_name","name":"counter","runtimeId":"rt_1"},`+
						`{"id":"job_wanted","name":"other","runtimeId":"rt_1"}]}`)
				case r.Method == http.MethodDelete && r.URL.Path == "/api/jobs/"+tc.wantID:
					io.WriteString(w, `{"applied":true,"value":{"jobId":"`+tc.wantID+`","deleted":true,"note":"purged"}}`)
				default:
					w.WriteHeader(http.StatusNotFound)
					io.WriteString(w, `{"message":"no route"}`)
				}
			}}
			srv := rec.server(t)
			defer srv.Close()
			if err := cloud.SaveConfig(cloud.Config{CloudURL: srv.URL, Token: "otk_1_secret"}); err != nil {
				t.Fatalf("SaveConfig: %v", err)
			}

			stdout, stderr, code := runDelete(t, unreadableStdin{t: t}, "delete", "--cloud", tc.ref, "--yes")
			if code != 0 {
				t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
			}
			deletes := 0
			for _, call := range rec.recorded() {
				if call.method == http.MethodDelete {
					deletes++
					if call.path != "/api/jobs/"+tc.wantID {
						t.Errorf("DELETE path = %q, want /api/jobs/%s", call.path, tc.wantID)
					}
				}
			}
			if deletes != 1 {
				t.Fatalf("DELETE requests = %d, want 1: %+v", deletes, rec.recorded())
			}
			if !strings.Contains(stdout, "deleted") {
				t.Errorf("output does not report the delete:\n%s", stdout)
			}
		})
	}
}

// A delete the control plane says did NOT leave its desired state is not a
// deletion. Reporting success here is the overclaim this path exists to stop:
// the release is still desired, so the agent will converge the runtime back onto
// it.
func TestDeleteCloudReportsAJobStillDesiredAsNotRemoved(t *testing.T) {
	cloudHome(t)
	rec := &deleteRecorder{reply: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/cloud/me":
			io.WriteString(w, meJSON(`{"id":"rt_1","lifecycle":"running","placement":"aws"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/jobs":
			io.WriteString(w, `{"jobs":[{"id":"job_abc","name":"counter","runtimeId":"rt_1"}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/jobs/job_abc":
			io.WriteString(w, `{"applied":false,`+
				`"value":{"jobId":"job_abc","deleted":false,"removedFromDesired":false,`+
				`"note":"nothing was removed: operation op_0 is still pending"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"no route"}`)
		}
	}}
	srv := rec.server(t)
	defer srv.Close()
	if err := cloud.SaveConfig(cloud.Config{CloudURL: srv.URL, Token: "otk_1_secret"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	stdout, stderr, code := runDelete(t, unreadableStdin{t: t}, "delete", "--cloud", "counter", "--yes")
	if code == 0 {
		t.Fatalf("a job still in desired state exited 0; stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "deleted     ") {
		t.Errorf("a job still in desired state was reported deleted:\n%s", stdout)
	}
	if !strings.Contains(stderr, "still in Otter Cloud's desired state") {
		t.Errorf("the refusal does not say the job is still desired:\n%s", stderr)
	}
	if !strings.Contains(stderr, "op_0") {
		t.Errorf("the control plane's own reason was swallowed:\n%s", stderr)
	}
}

// A removal Cloud recorded but the runtime has not CONFIRMED is reported
// pending, not deleted. Cloud must not mark an operation successful without
// runtime confirmation, and the CLI must not print one either.
func TestDeleteCloudReportsAnUnconfirmedRemovalAsPending(t *testing.T) {
	cloudHome(t)
	rec := &deleteRecorder{reply: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/cloud/me":
			io.WriteString(w, meJSON(`{"id":"rt_1","lifecycle":"running","placement":"aws"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/jobs":
			io.WriteString(w, `{"jobs":[{"id":"job_abc","name":"counter","runtimeId":"rt_1"}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/jobs/job_abc":
			io.WriteString(w, `{"applied":true,`+
				`"value":{"jobId":"job_abc","deleted":false,"removedFromDesired":true,"pending":true,`+
				`"note":"removed from Cloud's desired state, but runtime rt_1 has not confirmed the removal yet"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"no route"}`)
		}
	}}
	srv := rec.server(t)
	defer srv.Close()
	if err := cloud.SaveConfig(cloud.Config{CloudURL: srv.URL, Token: "otk_1_secret"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	stdout, stderr, code := runDelete(t, unreadableStdin{t: t}, "delete", "--cloud", "counter", "--yes")
	if code == 0 {
		t.Fatalf("an unconfirmed removal exited 0; stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "deleted     ") {
		t.Errorf("an unconfirmed removal was reported deleted:\n%s", stdout)
	}
	if !strings.Contains(stdout, "pending     counter (job_abc)") {
		t.Errorf("output does not report the removal as pending:\n%s", stdout)
	}
	if !strings.Contains(stdout, "has not confirmed the removal") {
		t.Errorf("the runtime's note was swallowed:\n%s", stdout)
	}
	if strings.Contains(stderr, "still in Otter Cloud's desired state") {
		t.Errorf("a job that DID leave desired state was reported as still desired:\n%s", stderr)
	}
}
