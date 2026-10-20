package cli

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tkoizumi/otter/internal/awscreds"
	"github.com/tkoizumi/otter/internal/awssecrets"
	"github.com/tkoizumi/otter/internal/pool"
	"github.com/tkoizumi/otter/internal/secretbroker"
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
	case "broker":
		return a.cmdPoolBroker(ctx, args[1:])
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
  broker   Serve the secret exchange a pooled tenant's runtime resolves from.

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

	base := strings.TrimSpace(firstNonEmptyString(*cloudURL, pool.ResolveCloudURL()))
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

// `otter pool broker` is the host side of the secret exchange (D2).
//
// A pooled tenant's otterd mints a short-lived identity bound to its runtime and
// job and presents it here; the broker verifies it against the host-local
// credential for that runtime and returns the job's secrets from AWS Secrets
// Manager. The AWS credential is the instance role, so the host holds no
// long-lived secret, and the control plane is not in the path at all.
//
// It listens on the TENANT GATEWAYS rather than a wildcard. Each tenant reaches
// its own bridge gateway, so binding there makes the broker reachable from the
// tenant networks and from nowhere else -- a wildcard bind on an EC2 host would
// also expose it on the public interface, where the only thing protecting it
// would be a security group somebody has to remember to check.
func (a *App) cmdPoolBroker(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("pool broker", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	credentials := fs.String("credentials", "", "per-runtime broker credentials (default "+defaultBrokerCredentialsPath+")")
	tenants := fs.String("tenants", "", "tenant registry (default "+defaultTenantRegistryPath+")")
	port := fs.Int("port", defaultBrokerPort, "TCP port to serve on")
	listen := fs.String("listen", "", "extra addresses to listen on, comma-separated")
	region := fs.String("region", "", "AWS region (default: the instance's own)")
	endpoint := fs.String("secrets-endpoint", "", "Secrets Manager endpoint override")
	fs.Usage = func() { fmt.Fprint(a.Stderr, brokerUsage) }
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

	credentialsPath := firstNonEmptyString(*credentials, os.Getenv("OTTER_BROKER_CREDENTIALS_FILE"), defaultBrokerCredentialsPath)
	registryPath := firstNonEmptyString(*tenants, os.Getenv("OTTER_TENANTS_FILE"), defaultTenantRegistryPath)

	registry, err := os.Open(registryPath)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: tenant registry: %v\n", err)
		return 1
	}
	tenantList, err := secretbroker.ParseTenants(registry)
	_ = registry.Close()
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}

	var addrs []string
	for _, tenant := range tenantList {
		addrs = append(addrs, net.JoinHostPort(tenant.Gateway, strconv.Itoa(*port)))
	}
	for _, extra := range strings.Split(*listen, ",") {
		if extra = strings.TrimSpace(extra); extra != "" {
			addrs = append(addrs, extra)
		}
	}
	if len(addrs) == 0 {
		fmt.Fprintf(a.Stderr, "otter: no tenant gateways in %s and no --listen; refusing to bind every interface\n", registryPath)
		return 1
	}

	imds := awscreds.NewIMDS(os.Getenv("OTTER_IMDS_BASE"))
	resolvedRegion := strings.TrimSpace(*region)
	if resolvedRegion == "" {
		resolvedRegion, err = imds.Region(ctx)
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: cannot resolve the AWS region from instance metadata; pass --region: %v\n", err)
			return 1
		}
	}

	client := awssecrets.New(resolvedRegion, &awscreds.Source{IMDS: imds}, func(c *awssecrets.Client) {
		if *endpoint != "" {
			c.Endpoint = *endpoint
		}
	})
	logf := func(format string, args ...any) { fmt.Fprintf(a.Stdout, format+"\n", args...) }
	broker := secretbroker.New(
		secretbroker.NewFileCredentials(credentialsPath),
		&secretbroker.AWSStore{Client: client},
		secretbroker.Options{Logf: logf},
	)

	listeners, err := secretbroker.Listen(addrs)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}

	logf("pool broker: region %s, credentials %s", resolvedRegion, credentialsPath)
	for _, tenant := range tenantList {
		logf("pool broker: tenant %s on %s", tenant.Name, net.JoinHostPort(tenant.Gateway, strconv.Itoa(*port)))
	}
	if err := secretbroker.Serve(ctx, listeners, broker, logf); err != nil {
		fmt.Fprintf(a.Stderr, "otter: pool broker: %v\n", err)
		return 1
	}
	logf("pool broker: stopped")
	return 0
}

const (
	defaultBrokerCredentialsPath = "/var/lib/otter/pool-agent/broker-credentials.json"
	defaultTenantRegistryPath    = "/etc/otter/tenants.conf"
	defaultBrokerPort            = 7338
)

const brokerUsage = `Usage: otter pool broker

Serves the secret exchange a pooled tenant's runtime resolves its secrets from.

It listens on each tenant's bridge gateway (from the tenant registry), so the
broker is reachable from the tenant networks and nowhere else.

Flags:
  --credentials <path>      per-runtime broker credentials, mode 0600
  --tenants <path>          tenant registry: one "name subnet gateway" line each
  --port <n>                TCP port to serve on (default 7338)
  --listen <addrs>          extra addresses, comma-separated
  --region <region>         AWS region (default: the instance's own)
  --secrets-endpoint <url>  Secrets Manager endpoint override

The credentials file maps runtime id to the host-local credential the provisioner
also placed in that tenant's environment. It is the only thing that lets the
broker tell one tenant's request from another's, which is why it must be 0600
and why no value from it is ever logged.
`

// readPoolToken reads the host credential.
//
// A FILE rather than a flag or an environment variable: a variable in a systemd unit
// is readable from `/proc/<pid>/environ` by anything running as the same user, and a
// flag is in the process table.
//
// THE SYSTEMD CREDENTIAL WINS when one is present, and that ordering is the whole
// point. The unit runs as an unprivileged account because it must not be root, while
// `/etc/otter/pool-token` is a root-owned `0600` file — so reading it directly fails
// with "permission denied", which is what the first real service start did. systemd
// reads the file as root and hands the process a readable copy under
// `$CREDENTIALS_DIRECTORY`, so the credential is readable by exactly one process and
// is not visible to anything else on the host.
//
// The order is: an explicit flag, then the environment, then the systemd credential,
// then the default file. A manual run on a host without systemd still works.
func readPoolToken(flagPath string) (string, error) {
	if strings.TrimSpace(flagPath) != "" {
		return readTokenFile(flagPath, true)
	}
	if env := strings.TrimSpace(os.Getenv("OTTER_POOL_TOKEN_FILE")); env != "" {
		return readTokenFile(env, true)
	}
	if dir := strings.TrimSpace(os.Getenv("CREDENTIALS_DIRECTORY")); dir != "" {
		path := filepath.Join(dir, pool.DefaultCredentialName)
		if _, err := os.Stat(path); err == nil {
			return readTokenFile(path, false)
		}
	}
	return readTokenFile(pool.DefaultTokenPath, false)
}

// readTokenFile reads and trims one token file.
//
// `explicit` distinguishes "you told me where it is and I could not read it" — an
// error worth naming precisely — from "the default was not there", where the useful
// message is how to create it.
func readTokenFile(path string, explicit bool) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return "", fmt.Errorf("no pool token at %s: register the host with POST /api/pool/hosts and install the token there (mode 0600, owned by root)", path)
		}
		if os.IsPermission(err) {
			return "", fmt.Errorf(
				"the pool token at %s is not readable by this account: run the agent under the systemd unit (which loads it as a credential) or make the file readable by the service account",
				path,
			)
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
