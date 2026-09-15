package deploy

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is everything `otter deploy` needs to converge one host.
type Config struct {
	Target Target

	// RepoRoot is the checkout that gets pushed: the binary, the shared
	// library and every integration discovered under integrations/.
	RepoRoot string

	// IntegrationsPath is the local integrations directory.
	IntegrationsPath string

	// Integrations is the ordered list of integration names to ship.
	Integrations []string

	// EnvFiles maps an integration name to the local file holding its secrets.
	// An integration without an entry is deployed without secrets and warned
	// about.
	EnvFiles map[string]string

	// Version is the build version, reported by the deployed daemon.
	Version string

	// UVVersion overrides the pinned uv release vendored onto the host. The
	// uv version is part of every prepared environment's identity, so changing
	// it rebuilds environments.
	UVVersion string

	// Limited reports that the deploy covers a subset of the integrations, so
	// the push must protect the ones it does not carry.
	Limited bool

	// DaemonEnv is the resolved daemon-wide environment file, empty when the
	// checkout does not have one.
	DaemonEnv string

	// DryRun prints the plan and performs no remote change.
	DryRun bool
	// Verbose streams every remote command.
	Verbose bool
	// Timeout bounds the whole deploy.
	Timeout time.Duration
}

// Flags holds the parsed command line. Defaults are layered so an explicit
// flag always beats the config file, which beats the previous deploy.
type Flags struct {
	Target Target

	EnvFile string
	// DaemonEnv overrides the daemon environment file. Empty uses
	// <repo>/otter.daemon.env.
	DaemonEnv string
	// Integration limits the deploy to one integration. Empty deploys all of
	// them, which is the historical behavior.
	Integration string
	ConfigFor   string
	UV          string
	NoUV        bool
	UVVersion   string
	DryRun      bool
	Verbose     bool
	Timeout     time.Duration

	// set records which flags the operator actually passed.
	set map[string]bool
}

// ConfigFileName is the committed, secret-free deploy configuration.
const ConfigFileName = "otter.deploy.yaml"

// DaemonEnvFileName is the daemon-wide environment file at the repository
// root. It is not committed, because a notification URL or an API token is a
// credential, and it is not per-integration, because a setting such as the
// notification endpoint belongs to the daemon rather than to one integration.
//
// The name is deliberately visible rather than dotted. It is the primary
// configuration surface an operator has to create and edit, so hiding it
// behind a dot -- in every `ls`, every file explorer, and every "show hidden
// files" toggle that defaults to off -- makes the feature harder to find than
// the token inside it is worth. The file is still gitignored, which is what
// actually keeps the credential out of git; the dot added nothing to that.
//
// Locally `make sync-up` sources it. At deploy it becomes
// /etc/otter/daemon.env, which the unit loads alongside each integration's
// secrets file, so one file has one meaning in both places.
const DaemonEnvFileName = "otter.daemon.env"

// DaemonEnvIntegrationName is reserved: an integration of this name would
// write to the same remote path as the daemon-wide environment file.
const DaemonEnvIntegrationName = "daemon"

// deployFile is the on-disk shape of otter.deploy.yaml.
type deployFile struct {
	Host        string `yaml:"host"`
	User        string `yaml:"user"`
	Port        int    `yaml:"port"`
	Identity    string `yaml:"identity"`
	RemoteDir   string `yaml:"remote_dir"`
	Service     string `yaml:"service"`
	ServiceUser string `yaml:"service_user"`
	DataDir     string `yaml:"data_dir"`
	Listen      string `yaml:"listen"`
	EnvFile     string `yaml:"env_file"`
}

