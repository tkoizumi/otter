package deploy

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tkoizumi/otter/internal/config"
)

// Config is everything `otter deploy` needs to converge one host.
type Config struct {
	Target Target

	// ProjectRoot is the workspace being deployed: the nearest ancestor of the
	// working directory carrying .otter, .git or go.mod. It is scanned
	// recursively for integration manifests, and it is where otter.deploy.yaml,
	// otter.env and .otter/ live.
	//
	// It is a project, not necessarily a Go checkout. A workspace that holds
	// only Python integrations deploys from released binaries; one that
	// contains cmd/otterd is compiled from source.
	ProjectRoot string

	// Integrations is the ordered list of integrations to ship, each with the
	// local directory it lives in and the shared trees its manifest declares.
	Integrations []Integration

	// SharedEnv is the resolved environment file holding credentials that every
	// integration may use. Empty means the checkout has none; the deploy
	// proceeds anyway, because the daemon reports a missing secret far more
	// clearly than this command can.
	SharedEnv string

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

// Integration is one integration to deploy, resolved from the workspace.
//
// It carries the two things staging needs and that a bare name cannot express:
// where the integration lives locally, and which shared trees its manifest
// imports. Deploy no longer assumes a single top-level integrations/ directory,
// because a workspace is free to group integrations however it likes --
// shopify_integrations/customer_sync is as valid as integrations/customer_sync.
type Integration struct {
	// Name is the directory name the integration is deployed under, inside
	// <remote>/integrations. It is what the release command names.
	Name string
	// Label is the manifest's own `name:`, which is what the runtime registers
	// and what an operator sees in `otter integrations`. It may differ from the
	// directory name.
	Label string
	// Dir is the absolute local directory holding the manifest and its code.
	Dir string
	// Trees are the absolute local directories the manifest declares in
	// python.path, in declaration order.
	Trees []string
}

// IntegrationNames returns the deploy names in order, which is what the plan,
// the release command and the recorded result all speak in.
func (c Config) IntegrationNames() []string {
	names := make([]string, 0, len(c.Integrations))
	for _, integ := range c.Integrations {
		names = append(names, integ.Name)
	}
	return names
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
	// Workspace names the workspace on the host to deploy into. Empty uses the
	// project's own: the recorded one, or a new workspace named after it.
	Workspace string
	ConfigFor string
	UV        string
	NoUV      bool
	UVVersion string
	DryRun    bool
	Verbose   bool
	Timeout   time.Duration

	// Build forces a compile from Go source, refusing to fall back to released
	// binaries. It is how a contributor deploying from the runtime checkout
	// says "ship what I have, not what was tagged".
	Build bool
	// Source names the Go checkout to compile from, which is what makes a
	// Python project deployable from a runtime checkout that lives elsewhere.
	// Naming one implies building from source.
	Source string
	// Binaries names a local directory holding otterd and otter built for the
	// target platform, which is the offline and air-gapped path.
	Binaries string

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

// SharedEnvFileName is the credentials file at the repository root, shared by
// every integration.
//
// It is deliberately not per-integration. The daemon's environment is a single
// process environment -- every EnvironmentFile= is merged into it -- and an
// integration receives only the keys its own manifest declares. So a
// per-integration file isolated nothing; it just turned one rotated credential
// into an N-file edit and let those copies drift apart.
//
// Named without a leading dot for the same reason as otter.daemon.env: it is
// configuration an operator has to find and edit. The *.env gitignore rule is
// what actually keeps it out of git.
const SharedEnvFileName = "otter.env"

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
	// Workspace and Slug name the workspace this project owns on the host. They
	// are committed so every checkout, machine and directory of the same
	// project deploys into the same workspace instead of creating a new one.
	Workspace string `yaml:"workspace"`
	Slug      string `yaml:"slug"`
}

// RegisterFlags binds the deploy flags.
func (f *Flags) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&f.Target.Host, "host", "", "SSH destination, e.g. root@203.0.113.10 or an ssh_config alias")
	fs.StringVar(&f.Target.User, "user", "", "SSH login user (default: taken from --host or ssh_config)")
	fs.IntVar(&f.Target.Port, "port", 0, "SSH port (default 22)")
	fs.StringVar(&f.Target.IdentityFile, "identity", "", "private key to use for SSH")
	fs.StringVar(&f.Target.RemoteDir, "remote-dir", "", "remote install root (default "+DefaultRemoteDir+")")
	fs.StringVar(&f.Target.ServiceName, "service", "", "systemd unit name (default "+DefaultServicePrefix+"-<workspace>)")
	fs.StringVar(&f.Target.RunAsUser, "service-user", "", "service account owning the unit and data (default otter)")
	fs.StringVar(&f.Target.DataDir, "data-dir", "", "remote data directory holding otter.db (default <workspace>/.otter/data)")
	fs.StringVar(&f.Target.Listen, "listen", "", "remote API listen address (default the first free port from "+DefaultListenAddr+")")
	fs.StringVar(&f.Workspace, "workspace", "", "workspace on the host to deploy into (default: this project's own)")
	fs.StringVar(&f.Target.Platform, "platform", "", "remote GOOS/GOARCH; detected over SSH when empty")
	fs.BoolVar(&f.Target.RotateAPIToken, "rotate-token", false, "generate and install a fresh API token")
	fs.StringVar(&f.Target.APIToken, "api-token", "", "use this API token instead of the stored or remote one")
	fs.StringVar(&f.EnvFile, "env-file", "", "shared credentials file for every integration (default "+SharedEnvFileName+")")
	fs.StringVar(&f.DaemonEnv, "daemon-env", "", "daemon-wide environment file (default "+DaemonEnvFileName+")")
	fs.StringVar(&f.Integration, "integration", "", "deploy only this integration, leaving the others untouched")
	fs.StringVar(&f.ConfigFor, "config", "", "deploy config file (default "+ConfigFileName+")")
	fs.StringVar(&f.UV, "uv", "", "uv executable on the host for Python preparation (default the vendored copy)")
	fs.BoolVar(&f.NoUV, "no-uv", false, "do not vendor uv; use one already present on the host")
	fs.StringVar(&f.UVVersion, "uv-version", "", "uv release to vendor (default "+PinnedUVVersion+")")
	fs.BoolVar(&f.Build, "build", false, "compile the runtime from Go source instead of using released binaries")
	fs.StringVar(&f.Source, "source", "", "Otter Go checkout to compile from (implies --build)")
	fs.StringVar(&f.Binaries, "binaries", "", "directory holding otterd and otter for the target platform (offline deploy)")
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
		fmt.Fprint(stderr, "Run it from a project: the directory (or nearest ancestor) holding\n")
		fmt.Fprint(stderr, ".otter, .git or go.mod is scanned recursively for otter.yaml, so\n")
		fmt.Fprint(stderr, "integrations may be nested however you like.\n")
		fmt.Fprint(stderr, "\nWhat it does, every time:\n")
		fmt.Fprint(stderr, "  1. detect the remote platform over SSH\n")
		fmt.Fprint(stderr, "  2. obtain otterd and otter for it -- compiled from source when this\n")
		fmt.Fprint(stderr, "     project is a Go checkout, otherwise fetched from the matching release\n")
		fmt.Fprint(stderr, "     (--build and --binaries override that choice)\n")
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
func LoadConfig(projectRoot string, f *Flags, previous HostDeploy) (Config, error) {
	cfg := Config{
		ProjectRoot: projectRoot,
		DryRun:      f.DryRun,
		Verbose:     f.Verbose,
		Timeout:     f.Timeout,
		Target:      DefaultTarget(),
	}

	// 1. The config file, if present. Committed, and holds no secrets.
	path := f.ConfigFor
	if path == "" {
		path = projectRoot + "/" + ConfigFileName
	}
	fromFile, fileEnv, fromWorkspace, err := loadConfigFile(path)
	if err != nil {
		return cfg, err
	}
	if fromFile != nil {
		cfg.Target = mergeTarget(cfg.Target, *fromFile)
	}
	// A config file may point at a shared secrets file, but a flag still wins.
	if f.EnvFile == "" && fileEnv != "" {
		f.EnvFile = resolveRelative(projectRoot, fileEnv)
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
		carried := differentHostLayout(previous.Target)
		if sameHost(previous, cfg.Target, f) {
			// Same machine: everything about the last deploy to it travels,
			// which is what lets a bare `otter deploy` repeat it without the
			// operator retyping --host and the login user.
			carried = sameHostLayout(previous.Target)
		}
		cfg.Target = mergeTarget(cfg.Target, carried)
	}

	// 3. Explicit flags.
	cfg.Target = mergeTarget(cfg.Target, f.Target)

	// 4. The workspace this project owns on the host.
	//
	//    Precedence mirrors everything else: an explicit --workspace, then the
	//    committed otter.deploy.yaml (which is what makes a second machine or a
	//    fresh clone land on the same workspace), then the host's recorded
	//    workspace, then a new identity named after the project directory.
	if f.Workspace != "" {
		cfg.Target.WorkspaceSlug = SanitizeSlug(f.Workspace)
		cfg.Target.WorkspaceNameOverride = ""
	}
	if fromWorkspace.ID != "" {
		id := strings.TrimSpace(fromWorkspace.ID)
		if id != cfg.Target.WorkspaceID {
			// A committed id names a different workspace than this checkout last
			// deployed to, so the recorded directory name no longer applies.
			cfg.Target.WorkspaceNameOverride = ""
		}
		cfg.Target.WorkspaceID = id
	}
	if fromWorkspace.Slug != "" && f.Workspace == "" {
		cfg.Target.WorkspaceSlug = SanitizeSlug(fromWorkspace.Slug)
	}
	if cfg.Target.WorkspaceSlug == "" {
		cfg.Target.WorkspaceSlug = SanitizeSlug(filepath.Base(projectRoot))
	}
	cfg.Target.ApplyDefaults()

	// 4. The daemon-wide environment file. It is optional: a deployment that
	//    configures nothing beyond per-integration secrets does not need one.
	daemonEnv := f.DaemonEnv
	if daemonEnv == "" {
		daemonEnv = filepath.Join(projectRoot, DaemonEnvFileName)
	}
	daemonEnv = resolveRelative(projectRoot, daemonEnv)
	if _, err := os.Stat(daemonEnv); err == nil {
		cfg.DaemonEnv = daemonEnv
	}

	// 5. The shared credentials file, which every integration draws from. Also
	//    optional: an integration whose secrets all come from its own manifest
	//    needs none, and a deployment with no credentials at all is legal.
	sharedEnv := f.EnvFile
	if sharedEnv == "" {
		sharedEnv = filepath.Join(projectRoot, SharedEnvFileName)
	}
	sharedEnv = resolveRelative(projectRoot, sharedEnv)
	if _, err := os.Stat(sharedEnv); err == nil {
		cfg.SharedEnv = sharedEnv
	}

	if err := cfg.resolveIntegrations(f); err != nil {
		return cfg, err
	}

	// The reservation applies to every integration being deployed, not only a
	// filtered one: a directory named `daemon` would be released to the same
	// path the daemon-wide environment file occupies.
	for _, integ := range cfg.Integrations {
		if integ.Name == DaemonEnvIntegrationName {
			return cfg, fmt.Errorf("integration name %q is reserved for the daemon-wide environment file; rename the integration",
				DaemonEnvIntegrationName)
		}
	}
	return cfg, nil
}

