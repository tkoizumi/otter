package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/config"
)

// inWorkspace runs fn with the working directory set to dir.
func inWorkspace(t *testing.T, dir string, fn func()) {
	t.Helper()
	original := workingDirForTest
	workingDirForTest = func() (string, error) { return dir, nil }
	defer func() { workingDirForTest = original }()
	fn()
}

// A daemon command outside any workspace must refuse rather than fall back to
// a fixed port, which could belong to a different workspace entirely.
func TestDaemonCommandsRefuseOutsideAWorkspace(t *testing.T) {
	dir := t.TempDir() // no .otter, no .git, no go.mod

	for _, command := range []string{"status", "integrations", "runs", "run", "logs", "state", "inspect"} {
		inWorkspace(t, dir, func() {
			t.Setenv("OTTER_API_URL", "")
			var out, errOut bytes.Buffer
			app := New("test", &out, &errOut)
			code := app.Run(context.Background(), []string{command, "x"})
			if code != 2 {
				t.Errorf("%s outside a workspace exited %d, want 2", command, code)
			}
			if !strings.Contains(errOut.String(), "no workspace here") {
				t.Errorf("%s refusal does not explain itself:\n%s", command, errOut.String())
			}
			if out.Len() != 0 {
				t.Errorf("%s wrote to stdout outside a workspace: %q", command, out.String())
			}
		})
	}
}

// The local commands are their own authority on where state lives and must keep
// working with no workspace in sight.
func TestLocalCommandsWorkOutsideAWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "svc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "svc", "otter.yaml"), []byte("version: 1\nname: svc\nentrypoint: main.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// validate checks that the entrypoint exists, so the fixture needs one.
	if err := os.WriteFile(filepath.Join(dir, "svc", "main.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inWorkspace(t, dir, func() {
		t.Setenv("OTTER_API_URL", "")
		var out, errOut bytes.Buffer
		app := New("test", &out, &errOut)
		// Absolute: validate is a file command and resolves against the process
		// working directory, not against the injected one.
		if code := app.Run(context.Background(), []string{"validate", filepath.Join(dir, "svc")}); code != 0 {
			t.Errorf("validate outside a workspace exited %d: %s", code, errOut.String())
		}
	})
}

// An explicit --api is still honoured anywhere: a tunnel or a remote host is a
// deliberate choice, not an ambient default.
func TestExplicitAPIBypassesTheWorkspaceRequirement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok","version":"test"}`)
	}))
	defer server.Close()

	dir := t.TempDir()
	inWorkspace(t, dir, func() {
		t.Setenv("OTTER_API_URL", "")
		var out, errOut bytes.Buffer
		app := New("test", &out, &errOut)
		if code := app.Run(context.Background(), []string{"--api", server.URL, "status"}); code != 0 {
			t.Errorf("explicit --api was refused: %d %s", code, errOut.String())
		}
	})
}

