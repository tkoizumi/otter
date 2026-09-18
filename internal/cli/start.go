package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tkoizumi/otter/internal/config"
)

// The two files `otter start` reads, if a project has them. They are the same
// two files `otter deploy` turns into /etc/otter/*.env on a host, so local and
// deployed behaviour cannot drift: a developer who adds a credential to
// otter.env has it locally and on the host.
const (
	projectEnvFileName = "otter.env"
	daemonEnvFileName  = "otter.daemon.env"
)

// projectMarkers are the files and directories that identify a project, in
// priority order. `.otter` is the strongest because Otter creates it, and the
// others let `otter start` work in a checkout or a git repository that has not
// been started before.
var projectMarkers = []string{stateDirName, ".git", "go.mod"}

// ProjectRootEnvName tells the daemon which project it is serving, so it can
// record its address and pid where every other command looks for them. It is
// an environment variable rather than a config field because the daemon does
// not otherwise care about projects: it serves a directory.
const ProjectRootEnvName = "OTTER_PROJECT_ROOT"

// ServeDirName is the directory under a project's state that holds what the
// running daemon recorded: where it listens and which pid owns it.
const ServeDirName = "serve"

// serveDirEnvName overrides where the record lives. It exists so a test can
// point `stop` at a temporary project without changing directories, and so an
// operator running one project from two checkouts can keep the records apart.
const serveDirEnvName = "OTTER_SERVE_DIR"

// serveDir resolves where a record lives for a project. DataDir is the
// fallback for a daemon started by hand without a project, where there is no
// better place to put it.
func serveDir(projectRoot, dataDir string) string {
	if projectRoot != "" {
		return filepath.Join(projectRoot, stateDirName, ServeDirName)
	}
	return dataDir
}

// detachEnvName carries the parent's resolved data directory to the child of
// `otter start --detach`. It exists because the child must not run project
// inference again: the parent has already decided, and a second round could
// disagree if the working directory changed.
const detachEnvName = "OTTER_START_DETACHED"

// pidRecordedEnvName tells a detached child that its parent already wrote the
// pid file. It is separate from detachEnvName, which the child clears before
// the daemon starts: the pid decision has to survive that.
const pidRecordedEnvName = "OTTER_START_PID_RECORDED"

// startOptions is everything `otter start` decided before handing over to the
// daemon, so the decision can be tested without binding a port or starting a
// process.
type startOptions struct {
	ProjectRoot  string
	Integrations string
	Data         string
	Listen       string
	LogFormat    string
	EnvFiles     []string
}

// detectProjectRoot walks up from dir to the nearest directory carrying a
// project marker. The boolean reports whether a marker was found; when it is
// false the caller keeps its own defaults rather than guessing.
func detectProjectRoot(dir string) (string, bool) {
	dir = filepath.Clean(dir)
	for depth := 0; depth < maxDiscoveryDepth; depth++ {
		for _, marker := range projectMarkers {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // filesystem root
		}
		dir = parent
	}
	return "", false
}

