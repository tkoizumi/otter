package deploy

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	// recursively for job manifests, and it is where otter.deploy.yaml,
	// otter.env and .otter/ live.
	//
	// It is a project, not necessarily a Go checkout. A workspace that holds
	// only Python jobs deploys from released binaries; one that
	// contains cmd/otterd is compiled from source.
	ProjectRoot string

	// Jobs is the ordered list of jobs to ship, each with the
	// local directory it lives in and the shared trees its manifest declares.
	Jobs []Job

	// SharedEnv is the resolved environment file holding credentials that every
	// job may use. Empty means the checkout has none; the deploy
	// proceeds anyway, because the daemon reports a missing secret far more
	// clearly than this command can.
	SharedEnv string

	// Version is the build version, reported by the deployed daemon.
	Version string

	// UVVersion overrides the pinned uv release vendored onto the host. The
	// uv version is part of every prepared environment's identity, so changing
	// it rebuilds environments.
	UVVersion string

	// Limited reports that the deploy covers a subset of the jobs, so
	// the push must protect the ones it does not carry.
	Limited bool

	// DaemonEnv is the resolved daemon-wide environment file, empty when the
	// checkout does not have one.
	DaemonEnv string

	// Keep is how many inactive releases a deploy retains per job, passed to
	// `otter release --keep`. It bounds only the releases beyond the ones
	// retention always protects: the active release, the newest inactive one
	// (the rollback target), and every release a non-terminal run is bound to.
	// Zero keeps every release, which is the explicit opt-out; the default is
	// DefaultKeep.
	Keep int

	// MemoryMax, MemoryHigh, CPUQuota and TasksMax are the systemd resource
	// caps written into the generated unit. They bound the whole workspace:
	// otterd and every job it runs share one cgroup, so the value covers the
	// sum of the concurrent runs plus the daemon, not one run. A value of
	// CapOff (or empty) emits no directive and leaves systemd's own default.
	MemoryMax  string
	MemoryHigh string
	CPUQuota   string
	TasksMax   string

	// ReadWritePaths are extra absolute paths the service may write, on top of
	// the workspace and data directories the unit always lists. They are how a
	// job that writes scratch outside its workspace keeps working under
	// ProtectSystem=strict.
	ReadWritePaths []string

	// DryRun prints the plan and performs no remote change.
	DryRun bool
	// Verbose streams every remote command.
	Verbose bool
	// Timeout bounds the whole deploy.
	Timeout time.Duration
}

// Job is one job to deploy, resolved from the workspace.
//
// It carries the two things staging needs and that a bare name cannot express:
// where the job lives locally, and which shared trees its manifest
// imports. Deploy no longer assumes a single top-level jobs/ directory,
// because a workspace is free to group jobs however it likes --
// shopify_jobs/customer_sync is as valid as jobs/customer_sync.
type Job struct {
	// Name is the directory name the job is deployed under, inside
	// <remote>/jobs. It is what the release command names.
	Name string
	// Label is the manifest's own `name:`, which is what the runtime registers
	// and what an operator sees in `otter jobs`. It may differ from the
	// directory name.
	Label string
	// Dir is the absolute local directory holding the manifest and its code.
	Dir string
	// Trees are the absolute local directories the manifest declares in
	// python.path, in declaration order.
	Trees []string
}

