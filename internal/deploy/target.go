// Package deploy implements `otter deploy`: pushing a built Otter runtime to a
// remote Linux host over SSH.
//
// It is deliberately provider-agnostic. There is no cloud API client here and
// no infrastructure-as-code engine: the only remote capability assumed is SSH
// plus rsync, which every VPS, EC2 instance, Lightsail box and bare-metal
// machine already has. Choosing and provisioning the host is the operator's
// job; installing Otter onto it is this package's job.
//
// The deployment is a converge rather than a list of one-off actions: running
// it twice with the same inputs does the same thing and reports that nothing
// changed the second time. Everything it remembers about the remote lives in a
// small state file under .otter/.
package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Defaults for a deploy target.
const (
	DefaultRemoteDir = "/opt/otter"
	DefaultService   = "otterd"
	DefaultSSHPort   = 22

	// DefaultListenAddr is the address the remote daemon binds. Loopback only:
	// the CLI reaches it over an SSH tunnel, which means no open port, no
	// firewall rule and no TLS certificate.
	DefaultListenAddr = "127.0.0.1:7337"

	// StateDirName is the per-checkout directory holding deploy state. It is
	// gitignored and holds a bearer token, so it is private to the operator.
	StateDirName = ".otter"

	// StateFileName is the state file inside that directory.
	StateFileName = "deploy.json"

	// LocalIntegrationsDir is the integrations directory inside the repository.
	LocalIntegrationsDir = "integrations"
)

// Target describes where and how to deploy.
type Target struct {
	// Host is the SSH destination: an address or an alias from ~/.ssh/config.
	Host string `json:"host"`
	// User is the SSH login user. Empty means "let ssh decide".
	User string `json:"user,omitempty"`
	// Port is the SSH port.
	Port int `json:"port,omitempty"`
	// IdentityFile is an optional private key passed to ssh and rsync.
	IdentityFile string `json:"identity_file,omitempty"`

	// RemoteDir is the root of the deployed runtime on the remote host.
	RemoteDir string `json:"remote_dir"`
	// ServiceName is the systemd unit name, without the .service suffix.
	ServiceName string `json:"service_name"`
	// RunAsUser is the owner of both the unit and the data directory.
	RunAsUser string `json:"run_as_user"`
	// DataDir is the remote SQLite and SDK directory.
	DataDir string `json:"data_dir"`
	// Listen is the daemon listen address on the remote host.
	Listen string `json:"listen"`

	// Platform is the remote GOOS/GOARCH. It is detected over SSH before
	// anything is built, so a local guess can never produce a binary for the
	// wrong machine.
	Platform string `json:"platform"`

	// APIToken is the bearer token the remote daemon will require. It is never
	// written to the state file; it lives in StateStore's separate secret file.
	APIToken string `json:"-"`
	// RotateAPIToken forces a fresh token instead of reusing the remote one.
	RotateAPIToken bool `json:"-"`
}

// String renders the target as user@host:port for messages.
func (t Target) String() string {
	dest := t.Host
	if t.User != "" {
		dest = t.User + "@" + dest
	}
	if t.Port != 0 && t.Port != DefaultSSHPort {
		return fmt.Sprintf("%s:%d", dest, t.Port)
	}
	return dest
}

// ServiceUnit is the systemd unit name.
func (t Target) ServiceUnit() string { return t.ServiceName + ".service" }

// APIURL is the loopback URL the daemon serves on the remote host.
func (t Target) APIURL() string { return "http://" + t.Listen }

// EnvDir, UnitPath, BinaryPath and CLIPath are derived remote paths.
func (t Target) EnvDir() string     { return "/etc/otter" }
func (t Target) UnitPath() string   { return "/etc/systemd/system/" + t.ServiceUnit() }
func (t Target) BinaryPath() string { return filepath.Join(t.RemoteDir, "bin", "otterd") }
func (t Target) CLIPath() string    { return filepath.Join(t.RemoteDir, "bin", "otter") }

// IntegrationsDir is where the integration tree lands inside RemoteDir. It has
// to sit next to lib/, because an integration's manifest points at the shared
// library with a relative path such as ../../lib/python.
func (t Target) IntegrationsDir() string { return t.RemoteDir + "/integrations" }

// EnvFilePath is the EnvironmentFile for one integration. Secrets never take
// part in a command line: this file is written over SSH stdin.
func (t Target) EnvFilePath(integration string) string {
	return filepath.Join(t.EnvDir(), integration+".env")
}

// DaemonEnvFilePath is the daemon-wide environment file. It is separate from
// the per-integration files because its settings belong to the daemon:
// notification endpoints, log level, retention. Putting them in an
// integration's secrets file made them look like that integration's
// credentials and gave them the wrong lifetime.
func (t Target) DaemonEnvFilePath() string {
	return filepath.Join(t.EnvDir(), "daemon.env")
}

// DefaultTarget returns the target used when nothing else specifies one.
func DefaultTarget() Target {
	return Target{
		Port:        DefaultSSHPort,
		RemoteDir:   DefaultRemoteDir,
		ServiceName: DefaultService,
		RunAsUser:   "otter",
		DataDir:     filepath.Join(DefaultRemoteDir, "data"),
		Listen:      DefaultListenAddr,
	}
}

// SplitHostUser separates the user embedded in an SSH destination from the
// host, so `--host root@10.0.0.1` and `--user root --host 10.0.0.1` behave
// identically. Without this the login user is only known to ssh, and every
// decision that depends on it -- notably whether to prepend sudo -- is made
// against an empty value.
func (t *Target) SplitHostUser() {
	user, host, ok := strings.Cut(t.Host, "@")
	if !ok {
		return
	}
	// A malformed destination is left alone so validation can report it.
	if strings.TrimSpace(host) == "" {
		return
	}
	if t.User == "" && strings.TrimSpace(user) != "" {
		t.User = user
	}
	// The destination must not keep the login either way: ssh would be handed
	// a host it cannot resolve.
	t.Host = host
}

