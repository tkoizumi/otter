package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// initIn runs `otter init` from dir with the given arguments.
func initIn(t *testing.T, dir, version string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	original := workingDirForTest
	workingDirForTest = func() (string, error) { return dir, nil }
	defer func() { workingDirForTest = original }()

	var out, errOut bytes.Buffer
	app := New(version, &out, &errOut)
	code = app.cmdInit(args)
	return out.String(), errOut.String(), code
}

func TestInitScaffoldsAWorkspaceAndIntegration(t *testing.T) {
	dir := t.TempDir()

	stdout, stderr, code := initIn(t, dir, "0.1.0", "acme-sync")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}

	// The marker and the version stamp are what make it a workspace.
	if got := readFile(t, filepath.Join(dir, stateDirName, versionFileName)); got != "0.1.0\n" {
		t.Errorf(".otter/version = %q, want the runtime version", got)
	}

	// Paths the runtime and the developer both depend on.
	for _, rel := range []string{
		filepath.Join("acme-sync", "otter.yaml"),
		filepath.Join("acme-sync", "main.py"),
		filepath.Join("acme-sync", "logic.py"),
		filepath.Join("acme-sync", "tests", "test_logic.py"),
		workspaceEnvFileName,
		gitignoreFileName,
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("%s was not written: %v", rel, err)
		}
	}

	// The manifest must be valid, must carry the requested name, and must not
	// carry a trigger: a scaffolded cron firing under a developer is the whole
	// reason the template omits it.
	manifest := readFile(t, filepath.Join(dir, "acme-sync", "otter.yaml"))
	if !strings.Contains(manifest, "name: acme-sync") {
		t.Errorf("manifest does not name the integration:\n%s", manifest)
	}
	if strings.Contains(manifest, "\ntrigger:") {
		t.Errorf("manifest has an active trigger:\n%s", manifest)
	}
	if !strings.Contains(manifest, "ACME_SYNC_TOKEN") {
		t.Errorf("manifest does not suggest a secret name:\n%s", manifest)
	}

	// Output tells the developer what to run next.
	for _, want := range []string{"otter validate acme-sync", "otter start --detach", "otter run acme-sync"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output does not mention %q:\n%s", want, stdout)
		}
	}

	// And the scaffold's own test passes with no daemon.
	if out, err := runUnittest(t, filepath.Join(dir, "acme-sync")); err != nil {
		t.Errorf("scaffolded tests failed: %v\n%s", err, out)
	}
}

// Running init inside an existing workspace adds an integration to it and
// leaves the workspace's own files alone.
func TestInitAddsToAnExistingWorkspace(t *testing.T) {
	dir := t.TempDir()
	if _, stderr, code := initIn(t, dir, "0.1.0", "first"); code != 0 {
		t.Fatalf("first init failed: %s", stderr)
	}
	originalIgnore := readFile(t, filepath.Join(dir, gitignoreFileName))

	stdout, stderr, code := initIn(t, dir, "0.2.0", "second")
	if code != 0 {
		t.Fatalf("second init failed: %s", stderr)
	}
	if strings.Contains(stdout, "created workspace") {
		t.Errorf("the second init claimed to create the workspace:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, "second", "otter.yaml")); err != nil {
		t.Errorf("the second integration was not written: %v", err)
	}
	// The version records the workspace's origin, not the last init.
	if got := readFile(t, filepath.Join(dir, stateDirName, versionFileName)); got != "0.1.0\n" {
		t.Errorf(".otter/version = %q, want the original 0.1.0", got)
	}
	if got := readFile(t, filepath.Join(dir, gitignoreFileName)); got != originalIgnore {
		t.Errorf("an existing .gitignore was overwritten")
	}
	if !strings.Contains(stdout, "kept") {
		t.Errorf("output does not say the existing files were kept:\n%s", stdout)
	}
}

// The scaffolded .gitignore must cover env templates, not only real env files.
// `otter init` itself writes otter.env.example, and a template is exactly the
// file an operator tends to paste a live credential into. Ask git for its
// verdict rather than reading the pattern text, because the pattern text is
// what was wrong in the first place.
func TestInitGitignoreCoversEnvTemplates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	if _, stderr, code := initIn(t, dir, "0.1.0", "acme-sync"); code != 0 {
		t.Fatalf("init failed: %s", stderr)
	}
	// Real env files, templates under both the dot- and word-prefixed spellings,
	// and a control file that must stay committable.
	paths := []string{
		".env", "prod.env", ".env.local", ".env.production",
		"otter.env.example", "acme-sync/.env.example", "main.py",
	}
	for _, rel := range paths {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte("X=1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	initCmd := exec.Command("git", "init", "-q", ".")
	initCmd.Dir = dir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	// `git check-ignore` is the only authority on gitignore semantics.
	cmd := exec.Command("git", "check-ignore", "--stdin")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n"))
	out, _ := cmd.Output() // exit 1 when nothing matches
	ignored := strings.Fields(string(out))

	// Both categories: a real secret and a template that may hold one.
	for _, want := range []string{
		".env", "prod.env", ".env.local", ".env.production",
		"otter.env.example", "acme-sync/.env.example",
	} {
		if !slices.Contains(ignored, want) {
			t.Errorf("%s is not ignored; .gitignore is:\n%s", want, readFile(t, filepath.Join(dir, gitignoreFileName)))
		}
	}
	if slices.Contains(ignored, "main.py") {
		t.Errorf("main.py is ignored; .gitignore is too broad:\n%s", readFile(t, filepath.Join(dir, gitignoreFileName)))
	}
}

func TestInitRefusesAnExistingIntegration(t *testing.T) {
	dir := t.TempDir()
	if _, stderr, code := initIn(t, dir, "0.1.0", "acme-sync"); code != 0 {
		t.Fatalf("first init failed: %s", stderr)
	}
	_, stderr, code := initIn(t, dir, "0.1.0", "acme-sync")
	if code == 0 {
		t.Fatalf("a second init over the same name succeeded")
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("refusal does not explain itself:\n%s", stderr)
	}

	// --force overwrites.
	if _, stderr, code := initIn(t, dir, "0.1.0", "--force", "acme-sync"); code != 0 {
		t.Errorf("--force did not overwrite: %s", stderr)
	}
}

// Scaffolding into the middle of someone's project would bury four files among
// their own, so a non-empty directory that is not a workspace is refused.
func TestInitRefusesANonEmptyDirectoryThatIsNotAWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := initIn(t, dir, "0.1.0")
	if code == 0 {
		t.Fatal("init scaffolded into a non-empty directory")
	}
	if !strings.Contains(stderr, "not empty") {
		t.Errorf("refusal does not explain itself:\n%s", stderr)
	}
}

// Dotfiles alone are fine: a directory with a .git or an editor config is
// still an empty starting point.
func TestInitAcceptsADirectoryWithOnlyDotfiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".editorconfig"), []byte("root = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := initIn(t, dir, "0.1.0", "svc"); code != 0 {
		t.Fatalf("init refused a directory with only dotfiles: %s", stderr)
	}
}