// portIsFree reports whether a TCP port on loopback can be bound right now.
func portIsFree(port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// freeLoopbackPort returns the first bindable port at or above want.
//
// A port is a detail of one machine at one moment, so it is chosen rather than
// configured: asking a developer to pick one, remember it, and change it when
// something else takes it is a worse interface than trying ports until one
// works. There is an unavoidable race between this probe and the daemon's
// bind, which is why the bind failure is reported clearly instead of
// retried -- at most one developer action is needed, and guessing again could
// silently move a runtime a script is about to talk to.
func freeLoopbackPort(want int) (int, error) {
	if want <= 0 {
		want = defaultServePort
	}
	for port := want; port <= want+maxPortProbe; port++ {
		if portIsFree(port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free port between %d and %d", want, want+maxPortProbe)
}

// loadEnvFile reads the subset of shell that systemd's EnvironmentFile=
// accepts -- KEY=value, `#` comments, optional `export` -- and applies it to
// the process environment. A key already set in the environment wins: the
// caller's shell is closer to the operator than a file is.
func loadEnvFile(path string) ([]string, error) {
	secrets, err := loadEnvValues(path)
	if err != nil {
		return nil, err
	}
	applied := make([]string, 0, len(secrets))
	for _, key := range sortedStringKeys(secrets) {
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, secrets[key]); err != nil {
			return applied, err
		}
		applied = append(applied, key)
	}
	return applied, nil
}

// loadEnvValues parses one environment file without touching the process
// environment, which is what makes it testable and what lets the caller decide
// precedence.
func loadEnvValues(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=value, got %q", path, i+1, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, i+1)
		}
		values[key] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return values, nil
}

// resolveStart decides the four things a project-aware start needs, in the
// order a developer expects: the project first, then what the project's own
// files say, then anything the operator typed.
func resolveStart(projectRoot string, fs *flag.FlagSet, integrations, data, listen string) (startOptions, error) {
	opts := startOptions{
		ProjectRoot:  projectRoot,
		Integrations: integrations,
		Data:         data,
		Listen:       listen,
	}

	// The data directory default belongs to the project, not to the process
	// that happens to be running.
	if !flagWasSet(fs, "data") && filepath.Clean(opts.Data) == filepath.Clean(config.DefaultDataDir) {
		if projectRoot == "" {
			return opts, errors.New("no project found (no .otter, .git or go.mod in this directory or above); pass --data and --integrations")
		}
		opts.Data = filepath.Join(projectRoot, stateDirName, "data")
	}

	// Integrations default to the project root, which is what makes
	// `otter start` work in an integrations project and in the Otter checkout
	// without either one being told where its integrations live.
	if !flagWasSet(fs, "integrations") && filepath.Clean(opts.Integrations) == filepath.Clean(config.DefaultIntegrations) && projectRoot != "" {
		opts.Integrations = projectRoot
	}

	// An empty listen address is an answer, not an omission: the caller picks a
	// free port. Only a missing data directory is unusable.
	if opts.Data == "" {
		return opts, errors.New("start needs a data directory")
	}
	return opts, nil
}

// projectEnvFiles returns the environment files a project has, in the order
// systemd loads them on a host. Absent files are not an error: a project that
// configures nothing is a valid project.
func projectEnvFiles(projectRoot string) []string {
	if projectRoot == "" {
		return nil
	}
	found := make([]string, 0, 2)
	for _, name := range []string{daemonEnvFileName, projectEnvFileName} {
		path := filepath.Join(projectRoot, name)
		if _, err := os.Stat(path); err == nil {
			found = append(found, path)
		}
	}
	return found
}

// cmdStart implements `otter start`.
func (a *App) cmdStart(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	integrations := fs.String("integrations", config.DefaultIntegrations, "directory scanned recursively for "+config.ManifestFileName)
	data := fs.String("data", config.DefaultDataDir, "data directory holding otter.db and the extracted SDK")
	listen := fs.String("listen", "", "HTTP API listen address; default picks a free loopback port")
	detach := fs.Bool("detach", false, "run in the background and return once the API answers")
	// The daemon's own optional flags are accepted here and forwarded, so
	// `otter start --log-format=pretty --workers 4` works without start
	// re-deriving the daemon's defaults or keeping a second copy of them in
	// step. Zero values mean "not set": daemonArgs forwards only what was
	// actually typed, and the daemon fills the rest.
	workers := fs.Int("workers", 0, "maximum number of concurrently running integrations")
	apiToken := fs.String("api-token", "", "bearer token required for API access")
	logFormat := fs.String("log-format", "", "daemon log format: json or pretty")
	logLevel := fs.String("log-level", "", "daemon log level: debug, info, warn or error")
	shutdownGrace := fs.Duration("shutdown-grace", 0, "how long running integrations may finish after SIGTERM")
	sdkPath := fs.String("sdk-path", "", "directory prepended to the child PYTHONPATH")
	notifyURL := fs.String("notify-url", "", "POST failed runs to this URL")
	notifyFormat := fs.String("notify-format", "", "notification body format")
	notifyOn := fs.String("notify-on", "", "comma-separated terminal statuses that notify")
	_ = []any{workers, apiToken, logFormat, logLevel, shutdownGrace, sdkPath, notifyURL, notifyFormat, notifyOn}

	// Anything left over is forwarded verbatim, so a daemon flag added later
	// works here before start knows about it.
	passthrough := fs.Args()
	fs.Usage = func() {
		fmt.Fprintf(a.Stderr, "Usage: otter start [flags]\n\n")
		fmt.Fprintf(a.Stderr, "Starts the runtime for the project containing the working directory:\n")
		fmt.Fprintf(a.Stderr, "its integrations are the project, its state lives in .otter/data, and\n")
		fmt.Fprintf(a.Stderr, "it records the address it binds so every other command finds it.\n\n")
		fmt.Fprintf(a.Stderr, "otter.env and otter.daemon.env at the project root are loaded first;\n")
		fmt.Fprintf(a.Stderr, "a variable already set in the environment wins over the file.\n\n")
		fmt.Fprintf(a.Stderr, "The daemon's own flags are accepted and forwarded, so --workers,\n")
		fmt.Fprintf(a.Stderr, "--log-format, --api-token and the rest work here too.\n\n")
		fmt.Fprintf(a.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	// A bare word is a mistake worth catching here rather than in the daemon,
	// whose complaint would arrive after a port had been chosen and recorded.
	for _, arg := range passthrough {
		if !strings.HasPrefix(arg, "-") {
			fmt.Fprintf(a.Stderr, "otter: unexpected argument %q; daemon flags are forwarded, integration names are not\n", arg)
			fs.Usage()
			return 2
		}
	}

	// A detached child re-runs this path. It inherits the parent's resolved
	// configuration through OTTER_* in its environment, so it must not run
	// project inference again: a second round could disagree with the parent
	// that is waiting for it, and the flags below already carry the answer.
	detachedChild := os.Getenv(detachEnvName) != ""
	_ = os.Unsetenv(detachEnvName)

	wd, err := workingDirForTest()
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: cannot determine the working directory: %v\n", err)
		return 1
	}
	root, _ := detectProjectRoot(wd)

	// Project files first: they may set OTTER_LISTEN, a token, notify settings,
	// or anything else the daemon reads.
	for _, path := range projectEnvFiles(root) {
		applied, err := loadEnvFile(path)
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: %s: %v\n", path, err)
			return 2
		}
		if len(applied) > 0 {
			fmt.Fprintf(a.Stdout, "loaded      %s (%d variable(s))\n", path, len(applied))
		}
	}

	// Environment, then flags: an explicit flag beats a project file, and a
	// project file beats the built-in default.
	if v := os.Getenv("OTTER_INTEGRATIONS_DIR"); v != "" && !flagWasSet(fs, "integrations") {
		*integrations = v
	}
	if v := os.Getenv("OTTER_DATA_DIR"); v != "" && !flagWasSet(fs, "data") {
		*data = v
	}
	if v := os.Getenv("OTTER_LISTEN"); v != "" && !flagWasSet(fs, "listen") {
		*listen = v
	}

	opts, err := resolveStart(root, fs, *integrations, *data, *listen)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}
	// Absolute paths from here on. The daemon outlives this shell, and a
	// detached child starts with a different working directory, so a relative
	// watch root is wrong the moment it is written down.
	if abs, err := filepath.Abs(opts.Integrations); err == nil {
		opts.Integrations = abs
	}
	if abs, err := filepath.Abs(opts.Data); err == nil {
		opts.Data = abs
	}
	if opts.Listen == "" {
		port, err := freeLoopbackPort(defaultServePort)
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: %v\n", err)
			return 1
		}
		opts.Listen = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	}
	if detachedChild {
		// Every decision was made by the parent; run the daemon and nothing
		// else, so the two processes cannot diverge.
		return RunDaemon(ctx, a.Version,
			daemonArgs(fs, opts.Integrations, opts.Data, opts.Listen, passthrough), a.Stdout, a.Stderr)
	}
	if root != "" {
		opts.EnvFiles = projectEnvFiles(root)
	}

	// Starting twice is a mistake worth naming, not a second daemon sharing
	// one SQLite file.
	if base, ok := runningURL(serveDir(opts.ProjectRoot, opts.Data)); ok {
		fmt.Fprintf(a.Stderr, "otter: already running at %s for %s\n", base, describeProject(opts))
		return 1
	}

	if *detach {
		return a.startDetached(opts, passthrough)
	}

	printStartBanner(a.Stdout, opts, "foreground; Ctrl-C to stop")
	return RunDaemon(ctx, a.Version,
		daemonArgs(fs, opts.Integrations, opts.Data, opts.Listen, passthrough), a.Stdout, a.Stderr)
}