// JobNames returns the deploy names in order, which is what the plan,
// the release command and the recorded result all speak in.
func (c Config) JobNames() []string {
	names := make([]string, 0, len(c.Jobs))
	for _, integ := range c.Jobs {
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
	// Job limits the deploy to one job. Empty deploys all of
	// them, which is the historical behavior.
	Job string
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
	// Keep is how many inactive releases to retain per job on the host. Zero
	// keeps every release, which is the default of `otter release` itself; a
	// deploy converges, so its own default is DefaultKeep.
	Keep int

	// MemoryMax, MemoryHigh, CPUQuota and TasksMax size the unit's cgroup.
	// Empty means "use the built-in default"; CapOff means "emit no cap".
	MemoryMax  string
	MemoryHigh string
	CPUQuota   string
	TasksMax   string
	// ReadWritePaths are extra paths outside the workspace that jobs may
	// write, collected from every --rw-path. They extend the unit's
	// ReadWritePaths= under ProtectSystem=strict.
	ReadWritePaths stringList

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

// DefaultKeep is how many inactive releases a deploy retains per job. A deploy
// converges, so it prunes by default rather than waiting for an operator to
// remember `otter release --keep`; this keeps the active release plus a bounded
// rollback window. `--keep 0` keeps every release.
const DefaultKeep = 3

// Default resource caps for the generated unit.
//
// Memory is expressed as a percentage of the host's physical RAM rather than a
// fixed number of bytes, because systemd resolves the percentage on the host.
// The same deploy is therefore safe on a 2 GiB VPS and a 16 GiB machine, and
// the cap cannot silently become too small when the workload or the host
// changes. The cap is set below 100% on purpose: the kernel, sshd, systemd and
// the page cache live outside the unit's cgroup, and leaving them a quarter of
// RAM is what keeps the host answering while the unit is under pressure.
//
// CPUQuota is a percentage of one core, so 200% is two cores' worth on any
// host and a runaway job cannot monopolise a large machine. TasksMax bounds
// processes plus threads, which is the fork-bomb guard; it is deliberately
// generous because every Python thread counts against it.
const (
	DefaultMemoryMax  = "75%"
	DefaultMemoryHigh = "60%"
	DefaultCPUQuota   = "200%"
	DefaultTasksMax   = "512"

	// CapOff is the explicit "emit no directive" value for any cap. It is
	// deliberately not "0": systemd reads a zero memory limit as a real limit
	// of zero, which would kill the daemon at once.
	CapOff = "off"
)

// stringList collects a repeatable flag, one value per occurrence. It is used
// for --rw-path, where a deployment may need several extra writable paths and
// losing an earlier one to a later flag would be a surprise.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("path must not be empty")
	}
	*s = append(*s, value)
	return nil
}

// DaemonEnvFileName is the daemon-wide environment file at the repository
// root. It is not committed, because a notification URL or an API token is a
// credential, and it is not per-job, because a setting such as the
// notification endpoint belongs to the daemon rather than to one job.
//
// The name is deliberately visible rather than dotted. It is the primary
// configuration surface an operator has to create and edit, so hiding it
// behind a dot -- in every `ls`, every file explorer, and every "show hidden
// files" toggle that defaults to off -- makes the feature harder to find than
// the token inside it is worth. The file is still gitignored, which is what
// actually keeps the credential out of git; the dot added nothing to that.
//
// Locally `make sync-up` sources it. At deploy it becomes
// /etc/otter/daemon.env, which the unit loads alongside each job's
// secrets file, so one file has one meaning in both places.
const DaemonEnvFileName = "otter.daemon.env"

// SharedEnvFileName is the credentials file at the repository root, shared by
// every job.
//
// It is deliberately not per-job. The daemon's environment is a single
// process environment -- every EnvironmentFile= is merged into it -- and an
// job receives only the keys its own manifest declares. So a
// per-job file isolated nothing; it just turned one rotated credential
// into an N-file edit and let those copies drift apart.
//
// Named without a leading dot for the same reason as otter.daemon.env: it is
// configuration an operator has to find and edit. The *.env gitignore rule is
// what actually keeps it out of git.
const SharedEnvFileName = "otter.env"

// DaemonEnvJobName is reserved: a job of this name would
// write to the same remote path as the daemon-wide environment file.
const DaemonEnvJobName = "daemon"

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

	// Unit resource policy, committed so the caps a workspace was sized for do
	// not have to be retyped on every deploy. Empty means "use the built-in
	// default"; CapOff means "emit no directive".
	MemoryMax      string   `yaml:"memory_max"`
	MemoryHigh     string   `yaml:"memory_high"`
	CPUQuota       string   `yaml:"cpu_quota"`
	TasksMax       string   `yaml:"tasks_max"`
	ReadWritePaths []string `yaml:"read_write_paths"`
}

