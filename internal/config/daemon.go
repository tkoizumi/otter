package config

import (
	"flag"
	"fmt"
	"net"
	"net/url"
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

	// Notify is where a failed run is reported. Empty disables notification
	// entirely, which is the default: no configuration means no outbound
	// traffic of any kind.
	Notify NotifyConfig
}

// NotifyConfig describes an outbound failure notification.
//
// Only failures are reported. Alerting on success doubles the message volume
// and trains the reader to ignore it, which is worse than not alerting.
type NotifyConfig struct {
	// URL is the endpoint a failure is POSTed to. It is redacted everywhere
	// the daemon reports its configuration, because providers such as Slack
	// embed a secret in the path.
	URL string

	// On lists the terminal statuses that notify. Empty means every
	// non-succeeded terminal status.
	On []string

	// Format is the request body shape. The default is Otter's own JSON, which
	// any endpoint can parse; the chat services each require their own shape
	// and reject anything else.
	Format string

	// Timeout bounds one delivery attempt. Zero means the default.
	Timeout time.Duration
}

// Notification body formats.
const (
	// FormatJSON is Otter's own payload: every field, machine-readable. It is
	// what an endpoint of your own, healthchecks.io or a bridge expects.
	FormatJSON = "json"
	// FormatSlack is a Slack incoming webhook: {"text": "..."}. Slack rejects
	// a body without "text" with 400 invalid_payload.
	FormatSlack = "slack"
	// FormatDiscord is a Discord webhook: {"content": "..."}.
	FormatDiscord = "discord"
	// FormatTeams is a Microsoft Teams incoming webhook MessageCard.
	FormatTeams = "teams"
)

// NotifyFormats lists the accepted format names.
func NotifyFormats() []string {
	return []string{FormatJSON, FormatSlack, FormatDiscord, FormatTeams}
}

// FormatOrJSON returns the configured format, defaulting to JSON.
func (n NotifyConfig) FormatOrJSON() string {
	if strings.TrimSpace(n.Format) == "" {
		return FormatJSON
	}
	return strings.ToLower(strings.TrimSpace(n.Format))
}

// ValidateFormat checks the configured format name.
func (n NotifyConfig) ValidateFormat() error {
	switch n.FormatOrJSON() {
	case FormatJSON, FormatSlack, FormatDiscord, FormatTeams:
		return nil
	default:
		return fmt.Errorf("--notify-format accepts %s, got %q",
			strings.Join(NotifyFormats(), ", "), n.Format)
	}
}

// IsChatFormat reports whether the body is shaped for a chat service, in which
// case the endpoint is expected to be that service's webhook rather than a
// generic JSON consumer.
func (n NotifyConfig) IsChatFormat() bool {
	switch n.FormatOrJSON() {
	case FormatSlack, FormatDiscord, FormatTeams:
		return true
	default:
		return false
	}
}

// Enabled reports whether notifications are configured at all.
func (n NotifyConfig) Enabled() bool { return strings.TrimSpace(n.URL) != "" }

// Matches reports whether a terminal status should be reported.
//
// Succeeded is never reported: the caller passes only failures, and an
// allow-list that mentioned it would still be ignored.
func (n NotifyConfig) Matches(status string) bool {
	if !n.Enabled() || status == "succeeded" {
		return false
	}
	if len(n.On) == 0 {
		return true
	}
	for _, want := range n.On {
		if strings.EqualFold(strings.TrimSpace(want), status) {
			return true
		}
	}
	return false
}

// RedactedURL is the URL with any embedded secret removed, for logging and
// diagnostics.
func (n NotifyConfig) RedactedURL() string {
	if !n.Enabled() {
		return ""
	}
	return redactURL(n.URL)
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
	if v, ok := os.LookupEnv("OTTER_NOTIFY_URL"); ok && v != "" {
		c.Notify.URL = v
	}
	if v, ok := os.LookupEnv("OTTER_NOTIFY_ON"); ok && strings.TrimSpace(v) != "" {
		c.Notify.On = SplitList(v)
	}
	if v, ok := os.LookupEnv("OTTER_NOTIFY_FORMAT"); ok && strings.TrimSpace(v) != "" {
		c.Notify.Format = strings.ToLower(strings.TrimSpace(v))
	}
	return nil
}

// SplitList parses a comma-separated setting, dropping empty entries.
func SplitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// redactURL removes the path and query from a URL, keeping the scheme and host
// so the setting is still identifiable in a log line.
func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "(configured)"
	}
	return parsed.Scheme + "://" + parsed.Host + "/..."
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
	fs.StringVar(&c.Notify.URL, "notify-url", c.Notify.URL, "POST failed runs to this URL (empty disables notification)")
	fs.StringVar(&c.Notify.Format, "notify-format", c.Notify.Format,
		"notification body format: "+strings.Join(NotifyFormats(), ", "))
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

	if c.Notify.Enabled() {
		parsed, err := url.Parse(c.Notify.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("--notify-url must be an http(s) URL, got %q", redactURL(c.Notify.URL))
		}
		for _, status := range c.Notify.On {
			switch status {
			case "failed", "timed_out", "cancelled":
			default:
				return fmt.Errorf("--notify-on accepts failed, timed_out or cancelled, got %q", status)
			}
		}
		if err := c.Notify.ValidateFormat(); err != nil {
			return err
		}
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
