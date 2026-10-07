package cli

// otter agent -- run the pooled-runtime agent.
//
// The agent is a client of two APIs: the control plane (outbound, authenticated
// by the host's instance role) and the local runtime (loopback, authenticated by
// a runtime token). It never accepts an inbound connection from the control
// plane, which is decision D1 and the reason the protocol is shaped as it is.
//
// This command exists so the agent can actually be RUN. Until it did, the agent
// was a tested library that no process invoked, which meant the protocol had
// never been exercised end to end.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tkoizumi/otter/internal/agent"
)

func (a *App) cmdAgent(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)

	cloudURL := fs.String("cloud", os.Getenv("OTTER_AGENT_CLOUD_URL"),
		"control plane base URL (env OTTER_AGENT_CLOUD_URL)")
	runtimeID := fs.String("runtime-id", os.Getenv("OTTER_AGENT_RUNTIME_ID"),
		"the runtime this agent manages (env OTTER_AGENT_RUNTIME_ID)")
	runtimeURL := fs.String("runtime-url", envOr("OTTER_AGENT_RUNTIME_URL", "http://127.0.0.1:7337"),
		"local runtime API base URL")
	// The agent's own credential. `otter token create --scope agent` mints one;
	// it reaches only the deploy surface (maintenance, releases), so a leaked
	// agent credential is not a way into the tenant's jobs, runs or state. The
	// admin token still works for a hand-run diagnostic, but is no longer the
	// recommended shape.
	defaultRuntimeToken := os.Getenv("OTTER_AGENT_RUNTIME_TOKEN")
	if defaultRuntimeToken == "" {
		defaultRuntimeToken = os.Getenv("OTTER_API_TOKEN")
	}
	runtimeToken := fs.String("runtime-token", defaultRuntimeToken,
		"token for the local runtime API; prefer an agent-scoped one (env OTTER_AGENT_RUNTIME_TOKEN, then OTTER_API_TOKEN)")
	dataDir := fs.String("data", os.Getenv("OTTER_DATA_DIR"),
		"runtime data directory, where releases are staged (env OTTER_DATA_DIR)")
	// The identity mechanism. Defaulted from the environment so a unit or a
	// container can select it without a CLI change, but exposed as a flag so
	// `otter agent -h` documents the modes rather than hiding them in the code.
	bootstrapFlag := fs.String("bootstrap", os.Getenv("OTTER_AGENT_BOOTSTRAP"),
		"bootstrap identity provider: aws-instance-role, static-credentials or runtime-secret "+
			"(env OTTER_AGENT_BOOTSTRAP); runtime-secret reads OTTER_AGENT_BOOTSTRAP_SECRET")
	once := fs.Bool("once", false,
		"run one iteration and exit, for tests and for an operator checking connectivity")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	// Everything required is named rather than defaulted. An agent that guessed
	// its runtime id would manage the wrong tenant, and one that guessed a cloud
	// URL would talk to the wrong control plane.
	var missing []string
	if *cloudURL == "" {
		missing = append(missing, "-cloud")
	}
	if *runtimeID == "" {
		missing = append(missing, "-runtime-id")
	}
	if *dataDir == "" {
		missing = append(missing, "-data")
	}
	if len(missing) > 0 {
		fmt.Fprintf(a.Stderr, "otter agent: missing required: %v\n", missing)
		fmt.Fprintf(a.Stderr, "otter agent: the agent does not guess its identity or its control plane\n")
		return 2
	}

	// The bootstrap provider is chosen by the flag (defaulted from environment),
	// which is what keeps the protocol abstract while the AWS implementation is
	// the only one built.
	// Adding a provider means adding a case here and nothing else.
	bootstrap, err := agentBootstrap(
		*bootstrapFlag,
		os.Getenv("OTTER_IMDS_BASE"),
		os.Getenv("OTTER_AGENT_BOOTSTRAP_SECRET"),
	)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter agent: %v\n", err)
		return 2
	}

	creds := &agent.MemoryCredentials{}
	loop := &agent.Loop{
		Control: &agent.HTTPControlPlane{
			BaseURL: *cloudURL,
			Creds:   creds,
			// The control plane refuses a request that does not name a runtime,
			// and this was unset here: the agent bootstrapped successfully and then
			// got "request does not name a runtime" on every poll. The field
			// existed and the method used it; nothing filled it in.
			RuntimeID: *runtimeID,
		},
		Runtime: &agent.RuntimeHTTP{
			BaseURL: *runtimeURL,
			Token:   *runtimeToken,
			// Releases stage into the runtime's own release root, so activation
			// finds exactly what the agent verified.
			ReleaseDir: filepath.Join(*dataDir, "releases"),
			// Releases come from the control plane under the agent's own
			// credential, which is a different secret from the runtime's token.
			ReleaseClient: agent.AuthenticatedClient(*cloudURL, creds),
		},
		Creds:        creds,
		Bootstrap:    bootstrap,
		Exchanger:    &agent.Exchanger{BaseURL: *cloudURL},
		RuntimeID:    *runtimeID,
		AgentVersion: a.Version,
		Drain:        agent.Drain{Timeout: agent.DrainTimeout},
	}

	if *once {
		// One iteration with a short deadline, so an operator can answer "can
		// this host reach its control plane and authenticate" without leaving a
		// process running.
		runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if err := loop.RunOnce(runCtx); err != nil {
			fmt.Fprintf(a.Stderr, "otter agent: %v\n", err)
			return 1
		}
		fmt.Fprintln(a.Stdout, "otter agent: one iteration completed")
		return 0
	}

	// Signals stop the loop cleanly. Ctrl-C must not leave the runtime gated,
	// which is why the loop's cancellation path never gates and returns.
	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := loop.Run(runCtx); err != nil {
		fmt.Fprintf(a.Stderr, "otter agent: %v\n", err)
		return 1
	}
	return 0
}

