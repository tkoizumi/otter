package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/release"
)

// A managed manifest with a pinned interpreter: the shape the Castor jobs have.
const managedManifest = `
version: 1
name: managed
entrypoint: main.py
timeout: 30
python:
  mode: managed
trigger:
  cron: "*/5 * * * *"
`

// The three files managed mode requires, with a version that cannot be
// installed. Pinning an impossible version is what makes the test deterministic:
// preparation fails whether or not this machine has `uv` and whether or not it
// has a network, so the assertion is about the ACTIVATION refusing rather than
// about the local toolchain.
func writeManagedFiles(t *testing.T, dir, pin string) {
	t.Helper()
	files := map[string]string{
		".python-version": pin + "\n",
		"pyproject.toml": `[project]
name = "managed"
version = "0.1.0"
requires-python = ">=3.10"
dependencies = []
`,
		"uv.lock": `version = 1
requires-python = ">=3.10"

[[package]]
name = "managed"
version = "0.1.0"
source = { virtual = "." }
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// stagedRelease returns one staged release's (job, digest), the way the
// activation API addresses it.
func stagedRelease(t *testing.T, d *Daemon) (string, string) {
	t.Helper()
	m := release.Manager{DataDir: d.cfg.DataDir}
	relRoot, err := m.Root()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(relRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == release.ActiveDirName {
			continue
		}
		list, err := m.List(e.Name())
		if err != nil || len(list) == 0 {
			continue
		}
		return e.Name(), list[0].Digest
	}
	t.Fatal("the fixture staged no release")
	return "", ""
}

// A pooled tenant has no operator to run `otter prepare`, and preparation is a
// client-side step in the release path -- so a managed release used to activate
// with no environment behind it. The job then registered, imported its schedule,
// advanced `next_run_at` and never ran: a healthy-looking job doing nothing, with
// the refusal only ever reaching a log line.
//
// Activation now prepares, and a release whose environment cannot be prepared
// here must FAIL to activate rather than activate unrunnable.
func TestAManagedReleaseIsNotActivatedWithoutAnEnvironment(t *testing.T) {
	root := t.TempDir()
	dir := writeJob(t, root, "managed", managedManifest, "print('never runs')")
	writeManagedFiles(t, dir, "3.99.99")

	d := newGatedDaemon(t, root)
	ctx := context.Background()
	job, digest := stagedRelease(t, d)

	// The fixture activates what it stages; put the tenant back into the state a
	// pulled deploy actually leaves between "the bytes arrived" and "it is live".
	m := release.Manager{DataDir: d.cfg.DataDir}
	link, err := m.ActivePath(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatalf("unset the active release: %v", err)
	}

	_, err = d.ActivateRelease(ctx, digest)
	if err == nil {
		t.Fatal("a managed release whose environment cannot be prepared must not activate")
	}
	if !strings.Contains(err.Error(), "managed environment") {
		t.Errorf("error = %v, want it to name the managed environment", err)
	}

	// The failure must not leave the release activated. Activating first and
	// reporting the failure afterwards would be worse than refusing: the job
	// would be live and unrunnable.
	if _, ok, err := m.Active(job); err != nil || ok {
		t.Errorf("the release was activated anyway: ok=%v err=%v", ok, err)
	}
}

// An external release has no environment to prepare, and activation must not
// start depending on the Python toolchain for it.
func TestAnExternalReleaseStillActivates(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newGatedDaemon(t, root)
	job, digest := stagedRelease(t, d)

	m := release.Manager{DataDir: d.cfg.DataDir}
	link, err := m.ActivePath(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatalf("unset the active release: %v", err)
	}

	if _, err := d.ActivateRelease(context.Background(), digest); err != nil {
		t.Fatalf("an external release must activate without preparation: %v", err)
	}
	if _, ok, err := m.Active(job); err != nil || !ok {
		t.Errorf("the release was not activated: ok=%v err=%v", ok, err)
	}
}
