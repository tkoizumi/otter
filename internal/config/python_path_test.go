package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// python.path lets integrations share client code. Entries are relative to the
// integration directory, and escaping upwards is the point.
func TestPythonPathResolvesRelativeToIntegrationDir(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "lib", "python")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(root, "integrations", "sync")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('x')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manifest{
		Version:    SupportedVersion,
		Name:       "sync",
		Entrypoint: "main.py",
		Dir:        dir,
		Python:     PythonConfig{Path: []string{"../../lib/python"}},
	}
	m.ApplyDefaults()

	if err := m.Validate(); err != nil {
		t.Fatalf("a path that exists should validate, got: %v", err)
	}

	got := m.PythonPaths()
	if len(got) != 1 {
		t.Fatalf("PythonPaths = %v, want one entry", got)
	}
	if got[0] != shared {
		t.Errorf("PythonPaths[0] = %q, want %q", got[0], shared)
	}
}

func TestPythonPathAcceptsAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "integration")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('x')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}

	m := &Manifest{
		Version: SupportedVersion, Name: "sync", Entrypoint: "main.py", Dir: dir,
		Python: PythonConfig{Path: []string{shared}},
	}
	m.ApplyDefaults()
	if err := m.Validate(); err != nil {
		t.Fatalf("absolute path should validate: %v", err)
	}
	if got := m.PythonPaths(); len(got) != 1 || got[0] != shared {
		t.Errorf("PythonPaths = %v, want [%s]", got, shared)
	}
}

func TestPythonPathMustExistAndBeADirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "integration")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('x')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file, not a directory.
	if err := os.WriteFile(filepath.Join(root, "notadir"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		entry string
		want  string
	}{
		{"missing", "../does-not-exist", "does not exist"},
		{"not a directory", "../notadir", "is not a directory"},
		{"empty", "   ", "is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manifest{
				Version: SupportedVersion, Name: "sync", Entrypoint: "main.py", Dir: dir,
				Python: PythonConfig{Path: []string{tc.entry}},
			}
			m.ApplyDefaults()

			err := m.Validate()
			if err == nil {
				t.Fatalf("python.path %q should be rejected", tc.entry)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.want)
			}
			if !strings.Contains(err.Error(), "python.path") {
				t.Errorf("error = %q, want it to name the field", err.Error())
			}
		})
	}
}

// A manifest written before python.path existed still loads unchanged.
func TestPythonPathIsOptional(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('x')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{Version: SupportedVersion, Name: "sync", Entrypoint: "main.py", Dir: dir}
	m.ApplyDefaults()
	if err := m.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := m.PythonPaths(); len(got) != 0 {
		t.Errorf("PythonPaths = %v, want empty", got)
	}
}