// agentBootstrap selects the identity mechanism. The name is the protocol's
// provider string, so an agent configured for a provider the control plane does
// not verify fails at bootstrap rather than silently using another one.
//
// Static credentials are selected by their presence, NOT by preference: an agent
// that quietly preferred a key on disk over the instance role would be exactly
// the long-lived-secret problem the instance-role bootstrap exists to avoid. If
// both are configured, the name decides, and an unknown name is an error rather
// than a fallback.
//
// `runtime-secret` is the pooled sidecar's mechanism: it presents a per-runtime
// secret (OTTER_AGENT_BOOTSTRAP_SECRET) that only that tenant's sidecar holds,
// instead of a SigV4 assertion. Selecting it with no secret is refused here, so
// the agent fails to start rather than reaching the control plane with an empty
// proof.
func agentBootstrap(name, imdsBase, secret string) (agent.Bootstrap, error) {
	static, err := agent.StaticCredentialsFromEnv(agent.EnvLookup)
	if err != nil {
		return nil, err
	}
	switch name {
	case "":
		if static != nil {
			return static, nil
		}
		return &agent.AWSInstanceRole{MetadataBase: imdsBase}, nil
	case "aws-instance-role":
		return &agent.AWSInstanceRole{MetadataBase: imdsBase}, nil
	case "static-credentials":
		if static == nil {
			return nil, fmt.Errorf("bootstrap provider %q selected but OTTER_AGENT_STATIC_ACCESS_KEY_ID is not set", name)
		}
		return static, nil
	case "runtime-secret":
		if secret == "" {
			return nil, fmt.Errorf("bootstrap provider %q selected but OTTER_AGENT_BOOTSTRAP_SECRET is not set", name)
		}
		return &agent.RuntimeSecret{Secret: secret}, nil
	default:
		return nil, fmt.Errorf("unknown bootstrap provider %q", name)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