// fileConfig is the parsed, committed otter.deploy.yaml.
type fileConfig struct {
	Target    Target
	EnvFile   string
	Workspace fileWorkspace
	Unit      unitSettings
}

// unitSettings is what a deploy config file may say about the generated unit's
// resource policy. It is separate from Target because a cap is resource policy
// for the host, not part of how to reach it, and it must not travel to another
// machine through the deploy state.
type unitSettings struct {
	MemoryMax      string
	MemoryHigh     string
	CPUQuota       string
	TasksMax       string
	ReadWritePaths []string
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
	fs.StringVar(&f.EnvFile, "env-file", "", "shared credentials file for every job (default "+SharedEnvFileName+")")
	fs.StringVar(&f.DaemonEnv, "daemon-env", "", "daemon-wide environment file (default "+DaemonEnvFileName+")")
	fs.StringVar(&f.Job, "job", "", "deploy only this job, leaving the others untouched")
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
	fs.IntVar(&f.Keep, "keep", DefaultKeep, "inactive releases to retain per job (0 keeps every release)")
	fs.StringVar(&f.MemoryMax, "memory-max", "", "systemd MemoryMax for the workspace: a size (4G) or a percent of host RAM (default "+DefaultMemoryMax+"; \""+CapOff+"\" emits no cap)")
	fs.StringVar(&f.MemoryHigh, "memory-high", "", "systemd MemoryHigh, the soft memory ceiling (default "+DefaultMemoryHigh+"; \""+CapOff+"\" emits no cap)")
	fs.StringVar(&f.CPUQuota, "cpu-quota", "", "systemd CPUQuota, percent of one CPU (default "+DefaultCPUQuota+"; \""+CapOff+"\" emits no cap)")
	fs.StringVar(&f.TasksMax, "tasks-max", "", "systemd TasksMax, processes and threads (default "+DefaultTasksMax+"; \""+CapOff+"\" emits no cap)")
	fs.Var(&f.ReadWritePaths, "rw-path", "extra path the service may write under ProtectSystem=strict (repeatable)")
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
		fmt.Fprint(stderr, "jobs may be nested however you like.\n")
		fmt.Fprint(stderr, "\nWhat it does, every time:\n")
		fmt.Fprint(stderr, "  1. detect the remote platform over SSH\n")
		fmt.Fprint(stderr, "  2. obtain otterd and otter for it -- compiled from source when this\n")
		fmt.Fprint(stderr, "     project is a Go checkout, otherwise fetched from the matching release\n")
		fmt.Fprint(stderr, "     (--build and --binaries override that choice)\n")
		fmt.Fprint(stderr, "  3. rsync the binaries and the job tree\n")
		fmt.Fprint(stderr, "  4. write the systemd unit and the secrets files\n")
		fmt.Fprint(stderr, "  5. restart the service and wait for its health endpoint\n")
		fmt.Fprint(stderr, "\nIt never writes or deletes run history, watermarks or the extracted Python\n")
		fmt.Fprint(stderr, "SDK, so they survive every deploy. It reads the run registry only to find\n")
		fmt.Fprint(stderr, "the releases a pending run is bound to, and prunes old releases to --keep\n")
		fmt.Fprint(stderr, "inactive releases per job, always keeping the active release, the rollback\n")
		fmt.Fprint(stderr, "target, and any release a pending run is bound to.\n\n")
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
		Keep:        f.Keep,
		Target:      DefaultTarget(),
	}

	// 1. The config file, if present. Committed, and holds no secrets.
	path := f.ConfigFor
	if path == "" {
		path = projectRoot + "/" + ConfigFileName
	}
	file, err := loadConfigFile(path)
	if err != nil {
		return cfg, err
	}
	if file != nil {
		cfg.Target = mergeTarget(cfg.Target, file.Target)
	}
	// A config file may point at a shared secrets file, but a flag still wins.
	if f.EnvFile == "" && file != nil && file.EnvFile != "" {
		f.EnvFile = resolveRelative(projectRoot, file.EnvFile)
	}
	fromWorkspace := fileWorkspace{}
	if file != nil {
		fromWorkspace = file.Workspace
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

	// 5. The unit's resource policy. Precedence matches everything else: an
	//    explicit flag beats the committed file, which beats the built-in
	//    default. A config file's writable paths are kept and the flag's are
	//    added, so a one-off --rw-path does not drop the committed ones.
	var fileUnit unitSettings
	if file != nil {
		fileUnit = file.Unit
	}
	cfg.MemoryMax = firstNonEmpty(f.MemoryMax, fileUnit.MemoryMax, DefaultMemoryMax)
	cfg.MemoryHigh = firstNonEmpty(f.MemoryHigh, fileUnit.MemoryHigh, DefaultMemoryHigh)
	cfg.CPUQuota = firstNonEmpty(f.CPUQuota, fileUnit.CPUQuota, DefaultCPUQuota)
	cfg.TasksMax = firstNonEmpty(f.TasksMax, fileUnit.TasksMax, DefaultTasksMax)
	cfg.ReadWritePaths = append(append([]string{}, fileUnit.ReadWritePaths...), f.ReadWritePaths...)

	// 6. The daemon-wide environment file. It is optional: a deployment that
	//    configures nothing beyond per-job secrets does not need one.
	daemonEnv := f.DaemonEnv
	if daemonEnv == "" {
		daemonEnv = filepath.Join(projectRoot, DaemonEnvFileName)
	}
	daemonEnv = resolveRelative(projectRoot, daemonEnv)
	if _, err := os.Stat(daemonEnv); err == nil {
		cfg.DaemonEnv = daemonEnv
	}

	// 7. The shared credentials file, which every job draws from. Also
	//    optional: a job whose secrets all come from its own manifest
	//    needs none, and a deployment with no credentials at all is legal.
	sharedEnv := f.EnvFile
	if sharedEnv == "" {
		sharedEnv = filepath.Join(projectRoot, SharedEnvFileName)
	}
	sharedEnv = resolveRelative(projectRoot, sharedEnv)
	if _, err := os.Stat(sharedEnv); err == nil {
		cfg.SharedEnv = sharedEnv
	}

	if err := cfg.resolveJobs(f); err != nil {
		return cfg, err
	}

	// The reservation applies to every job being deployed, not only a
	// filtered one: a directory named `daemon` would be released to the same
	// path the daemon-wide environment file occupies.
	for _, integ := range cfg.Jobs {
		if integ.Name == DaemonEnvJobName {
			return cfg, fmt.Errorf("job name %q is reserved for the daemon-wide environment file; rename the job",
				DaemonEnvJobName)
		}
	}
	return cfg, nil
}