// loadConfigFile reads otter.deploy.yaml. A missing file is not an error: the
// whole configuration can come from flags instead.
func loadConfigFile(path string) (*Target, string, fileWorkspace, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", fileWorkspace{}, nil
	}
	if err != nil {
		return nil, "", fileWorkspace{}, fmt.Errorf("read deploy config %s: %w", path, err)
	}

	var df deployFile
	if err := yaml.Unmarshal(data, &df); err != nil {
		return nil, "", fileWorkspace{}, fmt.Errorf("parse deploy config %s: %w", path, err)
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
	}, df.EnvFile, fileWorkspace{ID: df.Workspace, Slug: df.Slug}, nil
}

// fileWorkspace is the workspace identity a committed config file names.
type fileWorkspace struct {
	ID   string
	Slug string
}

func resolveRelative(root, path string) string {
	if strings.HasPrefix(path, "/") {
		return path
	}
	return root + "/" + strings.TrimPrefix(path, "./")
}

// sameHostLayout is everything a deploy remembers about the machine it just
// converged: how to reach it, the operator's layout choices, and which
// workspace this project owns there.
//
// Deliberately not carried: ServiceName, DataDir and Listen. All three follow
// from the workspace, and the workspace's own record on the host is the
// authority on them. Carrying them is how a state file written before
// workspaces existed would drag the old flat layout -- unit `otterd`, data at
// <remote>/data -- into a workspace that must have neither.
func sameHostLayout(t Target) Target {
	return Target{
		Host:                  t.Host,
		User:                  t.User,
		Port:                  t.Port,
		IdentityFile:          t.IdentityFile,
		RemoteDir:             t.RemoteDir,
		RunAsUser:             t.RunAsUser,
		Platform:              t.Platform,
		WorkspaceID:           t.WorkspaceID,
		WorkspaceSlug:         t.WorkspaceSlug,
		WorkspaceNameOverride: t.WorkspaceNameOverride,
	}
}

