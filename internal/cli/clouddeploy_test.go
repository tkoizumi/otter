package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tkoizumi/otter/internal/cloud"
	"github.com/tkoizumi/otter/internal/release"
)

// cloudFake is a control plane that records what a promotion did, so a test can
// assert the ORDER of the upload and the admission rather than only the result.
// The order is part of the contract: the control plane refuses to point a
// runtime at an artifact the store cannot serve.
type cloudFake struct {
	mu       sync.Mutex
	requests []string
	uploads  []string
	admitted cloud.DeployRequest
	// held is the set of digests Cloud already has. A probe for anything else
	// is a 404, which is how the control plane says "never uploaded".
	held map[string]bool
	// probeStatus, when non-zero, answers every presence probe with that
	// status instead of consulting held. It is how a deployment that does not
	// serve the lookup at all (405) is exercised.
	probeStatus int
}

func newCloudFake() *cloudFake {
	return &cloudFake{held: map[string]bool{}}
}

func (f *cloudFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()

		// The optional presence probe is a read and must be told apart from the
		// upload below, which is a mutation.
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/releases/") {
			if f.probeStatus != 0 {
				w.WriteHeader(f.probeStatus)
				return
			}
			digest := strings.TrimPrefix(r.URL.Path, "/api/releases/")
			f.mu.Lock()
			held := f.held[digest]
			f.mu.Unlock()
			if !held {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"message":"no such release"}`)
				return
			}
			io.WriteString(w, `{"digest":"`+digest+`"}`)
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
			f.mu.Lock()
			f.uploads = append(f.uploads, r.Header.Get("x-otter-release-digest"))
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"rel_1"}`)
		case "POST /api/runtimes/rt_1/operations":
			var body cloud.DeployRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode deploy body: %v", err)
			}
			f.mu.Lock()
			f.admitted = body
			f.mu.Unlock()
			io.WriteString(w, `{"admitted":true,"operation":{"id":"op_1","status":"queued"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func (f *cloudFake) uploaded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.uploads...)
}

func (f *cloudFake) requestsList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *cloudFake) indexOf(want string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, got := range f.requests {
		if got == want {
			return i
		}
	}
	return -1
}

func (f *cloudFake) containsPrefix(prefix string) bool {
	for _, got := range f.requestsList() {
		if strings.HasPrefix(got, prefix) {
			return true
		}
	}
	return false
}

// mutations counts the requests that change something. --dry-run must make none
// of them.
func (f *cloudFake) mutations() int {
	count := 0
	for _, got := range f.requestsList() {
		if !strings.HasPrefix(got, http.MethodGet+" ") {
			count++
		}
	}
	return count
}

func (f *cloudFake) deployedDigest() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.admitted.DesiredDigest
}

// twoReleases lays out a workspace with two staged releases of one job and
// returns the workspace root and the two digests in creation order (older,
// newer). The second release has different content on purpose: releases are
// content-addressed, so changing the source is what makes a second one.
//
// Two releases, not one, is what makes "latest" testable: a selection that
// picked the first directory read, or the oldest, passes a one-release fixture.
func twoReleases(t *testing.T) (root, dir, older, newer string) {
	t.Helper()
	root, dir = releaseWorkspace(t, "counter")
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}

	if _, stderr, code := otterIn(t, root, "release", "counter"); code != 0 {
		t.Fatalf("first release exited %d: %s", code, stderr)
	}
	older = activeDigest(t, manager, dir)

	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('two')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := otterIn(t, root, "release", "counter"); code != 0 {
		t.Fatalf("second release exited %d: %s", code, stderr)
	}
	newer = activeDigest(t, manager, dir)
	if older == newer {
		t.Fatalf("the two releases carry the same digest %s", older)
	}
	return root, dir, older, newer
}

func activeDigest(t *testing.T, manager release.Manager, dir string) string {
	t.Helper()
	meta, ok, err := manager.Active(idFor(t, dir))
	if err != nil || !ok {
		t.Fatalf("no active release for %s (ok=%v err=%v)", dir, ok, err)
	}
	return meta.Digest
}

// The default names no release, so it promotes the job's latest local release.
func TestDeployCloudPromotesTheLatestLocalRelease(t *testing.T) {
	cloudHome(t)
	t.Setenv(cloud.TokenEnv, "otk_1_secret")
	root, dir, older, newer := twoReleases(t)

	fake := newCloudFake()
	srv := fake.server(t)
	defer srv.Close()
	t.Setenv(cloud.URLEnv, srv.URL)

	stdout, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	uploads := fake.uploaded()
	if len(uploads) != 1 || uploads[0] != newer {
		t.Fatalf("uploaded %v, want exactly the newer release %s (older is %s)", uploads, newer, older)
	}
	if got := fake.deployedDigest(); got != newer {
		t.Errorf("admitted %q, want the newer release %s", got, newer)
	}
	// The job id must travel with the digest. A control plane keeping per-job
	// desired state cannot place a release it cannot attribute, and this is the
	// only place the operator's `--job` choice reaches it.
	if id := idFor(t, dir); fake.admitted.JobID != id {
		t.Errorf("admitted job_id = %q, want %q", fake.admitted.JobID, id)
	}
	if !strings.Contains(stdout, newer) {
		t.Errorf("output does not name the promoted release:\n%s", stdout)
	}
}

// --release promotes exactly the digest it names, in either spelling the rest
// of the codebase accepts.
func TestDeployCloudPromotesTheNamedReleaseInBothSpellings(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  func(older string) string
	}{
		{"bare hex", func(older string) string { return older }},
		{"sha256 prefix", func(older string) string { return "sha256:" + older }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloudHome(t)
			t.Setenv(cloud.TokenEnv, "otk_1_secret")
			root, _, older, newer := twoReleases(t)

			fake := newCloudFake()
			srv := fake.server(t)
			defer srv.Close()
			t.Setenv(cloud.URLEnv, srv.URL)

			if _, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter", "--release", tc.ref(older)); code != 0 {
				t.Fatalf("exit %d, stderr:\n%s", code, stderr)
			}
			uploads := fake.uploaded()
			if len(uploads) != 1 || uploads[0] != older {
				t.Fatalf("uploaded %v, want the named older release %s (newer is %s)", uploads, older, newer)
			}
			if got := fake.deployedDigest(); got != older {
				t.Errorf("admitted %q, want the named release %s", got, older)
			}
		})
	}
}

// --release latest is the same request as omitting the flag.
func TestDeployCloudReleaseLatestMatchesTheDefault(t *testing.T) {
	cloudHome(t)
	t.Setenv(cloud.TokenEnv, "otk_1_secret")
	root, _, _, newer := twoReleases(t)

	fake := newCloudFake()
	srv := fake.server(t)
	defer srv.Close()
	t.Setenv(cloud.URLEnv, srv.URL)

	if _, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter", "--release", "latest"); code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	uploads := fake.uploaded()
	if len(uploads) != 1 || uploads[0] != newer {
		t.Fatalf("uploaded %v, want the latest release %s", uploads, newer)
	}
}

// --build keeps the original path: the job is packaged from the workspace even
// though no local release exists yet. Nothing else can deploy this workspace,
// which is what makes the flag the escape hatch.
func TestDeployCloudBuildKeepsTheWorkspacePath(t *testing.T) {
	cloudHome(t)
	t.Setenv(cloud.TokenEnv, "otk_1_secret")
	root, dir := releaseWorkspace(t, "counter")

	fake := newCloudFake()
	srv := fake.server(t)
	defer srv.Close()
	t.Setenv(cloud.URLEnv, srv.URL)

	if _, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter", "--build"); code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	uploads := fake.uploaded()
	if len(uploads) != 1 {
		t.Fatalf("uploads = %v, want exactly one", uploads)
	}
	// The digest is the one staging just computed over the workspace, so the
	// release now in the local store carries it.
	manager := release.Manager{DataDir: filepath.Join(root, stateDirName, "data")}
	releases, err := manager.List(idFor(t, dir))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(releases) != 1 || releases[0].Digest != uploads[0] {
		t.Fatalf("store holds %v, uploaded %s: --build did not package the workspace", releases, uploads[0])
	}
	// --build never probes: it always sends the package, exactly as before.
	if fake.containsPrefix("GET /api/releases/") {
		t.Errorf("--build made a presence probe; requests: %v", fake.requestsList())
	}
}

// Without a local release and without --build there is nothing to promote, and
// the error names the command that creates one. Nothing is uploaded or
// admitted.
func TestDeployCloudWithoutAReleaseNamesTheFix(t *testing.T) {
	cloudHome(t)
	t.Setenv(cloud.TokenEnv, "otk_1_secret")
	root, _ := releaseWorkspace(t, "counter")

	fake := newCloudFake()
	srv := fake.server(t)
	defer srv.Close()
	t.Setenv(cloud.URLEnv, srv.URL)

	_, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter")
	if code != 2 {
		t.Fatalf("exit %d, want 2 (usage): %s", code, stderr)
	}
	for _, want := range []string{"no release for counter", "run otter release counter first"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, stderr)
		}
	}
	if got := fake.uploaded(); len(got) != 0 {
		t.Errorf("uploaded %v, want nothing", got)
	}
	if fake.containsPrefix("POST ") {
		t.Errorf("a release-less deploy sent a mutating request: %v", fake.requestsList())
	}
}

// A release Cloud does not hold is uploaded BEFORE the operation is admitted.
func TestDeployCloudUploadsBeforeAdmittingTheOperation(t *testing.T) {
	cloudHome(t)
	t.Setenv(cloud.TokenEnv, "otk_1_secret")
	root, _, _, newer := twoReleases(t)

	fake := newCloudFake()
	srv := fake.server(t)
	defer srv.Close()
	t.Setenv(cloud.URLEnv, srv.URL)

	if _, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter"); code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	uploadAt := fake.indexOf("POST /api/releases")
	admitAt := fake.indexOf("POST /api/runtimes/rt_1/operations")
	if uploadAt < 0 || admitAt < 0 {
		t.Fatalf("requests = %v, want an upload and an admission", fake.requestsList())
	}
	if uploadAt > admitAt {
		t.Errorf("upload at %d came after the admission at %d: %v", uploadAt, admitAt, fake.requestsList())
	}
	uploads := fake.uploaded()
	if len(uploads) != 1 || uploads[0] != newer {
		t.Errorf("uploaded %v, want the promoted release %s", uploads, newer)
	}
}

// A release Cloud already holds is not uploaded again: the digest is the
// content's address, so the copy already there IS the release.
func TestDeployCloudDoesNotReuploadAReleaseCloudAlreadyHas(t *testing.T) {
	cloudHome(t)
	t.Setenv(cloud.TokenEnv, "otk_1_secret")
	root, _, _, newer := twoReleases(t)

	fake := newCloudFake()
	fake.held[newer] = true
	srv := fake.server(t)
	defer srv.Close()
	t.Setenv(cloud.URLEnv, srv.URL)

	if _, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter"); code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if got := fake.uploaded(); len(got) != 0 {
		t.Errorf("uploaded %v, want nothing: Cloud already held the release", got)
	}
	if got := fake.deployedDigest(); got != newer {
		t.Errorf("admitted %q, want %s", got, newer)
	}
}

// A deployment that does not serve the presence probe is not a failure. The
// upload is content-addressed and idempotent, so "cannot answer" is read as
// "not known to be present" and the promotion uploads and proceeds.
func TestDeployCloudUploadsWhenTheProbeIsUnsupported(t *testing.T) {
	cloudHome(t)
	t.Setenv(cloud.TokenEnv, "otk_1_secret")
	root, _, _, newer := twoReleases(t)

	fake := newCloudFake()
	fake.probeStatus = http.StatusMethodNotAllowed
	srv := fake.server(t)
	defer srv.Close()
	t.Setenv(cloud.URLEnv, srv.URL)

	if _, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter"); code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	uploads := fake.uploaded()
	if len(uploads) != 1 || uploads[0] != newer {
		t.Errorf("uploaded %v, want the promoted release %s", uploads, newer)
	}
}

// --dry-run names the release and whether it would need uploading, and sends
// no mutating request at all.
func TestDeployCloudDryRunNamesTheReleaseAndTheUpload(t *testing.T) {
	cloudHome(t)
	t.Setenv(cloud.TokenEnv, "otk_1_secret")
	root, _, _, newer := twoReleases(t)

	fake := newCloudFake()
	srv := fake.server(t)
	defer srv.Close()
	t.Setenv(cloud.URLEnv, srv.URL)

	stdout, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"would deploy", "job:", "release:", newer, "upload:", "needed", "nothing was sent"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry run does not mention %q:\n%s", want, stdout)
		}
	}
	if got := fake.mutations(); got != 0 {
		t.Errorf("--dry-run sent %d mutating request(s): %v", got, fake.requestsList())
	}

	// The same command against a Cloud that already holds the release says so,
	// and still sends nothing.
	fake.mu.Lock()
	fake.held[newer] = true
	fake.mu.Unlock()
	stdout, stderr, code = otterIn(t, root, "deploy", "--cloud", "--job", "counter", "--dry-run")
	if code != 0 {
		t.Fatalf("second dry run exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "already in Cloud") {
		t.Errorf("dry run does not report an already-present release:\n%s", stdout)
	}
	if got := fake.mutations(); got != 0 {
		t.Errorf("--dry-run sent %d mutating request(s): %v", got, fake.requestsList())
	}
}

// A malformed --release is a usage error, and the combination with --build has
// no meaning; both are refused before anything is sent.
func TestDeployCloudRefusesAMalformedOrContradictoryRelease(t *testing.T) {
	cloudHome(t)
	t.Setenv(cloud.TokenEnv, "otk_1_secret")
	root, _, _, _ := twoReleases(t)

	// --build names the workspace as the source and --release names a stored
	// release; there is no third thing to guess between.
	_, stderr, code := otterIn(t, root, "deploy", "--cloud", "--job", "counter", "--build", "--release", "latest")
	if code != 2 {
		t.Fatalf("--build --release exited %d, want 2: %s", code, stderr)
	}
	if !strings.Contains(stderr, "cannot be combined with --release") {
		t.Errorf("stderr does not explain the contradiction:\n%s", stderr)
	}

	fake := newCloudFake()
	srv := fake.server(t)
	defer srv.Close()
	t.Setenv(cloud.URLEnv, srv.URL)

	_, stderr, code = otterIn(t, root, "deploy", "--cloud", "--job", "counter", "--release", "not-a-digest")
	if code != 2 {
		t.Fatalf("a malformed digest exited %d, want 2: %s", code, stderr)
	}
	if !strings.Contains(stderr, "sha256:<hex>") || !strings.Contains(stderr, "latest") {
		t.Errorf("stderr does not explain the accepted spellings:\n%s", stderr)
	}
	if fake.containsPrefix("POST ") {
		t.Errorf("a malformed digest still sent a mutating request: %v", fake.requestsList())
	}
}
