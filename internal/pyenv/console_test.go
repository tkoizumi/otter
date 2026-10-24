package pyenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConsoleScript creates dir/bin/<name> with the given body and mode, making
// the bin directory first.
func writeConsoleScript(t *testing.T, bin, name, body string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", bin, err)
	}
	path := filepath.Join(bin, name)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func readConsoleScript(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// A managed console script's shebang names an absolute interpreter inside an
// `environments/<digest>/bin` layout. After the data directory moves that path
// is gone, so the shebang is rewritten to the environment's new interpreter.
func TestRelocateConsoleScriptsRewritesManagedShebangs(t *testing.T) {
	envDir := filepath.Join(t.TempDir(), "data", "environments", "digest")
	bin := filepath.Join(envDir, "bin")
	oldInterpreter := filepath.Join(bin, "python")
	newInterpreter := filepath.Join(t.TempDir(), "moved", "environments", "digest", "bin", "python")

	probe := writeConsoleScript(t, bin, "probe",
		"#!"+oldInterpreter+"\nprint(\"console-ok\")\n", 0o700)
	versioned := writeConsoleScript(t, bin, "versioned",
		"#!"+filepath.Join(filepath.Dir(oldInterpreter), "python3.13")+" -u\nprint(1)\n", 0o700)
	plain := writeConsoleScript(t, bin, "plain", "#!/bin/sh\necho hi\n", 0o700)
	hostpy := writeConsoleScript(t, bin, "hostpy", "#!/usr/bin/python3\nprint(2)\n", 0o700)
	// The environment's own interpreter symlink is not a console script.
	if err := os.Symlink("/bin/sh", filepath.Join(bin, "python")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := relocateConsoleScripts(envDir, newInterpreter); err != nil {
		t.Fatalf("relocateConsoleScripts: %v", err)
	}

	if got, want := readConsoleScript(t, probe), "#!"+newInterpreter+"\nprint(\"console-ok\")\n"; got != want {
		t.Errorf("probe = %q, want %q", got, want)
	}
	if got, want := readConsoleScript(t, versioned), "#!"+newInterpreter+" -u\nprint(1)\n"; got != want {
		t.Errorf("versioned = %q, want %q (arguments are preserved)", got, want)
	}
	if got := readConsoleScript(t, plain); got != "#!/bin/sh\necho hi\n" {
		t.Errorf("a /bin/sh script was rewritten: %q", got)
	}
	if got := readConsoleScript(t, hostpy); got != "#!/usr/bin/python3\nprint(2)\n" {
		t.Errorf("a host python shebang was rewritten: %q", got)
	}
	if info, err := os.Lstat(filepath.Join(bin, "python")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the interpreter symlink was replaced: mode=%v err=%v", info.Mode(), err)
	}
	for _, path := range []string{probe, versioned} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("%s mode = %o, want 700", filepath.Base(path), got)
		}
	}
}

// The rewrite runs from GetReady on every run, so an unchanged script must not
// be written: a rewrite would churn the environment's files on every run.
func TestRelocateConsoleScriptsIsIdempotent(t *testing.T) {
	envDir := filepath.Join(t.TempDir(), "data", "environments", "digest")
	bin := filepath.Join(envDir, "bin")
	oldInterpreter := filepath.Join(bin, "python")
	interpreter := filepath.Join(t.TempDir(), "moved", "environments", "digest", "bin", "python")
	probe := writeConsoleScript(t, bin, "probe", "#!"+oldInterpreter+"\nprint(1)\n", 0o700)

	if err := relocateConsoleScripts(envDir, interpreter); err != nil {
		t.Fatalf("first relocate: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(probe, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := relocateConsoleScripts(envDir, interpreter); err != nil {
		t.Fatalf("second relocate: %v", err)
	}
	info, err := os.Stat(probe)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().Equal(old) {
		t.Errorf("the second GetReady rewrote an already-correct script (mtime %v, want %v)", info.ModTime(), old)
	}
	if got, want := strings.TrimRight(readConsoleScript(t, probe), "\n"), "#!"+interpreter+"\nprint(1)"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

// An environment with no bin directory (a broken or foreign one) has no console
// scripts, which is not an error: GetReady's own marker check is the gate.
func TestRelocateConsoleScriptsToleratesAMissingBinDirectory(t *testing.T) {
	if err := relocateConsoleScripts(t.TempDir(), "/x/bin/python"); err != nil {
		t.Fatalf("a missing bin directory was an error: %v", err)
	}
}

// The rewrite must not touch anything outside the managed layout even when the
// name looks like an interpreter.
func TestRelocateConsoleScriptsLeavesAnAlreadyCorrectShebangAlone(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin")
	envPath := filepath.Join(t.TempDir(), "data", "environments", "digest", "bin", "python")
	script := writeConsoleScript(t, bin, "tool", "#!"+envPath+"\nprint(1)\n", 0o700)
	if err := os.MkdirAll(filepath.Dir(envPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(envPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("write interpreter: %v", err)
	}
	// The same environment is not "moved": the shebang already names it.
	if err := relocateConsoleScripts(filepath.Dir(filepath.Dir(envPath)), envPath); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	if got, want := readConsoleScript(t, script), "#!"+envPath+"\nprint(1)\n"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}
