package config

import (
	"flag"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Daemon defaults.
const (
	DefaultListen        = "127.0.0.1:7337"
	DefaultDataDir       = "./tmp"
	DefaultIntegrations  = "./integrations"
	DefaultShutdownGrace = 15 * time.Second
	DefaultLogFormat     = "json"
	DefaultLogLevel      = "info"
	maxDefaultWorkers    = 8
)

// DaemonConfig is the runtime configuration of otterd. Values come from
// defaults, then environment variables, then command-line flags.
type DaemonConfig struct {
	IntegrationsDir string
	DataDir         string
	Listen          string
	Workers         int
	APIToken        string
	LogFormat       string
	LogLevel        string
	ShutdownGrace   time.Duration

	// SDKPath overrides the directory prepended to the child process
	// PYTHONPATH. When empty the daemon extracts its embedded Python SDK into
	// the data directory and uses that.
	SDKPath string

	// Version is the build version, reported by /health and the CLI.
	Version string
}

// DefaultDaemonConfig returns the documented defaults.
func DefaultDaemonConfig(version string) DaemonConfig {
	return DaemonConfig{
		IntegrationsDir: DefaultIntegrations,
		DataDir:         DefaultDataDir,
		Listen:          DefaultListen,
		Workers:         DefaultWorkers(),
		LogFormat:       DefaultLogFormat,
		LogLevel:        DefaultLogLevel,
		ShutdownGrace:   DefaultShutdownGrace,
		Version:         version,
	}
}

// DefaultWorkers is the number of CPU cores, capped at a sensible maximum.
func DefaultWorkers() int {
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	if n > maxDefaultWorkers {
		n = maxDefaultWorkers
	}
	return n
}

// ApplyEnv overlays OTTER_* environment variables onto the configuration.
// Malformed values are returned as errors rather than ignored.
func (c *DaemonConfig) ApplyEnv() error {
	if v, ok := os.LookupEnv("OTTER_INTEGRATIONS_DIR"); ok && v != "" {
		c.IntegrationsDir = v
	}
	if v, ok := os.LookupEnv("OTTER_DATA_DIR"); ok && v != "" {
		c.DataDir = v
	}
	if v, ok := os.LookupEnv("OTTER_LISTEN"); ok && v != "" {
		c.Listen = v
	}
	if v, ok := os.LookupEnv("OTTER_WORKERS"); ok && v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("OTTER_WORKERS must be an integer, got %q", v)
		}
		c.Workers = n
	}
	if v, ok := os.LookupEnv("OTTER_API_TOKEN"); ok {
		c.APIToken = v
	}
	if v, ok := os.LookupEnv("OTTER_LOG_FORMAT"); ok && v != "" {
		c.LogFormat = v
	}
	if v, ok := os.LookupEnv("OTTER_LOG_LEVEL"); ok && v != "" {
		c.LogLevel = v
	}
	if v, ok := os.LookupEnv("OTTER_SHUTDOWN_GRACE"); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("OTTER_SHUTDOWN_GRACE must be a duration such as 15s, got %q", v)
		}
		c.ShutdownGrace = d
	}
	if v, ok := os.LookupEnv("OTTER_SDK_PATH"); ok && v != "" {
		c.SDKPath = v
	}
	return nil
}

// RegisterFlags binds daemon flags, seeding each default from the current
// configuration so that flags win over environment variables.
func (c *DaemonConfig) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.IntegrationsDir, "integrations", c.IntegrationsDir, "root directory scanned recursively for "+ManifestFileName)
	fs.StringVar(&c.DataDir, "data", c.DataDir, "data directory holding otter.db and the extracted Python SDK")
	fs.StringVar(&c.Listen, "listen", c.Listen, "HTTP API listen address (host:port)")
	fs.IntVar(&c.Workers, "workers", c.Workers, "maximum number of concurrently running integrations")
	fs.StringVar(&c.APIToken, "api-token", c.APIToken, "bearer token required for API access (mandatory when --listen is not loopback)")
	fs.StringVar(&c.LogFormat, "log-format", c.LogFormat, "daemon log format: json or pretty")
	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "daemon log level: debug, info, warn or error")
	fs.DurationVar(&c.ShutdownGrace, "shutdown-grace", c.ShutdownGrace, "how long running integrations may finish after SIGTERM before being terminated")
	fs.StringVar(&c.SDKPath, "sdk-path", c.SDKPath, "directory prepended to the child PYTHONPATH (defaults to the embedded SDK extracted into the data directory)")
}

// Validate checks the configuration before the daemon starts.
func (c *DaemonConfig) Validate() error {
	if strings.TrimSpace(c.IntegrationsDir) == "" {
		return fmt.Errorf("--integrations must not be empty")
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return fmt.Errorf("--data must not be empty")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("--listen must be host:port, got %q: %w", c.Listen, err)
	}
	if c.Workers < 1 {
		return fmt.Errorf("--workers must be at least 1, got %d", c.Workers)
	}
	if c.ShutdownGrace < 0 {
		return fmt.Errorf("--shutdown-grace must not be negative")
	}

	switch c.LogFormat {
	case "json", "pretty":
	default:
		return fmt.Errorf("--log-format must be json or pretty, got %q", c.LogFormat)
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("--log-level must be one of debug, info, warn, error; got %q", c.LogLevel)
	}

	// Binding to anything other than loopback without a token would expose
	// arbitrary execution to the network, so refuse to start.
	if !c.ListenIsLoopback() && strings.TrimSpace(c.APIToken) == "" {
		return fmt.Errorf("refusing to listen on %s without an API token: set --api-token or OTTER_API_TOKEN, or bind to %s",
			c.Listen, DefaultListen)
	}

	return nil
}

// ListenIsLoopback reports whether the API is reachable only from this host.
func (c *DaemonConfig) ListenIsLoopback() bool {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return false
	}
	return isLoopbackHost(host)
}

func isLoopbackHost(host string) bool {
	if host == "" {
		return false // ":7337" binds every interface
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// ChildAPIURL is the API base URL handed to integration processes. Children
// always talk to a concrete loopback address, never to a wildcard bind.
func (c *DaemonConfig) ChildAPIURL() string {
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return "http://" + DefaultListen
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
