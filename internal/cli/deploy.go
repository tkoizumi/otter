package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	status := fs.Bool("status", false, "show the last deploy recorded in this checkout")
	keepData := fs.Bool("keep-data", false, "with --destroy, leave the remote data directory in place")
	assumeYes := fs.Bool("yes", false, "skip the confirmation prompt")

	fs.Usage = func() {
		fmt.Fprint(a.Stderr, `Usage:
  otter deploy --host <user@host> [flags]   install or update a remote runtime
  otter deploy --status                     show the last deploy from this checkout
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

	repoRoot, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}
	store := deploy.NewStateStore(repoRoot)

	if *status {
		return a.deployStatus(store)
	}

	previous, _, err := store.Load()
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}

	cfg, err := deploy.LoadConfig(repoRoot, f, previous)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}
	cfg.Version = a.Version

	runner := deploy.NewSSH(cfg.Target, a.Stdout, a.Stderr)
	defer runner.Close()

	builder := deploy.NewLocalBuilder(a.Stderr, a.Stderr)
	defer builder.Cleanup()

	d := &deploy.Deployer{
		Config:            cfg,
		Store:             store,
		Builder:           builder,
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
	fmt.Fprintf(a.Stderr, "This stops %s on %s and deletes %s and %s.\n",
		cfg.Target.ServiceUnit(), cfg.Target, cfg.Target.RemoteDir, cfg.Target.DataDir)
	fmt.Fprintf(a.Stderr, "The data directory holds every sync watermark and all run history.\n")
	fmt.Fprintf(a.Stderr, "Type the host name (%s) to confirm, or re-run with --keep-data: ", cfg.Target.Host)

	reader := bufio.NewReader(os.Stdin)
	answer, err := reader.ReadString('\n')
	if err != nil && answer == "" {
		return false
	}
	return strings.TrimSpace(answer) == cfg.Target.Host
}

func (a *App) deployStatus(store deploy.StateStore) int {
	st, ok, err := store.Load()
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}
	if !ok {
		fmt.Fprintln(a.Stderr, "otter: this checkout has no recorded deploy; run otter deploy --host <host>")
		return 1
	}

	fmt.Fprintf(a.Stdout, "host:          %s\n", st.Target)
	fmt.Fprintf(a.Stdout, "version:       %s\n", st.Version)
	fmt.Fprintf(a.Stdout, "platform:      %s\n", st.Target.Platform)
	fmt.Fprintf(a.Stdout, "revision:      %s\n", st.Revision)
	fmt.Fprintf(a.Stdout, "deployed at:   %s\n", st.DeployedAt.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(a.Stdout, "install:       %s\n", st.Target.RemoteDir)
	fmt.Fprintf(a.Stdout, "data:          %s\n", st.Target.DataDir)
	fmt.Fprintf(a.Stdout, "service:       %s\n", st.Target.ServiceUnit())
	fmt.Fprintf(a.Stdout, "api:           %s (loopback on the host)\n", st.Target.APIURL())
	if token, err := store.LoadToken(); err == nil && token != "" {
		fmt.Fprintf(a.Stdout, "\nexport OTTER_API_TOKEN=%s\n", token)
		fmt.Fprintf(a.Stdout, "ssh -N -L 7337:127.0.0.1:7337 %s &\n", st.Target)
	}
	return 0
}

func (a *App) printDeployResult(r *deploy.Result) {
	verb := "deployed"
	if r.DryRun {
		verb = "would deploy"
	}
	fmt.Fprintf(a.Stdout, "%s %s to %s (%s)\n", verb, r.Version, r.Host, r.Platform)
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

	fmt.Fprintf(a.Stdout, "\nthe API listens on loopback only. Reach it with a tunnel:\n")
	fmt.Fprintf(a.Stdout, "  ssh -N -L 7337:127.0.0.1:7337 %s &\n", r.Host)
	fmt.Fprintf(a.Stdout, "  otter --api %s status\n", r.APIURL)
}

// findRepoRoot walks up from the working directory looking for the module
// root, so `otter deploy` works from a subdirectory.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found above %s: run otter deploy from inside the Otter checkout", dir)
		}
		dir = parent
	}
}