// startDetached forks the same command, waits for the API to answer and
// returns, leaving the daemon running with its pid and log recorded.
func (a *App) startDetached(opts startOptions, passthrough []string) int {
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: cannot find this executable: %v\n", err)
		return 1
	}
	logPath := filepath.Join(serveDir(opts.ProjectRoot, opts.Data), "serve.log")
	// The log lives with the record, not with --data, so it has to be created
	// before it can be opened; the record directory is the project's and may
	// not exist on the first start.
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}
	defer log.Close()

	childArgs := []string{"start",
		"--integrations", opts.Integrations,
		"--data", opts.Data,
		"--listen", opts.Listen}
	childArgs = append(childArgs, passthrough...)
	cmd := exec.Command(executable, childArgs...)
	cmd.Env = append(os.Environ(),
		detachEnvName+"=1",
		pidRecordedEnvName+"=1",
		ProjectRootEnvName+"="+opts.ProjectRoot)
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive this shell
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}

	// The child deliberately does not record its own pid (it cannot tell a
	// detached re-exec from a foreground run), so the parent does it here,
	// before waiting: the runtime is stoppable from the moment it exists.
	if err := writeServePID(serveDir(opts.ProjectRoot, opts.Data), cmd.Process.Pid); err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		_ = cmd.Process.Kill()
		return 1
	}

	base := listenAPIURL(opts.Listen)
	deadline := time.Now().Add(detachWait)
	for time.Now().Before(deadline) {
		if _, ok := runningURL(serveDir(opts.ProjectRoot, opts.Data)); ok {
			pid := cmd.Process.Pid
			_ = cmd.Process.Release()
			fmt.Fprintf(a.Stdout, "started     pid %d\n", pid)
			printStartBanner(a.Stdout, opts, "background; otter stop to stop it")
			return 0
		}
		time.Sleep(50 * time.Millisecond)
	}

	// It never came up: say why from its own log rather than leaving a
	// half-started daemon behind.
	_ = cmd.Process.Kill()
	fmt.Fprintf(a.Stderr, "otter: the runtime did not answer at %s within %s\n", base, detachWait)
	if tail := tailFile(logPath, 10); tail != "" {
		fmt.Fprintf(a.Stderr, "%s\n", tail)
	}
	return 1
}

