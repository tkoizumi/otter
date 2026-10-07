package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/tkoizumi/otter/internal/cloud"
)

// cloudHome points HOME at a temporary directory and clears the environment
// overrides, so a test controls exactly which credential a command finds. The
// real ~/.otter is never read or written.
func cloudHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(cloud.URLEnv, "")
	t.Setenv(cloud.TokenEnv, "")
	return home
}

// meJSON is the /api/cloud/me answer for an organization with the given
// runtimes.
func meJSON(runtimes string) string {
	return `{"organization":{"id":"org_1","name":"Acme"},"runtimes":[` + runtimes + `]}`
}

func TestLoginStoresTheVerifiedCredential(t *testing.T) {
	home := cloudHome(t)

	var mu sync.Mutex
	var paths, auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, meJSON(
			`{"id":"rt_1","lifecycle":"running","placement":"aws"},`+
				`{"id":"rt_2","lifecycle":"paused","placement":"gcp"}`))
	}))
	defer srv.Close()

	stdout, stderr, code := otterIn(t, t.TempDir(), "login", "--cloud", srv.URL, "--token", "otk_1_secret")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"logged in to " + srv.URL, "Acme", "org_1", "rt_1", "rt_2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output does not mention %q:\n%s", want, stdout)
		}
	}

	cfg, err := cloud.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Token != "otk_1_secret" || cfg.OrganizationID != "org_1" || cfg.OrganizationName != "Acme" {
		t.Errorf("stored config = %+v", cfg)
	}

	path, err := cloud.ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if !strings.HasPrefix(path, home) {
		t.Errorf("config landed at %s, not under the test HOME %s", path, home)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config mode = %04o, want 0600", perm)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/api/cloud/me" {
		t.Errorf("requests = %v, want exactly the verification call", paths)
	}
	if len(auths) != 1 || auths[0] != "Bearer otk_1_secret" {
		t.Errorf("Authorization headers = %v", auths)
	}
}

func TestLoginRejectsARevokedToken(t *testing.T) {
	cloudHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message":"token revoked"}`)
	}))
	defer srv.Close()

	_, stderr, code := otterIn(t, t.TempDir(), "login", "--cloud", srv.URL, "--token", "otk_bad")
	if code == 0 {
		t.Fatalf("a rejected token exited 0:\n%s", stderr)
	}
	for _, want := range []string{"rejected", "token revoked"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, stderr)
		}
	}
	if _, err := cloud.LoadConfig(); !errors.Is(err, cloud.ErrNotLoggedIn) {
		t.Errorf("a rejected token was stored: %v", err)
	}
}

func TestLoginStatusReportsTheStoredIdentity(t *testing.T) {
	cloudHome(t)
	if err := cloud.SaveConfig(cloud.Config{
		CloudURL:         "https://cloud.example",
		Token:            "otk_1_secret",
		OrganizationID:   "org_1",
		OrganizationName: "Acme",
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// No server is started: --status must answer from the file alone.
	stdout, stderr, code := otterIn(t, t.TempDir(), "login", "--status")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"Acme", "org_1", "https://cloud.example"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output does not mention %q:\n%s", want, stdout)
		}
	}
}

