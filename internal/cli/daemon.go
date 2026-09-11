package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/otter-runtime/otter/internal/config"
	"github.com/otter-runtime/otter/internal/daemon"
	"github.com/otter-runtime/otter/internal/logging"
)

// RunDaemon implements both `otterd` and `otter serve`. It parses the daemon
// flags, installs signal handling and runs the daemon until it shuts down.
func RunDaemon(ctx context.Context, version string, args []string, stdout, stderr io.Writer) int {
	cfg := config.DefaultDaemonConfig(version)

	// Environment first, then flags: flags always win.
	if err := cfg.ApplyEnv(); err != nil {
		fmt.Fprintf(stderr, "otterd: %v\n", err)
		return 2
	}

	fs := flag.NewFlagSet("otterd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg.RegisterFlags(fs)
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: otterd [flags]\n\n")
		fmt.Fprintf(stderr, "Runs the Otter daemon: it discovers integrations, registers their\n")
		fmt.Fprintf(stderr, "triggers, executes them as child processes and serves the HTTP API.\n\n")
		fmt.Fprintf(stderr, "Flags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(stderr, "\nEnvironment:\n")
		fmt.Fprintf(stderr, "  OTTER_INTEGRATIONS_DIR OTTER_DATA_DIR OTTER_LISTEN OTTER_WORKERS\n")
		fmt.Fprintf(stderr, "  OTTER_API_TOKEN OTTER_LOG_FORMAT OTTER_LOG_LEVEL OTTER_SHUTDOWN_GRACE\n")
		fmt.Fprintf(stderr, "  OTTER_SDK_PATH\n")
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "otterd %s\n", version)
		return 0
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "otterd: unexpected arguments: %v\n", fs.Args())
		fs.Usage()
		return 2
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "otterd: %v\n", err)
		return 2
	}

	format, err := logging.ParseFormat(cfg.LogFormat)
	if err != nil {
		fmt.Fprintf(stderr, "otterd: %v\n", err)
		return 2
	}
	level, err := logging.ParseLevel(cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(stderr, "otterd: %v\n", err)
		return 2
	}

	// Daemon logs go to stdout so they can be piped into a log shipper;
	// human-facing flag errors above go to stderr.
	logger := logging.New(stdout, format, level)

	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := daemon.New(signalCtx, daemon.Options{
		Config:  cfg,
		Logger:  logger,
		Version: version,
	})
	if err != nil {
		logger.Error("startup_failed", err)
		return 1
	}

	if err := d.Run(signalCtx); err != nil {
		logger.Error("daemon_failed", err)
		return 1
	}
	return 0
}