// cmdStop implements `otter stop`.
func (a *App) cmdStop(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	timeout := fs.Duration("timeout", 20*time.Second, "how long to wait for a graceful shutdown")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	wd, err := workingDirForTest()
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: cannot determine the working directory: %v\n", err)
		return 1
	}
	root, _ := detectProjectRoot(wd)
	if root == "" {
		fmt.Fprintln(a.Stderr, "otter: no project found; nothing to stop")
		return 1
	}
	data := serveDir(root, "")
	if override := os.Getenv(serveDirEnvName); override != "" {
		data = override
	}

	pid, hasPID := readServePID(data)
	if !hasPID || !processAlive(pid) || !apiAnswers(recordedURL(data)) {
		// Nothing is serving this project. A stale record -- a crash, a
		// recycled pid -- is cleaned up rather than acted on: signalling a pid
		// whose daemon is gone risks hitting an unrelated process.
		if _, live := runningURL(data); live {
			fmt.Fprintf(a.Stderr, "otter: a runtime is answering at %s but did not record a pid\n", recordedURL(data))
			fmt.Fprintf(a.Stderr, "otter: stop it by pid\n")
			return 1
		}
		removeServeFiles(data)
		fmt.Fprintf(a.Stdout, "not running  %s\n", root)
		return 0
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		// Already gone: clean up and report success, because the desired state
		// is reached.
		removeServeFiles(data)
		fmt.Fprintf(a.Stdout, "not running  %s (stale pid %d)\n", root, pid)
		return 0
	}

	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			removeServeFiles(data)
			fmt.Fprintf(a.Stdout, "stopped      %s (pid %d)\n", root, pid)
			return 0
		}
		select {
		case <-ctx.Done():
			return 1
		case <-time.After(100 * time.Millisecond):
		}
	}

	fmt.Fprintf(a.Stderr, "otter: pid %d did not stop within %s; sending SIGKILL\n", pid, *timeout)
	_ = process.Signal(syscall.SIGKILL)
	removeServeFiles(data)
	return 1
}