// differentHostLayout is what may travel to a *different* machine: the
// operator's layout choices and nothing about how to reach the old host. The
// workspace does not travel either -- a workspace is a directory, unit and port
// that belong to the host it was created on.
func differentHostLayout(t Target) Target {
	return Target{
		RemoteDir:   t.RemoteDir,
		RunAsUser:   t.RunAsUser,
		WorkspaceID: NewWorkspaceID(),
	}
}

// sameHost reports whether the previous deploy and the requested target are the
// same machine. The requested host wins when the operator named one.
func sameHost(previous HostDeploy, requested Target, f *Flags) bool {
	host := requested.Host
	if f.Target.Host != "" {
		host = f.Target.Host
	}
	if host == "" {
		// Nothing named a host, so the record in hand is the machine being
		// deployed to. That is what makes a bare `otter deploy` repeat the last
		// one instead of treating it as a new host.
		return previous.Host != ""
	}
	// The login user is part of --host's spelling, not part of the machine:
	// `--host root@1.2.3.4` and a stored `1.2.3.4` are the same host.
	return hostOnly(host) == previous.Host
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
	if over.WorkspaceID != "" {
		base.WorkspaceID = over.WorkspaceID
	}
	if over.WorkspaceSlug != "" {
		base.WorkspaceSlug = over.WorkspaceSlug
	}
	if over.WorkspaceNameOverride != "" {
		base.WorkspaceNameOverride = over.WorkspaceNameOverride
	}
	if over.APIToken != "" {
		base.APIToken = over.APIToken
	}
	if over.RotateAPIToken {
		base.RotateAPIToken = true
	}
	return base
}

