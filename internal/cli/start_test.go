package cli

import (
	"flag"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectProjectRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "integrations", "hello")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{root, filepath.Join(root, "integrations"), nested} {
		got, ok := detectProjectRoot(dir)
		if !ok {
			t.Fatalf("no project found from %s", dir)
		}
		if got != root {
			t.Errorf("from %s got %q, want %q", dir, got, root)
		}
	}

	// A directory with none of the markers is not a project, and the caller
	// keeps its own defaults rather than guessing.
	plain := t.TempDir()
	if got, ok := detectProjectRoot(plain); ok {
		t.Errorf("detectProjectRoot(%s) = %q, want no project", plain, got)
	}
}

// The nearest project wins, so a checkout inside an integrations project (or
// the reverse) resolves to the one the developer is standing in.
func TestDetectProjectRootPrefersTheNearest(t *testing.T) {
	outer := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outer, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := detectProjectRoot(inner); !ok || got != inner {
		t.Errorf("got (%q, %v), want the inner project %q", got, ok, inner)
	}
}

func TestFreeLoopbackPort(t *testing.T) {
	port, err := freeLoopbackPort(0)
	if err != nil {
		t.Fatalf("no free port: %v", err)
	}
	if !portIsFree(port) {
		t.Errorf("port %d was reported free but cannot be bound", port)
	}

	// A taken port must be skipped rather than returned.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	taken := listener.Addr().(*net.TCPAddr).Port

	got, err := freeLoopbackPort(taken)
	if err != nil {
		t.Fatalf("no free port above %d: %v", taken, err)
	}
	if got == taken {
		t.Errorf("freeLoopbackPort(%d) returned the busy port", taken)
	}
}

func TestLoadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), projectEnvFileName)
	body := `# a comment
OTTER_TEST_FROM_FILE=file
export OTTER_TEST_EXPORTED=exported
OTTER_TEST_QUOTED="quoted"
OTTER_TEST_ALREADY=file
OTTER_TEST_EMPTY=
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// The shell is closer to the operator than a file, so it wins.
	t.Setenv("OTTER_TEST_ALREADY", "shell")

	applied, err := loadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"OTTER_TEST_FROM_FILE": "file",
		"OTTER_TEST_EXPORTED":  "exported",
		"OTTER_TEST_QUOTED":    "quoted",
		"OTTER_TEST_EMPTY":     "",
		"OTTER_TEST_ALREADY":   "shell",
	} {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for _, key := range applied {
		if key == "OTTER_TEST_ALREADY" {
			t.Errorf("a variable already in the environment was reported as applied")
		}
	}
	t.Cleanup(func() {
		for _, key := range []string{"OTTER_TEST_FROM_FILE", "OTTER_TEST_EXPORTED", "OTTER_TEST_QUOTED", "OTTER_TEST_ALREADY", "OTTER_TEST_EMPTY"} {
			_ = os.Unsetenv(key)
		}
	})
}

// A malformed file is a loud failure, not a half-applied environment.
func TestLoadEnvFileRejectsMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), projectEnvFileName)
	if err := os.WriteFile(path, []byte("NOT_A_PAIR\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEnvFile(path); err == nil {
		t.Error("a line without = was accepted")
	}
}

func TestProjectEnvFilesOrderAndAbsence(t *testing.T) {
	root := t.TempDir()
	// No files at all is a valid project.
	if got := projectEnvFiles(root); len(got) != 0 {
		t.Errorf("projectEnvFiles on an empty project = %v, want none", got)
	}
	// systemd loads the daemon settings first and the credentials second, and
	// the later file wins. Local start must match, or a value could differ
	// between a laptop and a host.
	for _, name := range []string{projectEnvFileName, daemonEnvFileName} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("# empty\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := projectEnvFiles(root)
	if len(got) != 2 {
		t.Fatalf("got %v, want both files", got)
	}
	if filepath.Base(got[0]) != daemonEnvFileName || filepath.Base(got[1]) != projectEnvFileName {
		t.Errorf("order = %v, want daemon settings then credentials", got)
	}
}

// With no project, and nothing typed, start cannot guess where state belongs.
func TestResolveStartWithoutAProjectNeedsData(t *testing.T) {
	fs, _, _ := startFlagSet(t)
	if _, err := resolveStart("", fs, "./integrations", "./tmp", ""); err == nil {
		t.Error("resolveStart guessed a data directory with no project and no flag")
	}
}

func TestResolveStartDerivesFromTheProject(t *testing.T) {
	root := t.TempDir()
	fs, _, _ := startFlagSet(t)
	opts, err := resolveStart(root, fs, "./integrations", "./tmp", "127.0.0.1:7400")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Data != filepath.Join(root, stateDirName, "data") {
		t.Errorf("Data = %q, want the project's own state directory", opts.Data)
	}
	if opts.Integrations != root {
		t.Errorf("Integrations = %q, want the project root", opts.Integrations)
	}
}

// An explicit flag always beats what the project would have chosen.
func TestResolveStartHonoursExplicitFlags(t *testing.T) {
	root := t.TempDir()
	fs, _, _ := startFlagSet(t, "--integrations", "/elsewhere", "--data", "/var/otter")
	opts, err := resolveStart(root, fs, "/elsewhere", "/var/otter", "127.0.0.1:7400")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Data != "/var/otter" {
		t.Errorf("Data = %q, want the flag value", opts.Data)
	}
	if opts.Integrations != "/elsewhere" {
		t.Errorf("Integrations = %q, want the flag value", opts.Integrations)
	}
}

// A project without a listen flag gets a free port, and never the production
// default: a developer's runtime must not shadow a host's service.
func TestResolveStartLeavesListenForTheCallerToChoose(t *testing.T) {
	root := t.TempDir()
	fs, _, _ := startFlagSet(t)
	opts, err := resolveStart(root, fs, "./integrations", "./tmp", "")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Listen != "" {
		t.Errorf("Listen = %q, want empty so the caller picks a free port", opts.Listen)
	}
	port, err := freeLoopbackPort(defaultServePort)
	if err != nil {
		t.Fatal(err)
	}
	if port < defaultServePort {
		t.Errorf("picked port %d, below the start of the range %d", port, defaultServePort)
	}
}

// startFlagSet builds the flag set `otter start` parses, so tests exercise the
// same flag names and the same flagWasSet behaviour.
func startFlagSet(t *testing.T, args ...string) (*flag.FlagSet, *string, *string) {
	t.Helper()
	integrations := new(string)
	data := new(string)
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.StringVar(integrations, "integrations", "./integrations", "")
	fs.StringVar(data, "data", "./tmp", "")
	fs.String("listen", "", "")
	fs.Bool("detach", false, "")
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return fs, integrations, data
}