func TestInitNameDefaultsToTheDirectory(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "invoice-sync")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := initIn(t, dir, "0.1.0"); code != 0 {
		t.Fatalf("init failed: %s", stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "invoice-sync", "otter.yaml")); err != nil {
		t.Errorf("the integration was not named after the directory: %v", err)
	}
}

// The name rule must match the manifest's, not be stricter: a name the runtime
// accepts must be scaffoldable.
func TestInitAcceptsTheNamesTheRuntimeAccepts(t *testing.T) {
	for _, name := range []string{"tshirt_company", "acme.sync", "acme-sync", "a", "svc2"} {
		dir := t.TempDir()
		if _, stderr, code := initIn(t, dir, "0.1.0", name); code != 0 {
			t.Errorf("init refused %q, which the manifest pattern allows: %s", name, stderr)
		}
	}
}

func TestInitRejectsUnusableNames(t *testing.T) {
	for _, name := range []string{"Acme", "acme sync", "acme/sync", "-acme", "_acme", "acme_"} {
		dir := t.TempDir()
		_, stderr, code := initIn(t, dir, "0.1.0", name)
		if code == 0 {
			t.Errorf("init accepted the name %q", name)
		}
		// A leading dash is read as a flag, so the parser refuses it and
		// prints usage; anything else reaches the name check. Both must be
		// non-zero and say something.
		if strings.TrimSpace(stderr) == "" {
			t.Errorf("refusal for %q said nothing", name)
		}
	}
}

// warnVersionDrift is a warning, not a gate: the workspace still starts.
func TestVersionDriftWarnsWithoutRefusing(t *testing.T) {
	dir := t.TempDir()
	if _, stderr, code := initIn(t, dir, "0.1.0", "svc"); code != 0 {
		t.Fatalf("init failed: %s", stderr)
	}

	var out bytes.Buffer
	warnVersionDrift(&out, dir, "0.2.0")
	if !strings.Contains(out.String(), "created with otter 0.1.0") {
		t.Errorf("no drift warning:\n%s", out.String())
	}

	out.Reset()
	warnVersionDrift(&out, dir, "0.1.0")
	if out.Len() != 0 {
		t.Errorf("warned about a matching version:\n%s", out.String())
	}

	// A build with no version (local make build without a tag) says nothing.
	out.Reset()
	warnVersionDrift(&out, dir, "dev")
	if out.Len() != 0 {
		t.Errorf("warned for a dev build:\n%s", out.String())
	}

	// And a workspace with no stamp says nothing either.
	out.Reset()
	warnVersionDrift(&out, t.TempDir(), "0.2.0")
	if out.Len() != 0 {
		t.Errorf("warned for an unstamped workspace:\n%s", out.String())
	}
}

// runUnittest runs the scaffolded suite the way a developer would.
func runUnittest(t *testing.T, dir string) (string, error) {
	t.Helper()
	cmd := exec.Command("python3", "-m", "unittest", "discover", "-s", "tests")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// The identity marker is instance metadata written by the runtime, so the
// scaffold must keep it out of git: a fresh clone registering its own identity
// is the intended behaviour, not a conflict to resolve.
func TestInitGitignoreCoversIdentityMarkers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	if _, stderr, code := initIn(t, dir, "0.1.0", "acme-sync"); code != 0 {
		t.Fatalf("init failed: %s", stderr)
	}
	paths := []string{".otter-id", "acme-sync/.otter-id"}
	for _, rel := range paths {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte("counter\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	initCmd := exec.Command("git", "init", "-q", ".")
	initCmd.Dir = dir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	cmd := exec.Command("git", "check-ignore", "--stdin")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n"))
	out, _ := cmd.Output()
	ignored := strings.Fields(string(out))
	for _, want := range paths {
		if !slices.Contains(ignored, want) {
			t.Errorf("%s is not ignored; .gitignore is:\n%s", want, readFile(t, filepath.Join(dir, gitignoreFileName)))
		}
	}
}
