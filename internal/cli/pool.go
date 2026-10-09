package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/tkoizumi/otter/internal/pool"
)

// `otter pool agent` is the host side of automated tenant provisioning.
//
// It is a SEPARATE COMMAND from `otter agent` on purpose. That one is a
// per-runtime agent: it holds one runtime id, bootstraps with that runtime's secret,
// and applies releases to it. This one is host-scoped and runs before any runtime
// exists — it is what creates them. Merging them would mean one process holding two
// unrelated credentials and two different scopes, which is exactly the kind of
// conflation that makes an authorization review impossible.
//
// The token is read from a file rather than a flag, because a credential on an argv
// is visible to every process on the host and ends up in shell history. The operator
// writes it once, mode 0600, from the value `POST /api/pool/hosts` returned.
func (a *App) cmdPool(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.Stderr, poolUsage)
		return 2
	}
	switch args[0] {
	case "agent":
		return a.cmdPoolAgent(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(a.Stdout, poolUsage)
		return 0
	default:
		fmt.Fprintf(a.Stderr, "otter: unknown pool command %q\n\n", args[0])
		fmt.Fprint(a.Stderr, poolUsage)
		return 2
	}
}

const poolUsage = `Usage: otter pool <command>

Commands:
  agent    Run the pool host agent: ask the control plane for tenants to create.

The agent needs a pool token from the control plane
(POST /api/pool/hosts) and the control plane's public URL.

Flags for ` + "`otter pool agent`" + `:
  --cloud <url>         control plane URL (or OTTER_POOL_CLOUD_URL)
  --token-file <path>   pool token file, mode 0600 (default ` + pool.DefaultTokenPath + `)
  --once                run a single cycle and exit
  --dry-run             print what would be done; change nothing
  --poll <duration>     how long to wait between cycles (default 15s)
  --provisioner <path>  the provisioner script (default ` + pool.DefaultProvisionerPath + `)
`

func (a *App) cmdPoolAgent(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("pool agent", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cloudURL := fs.String("cloud", "", "control plane URL (default $OTTER_POOL_CLOUD_URL)")
	tokenFile := fs.String("token-file", "", "file holding the pool token (default "+pool.DefaultTokenPath+")")
	once := fs.Bool("once", false, "run a single cycle and exit")
	dryRun := fs.Bool("dry-run", false, "print what would be done; change nothing")
	poll := fs.Duration("poll", 0, "wait between cycles (default 15s)")
	provisioner := fs.String("provisioner", "", "provisioner script (default "+pool.DefaultProvisionerPath+")")
	statePath := fs.String("state", "", "agent state directory (default "+pool.DefaultStatePath+")")
	fs.Usage = func() { fmt.Fprint(a.Stderr, poolUsage) }
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

	base := strings.TrimSpace(firstNonEmptyString(*cloudURL, os.Getenv("OTTER_POOL_CLOUD_URL")))
	if base == "" {
		fmt.Fprintln(a.Stderr, "otter: no control plane URL: pass --cloud <url> or set OTTER_POOL_CLOUD_URL")
		fmt.Fprintln(a.Stderr, "otter: the tenant's agent must reach this plane over the public internet; a loopback address will not work")
		return 2
	}

	token, err := readPoolToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}

	options := pool.Options{
		CloudURL:     base,
		TokenPath:    *tokenFile,
		StatePath:    *statePath,
		Provisioner:  *provisioner,
		PollInterval: *poll,
		Once:         *once,
		DryRun:       *dryRun,
		Logger: func(format string, args ...any) {
			fmt.Fprintf(a.Stdout, format+"\n", args...)
		},
	}

	agent := &pool.Agent{
		Client:   pool.NewClient(base, token),
		CloudURL: base,
		Options:  options,
	}

	fmt.Fprintf(a.Stdout, "pool agent: %s\n", base)
	if *dryRun {
		fmt.Fprintln(a.Stdout, "pool agent: dry run, no changes will be made")
	}
	if err := agent.Run(ctx); err != nil {
		// A cancelled context is a clean shutdown, which systemd and Ctrl-C both
		// produce; reporting it as a failure would fill the journal with noise on
		// every restart.
		if ctx.Err() != nil {
			fmt.Fprintln(a.Stdout, "pool agent: stopped")
			return 0
		}
		fmt.Fprintf(a.Stderr, "otter: pool agent: %v\n", err)
		return 1
	}
	return 0
}

// readPoolToken reads the host credential from a file.
//
// A file rather than a flag or an environment variable: `OTTER_POOL_TOKEN` in a
// systemd unit is readable from `/proc/<pid>/environ` by anything running as the
// same user, and a flag is in the process table. The file is checked for the two
// mistakes that actually happen — missing, and empty — with a message that says what
// to do rather than "permission denied".
func readPoolToken(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = pool.DefaultTokenPath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no pool token at %s: register the host with POST /api/pool/hosts and write the token there (mode 0600)", path)
		}
		return "", fmt.Errorf("read pool token %s: %w", path, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("the pool token at %s is empty", path)
	}
	return token, nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