func TestLogoutRemovesTheCredentialAndIsIdempotent(t *testing.T) {
	cloudHome(t)
	if err := cloud.SaveConfig(cloud.Config{
		CloudURL:         "https://cloud.example",
		Token:            "otk_1_secret",
		OrganizationName: "Acme",
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	stdout, stderr, code := otterIn(t, t.TempDir(), "logout")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"removed", "logged out of Acme"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output does not mention %q:\n%s", want, stdout)
		}
	}
	if _, err := cloud.LoadConfig(); !errors.Is(err, cloud.ErrNotLoggedIn) {
		t.Errorf("config survived logout: %v", err)
	}

	stdout, stderr, code = otterIn(t, t.TempDir(), "logout")
	if code != 0 {
		t.Fatalf("second logout exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "nothing to remove") {
		t.Errorf("second logout output:\n%s", stdout)
	}
}

func TestDeployCloudPromotesTheCanonicalRelease(t *testing.T) {
	cloudHome(t)
	t.Setenv(cloud.TokenEnv, "otk_1_secret")
	root, _ := releaseWorkspace(t, "counter")
	// The default cloud path promotes a release that already exists locally, so
	// build one first. The canonical digest asserted below is that release's.
	if _, stderr, code := otterIn(t, root, "release", "counter"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}

	var mu sync.Mutex
	var paths, auths []string
	var releaseDigest string
	var deployBody cloud.DeployRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/releases/") {
			// Not uploaded yet: the presence probe must say so.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/cloud/me":
			io.WriteString(w, meJSON(`{"id":"rt_1","lifecycle":"running","placement":"aws"}`))
		case "GET /api/runtimes/rt_1/operations":
			io.WriteString(w, `{"generation":7}`)
		case "POST /api/releases":
			body, _ := io.ReadAll(r.Body)
			if len(body) == 0 {
				t.Error("release upload carried no bytes")
			}
			mu.Lock()
			releaseDigest = r.Header.Get("x-otter-release-digest")
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"rel_1"}`)
		case "POST /api/runtimes/rt_1/operations":
			if err := json.NewDecoder(r.Body).Decode(&deployBody); err != nil {
				t.Errorf("decode deploy body: %v", err)
			}
			io.WriteString(w, `{"admitted":true,"operation":{"id":"op_1","status":"queued"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv(cloud.URLEnv, srv.URL)

	stdout, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "operation:") || !strings.Contains(stdout, "op_1") {
		t.Errorf("output does not report the operation:\n%s", stdout)
	}

	mu.Lock()
	gotDigest := releaseDigest
	gotPaths := append([]string(nil), paths...)
	gotAuths := append([]string(nil), auths...)
	mu.Unlock()

	if len(gotDigest) != 64 {
		t.Fatalf("x-otter-release-digest = %q, want a bare 64-char digest", gotDigest)
	}
	if strings.ContainsAny(gotDigest, "ghijklmnopqrstuvwxyz:") {
		t.Errorf("digest %q is not bare lowercase hex", gotDigest)
	}
	if !strings.Contains(stdout, gotDigest) {
		t.Errorf("output does not print the release digest %q:\n%s", gotDigest, stdout)
	}
	if deployBody.Action != "deploy" || deployBody.DesiredDigest != gotDigest || deployBody.ExpectedGeneration != 7 {
		t.Errorf("deploy body = %+v, want the bare digest and generation 7", deployBody)
	}
	for _, auth := range gotAuths {
		if auth != "Bearer otk_1_secret" {
			t.Errorf("Authorization = %q, want the stored bearer token", auth)
		}
	}
	for _, want := range []string{
		"GET /api/cloud/me",
		"GET /api/runtimes/rt_1/operations",
		"POST /api/releases",
		"POST /api/runtimes/rt_1/operations",
	} {
		if !containsString(gotPaths, want) {
			t.Errorf("no %s request; saw %v", want, gotPaths)
		}
	}
}

func TestDeployCloudWithoutARuntimeFailsClearly(t *testing.T) {
	cloudHome(t)
	t.Setenv(cloud.TokenEnv, "otk_1_secret")
	root, _ := releaseWorkspace(t, "counter")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, meJSON(""))
	}))
	defer srv.Close()
	t.Setenv(cloud.URLEnv, srv.URL)

	_, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter")
	if code == 0 {
		t.Fatal("a deploy with no runtime exited 0")
	}
	if !strings.Contains(stderr, "no runtime is assigned to this organization yet") {
		t.Errorf("stderr does not explain the missing runtime:\n%s", stderr)
	}
}