// resolveIntegrations discovers the integrations to ship.
//
// Discovery walks the whole project, exactly as the runtime does, so a deploy
// ships what `otter start` would serve. The old rule -- one hardcoded
// integrations/ directory at the repository root -- only ever matched this
// repository's own layout, which is why a workspace that groups its
// integrations any other way could not deploy at all.
//
// Shared code is not discovered separately: each manifest's python.path is the
// authoritative list of what that integration imports, and every declared tree
// is staged at the relative depth the declaration names.
func (c *Config) resolveIntegrations(f *Flags) error {
	items, err := config.Discover(c.ProjectRoot)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("no %s found under %s: nothing to deploy",
			config.ManifestFileName, c.ProjectRoot)
	}

	// A limited deploy names exactly one integration, by manifest label or by
	// directory name. Selection happens before validation so that one broken
	// manifest elsewhere in the project cannot block shipping a different,
	// healthy integration.
	want := strings.TrimSpace(f.Integration)
	selected := items
	if want != "" {
		selected = nil
		for _, item := range items {
			if item.ID == want || filepath.Base(item.Dir) == want {
				selected = append(selected, item)
			}
		}
		if len(selected) == 0 {
			return fmt.Errorf("integration %q not found under %s (available: %s)",
				want, c.ProjectRoot, strings.Join(discoveredNames(items), ", "))
		}
		if len(selected) > 1 {
			var dirs []string
			for _, item := range selected {
				dirs = append(dirs, item.Dir)
			}
			return fmt.Errorf("integration %q is ambiguous: %s", want, strings.Join(dirs, ", "))
		}
	}

	var all []Integration
	byName := map[string]string{}
	for _, item := range selected {
		if !item.Valid {
			// Deploying with a broken manifest would push a tree the host
			// cannot run. Stop and name the file instead.
			return fmt.Errorf("%s: %s", item.Dir, item.Error)
		}
		integ, err := describeIntegration(item)
		if err != nil {
			return err
		}
		// Two directories with the same basename would collide on the host,
		// which names integrations by directory. Report both paths rather than
		// letting one silently overwrite the other.
		if first, dup := byName[integ.Name]; dup {
			return fmt.Errorf("two integrations are both named %q (%s and %s); rename one directory",
				integ.Name, first, integ.Dir)
		}
		byName[integ.Name] = integ.Dir
		all = append(all, integ)
	}

	c.Limited = want != ""
	c.Integrations = all
	return nil
}

