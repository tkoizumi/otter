package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProjectJob creates a minimal valid job at dir with the
// given python.path declarations.
func writeProjectJob(t *testing.T, dir, name string, pythonPath ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "version: 1\nname: " + name + "\nentrypoint: main.py\n"
	if len(pythonPath) > 0 {
		manifest += "python:\n  path:\n"
		for _, p := range pythonPath {
			manifest += "    - " + p + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A workspace is free to group its jobs, and the shared library's
// placement has to follow the declaration rather than a hardcoded depth. The
// bug this pins: deploy only ever knew the runtime repository's own layout, so
// shopify_jobs/<name> with python.path ../lib/python could not be
// deployed at all.
func TestTreePlacementFollowsTheDeclaration(t *testing.T) {
	project := t.TempDir()

	grouped := Job{
		Name: "customer_sync",
		Dir:  filepath.Join(project, "shopify_jobs", "customer_sync"),
	}
	got, err := TreePlacement(grouped, filepath.Join(project, "shopify_jobs", "lib", "python"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "jobs/lib/python" {
		t.Errorf("grouped placement = %q, want jobs/lib/python", got)
	}

	// The runtime repository's own layout: the library sits beside the
	// jobs directory, two levels up from the job.
	classic := Job{
		Name: "one",
		Dir:  filepath.Join(project, "jobs", "one"),
	}
	got, err = TreePlacement(classic, filepath.Join(project, "lib", "python"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "lib/python" {
		t.Errorf("classic placement = %q, want lib/python", got)
	}
}

// A tree that would escape the install root has no representable placement and
// must be refused rather than silently written somewhere unexpected.
func TestTreePlacementRefusesEscapingTrees(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "a", "b")
	integ := Job{Name: "one", Dir: filepath.Join(project, "jobs", "one")}
	outside := filepath.Join(base, "shared")

	if _, err := TreePlacement(integ, outside); err == nil {
		t.Error("a shared tree above the install root was accepted")
	}
}

// Stage must produce the geometry the host releases from: the job under
// jobs/, and each declared tree at the depth its manifest names.
func TestStagePreservesDeclaredGeometry(t *testing.T) {
	project := t.TempDir()
	integDir := filepath.Join(project, "shopify_jobs", "customer_sync")
	libDir := filepath.Join(project, "shopify_jobs", "lib", "python")
	writeProjectJob(t, integDir, "customer_sync", "../lib/python")
	if err := os.MkdirAll(filepath.Join(libDir, "otter_connectors"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "otter_connectors", "shopify.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	builder := NewLocalBuilder(nil, nil)
	cfg := Config{
		ProjectRoot: project,
		Jobs: []Job{{
			Name:  "customer_sync",
			Label: "customer_sync",
			Dir:   integDir,
			Trees: []string{libDir},
		}},
	}
	if err := builder.Stage(cfg, outDir); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{
		"jobs/customer_sync/otter.yaml",
		"jobs/customer_sync/main.py",
		// The declaration is ../lib/python from jobs/customer_sync,
		// so this is where it has to land for the manifest to resolve.
		"jobs/lib/python/otter_connectors/shopify.py",
	} {
		if _, err := os.Stat(filepath.Join(outDir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("staged tree is missing %s: %v", rel, err)
		}
	}
}

// Two different libraries claiming one path is a collision the host could not
// resolve, so staging refuses it while both sources are still known.
func TestStageRefusesConflictingTrees(t *testing.T) {
	project := t.TempDir()
	first := filepath.Join(project, "a", "one")
	second := filepath.Join(project, "b", "two")
	writeProjectJob(t, first, "one", "../../shared")
	writeProjectJob(t, second, "two", "../../shared")
	sharedA := filepath.Join(project, "a", "shared")
	sharedB := filepath.Join(project, "b", "shared")
	for _, dir := range []string{sharedA, sharedB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := Config{
		ProjectRoot: project,
		Jobs: []Job{
			{Name: "one", Dir: first, Trees: []string{sharedA}},
			{Name: "two", Dir: second, Trees: []string{sharedB}},
		},
	}
	err := NewLocalBuilder(nil, nil).Stage(cfg, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "would land at") {
		t.Errorf("conflicting trees were accepted: %v", err)
	}
}

// Discovery walks the project, so a nested grouping directory deploys exactly
// what `otter start` would serve.
func TestLoadConfigDiscoversNestedJobs(t *testing.T) {
	project := t.TempDir()
	group := filepath.Join(project, "shopify_jobs")
	customer := filepath.Join(group, "customer_sync")
	product := filepath.Join(group, "product_sync")
	writeProjectJob(t, customer, "customer_sync", "../lib/python")
	writeProjectJob(t, product, "product_sync", "../lib/python")
	for _, dir := range []string{customer, product, filepath.Join(group, "lib", "python")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := LoadConfig(project, &Flags{set: map[string]bool{}}, HostDeploy{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Jobs) != 2 {
		t.Fatalf("discovered %d jobs, want 2", len(cfg.Jobs))
	}
	for _, integ := range cfg.Jobs {
		if integ.Name == "customer_sync" && integ.Label != "customer_sync" {
			t.Errorf("label = %q, want the manifest name", integ.Label)
		}
		if len(integ.Trees) != 1 || !strings.HasSuffix(integ.Trees[0], filepath.Join("lib", "python")) {
			t.Errorf("%s trees = %v, want the declared lib/python", integ.Name, integ.Trees)
		}
	}
	if cfg.Limited {
		t.Error("an unfiltered deploy reported itself as limited")
	}

	// A limited deploy may name either the directory or the manifest label.
	filtered, err := LoadConfig(project, &Flags{Job: "product_sync", set: map[string]bool{}}, HostDeploy{})
	if err != nil {
		t.Fatal(err)
	}
	if !filtered.Limited || len(filtered.Jobs) != 1 || filtered.Jobs[0].Name != "product_sync" {
		t.Errorf("--job did not select exactly product_sync: %+v", filtered.Jobs)
	}
}

// A broken manifest blocks the job it belongs to, but must not block
// deploying a different, healthy one.
func TestBrokenManifestDoesNotBlockALimitedDeploy(t *testing.T) {
	project := t.TempDir()
	good := filepath.Join(project, "group", "good")
	bad := filepath.Join(project, "group", "bad")
	writeProjectJob(t, good, "good")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	// No entrypoint file: the manifest parses but does not validate.
	if err := os.WriteFile(filepath.Join(bad, "otter.yaml"),
		[]byte("version: 1\nname: bad\nentrypoint: missing.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(project, &Flags{set: map[string]bool{}}, HostDeploy{}); err == nil {
		t.Error("an unfiltered deploy accepted a broken manifest")
	}
	cfg, err := LoadConfig(project, &Flags{Job: "good", set: map[string]bool{}}, HostDeploy{})
	if err != nil {
		t.Fatalf("a healthy job could not be deployed: %v", err)
	}
	if len(cfg.Jobs) != 1 || cfg.Jobs[0].Name != "good" {
		t.Errorf("selected %+v, want just good", cfg.Jobs)
	}
}

// A duplicate job label must be caught locally: the host registers
// jobs by name, so shipping both would leave one unaddressable.
func TestLoadConfigRejectsDuplicateLabels(t *testing.T) {
	project := t.TempDir()
	writeProjectJob(t, filepath.Join(project, "envs", "prod"), "sync")
	writeProjectJob(t, filepath.Join(project, "envs", "stage"), "sync")

	_, err := LoadConfig(project, &Flags{set: map[string]bool{}}, HostDeploy{})
	if err == nil || !strings.Contains(err.Error(), "duplicate job name") {
		t.Errorf("duplicate labels were accepted: %v", err)
	}
}