// RegisterFlags binds the deploy flags.
func (f *Flags) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&f.Target.Host, "host", "", "SSH destination, e.g. root@203.0.113.10 or an ssh_config alias")
	fs.StringVar(&f.Target.User, "user", "", "SSH login user (default: taken from --host or ssh_config)")
	fs.IntVar(&f.Target.Port, "port", 0, "SSH port (default 22)")
	fs.StringVar(&f.Target.IdentityFile, "identity", "", "private key to use for SSH")
	fs.StringVar(&f.Target.RemoteDir, "remote-dir", "", "remote install root (default "+DefaultRemoteDir+")")
	fs.StringVar(&f.Target.ServiceName, "service", "", "systemd unit name (default "+DefaultService+")")
	fs.StringVar(&f.Target.RunAsUser, "service-user", "", "service account owning the unit and data (default otter)")
	fs.StringVar(&f.Target.DataDir, "data-dir", "", "remote data directory holding otter.db (default <remote-dir>/data)")
	fs.StringVar(&f.Target.Listen, "listen", "", "remote API listen address (default "+DefaultListenAddr+")")
	fs.StringVar(&f.Target.Platform, "platform", "", "remote GOOS/GOARCH; detected over SSH when empty")
	fs.BoolVar(&f.Target.RotateAPIToken, "rotate-token", false, "generate and install a fresh API token")
	fs.StringVar(&f.Target.APIToken, "api-token", "", "use this API token instead of the stored or remote one")
	fs.StringVar(&f.EnvFile, "env-file", "", "secrets file applied to every integration")
	fs.StringVar(&f.DaemonEnv, "daemon-env", "", "daemon-wide environment file (default "+DaemonEnvFileName+")")
	fs.StringVar(&f.Integration, "integration", "", "deploy only this integration, leaving the others untouched")
	fs.StringVar(&f.ConfigFor, "config", "", "deploy config file (default "+ConfigFileName+")")
	fs.StringVar(&f.UV, "uv", "", "uv executable on the host for Python preparation (default the vendored copy)")
	fs.BoolVar(&f.NoUV, "no-uv", false, "do not vendor uv; use one already present on the host")
	fs.StringVar(&f.UVVersion, "uv-version", "", "uv release to vendor (default "+PinnedUVVersion+")")
	fs.BoolVar(&f.DryRun, "dry-run", false, "print what would change and touch nothing")
	fs.BoolVar(&f.Verbose, "verbose", false, "stream every remote command")
	fs.DurationVar(&f.Timeout, "timeout", 10*time.Minute, "overall timeout for the deploy")
}