// describeIntegration reads one discovered manifest into the facts staging
// needs.
func describeIntegration(item *config.Integration) (Integration, error) {
	manifest := item.Manifest
	if manifest == nil {
		return Integration{}, fmt.Errorf("%s: manifest could not be loaded", item.Dir)
	}
	// A release can only carry a tree whose depth is expressed relative to the
	// integration directory, so an absolute python.path is refused before
	// anything is staged or pushed.
	if err := manifest.ValidatePythonPathsForRelease(); err != nil {
		return Integration{}, err
	}

	integ := Integration{
		Name:  filepath.Base(item.Dir),
		Label: item.Name,
		Dir:   item.Dir,
	}
	for _, spec := range manifest.PythonPathEntries() {
		info, err := os.Stat(spec.Resolved)
		if err != nil {
			return Integration{}, fmt.Errorf("python.path %q: shared directory %s does not exist",
				spec.Declared, spec.Resolved)
		}
		if !info.IsDir() {
			return Integration{}, fmt.Errorf("python.path %q: %s is not a directory",
				spec.Declared, spec.Resolved)
		}
		integ.Trees = append(integ.Trees, spec.Resolved)
	}
	return integ, nil
}

// discoveredNames renders what is available for an error message, sorted so the
// list is stable regardless of discovery order.
func discoveredNames(items []*config.Integration) []string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.ID)
	}
	sort.Strings(names)
	return names
}

// Validate checks the whole configuration before anything is built or sent.
func (c *Config) Validate() error {
	if err := c.Target.Validate(); err != nil {
		return err
	}
	if c.ProjectRoot == "" {
		return errors.New("project root is unknown; run otter deploy from inside the project")
	}
	if info, err := os.Stat(c.ProjectRoot); err != nil || !info.IsDir() {
		return fmt.Errorf("project root %s is not a directory", c.ProjectRoot)
	}
	if c.Timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	return nil
}

// MissingSecrets lists the secret variables a deployment needs but could not
// find, either in the shared credentials file or in the local environment.
//
// A missing key is reported once, naming every integration that needs it: the
// file is shared, so the same absence cannot be fixed per integration.
func (c *Config) MissingSecrets(required map[string][]string) []string {
	neededBy := map[string][]string{}
	var order []string

	for _, integ := range c.Integrations {
		for _, key := range required[integ.Name] {
			if _, ok := os.LookupEnv(key); ok {
				continue
			}
			if _, seen := neededBy[key]; !seen {
				order = append(order, key)
			}
			neededBy[key] = append(neededBy[key], integ.Name)
		}
	}
	if len(order) == 0 {
		return nil
	}

	// One file, so read it once rather than once per key.
	shared := map[string]string{}
	var sharedErr error
	if c.SharedEnv != "" {
		shared, sharedErr = LoadSecrets(c.SharedEnv)
	}

	var missing []string
	for _, key := range order {
		who := strings.Join(neededBy[key], ", ")
		switch {
		case c.SharedEnv == "":
			missing = append(missing, fmt.Sprintf("%s (needed by %s; no %s found)",
				key, who, SharedEnvFileName))
		case sharedErr != nil:
			missing = append(missing, fmt.Sprintf("%s (needed by %s; %v)", key, who, sharedErr))
		default:
			if _, ok := shared[key]; !ok {
				missing = append(missing, fmt.Sprintf("%s (needed by %s; absent from %s)",
					key, who, c.SharedEnv))
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
	for _, integ := range c.Integrations {
		path := filepath.Join(integ.Dir, config.ManifestFileName)
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
		required[integ.Name] = probe.Secrets
	}
	return required
}
