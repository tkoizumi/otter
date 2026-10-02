package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/identity"
)

// seedWorkspaceRegistry lays out a workspace marker plus an identity registry
// and returns the workspace root. The listing must work with no daemon at all,
// which is the whole reason it reads the registry directly.
func seedWorkspaceRegistry(t *testing.T, instances ...identity.Instance) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(root, stateDirName, "data")

	ctx := context.Background()
	db, err := database.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate registry: %v", err)
	}
	now := time.Now().UTC()
	for _, inst := range instances {
		if inst.CreatedAt.IsZero() {
			inst.CreatedAt = now
		}
		if err := identity.NewStore(db.DB).CreateInstance(ctx, inst); err != nil {
			t.Fatalf("seed identity %s: %v", inst.ID, err)
		}
	}
	return root
}

// recordLiveDaemon writes the address record `resolveAPI` walks for, so a test
// can exercise the discovered-runtime path (as opposed to an explicit --api).
func recordLiveDaemon(t *testing.T, root, url string) {
	t.Helper()
	dir := filepath.Join(root, stateDirName, ServeDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ListenURLFileName), []byte(url+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The everyday question is "what is this job called, and what id do I type to
// address it?" -- so both columns are present even with the runtime stopped.
func TestJobsListsTheNameAndTheID(t *testing.T) {
	const id = "0195a7c2-8e31-7b64-9f02-6dcb482ea510"
	root := seedWorkspaceRegistry(t, identity.Instance{
		ID:            identity.MustParse(id),
		Name:          "counter",
		CanonicalPath: filepath.Join("/srv/otter/jobs", "counter"),
		Status:        identity.StatusActive,
		Generation:    1,
	})

	stdout, stderr, code := otterIn(t, root, "jobs")
	if code != 0 {
		t.Fatalf("jobs exited %d: %s", code, stderr)
	}
	for _, want := range []string{"NAME", "ID", "counter", id} {
		if !strings.Contains(stdout, want) {
			t.Errorf("jobs output is missing %q:\n%s", want, stdout)
		}
	}
}

// A retired identity is kept so its id is never reused, but it is not a job you
// can address any more: it stays hidden until --all, which is also what shows
// the deleted tombstones.
func TestJobsHidesRetiredIdentitiesUnlessAll(t *testing.T) {
	now := time.Now().UTC()
	root := seedWorkspaceRegistry(t,
		identity.Instance{
			ID:            identity.MustParse("active-1"),
			Name:          "counter",
			CanonicalPath: "/srv/otter/jobs/counter",
			Status:        identity.StatusActive,
			Generation:    1,
		},
		identity.Instance{
			ID:               identity.MustParse("retired-1"),
			Name:             "old-counter",
			CanonicalPath:    "/srv/otter/jobs/old-counter",
			Status:           identity.StatusRetired,
			Generation:       2,
			RetiredAt:        &now,
			RetirementReason: "directory removed",
		},
	)

	stdout, stderr, code := otterIn(t, root, "jobs")
	if code != 0 {
		t.Fatalf("jobs exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "counter") {
		t.Errorf("the active job is missing:\n%s", stdout)
	}
	if strings.Contains(stdout, "retired-1") || strings.Contains(stdout, "old-counter") {
		t.Errorf("a retired identity was listed without --all:\n%s", stdout)
	}

	stdout, stderr, code = otterIn(t, root, "jobs", "--all")
	if code != 0 {
		t.Fatalf("jobs --all exited %d: %s", code, stderr)
	}
	for _, want := range []string{"old-counter", "retired-1", "retired"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("jobs --all is missing %q:\n%s", want, stdout)
		}
	}
}

// --json is the machine view, so it carries the same two fields the table
// prints.
func TestJobsJSONCarriesTheNameAndTheID(t *testing.T) {
	const id = "0195a7c2-8e31-7b64-9f02-6dcb482ea510"
	root := seedWorkspaceRegistry(t, identity.Instance{
		ID:            identity.MustParse(id),
		Name:          "counter",
		CanonicalPath: "/srv/otter/jobs/counter",
		Status:        identity.StatusActive,
		Generation:    1,
	})

	stdout, stderr, code := otterIn(t, root, "--json", "jobs")
	if code != 0 {
		t.Fatalf("jobs --json exited %d: %s", code, stderr)
	}
	for _, want := range []string{`"id": "` + id + `"`, `"name": "counter"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("jobs --json is missing %s:\n%s", want, stdout)
		}
	}
}

// The listing is a local command now: --data alone is enough, with no
// workspace marker in sight, which is how it runs on a deployment host.
func TestJobsReadsAnExplicitDataDirectory(t *testing.T) {
	root := seedWorkspaceRegistry(t, identity.Instance{
		ID:            identity.MustParse("active-1"),
		Name:          "counter",
		CanonicalPath: "/srv/otter/jobs/counter",
		Status:        identity.StatusActive,
		Generation:    1,
	})
	dataDir := filepath.Join(root, stateDirName, "data")

	elsewhere := t.TempDir() // no .otter, no .git, no go.mod
	stdout, stderr, code := otterIn(t, elsewhere, "jobs", "--data", dataDir)
	if code != 0 {
		t.Fatalf("jobs --data exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "counter") || !strings.Contains(stdout, "active-1") {
		t.Errorf("jobs --data did not read the named registry:\n%s", stdout)
	}
}

// A runtime discovered in the workspace is this registry's runtime: the two
// sources are merged. The daemon decides validity, and a registry identity the
// daemon no longer lists (a retired one) still appears with --all.
func TestJobsEnrichesTheRegistryFromTheDaemon(t *testing.T) {
	now := time.Now().UTC()
	root := seedWorkspaceRegistry(t,
		identity.Instance{
			ID:            identity.MustParse("broken-1"),
			Name:          "broken",
			CanonicalPath: "/srv/otter/jobs/broken",
			Status:        identity.StatusActive,
			Generation:    1,
		},
		identity.Instance{
			ID:            identity.MustParse("ghost-1"),
			Name:          "ghost",
			CanonicalPath: "/srv/otter/jobs/ghost",
			Status:        identity.StatusRetired,
			Generation:    2,
			RetiredAt:     &now,
		},
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jobs":[`+
			`{"id":"broken-1","name":"broken","path":"/srv/otter/jobs/broken",`+
			`"status":"active","valid":false,"error":"entrypoint main.py does not exist"},`+
			`{"id":"live-1","name":"from-daemon","path":"/srv/otter/jobs/live",`+
			`"status":"active","valid":true}]}`)
	}))
	defer srv.Close()
	recordLiveDaemon(t, root, srv.URL)

	// The invalid job is hidden by default, exactly as before.
	stdout, stderr, code := otterIn(t, root, "jobs")
	if code != 0 {
		t.Fatalf("jobs exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "from-daemon") {
		t.Errorf("a live-only job is missing:\n%s", stdout)
	}
	if strings.Contains(stdout, "broken-1") {
		t.Errorf("an invalid job was listed without --all:\n%s", stdout)
	}

	// --all surfaces the invalid job with the daemon's error, and the retired
	// identity the daemon never mentioned.
	stdout, stderr, code = otterIn(t, root, "jobs", "--all")
	if code != 0 {
		t.Fatalf("jobs --all exited %d: %s", code, stderr)
	}
	for _, want := range []string{"broken-1", "entrypoint main.py does not exist", "ghost-1", "retired"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("jobs --all is missing %q:\n%s", want, stdout)
		}
	}
}