// loadConfigFile reads otter.deploy.yaml. A missing file is not an error: the
// whole configuration can come from flags instead. A nil result means the file
// was not there.
func loadConfigFile(path string) (*fileConfig, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read deploy config %s: %w", path, err)
	}

	var df deployFile
	if err := yaml.Unmarshal(data, &df); err != nil {
		return nil, fmt.Errorf("parse deploy config %s: %w", path, err)
	}

	return &fileConfig{
		Target: Target{
			Host:         df.Host,
			User:         df.User,
			Port:         df.Port,
			IdentityFile: df.Identity,
			RemoteDir:    df.RemoteDir,
			ServiceName:  df.Service,
			RunAsUser:    df.ServiceUser,
			DataDir:      df.DataDir,
			Listen:       df.Listen,
		},
		EnvFile:   df.EnvFile,
		Workspace: fileWorkspace{ID: df.Workspace, Slug: df.Slug},
		Unit: unitSettings{
			MemoryMax:      df.MemoryMax,
			MemoryHigh:     df.MemoryHigh,
			CPUQuota:       df.CPUQuota,
			TasksMax:       df.TasksMax,
			ReadWritePaths: df.ReadWritePaths,
		},
	}, nil
}

// firstNonEmpty returns the first value that is not blank. It is how the
// layered deploy configuration spells "lower precedence wins only when the
// higher one said nothing".
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
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