// A record left by a stopped daemon must not hide the live one further up, and
// must not be answered at all.
func TestResolutionSkipsDeadRecordsAndPrefersTheNearestLiveRuntime(t *testing.T) {
	outer := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outer, stateDirName, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(filepath.Join(inner, stateDirName, "data"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The inner project has a record pointing at a port nothing serves.
	dead := deadURL(t)
	record(t, filepath.Join(inner, stateDirName, "data"), dead)

	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer live.Close()
	record(t, filepath.Join(outer, stateDirName, "data"), live.URL)

	inWorkspace(t, filepath.Join(inner), func() {
		t.Setenv("OTTER_API_URL", "")
		var out, errOut bytes.Buffer
		app := New("test", &out, &errOut)
		if code := app.Run(context.Background(), []string{"status"}); code != 0 {
			t.Fatalf("status exited %d: %s", code, errOut.String())
		}
		if !strings.Contains(out.String(), live.URL) {
			t.Errorf("resolved to something other than the live runtime:\n%s", out.String())
		}
	})
}

// deadURL returns an address nothing is listening on.
func deadURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// A live runtime already serving this workspace on another port or data
// directory must be reported, not taken over.
func TestStartRefusesToTakeOverALiveWorkspaceRuntime(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer live.Close()

	recordDir := serveDir(root, "")
	if err := writeListenURLFile(recordDir, live.URL); err != nil {
		t.Fatal(err)
	}
	if err := writeServePID(recordDir, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := writeServeData(recordDir, "/somewhere/else"); err != nil {
		t.Fatal(err)
	}

	got, ok := otherLiveRuntime(startOptions{
		ProjectRoot: root,
		Data:        filepath.Join(root, stateDirName, "data"),
		Listen:      "127.0.0.1:7499",
	})
	if !ok {
		t.Fatal("a live runtime with a different data directory was not reported")
	}
	if got.URL != live.URL || !strings.Contains(got.Data, "else") {
		t.Errorf("reported %+v, want the live runtime and its data directory", got)
	}
}

// The same address and the same data directory is "already running", which the
// caller reports separately; it must not be mistaken for a takeover.
func TestStartDoesNotCallItsOwnRuntimeATakeover(t *testing.T) {
	root := t.TempDir()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer live.Close()

	data := filepath.Join(root, stateDirName, "data")
	recordDir := serveDir(root, data)
	if err := writeListenURLFile(recordDir, live.URL); err != nil {
		t.Fatal(err)
	}
	if err := writeServeData(recordDir, data); err != nil {
		t.Fatal(err)
	}
	// listenAPIURL of the address this start would use.
	addr := strings.TrimPrefix(live.URL, "http://")

	if _, ok := otherLiveRuntime(startOptions{ProjectRoot: root, Data: data, Listen: addr}); ok {
		t.Error("the workspace's own runtime was reported as a takeover")
	}
}

// State written where the live daemon does not look is refused, with both
// directories named: that is the mistake this guard exists to prevent.
func TestWorkspaceDataRefusesADirectoryTheRuntimeDoesNotRead(t *testing.T) {
	root := t.TempDir()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer live.Close()

	served := filepath.Join(root, "some", "data")
	recordDir := serveDir(root, served)
	if err := writeListenURLFile(recordDir, live.URL); err != nil {
		t.Fatal(err)
	}
	if err := writeServeData(recordDir, served); err != nil {
		t.Fatal(err)
	}

	inWorkspace(t, root, func() {
		var errOut bytes.Buffer
		_, code := resolveWorkspaceData(&errOut, "/tmp/elsewhere", true)
		if code == 0 {
			t.Fatal("a release into a data directory the runtime does not read was allowed")
		}
		// Both directories are named: what you asked for, and what the
		// runtime actually reads.
		for _, want := range []string{"/tmp/elsewhere", served} {
			if !strings.Contains(errOut.String(), want) {
				t.Errorf("refusal does not mention %s:\n%s", want, errOut.String())
			}
		}
	})
}

// Without a live runtime the project convention decides, so a workspace that
// has not been started still has exactly one place its state will appear.
func TestWorkspaceDataDefaultsToTheProjectConvention(t *testing.T) {
	root := t.TempDir()
	// A workspace is a directory with the marker; without it there is no
	// convention to default to.
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	inWorkspace(t, root, func() {
		var errOut bytes.Buffer
		got, code := resolveWorkspaceData(&errOut, "", false)
		if code != 0 {
			t.Fatalf("exited %d: %s", code, errOut.String())
		}
		if want := filepath.Join(root, stateDirName, "data"); got != want {
			t.Errorf("data = %q, want %q", got, want)
		}
	})
}

// Outside a workspace the data directory has to be named explicitly.
func TestWorkspaceDataRefusesOutsideAWorkspaceWithoutAFlag(t *testing.T) {
	dir := t.TempDir()
	inWorkspace(t, dir, func() {
		var errOut bytes.Buffer
		if _, code := resolveWorkspaceData(&errOut, "", false); code == 0 {
			t.Fatal("release outside a workspace was allowed to guess")
		}
		if !strings.Contains(errOut.String(), "no workspace here") {
			t.Errorf("refusal does not explain itself:\n%s", errOut.String())
		}
	})
}

// The integrations root defaults to the workspace, so a local command works
// wherever an integration sits under it, and refuses outside one rather than
// scanning a relative ./integrations that probably is not there.
func TestIntegrationsRootDefaultsToTheWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	inWorkspace(t, filepath.Join(root, "group"), func() {
		var errOut bytes.Buffer
		got, code := resolveIntegrationsRoot(&errOut, config.DefaultIntegrations, false)
		if code != 0 {
			t.Fatalf("exited %d: %s", code, errOut.String())
		}
		if got != root {
			t.Errorf("integrations root = %q, want the workspace root %q", got, root)
		}
		// An explicit flag is still honoured: deploy names a path on a host
		// that has no workspace marker of its own.
		if got, code := resolveIntegrationsRoot(&errOut, "/srv/otter/integrations", true); code != 0 || got != "/srv/otter/integrations" {
			t.Errorf("explicit --integrations = %q (code %d), want it honoured", got, code)
		}
	})

	outside := t.TempDir()
	inWorkspace(t, outside, func() {
		var errOut bytes.Buffer
		if _, code := resolveIntegrationsRoot(&errOut, config.DefaultIntegrations, false); code == 0 {
			t.Fatal("a workspace-scoped command was allowed to guess outside a workspace")
		}
		if !strings.Contains(errOut.String(), "no workspace here") {
			t.Errorf("refusal does not explain itself:\n%s", errOut.String())
		}
	})
}