// ParseDeployFlags parses the arguments of `otter deploy`.
func ParseDeployFlags(args []string, stderr io.Writer) (*Flags, error) {
	f := &Flags{set: map[string]bool{}}

	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f.RegisterFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: otter deploy --host <user@host> [flags]\n")
		fmt.Fprint(stderr, "\nConverges a remote Linux host onto a running Otter runtime over SSH.\n")
		fmt.Fprint(stderr, "No cloud API is involved: if you can ssh to it, you can deploy to it.\n")
		fmt.Fprint(stderr, "\nWhat it does, every time:\n")
		fmt.Fprint(stderr, "  1. detect the remote platform over SSH\n")
		fmt.Fprint(stderr, "  2. cross-compile otterd and otter for it\n")
		fmt.Fprint(stderr, "  3. rsync the binaries and the integration tree\n")
		fmt.Fprint(stderr, "  4. write the systemd unit and the secrets files\n")
		fmt.Fprint(stderr, "  5. restart the service and wait for its health endpoint\n")
		fmt.Fprint(stderr, "\nIt never touches the remote data directory: run history, watermarks\n")
		fmt.Fprint(stderr, "and the extracted Python SDK survive every deploy.\n\n")
		fmt.Fprint(stderr, "Flags:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
	return f, nil
}

// Set reports whether the operator passed a flag explicitly.
func (f *Flags) Set(name string) bool { return f.set[name] }

// LoadConfig assembles the effective configuration.
//
// Precedence, lowest to highest: built-in defaults, the committed deploy
// config file, the previous successful deploy, then the command line. Reusing
// the previous target is what makes a bare `otter deploy` after the first one
// go to the same host, with the platform SSH already told us about.
func LoadConfig(repoRoot string, f *Flags, previous State) (Config, error) {
	cfg := Config{
		RepoRoot:         repoRoot,
		IntegrationsPath: repoRoot + "/" + LocalIntegrationsDir,
		EnvFiles:         map[string]string{},
		DryRun:           f.DryRun,
		Verbose:          f.Verbose,
		Timeout:          f.Timeout,
		Target:           DefaultTarget(),
	}

	// 1. The config file, if present. Committed, and holds no secrets.
	path := f.ConfigFor
	if path == "" {
		path = repoRoot + "/" + ConfigFileName
	}
	fromFile, fileEnv, err := loadConfigFile(path)
	if err != nil {
		return cfg, err
	}
	if fromFile != nil {
		cfg.Target = mergeTarget(cfg.Target, *fromFile)
	}
	// A config file may point at a shared secrets file, but a flag still wins.
	if f.EnvFile == "" && fileEnv != "" {
		f.EnvFile = resolveRelative(repoRoot, fileEnv)
	}

	// 2. The previous deploy, so a bare `otter deploy` after the first one goes
	//    to the same host with the same layout.
	//
	//    Only the *layout* travels when a different host is named. How to reach
	//    a machine -- login user, port, key, detected architecture -- belongs to
	//    that machine, and reusing it elsewhere fails in confusing ways: a key
	//    path that no longer exists, a port that belongs to another service, a
	//    binary built for the wrong architecture. Each of those was hit while
	//    testing against more than one host from a single checkout.
	if previous.Host != "" {
		carried := previous.Target
		if !sameHost(previous, cfg.Target, f) {
			carried = layoutOnly(carried)
		}
		cfg.Target = mergeTarget(cfg.Target, carried)
	}

	// 3. Explicit flags.
	cfg.Target = mergeTarget(cfg.Target, f.Target)
	cfg.Target.ApplyDefaults()

	// 4. The daemon-wide environment file. It is optional: a deployment that
	//    configures nothing beyond per-integration secrets does not need one.
	daemonEnv := f.DaemonEnv
	if daemonEnv == "" {
		daemonEnv = filepath.Join(repoRoot, DaemonEnvFileName)
	}
	daemonEnv = resolveRelative(repoRoot, daemonEnv)
	if _, err := os.Stat(daemonEnv); err == nil {
		cfg.DaemonEnv = daemonEnv
	}

	if err := cfg.resolveIntegrations(f); err != nil {
		return cfg, err
	}

	// The reservation applies to every integration being deployed, not only a
	// filtered one.
	for _, name := range cfg.Integrations {
		if name == DaemonEnvIntegrationName {
			return cfg, fmt.Errorf("integration name %q is reserved for the daemon-wide environment file; rename the integration",
				DaemonEnvIntegrationName)
		}
	}
	return cfg, nil
}

// loadConfigFile reads otter.deploy.yaml. A missing file is not an error: the
// whole configuration can come from flags instead.
func loadConfigFile(path string) (*Target, string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read deploy config %s: %w", path, err)
	}

	var df deployFile
	if err := yaml.Unmarshal(data, &df); err != nil {
		return nil, "", fmt.Errorf("parse deploy config %s: %w", path, err)
	}

	return &Target{
		Host:         df.Host,
		User:         df.User,
		Port:         df.Port,
		IdentityFile: df.Identity,
		RemoteDir:    df.RemoteDir,
		ServiceName:  df.Service,
		RunAsUser:    df.ServiceUser,
		DataDir:      df.DataDir,
		Listen:       df.Listen,
	}, df.EnvFile, nil
}

func resolveRelative(root, path string) string {
	if strings.HasPrefix(path, "/") {
		return path
	}
	return root + "/" + strings.TrimPrefix(path, "./")
}

// layoutOnly strips the fields that describe how to reach a specific machine,
// keeping only the ones that describe how Otter is installed on it.
//
// Where the runtime lives is a deployment decision and travels; how to log in
// is a property of the host and does not.
func layoutOnly(t Target) Target {
	return Target{
		RemoteDir:   t.RemoteDir,
		ServiceName: t.ServiceName,
		RunAsUser:   t.RunAsUser,
		DataDir:     t.DataDir,
		Listen:      t.Listen,
	}
}

// sameHost reports whether the previous deploy and the requested target are the
// same machine. The requested host wins when the operator named one.
func sameHost(previous State, requested Target, f *Flags) bool {
	host := requested.Host
	if f.Target.Host != "" {
		host = f.Target.Host
	}
	return host != "" && host == previous.Host
}

// mergeTarget overlays the fields that are set in over onto base.
func mergeTarget(base, over Target) Target {
	if over.Host != "" {
		base.Host = over.Host
	}
	if over.User != "" {
		base.User = over.User
	}
	if over.Port != 0 {
		base.Port = over.Port
	}
	if over.IdentityFile != "" {
		base.IdentityFile = over.IdentityFile
	}
	if over.RemoteDir != "" {
		base.RemoteDir = over.RemoteDir
	}
	if over.ServiceName != "" {
		base.ServiceName = over.ServiceName
	}
	if over.RunAsUser != "" {
		base.RunAsUser = over.RunAsUser
	}
	if over.DataDir != "" {
		base.DataDir = over.DataDir
	}
	if over.Listen != "" {
		base.Listen = over.Listen
	}
	if over.Platform != "" {
		base.Platform = over.Platform
	}
	if over.APIToken != "" {
		base.APIToken = over.APIToken
	}
	if over.RotateAPIToken {
		base.RotateAPIToken = true
	}
	return base
}