// resolveJobs discovers the jobs to ship.
//
// Discovery walks the whole project, exactly as the runtime does, so a deploy
// ships what `otter start` would serve. The old rule -- one hardcoded
// jobs/ directory at the repository root -- only ever matched this
// repository's own layout, which is why a workspace that groups its
// jobs any other way could not deploy at all.
//
// Shared code is not discovered separately: each manifest's python.path is the
// authoritative list of what that job imports, and every declared tree
// is staged at the relative depth the declaration names.
func (c *Config) resolveJobs(f *Flags) error {
	items, err := config.Discover(c.ProjectRoot)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("no %s found under %s: nothing to deploy",
			config.ManifestFileName, c.ProjectRoot)
	}

	// A limited deploy names exactly one job, by manifest label or by
	// directory name. Selection happens before validation so that one broken
	// manifest elsewhere in the project cannot block shipping a different,
	// healthy job.
	want := strings.TrimSpace(f.Job)
	selected := items
	if want != "" {
		selected = nil
		for _, item := range items {
			if item.ID == want || filepath.Base(item.Dir) == want {
				selected = append(selected, item)
			}
		}
		if len(selected) == 0 {
			return fmt.Errorf("job %q not found under %s (available: %s)",
				want, c.ProjectRoot, strings.Join(discoveredNames(items), ", "))
		}
		if len(selected) > 1 {
			var dirs []string
			for _, item := range selected {
				dirs = append(dirs, item.Dir)
			}
			return fmt.Errorf("job %q is ambiguous: %s", want, strings.Join(dirs, ", "))
		}
	}

	var all []Job
	byName := map[string]string{}
	for _, item := range selected {
		if !item.Valid {
			// Deploying with a broken manifest would push a tree the host
			// cannot run. Stop and name the file instead.
			return fmt.Errorf("%s: %s", item.Dir, item.Error)
		}
		integ, err := describeJob(item)
		if err != nil {
			return err
		}
		// Two directories with the same basename would collide on the host,
		// which names jobs by directory. Report both paths rather than
		// letting one silently overwrite the other.
		if first, dup := byName[integ.Name]; dup {
			return fmt.Errorf("two jobs are both named %q (%s and %s); rename one directory",
				integ.Name, first, integ.Dir)
		}
		byName[integ.Name] = integ.Dir
		all = append(all, integ)
	}

	c.Limited = want != ""
	c.Jobs = all
	return nil
}

// describeJob reads one discovered manifest into the facts staging
// needs.
func describeJob(item *config.Job) (Job, error) {
	manifest := item.Manifest
	if manifest == nil {
		return Job{}, fmt.Errorf("%s: manifest could not be loaded", item.Dir)
	}
	// A release can only carry a tree whose depth is expressed relative to the
	// job directory, so an absolute python.path is refused before
	// anything is staged or pushed.
	if err := manifest.ValidatePythonPathsForRelease(); err != nil {
		return Job{}, err
	}

	integ := Job{
		Name:  filepath.Base(item.Dir),
		Label: item.Name,
		Dir:   item.Dir,
	}
	for _, spec := range manifest.PythonPathEntries() {
		info, err := os.Stat(spec.Resolved)
		if err != nil {
			return Job{}, fmt.Errorf("python.path %q: shared directory %s does not exist",
				spec.Declared, spec.Resolved)
		}
		if !info.IsDir() {
			return Job{}, fmt.Errorf("python.path %q: %s is not a directory",
				spec.Declared, spec.Resolved)
		}
		integ.Trees = append(integ.Trees, spec.Resolved)
	}
	return integ, nil
}

