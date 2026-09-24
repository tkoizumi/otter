package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tkoizumi/otter/internal/deploy"
)

// cmdDeploy implements `otter deploy` and `otter deploy --destroy`.
//
// Like every other Otter command this is a thin surface: all of the decision
// making lives in internal/deploy, which is testable without a server.
func (a *App) cmdDeploy(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)

	f := &deploy.Flags{}
	f.RegisterFlags(fs)

	destroy := fs.Bool("destroy", false, "stop and remove the deployment from the host")
	status := fs.Bool("status", false, "show the last deploy recorded in this project")
	keepData := fs.Bool("keep-data", false, "with --destroy, leave the remote data directory in place")
	assumeYes := fs.Bool("yes", false, "skip the confirmation prompt")

	fs.Usage = func() {
		fmt.Fprint(a.Stderr, `Usage:
  otter deploy --host <user@host> [flags]   install or update a remote runtime
  otter deploy --status                     show the last deploy from this project
  otter deploy --host <host> --destroy      stop and remove it

Deploying is a converge over SSH, not a recipe of one-off actions: running it
twice with the same inputs is safe, and the second run reports that nothing
changed. It never writes to the remote data directory, so run history, sync
watermarks and the extracted Python SDK survive every deploy.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprint(a.Stderr, `
Examples:
  otter deploy --host root@203.0.113.10
  otter deploy --host droplet --platform linux/arm64 --dry-run
  otter deploy --host droplet --env-file ~/.otter/shopify-prod.env
  otter deploy --host droplet --integration shopify-product-to-salesforce-product
  otter deploy --host droplet --binaries ./bin --platform linux/amd64
  ssh -N -L 7337:127.0.0.1:7337 droplet &
  otter --api http://127.0.0.1:7337 --token "$OTTER_TOKEN" integrations
`)
	}

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(a.Stderr, "otter: unexpected arguments: %v\n", fs.Args())
		return 2
	}

	projectRoot, err := findProjectRoot()
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}
	store := deploy.NewStateStore(projectRoot)

	state, _, err := store.Load()
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}

	if *status {
		return a.deployStatus(store, state, f)
	}

	previous, _, err := state.ForHost(f.Target.Host)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}

	cfg, err := deploy.LoadConfig(projectRoot, f, previous)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}
	cfg.Version = a.Version

	runner := deploy.NewSSH(cfg.Target, a.Stdout, a.Stderr)
	defer runner.Close()

	builder := deploy.NewLocalBuilder(a.Stderr, a.Stderr)
	defer builder.Cleanup()

	source, err := binarySourceFor(projectRoot, f, a.Version, a.Stderr)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}
	builder.Binaries = source

	d := &deploy.Deployer{
		Config:            cfg,
		Store:             store,
		WorkspaceRequest:  f.Workspace,
		Builder:           builder,
		Binaries:          source,
		Runner:            runner,
		Stdout:            a.Stdout,
		Stderr:            a.Stderr,
		UV:                f.UV,
		NoUV:              f.NoUV,
		UVVersionOverride: f.UVVersion,
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	if *destroy {
		if !*keepData && !*assumeYes {
			if !a.confirmDestroy(cfg) {
				fmt.Fprintln(a.Stderr, "otter: aborted; nothing was changed")
				return 1
			}
		}
		if err := d.Destroy(ctx, *keepData); err != nil {
			fmt.Fprintf(a.Stderr, "otter: %v\n", err)
			return 1
		}
		fmt.Fprintf(a.Stdout, "removed the deployment from %s\n", cfg.Target)
		return 0
	}

	result, err := d.Run(ctx)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}

	if g.jsonOut {
		return a.printJSON(result)
	}
	a.printDeployResult(result)
	return 0
}

// confirmDestroy guards the one irreversible action. Deleting the data
// directory discards every integration's watermark, and re-deriving it means
// rescanning the source system.
func (a *App) confirmDestroy(cfg deploy.Config) bool {
	fmt.Fprintf(a.Stderr, "This stops %s on %s and removes workspace %s:\n",
		cfg.Target.ServiceUnit(), cfg.Target, cfg.Target.WorkspaceName())
	fmt.Fprintf(a.Stderr, "  %s\n", cfg.Target.WorkspaceDir())
	fmt.Fprintf(a.Stderr, "  its data at %s\n", cfg.Target.DataDir)
	fmt.Fprintf(a.Stderr, "Other workspaces on the host are left alone.\n")
	fmt.Fprintf(a.Stderr, "The data directory holds every sync watermark and all run history.\n")
	fmt.Fprintf(a.Stderr, "Type the host name (%s) to confirm, or re-run with --keep-data: ", cfg.Target.Host)

	reader := bufio.NewReader(os.Stdin)
	answer, err := reader.ReadString('\n')
	if err != nil && answer == "" {
		return false
	}
	return strings.TrimSpace(answer) == cfg.Target.Host
}

func (a *App) deployStatus(store deploy.StateStore, state deploy.State, f *deploy.Flags) int {
	// With a host named, what the host holds is worth reporting even when this
	// checkout has no record of it -- that is exactly the case where another
	// machine deployed there, or where a workspace id was forgotten.
	if f != nil && strings.TrimSpace(f.Target.Host) != "" {
		a.deployStatusRemote(state, f)
	}

	if len(state.Hosts()) == 0 {
		fmt.Fprintln(a.Stderr, "otter: this project has no recorded deploy; run otter deploy --host <host>")
		if f == nil || strings.TrimSpace(f.Target.Host) == "" {
			return 1
		}
		return 0
	}

	for i, host := range state.Hosts() {
		rec := state.Deploys[host]
		if i > 0 {
			fmt.Fprintln(a.Stdout)
		}
		fmt.Fprintf(a.Stdout, "host:          %s\n", rec.Target)
		fmt.Fprintf(a.Stdout, "workspace:     %s\n", rec.Target.WorkspaceName())
		fmt.Fprintf(a.Stdout, "version:       %s\n", rec.Version)
		fmt.Fprintf(a.Stdout, "platform:      %s\n", rec.Target.Platform)
		fmt.Fprintf(a.Stdout, "revision:      %s\n", rec.Revision)
		fmt.Fprintf(a.Stdout, "deployed at:   %s\n", rec.DeployedAt.Format("2006-01-02 15:04:05 MST"))
		fmt.Fprintf(a.Stdout, "tree:          %s\n", rec.Target.WorkspaceDir())
		fmt.Fprintf(a.Stdout, "data:          %s\n", rec.Target.DataDir)
		fmt.Fprintf(a.Stdout, "service:       %s\n", rec.Target.ServiceUnit())
		fmt.Fprintf(a.Stdout, "api:           %s (loopback on the host)\n", rec.Target.APIURL())
		if len(rec.Bindings) > 0 {
			// The destination mints its own identity, so this is the only place
			// a local label and the remote identity it became are related.
			fmt.Fprintln(a.Stdout, "\ndestination identities:")
			for _, b := range rec.Bindings {
				fmt.Fprintf(a.Stdout, "  %-20s %s\n", b.Name, b.ID)
			}
		}
		if token, err := store.LoadToken(rec.Host, rec.Target.WorkspaceID); err == nil && token != "" {
			port := portFromListen(rec.Target.Listen)
			fmt.Fprintf(a.Stdout, "\nexport OTTER_API_TOKEN=%s\n", token)
			fmt.Fprintf(a.Stdout, "ssh -N -L %d:127.0.0.1:%d %s &\n", port, port, rec.Target)
		}
	}

	return 0
}

// deployStatusRemote lists the workspaces a host holds, which is how an
// operator learns the port to tunnel to for a workspace this checkout did not
// create. Failures are reported and do not fail the command: the local records
// above are still correct.
func (a *App) deployStatusRemote(state deploy.State, f *deploy.Flags) {
	previous, _, err := state.ForHost(f.Target.Host)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return
	}
	cfg, err := deploy.LoadConfig(a.deployProjectRoot(), f, previous)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return
	}

	runner := deploy.NewSSH(cfg.Target, a.Stdout, a.Stderr)
	defer runner.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := runner.Output(ctx, deploy.WorkspaceListScript(cfg.Target))
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: could not list workspaces on %s: %v\n", cfg.Target, err)
		return
	}
	records, _, err := deploy.ParseWorkspaceList(out)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return
	}
	fmt.Fprintf(a.Stdout, "\nworkspaces on %s:\n", cfg.Target)
	if len(records) == 0 {
		fmt.Fprintln(a.Stdout, "  (none)")
		return
	}
	for _, rec := range records {
		fmt.Fprintf(a.Stdout, "  %-28s %-22s %s\n", rec.Name, rec.Unit, rec.Listen)
	}
}

// deployProjectRoot is the project this process is running in, for status.
func (a *App) deployProjectRoot() string {
	root, err := findProjectRoot()
	if err != nil {
		return ""
	}
	return root
}

// portFromListen extracts the port so a tunnel command can be printed.
func portFromListen(listen string) int {
	_, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return deploy.DefaultListenPort
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return deploy.DefaultListenPort
	}
	return n
}

func (a *App) printDeployResult(r *deploy.Result) {
	verb := "deployed"
	if r.DryRun {
		verb = "would deploy"
	}
	fmt.Fprintf(a.Stdout, "%s %s to %s (%s)\n", verb, r.Version, r.Host, r.Platform)
	fmt.Fprintf(a.Stdout, "workspace:     %s\n", r.Workspace)
	fmt.Fprintf(a.Stdout, "integrations:  %s\n", strings.Join(r.Integrations, ", "))
	fmt.Fprintf(a.Stdout, "install:       %s\n", r.RemoteDir)

	for _, warning := range r.Warnings {
		fmt.Fprintf(a.Stderr, "warning: secret %s\n", warning)
	}

	if r.DryRun {
		fmt.Fprintf(a.Stdout, "\nnothing was changed (--dry-run)\n")
		return
	}

	if r.RevealToken && r.APIToken != "" {
		fmt.Fprintf(a.Stdout, "\nAPI token (store it now; it is also in %s/state.secret.json):\n", deploy.StateDirName)
		fmt.Fprintf(a.Stdout, "  export OTTER_API_TOKEN=%s\n", r.APIToken)
	}

	port := portFromListen(r.APIURL)
	fmt.Fprintf(a.Stdout, "\nthe API listens on loopback only. Reach it with a tunnel:\n")
	fmt.Fprintf(a.Stdout, "  ssh -N -L %d:127.0.0.1:%d %s &\n", port, port, r.Host)
	fmt.Fprintf(a.Stdout, "  otter --api %s status\n", r.APIURL)
}

// findProjectRoot walks up from the working directory to the nearest directory
// carrying a project marker, so `otter deploy` works from any subdirectory of a
// project and means the same project every other `otter` command means.
//
// It is deliberately not "the nearest go.mod". A deploy target is a project,
// and most projects are Python: requiring a Go module made the command usable
// only from the runtime's own checkout, which is not where integrations live.
func findProjectRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, ok := detectProjectRoot(dir)
	if !ok {
		return "", fmt.Errorf(
			"no project found above %s: run otter deploy from a directory holding %s, .git or go.mod",
			dir, stateDirName)
	}
	return root, nil
}

// hasRuntimeSource reports whether the project can compile the runtime itself.
// Both entry points must be present: compiling one of two binaries and
// deploying a stale other is worse than fetching a matched pair.
func hasRuntimeSource(root string) bool {
	for _, pkg := range []string{"cmd/otterd", "cmd/otter"} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(pkg)))
		if err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

// binarySourceFor decides where the two executables come from.
//
// A nil source is never returned: every path names a BinarySource, so "compile
// this checkout" and "fetch that release" are the same kind of decision.
//
// The rule, in order:
//
//  1. --binaries: use exactly what the operator supplied (offline path).
//  2. --source or --build: compile. --source names the checkout; --build alone
//     means the project itself, which must therefore be a checkout.
//  3. the project is a Go checkout: compile from it.
//  4. this otter is a development build and its own checkout is still on disk:
//     compile from that. A build reporting v0.1.13-dirty has no release to
//     fetch, and the project it is deploying usually has no Go source, but the
//     binary knows where it was built from.
//  5. otherwise: fetch the published release for the target platform.
func binarySourceFor(projectRoot string, f *deploy.Flags, version string, stderr io.Writer) (deploy.BinarySource, error) {
	if f.Binaries != "" {
		dir, err := filepath.Abs(f.Binaries)
		if err != nil {
			return nil, fmt.Errorf("resolve --binaries %s: %w", f.Binaries, err)
		}
		return deploy.DirBinaries{Dir: dir}, nil
	}

	if f.Source != "" || f.Build {
		dir := f.Source
		if dir == "" {
			dir = projectRoot
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("resolve --source %s: %w", dir, err)
		}
		if !hasRuntimeSource(abs) {
			if f.Source != "" {
				return nil, fmt.Errorf("--source %s is not an Otter checkout: no cmd/otterd in it", abs)
			}
			return nil, fmt.Errorf(
				"--build compiles the project, but %s has no cmd/otterd\n"+
					"hint: name the checkout with --source <dir>, use --binaries <dir>, "+
					"or drop --build to use the published release", abs)
		}
		return deploy.CompileSource{Dir: abs, Version: version, Stderr: stderr}, nil
	}

	if hasRuntimeSource(projectRoot) {
		return deploy.CompileSource{Dir: projectRoot, Version: version, Stderr: stderr}, nil
	}

	if !deploy.IsReleasedVersion(version) {
		if checkout := adjacentCheckout(); checkout != "" {
			return deploy.CompileSource{Dir: checkout, Version: version, Stderr: stderr}, nil
		}
	}

	return deploy.ReleaseBinaries{Version: version, Stderr: stderr}, nil
}

// executablePath is os.Executable, indirected so a test can place the running
// binary inside a checkout.
var executablePath = os.Executable

// adjacentCheckout returns the Otter checkout this executable was built from,
// when it is still on disk.
//
// A checkout build lands in <checkout>/bin/otter, so the source is one or two
// directories up. Walking up is bounded and only accepts a directory that
// really holds both entry points, so an installed binary in /usr/local/bin or a
// Homebrew Cellar never matches one.
func adjacentCheckout() string {
	exe, err := executablePath()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	for depth := 0; depth < 3; depth++ {
		if hasRuntimeSource(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}