// LoginIsRoot reports whether the SSH login is known to be root.
func (t Target) LoginIsRoot() bool { return t.User == "root" }

// ApplyDefaults fills every empty field in place, so a target assembled from
// flags behaves exactly like one loaded from the state file.
func (t *Target) ApplyDefaults() {
	t.SplitHostUser()
	d := DefaultTarget()
	if t.Port == 0 {
		t.Port = d.Port
	}
	if t.RemoteDir == "" {
		t.RemoteDir = d.RemoteDir
	}
	if t.ServiceName == "" {
		t.ServiceName = d.ServiceName
	}
	if t.RunAsUser == "" {
		t.RunAsUser = d.RunAsUser
	}
	if t.DataDir == "" {
		t.DataDir = filepath.Join(t.RemoteDir, "data")
	}
	if t.Listen == "" {
		t.Listen = d.Listen
	}
	// Platform is deliberately not defaulted here. An empty platform means
	// "detect it over SSH", and inventing a value would silently turn that
	// into a cross-compile for whatever machine happens to run the command.
}

// defaultPlatform is the initial guess, always confirmed by `uname` over SSH
// before a build happens.
func defaultPlatform() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// IntegrationNames returns the integrations that have an otter.yaml locally.
func (t Target) IntegrationNames(repoRoot string) ([]string, error) {
	dir := filepath.Join(repoRoot, LocalIntegrationsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read integrations directory %s: %w", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "otter.yaml")); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// Validate checks the target before any network access happens.
func (t *Target) Validate() error {
	if strings.TrimSpace(t.Host) == "" {
		return fmt.Errorf("--host is required, for example --host root@203.0.113.10")
	}
	if strings.ContainsAny(t.Host, " \t\n") {
		return fmt.Errorf("--host must not contain whitespace, got %q", t.Host)
	}
	if strings.Contains(t.Host, "@") {
		return fmt.Errorf("--host still contains a login user (%q); use --user for the login", t.Host)
	}
	if t.Port < 1 || t.Port > 65535 {
		return fmt.Errorf("--port must be between 1 and 65535, got %d", t.Port)
	}
	if !strings.HasPrefix(t.RemoteDir, "/") || strings.TrimSpace(t.RemoteDir) == "" {
		return fmt.Errorf("--remote-dir must be an absolute path, got %q", t.RemoteDir)
	}
	if strings.ContainsAny(t.RemoteDir, " \t") {
		return fmt.Errorf("--remote-dir must not contain whitespace, got %q", t.RemoteDir)
	}
	if strings.TrimSpace(t.ServiceName) == "" || strings.ContainsAny(t.ServiceName, "/ \t") {
		return fmt.Errorf("--service must be a plain unit name, got %q", t.ServiceName)
	}
	if strings.TrimSpace(t.RunAsUser) == "" || strings.ContainsAny(t.RunAsUser, ":/ \t") {
		return fmt.Errorf("--service-user must be a plain user name, got %q", t.RunAsUser)
	}
	if !strings.HasPrefix(t.DataDir, "/") {
		return fmt.Errorf("--data-dir must be an absolute path, got %q", t.DataDir)
	}
	if strings.ContainsAny(t.DataDir, " \t") {
		return fmt.Errorf("--data-dir must not contain whitespace, got %q", t.DataDir)
	}
	if err := validateListen(t.Listen); err != nil {
		return err
	}
	// An empty platform is not an error: it means Run will ask the host.
	if t.Platform != "" {
		if _, _, err := ParsePlatform(t.Platform); err != nil {
			return err
		}
	}
	return nil
}

func validateListen(listen string) error {
	if strings.TrimSpace(listen) == "" {
		return fmt.Errorf("--listen must not be empty")
	}
	if strings.ContainsAny(listen, " \t") {
		return fmt.Errorf("--listen must not contain whitespace, got %q", listen)
	}
	host, port, ok := strings.Cut(listen, ":")
	if !ok || port == "" {
		return fmt.Errorf("--listen must be host:port, got %q", listen)
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return fmt.Errorf("--listen port must be numeric, got %q", listen)
		}
	}
	// An API reachable from the internet with a static bearer token is exactly
	// the deployment this feature exists to avoid. An empty host means ":7337",
	// which binds every interface.
	switch host {
	case "", "0.0.0.0", "::", "[::]", "*":
		return fmt.Errorf("refusing a wildcard listen address %q: bind to %s and reach "+
			"the API through an SSH tunnel", listen, DefaultListenAddr)
	}
	return nil
}

// ParsePlatform splits and validates a "goos/goarch" platform string.
func ParsePlatform(s string) (goos, goarch string, err error) {
	fields := strings.Split(strings.TrimSpace(s), "/")
	if len(fields) != 2 || fields[0] == "" || fields[1] == "" {
		return "", "", fmt.Errorf("--platform must be GOOS/GOARCH such as linux/arm64, got %q", s)
	}
	goos, goarch = fields[0], fields[1]

	switch goos {
	case "linux", "darwin":
	default:
		return "", "", fmt.Errorf("unsupported GOOS %q: otter deploy targets linux", goos)
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return "", "", fmt.Errorf("unsupported GOARCH %q: otter deploy builds amd64 or arm64", goarch)
	}
	return goos, goarch, nil
}
