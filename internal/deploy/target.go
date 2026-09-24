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
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/uuid"
)

// Defaults for a deploy target.
const (
	DefaultRemoteDir = "/opt/otter"
	// DefaultServicePrefix names the systemd unit of a workspace: the unit is
	// <prefix>-<workspace>. One host can hold several workspaces, so a single
	// fixed unit name would make them fight over the same service.
	DefaultServicePrefix = "otterd"
	DefaultSSHPort       = 22

	// DefaultListenPort is the first loopback port offered to a workspace. Each
	// workspace gets its own, so two daemons run side by side on one host.
	DefaultListenPort = 7337
	// DefaultListenHost keeps every daemon off the network: the API is reached
	// through an SSH tunnel, never an open port.
	DefaultListenHost = "127.0.0.1"

	// DefaultListenAddr is the address of the first workspace on a host.
	DefaultListenAddr = "127.0.0.1:7337"

	// WorkspacesDirName is the directory under RemoteDir holding one
	// subdirectory per workspace.
	WorkspacesDirName = "workspaces"

	// WorkspaceRecordName is the per-workspace record kept on the host. It is
	// how a deploy from any machine finds the workspace's unit and port, and
	// how a new workspace picks a port nobody else holds.
	WorkspaceRecordName = "workspace.json"

	// StateDirName is the per-checkout directory holding deploy state. It is
	// gitignored and holds a bearer token, so it is private to the operator.
	StateDirName = ".otter"

	// StateFileName is the state file inside that directory.
	StateFileName = "deploy.json"

	// LocalIntegrationsDir is the directory name integrations are deployed
	// under, inside a workspace. It is the discovery root the host's runtime
	// scans, and it is fixed so that a manifest's relative python.path keeps
	// resolving the same way in every workspace.
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

	// WorkspaceID is the durable identity of the workspace this deploy owns on
	// the host. It is what makes a deploy from another machine, checkout or
	// directory update the same tree instead of creating a second one.
	WorkspaceID string `json:"workspace_id,omitempty"`
	// WorkspaceSlug is the human-facing half of the workspace name: the
	// project directory by default, or whatever --workspace was given.
	WorkspaceSlug string `json:"workspace_slug,omitempty"`
	// WorkspaceNameOverride is the directory name recorded on the host. It is
	// normally derived from the slug and id; carrying it explicitly means a
	// record written by a different version still resolves to the same
	// directory, rather than to a second one.
	WorkspaceNameOverride string `json:"workspace_name,omitempty"`

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

// WorkspaceName is the name a workspace is known by on the host: the directory
// under RemoteDir/workspaces, the systemd unit suffix and the environment file
// base. It combines the human slug with a short, stable id, so two projects
// called the same thing never collide and a renamed project does not orphan its
// host tree.
func (t Target) WorkspaceName() string {
	if name := strings.TrimSpace(t.WorkspaceNameOverride); name != "" {
		return name
	}
	slug := SanitizeSlug(t.WorkspaceSlug)
	short := shortID(t.WorkspaceID)
	switch {
	case slug == "" && short == "":
		return "workspace"
	case slug == "":
		return short
	case short == "":
		return slug
	}
	return slug + "-" + short
}

// WorkspaceDir is the private directory of this workspace on the host.
func (t Target) WorkspaceDir() string {
	return filepath.Join(t.RemoteDir, WorkspacesDirName, t.WorkspaceName())
}

// WorkspacesRoot is the directory holding every workspace on the host.
func (t Target) WorkspacesRoot() string { return filepath.Join(t.RemoteDir, WorkspacesDirName) }

// WorkspaceRecordPath is where the host keeps this workspace's record.
func (t Target) WorkspaceRecordPath() string {
	return filepath.Join(t.WorkspaceDir(), WorkspaceRecordName)
}

// ShortID is the abbreviated workspace identity used in names.
func (t Target) ShortID() string { return shortID(t.WorkspaceID) }

// fillWorkspaceDefaults completes the values that follow from the workspace
// name. It runs once the workspace is settled, which is on the host: the name
// can change when an existing workspace is adopted, and every path below it has
// to change with it.
func (t Target) fillWorkspaceDefaults() Target {
	if strings.TrimSpace(t.WorkspaceID) == "" {
		t.WorkspaceID = NewWorkspaceID()
	}
	if t.ServiceName == "" {
		t.ServiceName = DefaultServicePrefix + "-" + t.WorkspaceName()
	}
	if t.DataDir == "" {
		t.DataDir = filepath.Join(t.WorkspaceDir(), StateDirName, "data")
	}
	return t
}

// EnvDir, UnitPath, BinaryPath and CLIPath are derived remote paths. Both
// binaries and the vendored toolchain live inside the workspace, so upgrading
// one workspace's runtime never changes what another one executes.
func (t Target) EnvDir() string     { return "/etc/otter/" + WorkspacesDirName }
func (t Target) UnitPath() string   { return "/etc/systemd/system/" + t.ServiceUnit() }
func (t Target) BinaryPath() string { return filepath.Join(t.WorkspaceDir(), "bin", "otterd") }
func (t Target) CLIPath() string    { return filepath.Join(t.WorkspaceDir(), "bin", "otter") }

// IntegrationsDir is the discovery root handed to `otter release`. Each
// workspace has its own, so an integration name only has to be unique within
// the workspace that owns it.
func (t Target) IntegrationsDir() string {
	return filepath.Join(t.WorkspaceDir(), LocalIntegrationsDir)
}

// ToolsDir holds the vendored uv for this workspace.
func (t Target) ToolsDir() string { return filepath.Join(t.WorkspaceDir(), "tools") }

// SharedEnvFilePath is the workspace's credentials file. It is named for the
// workspace, so two workspaces cannot overwrite each other's secrets -- the
// failure that made a shared host unsafe before.
func (t Target) SharedEnvFilePath() string {
	return filepath.Join(t.EnvDir(), t.WorkspaceName()+".env")
}

// DaemonEnvFilePath is the workspace's daemon-wide settings file. Daemon
// settings are per daemon, and each workspace has its own daemon, so this is
// per workspace for the same reason the secrets are.
func (t Target) DaemonEnvFilePath() string {
	return filepath.Join(t.EnvDir(), t.WorkspaceName()+".daemon.env")
}

// NewWorkspaceID mints the durable identity of a workspace. It is recorded on
// the host and in the project's deploy state, so the second deploy -- from this
// machine, another checkout or another directory -- finds the same workspace.
func NewWorkspaceID() string { return uuid.NewString() }

// shortID is the first eight hex characters of a workspace id, which is enough
// to disambiguate the handful of workspaces a host will ever hold while staying
// short enough to read in a unit name.
func shortID(id string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
			return r
		default:
			return -1
		}
	}, strings.TrimSpace(id))
	if len(clean) > 8 {
		clean = clean[:8]
	}
	return strings.ToLower(clean)
}