// A daemon the operator named with --api may own a different data directory (a
// tunnel to a deployed host is the common case), so this workspace's registry
// must not be mixed into its listing. An explicit --data still opts back in.
func TestJobsNamedDaemonDoesNotReadTheLocalRegistry(t *testing.T) {
	root := seedWorkspaceRegistry(t, identity.Instance{
		ID:            identity.MustParse("local-only"),
		Name:          "local-only",
		CanonicalPath: "/local/jobs/local-only",
		Status:        identity.StatusActive,
		Generation:    1,
	})
	dataDir := filepath.Join(root, stateDirName, "data")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jobs":[{"id":"remote-1","name":"remote-job",`+
			`"path":"/srv/jobs/remote","status":"active","valid":true}]}`)
	}))
	defer srv.Close()

	stdout, stderr, code := otterIn(t, root, "--api", srv.URL, "jobs")
	if code != 0 {
		t.Fatalf("jobs --api exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "remote-job") {
		t.Errorf("the named daemon's job is missing:\n%s", stdout)
	}
	if strings.Contains(stdout, "local-only") {
		t.Errorf("the local registry leaked into a named daemon's listing:\n%s", stdout)
	}

	// --data names the registry on purpose, so now both are listed.
	stdout, stderr, code = otterIn(t, root, "--api", srv.URL, "jobs", "--data", dataDir)
	if code != 0 {
		t.Fatalf("jobs --api --data exited %d: %s", code, stderr)
	}
	for _, want := range []string{"remote-job", "local-only"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("jobs --api --data is missing %q:\n%s", want, stdout)
		}
	}
}
