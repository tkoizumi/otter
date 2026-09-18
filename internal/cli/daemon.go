package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/otter-runtime/otter/internal/config"
	"github.com/otter-runtime/otter/internal/daemon"
	"github.com/otter-runtime/otter/internal/logging"
)

// flagWasSet reports whether a flag appeared on the command line, so a value
// derived from another source is only overwritten when the operator asked.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

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
	notifyOn := fs.String("notify-on", strings.Join(cfg.Notify.On, ","), "comma-separated terminal statuses that notify (default: every failure)")
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

	// --notify-on is registered as a string so it can be given as a
	// comma-separated list; fold it into the configuration before validating.
	if flagWasSet(fs, "notify-on") {
		cfg.Notify.On = config.SplitList(*notifyOn)
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
		// Record where this daemon can be reached, beside the data it serves,
		// so the developer's next command in this project is `otter run x`
		// rather than a copy-pasted --api URL. The address is the one the
		// kernel actually bound, which is why the API server reports it back
		// instead of the CLI guessing from the requested flag.
		OnReady: func(addr string) {
			base := listenAPIURL(addr)
			// A daemon started by `otter start` is told which project it
			// serves, so the record goes there and survives any --data. A
			// daemon started by hand has no project, and the data directory is
			// the only place it could reasonably put one.
			record := serveDir(os.Getenv(ProjectRootEnvName), cfg.DataDir)
			if err := writeListenURLFile(record, base); err != nil {
				logger.Warn("listen_url_not_recorded", "error", err.Error(), "record", record)
			} else {
				logger.Info("listen_url_recorded",
					"url", base,
					"file", filepath.Join(record, ListenURLFileName))
			}
			// The pid goes beside it so `otter stop` stops the runtime that
			// serves this project instead of guessing from a process name.
			// A detached child is the exception: its parent recorded the pid
			// before returning, and overwriting it with the same value would
			// only create a window where neither is current.
			if os.Getenv(pidRecordedEnvName) == "" {
				if err := writeServePID(record, os.Getpid()); err != nil {
					logger.Warn("serve_pid_not_recorded", "error", err.Error(), "record", record)
				}
			}
		},
	})
	if err != nil {
		logger.Error("startup_failed", err)
		return 1
	}

	if err := d.Run(signalCtx); err != nil {
		logger.Error("daemon_failed", err)
		return 1
	}
	// The listener is closed by now, so the record would only send the next
	// command to a port nothing is serving.
	removeListenURLFile(cfg.DataDir)
	return 0
}