func TestDeployCloudWithSeveralRuntimesRequiresTheFlag(t *testing.T) {
	cloudHome(t)
	t.Setenv(cloud.TokenEnv, "otk_1_secret")
	root, _ := releaseWorkspace(t, "counter")
	if _, stderr, code := otterIn(t, root, "release", "counter"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}

	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/releases/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/cloud/me":
			io.WriteString(w, meJSON(
				`{"id":"rt_1","lifecycle":"running","placement":"aws"},`+
					`{"id":"rt_2","lifecycle":"running","placement":"gcp"}`))
		case "GET /api/runtimes/rt_2/operations":
			io.WriteString(w, `{"generation":2}`)
		case "POST /api/releases":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"rel_1"}`)
		case "POST /api/runtimes/rt_2/operations":
			io.WriteString(w, `{"admitted":true,"operation":{"id":"op_2"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv(cloud.URLEnv, srv.URL)

	_, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter")
	if code == 0 {
		t.Fatal("an ambiguous deploy exited 0")
	}
	if !strings.Contains(stderr, "--runtime") {
		t.Errorf("stderr does not ask for --runtime:\n%s", stderr)
	}

	stdout, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter", "--runtime", "rt_2")
	if code != 0 {
		t.Fatalf("--runtime rt_2 exited %d: %s", code, stderr)
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	if !containsString(gotPaths, "POST /api/runtimes/rt_2/operations") {
		t.Errorf("the chosen runtime was not deployed to; saw %v", gotPaths)
	}
	if !strings.Contains(stdout, "op_2") {
		t.Errorf("output does not report the operation:\n%s", stdout)
	}
}

func TestDeployCloudRefusalSurfacesTheServerMessage(t *testing.T) {
	cloudHome(t)
	t.Setenv(cloud.TokenEnv, "otk_1_secret")
	root, _ := releaseWorkspace(t, "counter")
	if _, stderr, code := otterIn(t, root, "release", "counter"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/releases/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/cloud/me":
			io.WriteString(w, meJSON(`{"id":"rt_1","lifecycle":"running","placement":"aws"}`))
		case "GET /api/runtimes/rt_1/operations":
			io.WriteString(w, `{"generation":9}`)
		case "POST /api/releases":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"rel_1"}`)
		case "POST /api/runtimes/rt_1/operations":
			io.WriteString(w, `{"admitted":false,"message":"generation moved; re-read and retry"}`)
		}
	}))
	defer srv.Close()
	t.Setenv(cloud.URLEnv, srv.URL)

	_, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter")
	if code == 0 {
		t.Fatal("a refused deploy exited 0")
	}
	if !strings.Contains(stderr, "generation moved; re-read and retry") {
		t.Errorf("stderr does not carry the refusal:\n%s", stderr)
	}
}

func TestDeployCloudDryRunSendsNoMutation(t *testing.T) {
	cloudHome(t)
	t.Setenv(cloud.TokenEnv, "otk_1_secret")
	root, _ := releaseWorkspace(t, "counter")
	if _, stderr, code := otterIn(t, root, "release", "counter"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}

	var mu sync.Mutex
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mu.Lock()
			posts++
			mu.Unlock()
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/releases/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/cloud/me":
			io.WriteString(w, meJSON(`{"id":"rt_1","lifecycle":"running","placement":"aws"}`))
		case "GET /api/runtimes/rt_1/operations":
			io.WriteString(w, `{"generation":4}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv(cloud.URLEnv, srv.URL)

	stdout, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"would deploy", "nothing was sent", "release:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output does not mention %q:\n%s", want, stdout)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 0 {
		t.Errorf("--dry-run sent %d mutating request(s)", posts)
	}
}

// `otter deploy --host` still converges a host; combining it with --cloud is
// refused rather than silently choosing one.
func TestDeployCloudRefusesToMixWithHost(t *testing.T) {
	cloudHome(t)
	_, stderr, code := otterIn(t, t.TempDir(), "deploy", "--cloud", "--host", "droplet")
	if code == 0 {
		t.Fatal("--cloud --host exited 0")
	}
	if !strings.Contains(stderr, "pick one") {
		t.Errorf("stderr does not explain the conflict:\n%s", stderr)
	}
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