// runningURL reports where the project's runtime is reachable, if the record
// it left belongs to a process that is still alive and answering.
//
// Both halves matter. `listen.url` can outlive a crash, and a pid can be
// recycled by an unrelated process, so neither alone is proof. An HTTP answer
// is proof -- any status counts, including 401, because the question is
// whether this address belongs to a running otterd, not whether we may use it.
func runningURL(dataDir string) (string, bool) {
	base, ok := recordedURLIn(dataDir)
	if !ok {
		return "", false
	}
	if pid, ok := readServePID(dataDir); ok && !processAlive(pid) {
		return "", false // a crash left the record behind
	}
	if !apiAnswers(base) {
		return "", false
	}
	return base, true
}

// apiAnswers probes /health with a short timeout. A 401 still proves a daemon
// is there, which is why any response is accepted.
func apiAnswers(base string) bool {
	req, err := http.NewRequest(http.MethodGet, base+"/health", nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// recordedURL is the address the project's runtime last recorded, whether or
// not anything is listening there.
func recordedURL(dataDir string) string {
	base, _ := recordedURLIn(dataDir)
	return base
}

// recordedURLIn reads the listen file from a record directory, falling back to
// the data directory itself. The fallback keeps `otter stop` working against a
// runtime started before the record moved out of --data.
func recordedURLIn(dir string) (string, bool) {
	if base, ok := readListenURLFile(filepath.Join(dir, ListenURLFileName)); ok {
		return base, true
	}
	if dir != "" {
		if base, ok := readListenURLFile(filepath.Join(dir, "data", ListenURLFileName)); ok {
			return base, true
		}
	}
	return "", false
}

// processAlive reports whether a pid exists and can be signalled. Signal 0
// performs the permission and existence checks without delivering anything.
func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func readServePID(dataDir string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(dataDir, ServePIDFileName))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func writeServePID(dataDir string, pid int) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, ServePIDFileName),
		[]byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// removeServeFiles clears everything a stopped daemon recorded.
func removeServeFiles(dataDir string) {
	removeListenURLFile(dataDir)
	_ = os.Remove(filepath.Join(dataDir, ServePIDFileName))
}

// daemonArgs renders the flags the daemon will be given, so `start` runs the
// same code path as `otterd` rather than a parallel one.
func daemonArgs(fs *flag.FlagSet, integrations, data, listen string, passthrough []string) []string {
	args := []string{
		"--integrations", integrations,
		"--data", data,
		"--listen", listen,
	}
	// Anything else the operator typed, such as --log-format or --workers,
	// must survive the handover.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "integrations", "data", "listen", "detach":
			return
		}
		args = append(args, "--"+f.Name, f.Value.String())
	})
	// Trailing flags the operator typed belong to the daemon. They go last so
	// they cannot be overridden by the derived values above.
	return append(args, passthrough...)
}

func printStartBanner(w io.Writer, opts startOptions, note string) {
	fmt.Fprintf(w, "project     %s\n", describeProject(opts))
	fmt.Fprintf(w, "api         %s\n", listenAPIURL(opts.Listen))
	fmt.Fprintf(w, "integrations %s\n", opts.Integrations)
	fmt.Fprintf(w, "data        %s\n", opts.Data)
	fmt.Fprintf(w, "log         %s\n", filepath.Join(serveDir(opts.ProjectRoot, opts.Data), "serve.log"))
	fmt.Fprintf(w, "%s\n", note)
}

func describeProject(opts startOptions) string {
	if opts.ProjectRoot != "" {
		return opts.ProjectRoot
	}
	return opts.Integrations
}

// tailFile returns the last n lines of a file, for reporting why a detached
// daemon failed to come up.
func tailFile(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

const (
	// defaultServePort is where a project runtime starts looking for a port.
	// 7337 is left alone: that is the production default, and a developer's
	// runtime must not shadow it by accident.
	defaultServePort = 7400
	// maxPortProbe bounds the search so a full range fails loudly.
	maxPortProbe = 64
	// detachWait bounds how long `start --detach` waits for the API.
	detachWait = 20 * time.Second
)

// detachDataEnvName carries the parent's resolved data directory to the
// detached child, which is also where the child finds its pid file path.
const detachDataEnvName = "OTTER_DATA_DIR"