// discoveredNames renders what is available for an error message, sorted so the
// list is stable regardless of discovery order.
func discoveredNames(items []*config.Job) []string {
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
	if c.Keep < 0 {
		return errors.New("--keep must not be negative; use 0 to keep every release")
	}
	// A malformed cap would otherwise be written into the unit and only fail on
	// the host, at restart, after the deploy had reported success.
	for _, cap := range []struct{ name, value string }{
		{"--memory-max", c.MemoryMax},
		{"--memory-high", c.MemoryHigh},
	} {
		if err := validateMemoryCap(cap.name, cap.value); err != nil {
			return err
		}
	}
	if err := validateCPUQuota(c.CPUQuota); err != nil {
		return err
	}
	if err := validateTasksMax(c.TasksMax); err != nil {
		return err
	}
	for _, path := range c.ReadWritePaths {
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " \t") {
			return fmt.Errorf("--rw-path must be an absolute path without whitespace, got %q", path)
		}
	}
	// The unit's own sandbox must not make the workspace it runs from
	// unreachable.
	if err := ValidateUnitPaths(c.Target, c.ReadWritePaths); err != nil {
		return err
	}
	return nil
}

// UnitOptions is the resource policy this deploy bakes into the generated unit.
func (c Config) UnitOptions() UnitOptions {
	return UnitOptions{
		MemoryMax:      c.MemoryMax,
		MemoryHigh:     c.MemoryHigh,
		CPUQuota:       c.CPUQuota,
		TasksMax:       c.TasksMax,
		ReadWritePaths: c.ReadWritePaths,
	}
}

// validateMemoryCap accepts what systemd's MemoryMax= and MemoryHigh= accept:
// the CapOff opt-out, a size with an optional binary or decimal suffix, or a
// percentage of the host's physical RAM.
func validateMemoryCap(name, value string) error {
	s := strings.TrimSpace(value)
	if s == "" || strings.EqualFold(s, CapOff) || strings.EqualFold(s, "infinity") {
		return nil
	}
	bad := fmt.Errorf("%s must be a size (4G, 512M), a percent of RAM (75%%), or %q, got %q", name, CapOff, value)
	if strings.HasSuffix(s, "%") {
		number := strings.TrimSuffix(s, "%")
		percent, err := strconv.ParseFloat(number, 64)
		if err != nil || percent <= 0 || percent != percent {
			return bad
		}
		return nil
	}
	digits := 0
	for digits < len(s) && s[digits] >= '0' && s[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return bad
	}
	switch strings.ToUpper(s[digits:]) {
	case "", "K", "M", "G", "T", "KB", "MB", "GB", "TB", "KIB", "MIB", "GIB", "TIB":
		return nil
	}
	return fmt.Errorf("%s has an unknown size suffix, got %q", name, value)
}

// validateCPUQuota accepts the CapOff opt-out or a positive percentage of one
// CPU.
func validateCPUQuota(value string) error {
	s := strings.TrimSpace(value)
	if s == "" || strings.EqualFold(s, CapOff) {
		return nil
	}
	bad := fmt.Errorf("--cpu-quota must be a positive percentage such as 200%% (or %q), got %q", CapOff, value)
	if !strings.HasSuffix(s, "%") {
		return bad
	}
	percent, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	if err != nil || percent <= 0 {
		return bad
	}
	return nil
}

// validateTasksMax accepts the CapOff opt-out, "infinity", or a positive
// integer.
func validateTasksMax(value string) error {
	s := strings.TrimSpace(value)
	if s == "" || strings.EqualFold(s, CapOff) || strings.EqualFold(s, "infinity") {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return fmt.Errorf("--tasks-max must be a positive integer or %q, got %q", CapOff, value)
	}
	return nil
}

// MissingSecrets lists the secret variables a deployment needs but could not
// find, either in the shared credentials file or in the local environment.
//
// A missing key is reported once, naming every job that needs it: the
// file is shared, so the same absence cannot be fixed per job.
func (c *Config) MissingSecrets(required map[string][]string) []string {
	neededBy := map[string][]string{}
	var order []string

	for _, integ := range c.Jobs {
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

// LoadSecrets reads one job's secrets file.
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
	for _, integ := range c.Jobs {
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