// resolveIntegrations finds the integrations to ship and the secrets file for
// each. `--env-file` wins; otherwise an integration's own .env is used when it
// exists. An integration with no secrets file is deployed anyway, with a
// warning, because the daemon reports the missing variables far more clearly
// than this command can.
func (c *Config) resolveIntegrations(f *Flags) error {
	names, err := c.Target.IntegrationNames(c.RepoRoot)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("no integrations found under %s: nothing to deploy", c.IntegrationsPath)
	}

	// A limited deploy names exactly one integration. It must exist locally:
	// silently deploying nothing would look like success.
	if want := strings.TrimSpace(f.Integration); want != "" {
		found := false
		for _, name := range names {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("integration %q not found under %s (available: %s)",
				want, c.IntegrationsPath, strings.Join(names, ", "))
		}
		names = []string{want}
		c.Limited = true
	}
	c.Integrations = names

	for _, name := range names {
		path := f.EnvFile
		if path == "" {
			candidate := c.IntegrationsPath + "/" + name + "/.env"
			if _, err := os.Stat(candidate); err == nil {
				path = candidate
			}
		}
		if path != "" {
			c.EnvFiles[name] = path
		}
	}
	return nil
}

// Validate checks the whole configuration before anything is built or sent.
func (c *Config) Validate() error {
	if err := c.Target.Validate(); err != nil {
		return err
	}
	if c.RepoRoot == "" {
		return errors.New("repository root is unknown; run otter deploy from inside the checkout")
	}
	if _, err := os.Stat(c.RepoRoot + "/go.mod"); err != nil {
		return fmt.Errorf("no Go module at %s: otter deploy must run from an Otter checkout", c.RepoRoot)
	}
	if c.Timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	return nil
}

// MissingSecrets lists the secret variables a deployment needs but could not
// find, either in an integration's env file or in the local environment.
func (c *Config) MissingSecrets(required map[string][]string) []string {
	var missing []string
	for _, name := range c.Integrations {
		for _, key := range required[name] {
			if _, ok := os.LookupEnv(key); ok {
				continue
			}
			path := c.EnvFiles[name]
			if path == "" {
				missing = append(missing, fmt.Sprintf("%s (no %s found)", key, c.IntegrationsPath+"/"+name+"/.env"))
				continue
			}
			secrets, err := LoadSecrets(path)
			if err != nil {
				missing = append(missing, fmt.Sprintf("%s (%v)", key, err))
				continue
			}
			if _, ok := secrets[key]; !ok {
				missing = append(missing, fmt.Sprintf("%s (absent from %s)", key, path))
			}
		}
	}
	return missing
}

// LoadSecrets reads one integration's secrets file.
//
// The format is deliberately the boring subset of shell that systemd's
// `EnvironmentFile=` actually supports: KEY=value, one per line, optional
// surrounding quotes, `#` comments. Anything more exotic is rejected rather
// than guessed at, because a misread credential is worse than a loud failure.
func LoadSecrets(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	secrets := map[string]string{}
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
			return nil, fmt.Errorf("%s:%d: empty variable name", path, i+1)
		}
		for _, r := range key {
			valid := r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
			if !valid {
				return nil, fmt.Errorf("%s:%d: invalid variable name %q", path, i+1, key)
			}
		}

		value = strings.TrimSpace(value)
		// A trailing inline comment is common in these files and systemd does
		// not strip it, so neither do we: quoting is the only unambiguous way
		// to write a value.
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		secrets[key] = value
	}
	return secrets, nil
}

// RequiredSecrets reads the `secrets:` lists out of the local manifests, so a
// deploy can tell the operator about a credential it will not be able to
// provide.
func (c *Config) RequiredSecrets() map[string][]string {
	required := map[string][]string{}
	for _, name := range c.Integrations {
		path := c.IntegrationsPath + "/" + name + "/otter.yaml"
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var probe struct {
			Secrets []string `yaml:"secrets"`
		}
		if err := yaml.Unmarshal(data, &probe); err != nil {
			continue
		}
		required[name] = probe.Secrets
	}
	return required
}