// SanitizeSlug turns a project directory name into something safe for a
// directory, a systemd unit and an environment file name. It is deliberately
// lossy: the id carries identity, the slug only has to be recognisable.
func SanitizeSlug(slug string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(slug)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// DefaultTarget returns the target used when nothing else specifies one.
func DefaultTarget() Target {
	return Target{
		Port:        DefaultSSHPort,
		RemoteDir:   DefaultRemoteDir,
		RunAsUser:   "otter",
		WorkspaceID: NewWorkspaceID(),
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
	if t.RunAsUser == "" {
		t.RunAsUser = d.RunAsUser
	}
	if strings.TrimSpace(t.WorkspaceID) == "" {
		t.WorkspaceID = d.WorkspaceID
	}
	if t.WorkspaceSlug == "" {
		t.WorkspaceSlug = "workspace"
	}
	// The unit is named after the workspace, so two workspaces on one host
	// never contend for one service name. --service still overrides.
	if t.ServiceName == "" {
		t.ServiceName = DefaultServicePrefix + "-" + t.WorkspaceName()
	}
	// Data lives inside the workspace so each daemon owns its own SQLite,
	// identities and prepared environments.
	if t.DataDir == "" {
		t.DataDir = filepath.Join(t.WorkspaceDir(), StateDirName, "data")
	}
	// Listen is deliberately not defaulted. A new workspace's port is chosen on
	// the host, where the ports already in use are known; inventing 7337 here
	// would collide with the first workspace on every host.
	//
	// Platform is deliberately not defaulted either: an empty platform means
	// "detect it over SSH", and inventing a value would silently turn that into
	// a cross-compile for whatever machine happens to run the command.
}

// defaultPlatform is the initial guess, always confirmed by `uname` over SSH
// before a build happens.
func defaultPlatform() string {
	return runtime.GOOS + "/" + runtime.GOARCH
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
	// The unit name and the data directory are derived from the workspace, and
	// the workspace's identity is minted if the project has none; all three are
	// filled in before anything is built. Validating only what was set keeps a
	// hand-written config honest without demanding values the tool owns.
	if t.ServiceName != "" && strings.ContainsAny(t.ServiceName, "/ \t") {
		return fmt.Errorf("--service must be a plain unit name, got %q", t.ServiceName)
	}
	if strings.TrimSpace(t.RunAsUser) == "" || strings.ContainsAny(t.RunAsUser, ":/ \t") {
		return fmt.Errorf("--service-user must be a plain user name, got %q", t.RunAsUser)
	}
	if t.DataDir != "" && !strings.HasPrefix(t.DataDir, "/") {
		return fmt.Errorf("--data-dir must be an absolute path, got %q", t.DataDir)
	}
	if strings.ContainsAny(t.DataDir, " \t") {
		return fmt.Errorf("--data-dir must not contain whitespace, got %q", t.DataDir)
	}
	// An empty listen address is resolved on the host, where the ports already
	// in use are known. Refusing it here would make every new workspace on a
	// busy host impossible to create.
	if strings.TrimSpace(t.Listen) != "" {
		if err := validateListen(t.Listen); err != nil {
			return err
		}
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
