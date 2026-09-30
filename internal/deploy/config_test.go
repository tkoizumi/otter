package deploy

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := `# a comment
SHOPIFY_CLIENT_ID=abc123
SHOPIFY_CLIENT_SECRET="quoted value"
export SALESFORCE_CLIENT_ID='single'
SALESFORCE_CLIENT_SECRET=

# a value may contain equals signs
SALESFORCE_INSTANCE_URL=https://example.my.salesforce.com?a=b
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	secrets, err := LoadSecrets(path)
	if err != nil {
		t.Fatalf("LoadSecrets: %v", err)
	}

	want := map[string]string{
		"SHOPIFY_CLIENT_ID":        "abc123",
		"SHOPIFY_CLIENT_SECRET":    "quoted value",
		"SALESFORCE_CLIENT_ID":     "single",
		"SALESFORCE_CLIENT_SECRET": "",
		"SALESFORCE_INSTANCE_URL":  "https://example.my.salesforce.com?a=b",
	}
	for key, value := range want {
		if got := secrets[key]; got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	if len(secrets) != len(want) {
		t.Errorf("parsed %d secrets, want %d: %v", len(secrets), len(want), secrets)
	}
}

func TestLoadSecretsRejectsGarbage(t *testing.T) {
	dir := t.TempDir()

	for name, content := range map[string]string{
		"no equals": "this is not an assignment\n",
		"bad name":  "NOT-A-NAME=1\n",
		"empty key": "=value\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSecrets(path); err == nil {
			t.Errorf("LoadSecrets accepted %q, want an error", content)
		}
	}
}

func TestMergeTargetPrecedence(t *testing.T) {
	base := DefaultTarget()
	base.Host = "from-state"
	base.Port = 2222
	base.RemoteDir = "/srv/otter"

	over := Target{Host: "from-flag"}
	got := mergeTarget(base, over)

	if got.Host != "from-flag" {
		t.Errorf("Host = %q, want from-flag", got.Host)
	}
	// Fields the override leaves empty must survive from the base.
	if got.Port != 2222 {
		t.Errorf("Port = %d, want 2222", got.Port)
	}
	if got.RemoteDir != "/srv/otter" {
		t.Errorf("RemoteDir = %q, want /srv/otter", got.RemoteDir)
	}
}

func TestApplyDefaultsDerivesWorkspaceLayout(t *testing.T) {
	target := Target{Host: "h", RemoteDir: "/srv/otter", WorkspaceID: "abcdef12-0000-0000-0000-000000000000", WorkspaceSlug: "shop"}
	target.ApplyDefaults()

	if want := "/srv/otter/workspaces/shop-abcdef12/.otter/data"; target.DataDir != want {
		t.Errorf("DataDir = %q, want %q", target.DataDir, want)
	}
	if want := "otterd-shop-abcdef12.service"; target.ServiceUnit() != want {
		t.Errorf("ServiceUnit = %q, want %q", target.ServiceUnit(), want)
	}
	if target.Listen != "" {
		t.Errorf("Listen = %q; a new workspace's port is chosen on the host", target.Listen)
	}
	if target.Platform != "" {
		t.Errorf("ApplyDefaults invented a platform %q; it must stay empty for SSH detection", target.Platform)
	}
}

func TestTargetValidate(t *testing.T) {
	valid := func() Target {
		target := DefaultTarget()
		target.Host = "droplet"
		return target
	}

	tests := []struct {
		name   string
		mutate func(*Target)
	}{
		{"missing host", func(t *Target) { t.Host = "" }},
		{"host with space", func(t *Target) { t.Host = "a b" }},
		{"bad port", func(t *Target) { t.Port = 0 }},
		{"port out of range", func(t *Target) { t.Port = 70000 }},
		{"relative remote dir", func(t *Target) { t.RemoteDir = "opt/otter" }},
		{"service with slash", func(t *Target) { t.ServiceName = "a/b" }},
		{"user with colon", func(t *Target) { t.RunAsUser = "a:b" }},
		{"relative data dir", func(t *Target) { t.DataDir = "data" }},
		{"wildcard listen", func(t *Target) { t.Listen = "0.0.0.0:7337" }},
		{"listen without port", func(t *Target) { t.Listen = "127.0.0.1" }},
		{"bad platform", func(t *Target) { t.Platform = "linux/386" }},
	}

	for _, tc := range tests {
		target := valid()
		tc.mutate(&target)
		if err := target.Validate(); err == nil {
			t.Errorf("%s: Validate succeeded, want an error", tc.name)
		}
	}

	target := valid()
	target.Platform = "" // means "detect over SSH"
	if err := target.Validate(); err != nil {
		t.Errorf("an unpinned platform should be valid: %v", err)
	}
}

// The login user must be resolved from either --host or --user. Everything
// else keys off it, including whether remote scripts are wrapped in sudo.
func TestSplitHostUser(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		user     string
		wantHost string
		wantUser string
	}{
		{"user in the destination", "root@10.0.0.1", "", "10.0.0.1", "root"},
		{"separate flag", "10.0.0.1", "ubuntu", "10.0.0.1", "ubuntu"},
		{"flag wins over the destination", "root@10.0.0.1", "ubuntu", "10.0.0.1", "ubuntu"},
		{"no user anywhere", "droplet", "", "droplet", ""},
		{"ssh config alias", "droplet", "", "droplet", ""},
		{"malformed, no host", "root@", "", "root@", ""},
		{"malformed, no user", "@10.0.0.1", "", "10.0.0.1", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target := Target{Host: tc.host, User: tc.user}
			target.SplitHostUser()
			if target.Host != tc.wantHost {
				t.Errorf("host = %q, want %q", target.Host, tc.wantHost)
			}
			if target.User != tc.wantUser {
				t.Errorf("user = %q, want %q", target.User, tc.wantUser)
			}
		})
	}
}

// A login embedded in --host must not still be present after resolution, or
// validation would silently accept a destination ssh cannot use.
func TestResolvedTargetHasNoUserInHost(t *testing.T) {
	target := DefaultTarget()
	target.Host = "root@10.0.0.1"
	target.ApplyDefaults()

	if err := target.Validate(); err != nil {
		t.Fatalf("a resolved target should be valid: %v", err)
	}
	if target.Host != "10.0.0.1" {
		t.Errorf("host = %q, want the login stripped", target.Host)
	}
	if !target.LoginIsRoot() {
		t.Error("root@host did not resolve to a root login; remote scripts would be wrapped in sudo")
	}

	// An explicit --user must win, and the destination must still be stripped
	// so ssh is handed a resolvable host.
	explicit := DefaultTarget()
	explicit.Host = "root@10.0.0.1"
	explicit.User = "ubuntu"
	explicit.ApplyDefaults()
	if err := explicit.Validate(); err != nil {
		t.Fatalf("an explicit user should resolve cleanly: %v", err)
	}
	if explicit.User != "ubuntu" {
		t.Errorf("user = %q, want the explicit --user value", explicit.User)
	}
}

// The ssh hint must appear only when ssh is the problem. A host that answers
// ssh but lacks rsync produces its own clear message, and appending "check your
// ssh key" sends the operator down the wrong path.
func TestConnectHintOnlyForSSHFailures(t *testing.T) {
	target := Target{Host: "10.0.0.1"}

	authFailure := errors.New("ssh: connect to host port 22: Permission denied (publickey)")
	if err := connectError(target, authFailure); !strings.Contains(err.Error(), "hint:") {
		t.Errorf("an authentication failure should suggest checking ssh: %v", err)
	}

	toolFailure := errors.New("otter: the host is missing tools this deploy needs: rsync")
	err := connectError(target, toolFailure)
	if strings.Contains(err.Error(), "hint:") {
		t.Errorf("a missing-tool failure should not blame ssh: %v", err)
	}
	if !strings.Contains(err.Error(), "rsync") {
		t.Errorf("the tool failure was obscured: %v", err)
	}
}

// A limited deploy must name exactly one job, must refuse an unknown
// one, and must be flagged so the push protects the jobs it is not
// carrying.
func TestJobFilter(t *testing.T) {
	newRepo := func(t *testing.T) string {
		t.Helper()
		repo := t.TempDir()
		if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"alpha", "beta"} {
			dir := filepath.Join(repo, "jobs", name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte("version: 1\nname: "+name+"\nentrypoint: main.py\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return repo
	}

	load := func(t *testing.T, repo string, f *Flags) (Config, error) {
		t.Helper()
		return LoadConfig(repo, f, HostDeploy{})
	}

	t.Run("no filter deploys everything", func(t *testing.T) {
		repo := newRepo(t)
		cfg, err := load(t, repo, &Flags{set: map[string]bool{}})
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Jobs) != 2 {
			t.Errorf("deployed %v, want both jobs", cfg.Jobs)
		}
		if cfg.Limited {
			t.Error("an unfiltered deploy reported itself as limited")
		}
	})

	t.Run("a named job is the only one", func(t *testing.T) {
		repo := newRepo(t)
		cfg, err := load(t, repo, &Flags{Job: "beta", set: map[string]bool{}})
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Jobs) != 1 || cfg.Jobs[0].Name != "beta" {
			t.Errorf("deployed %v, want just beta", cfg.Jobs)
		}
		if !cfg.Limited {
			t.Error("a single-job deploy should be marked limited")
		}
	})

	t.Run("an unknown job is an error", func(t *testing.T) {
		repo := newRepo(t)
		_, err := load(t, repo, &Flags{Job: "gamma", set: map[string]bool{}})
		if err == nil {
			t.Fatal("an unknown job was accepted")
		}
		// The error should list what is available, so a typo is obvious.
		for _, want := range []string{"gamma", "alpha", "beta"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})
}

// The platform was detected over SSH for one host. Carrying it to a different
// host builds for the wrong architecture, and the failure appears much later as
// an unhelpful "exit status 255" when the host cannot execute the binary.
func TestPlatformIsNotCarriedBetweenHosts(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repo, "jobs", "one")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte("version: 1\nname: one\nentrypoint: main.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	previous := HostDeploy{
		Host:   "droplet-amd64",
		Target: Target{Host: "droplet-amd64", Platform: "linux/amd64", RemoteDir: "/opt/otter"},
	}

	// A different host must not inherit the recorded platform.
	other := &Flags{Target: Target{Host: "graviton-arm64"}, set: map[string]bool{}}
	cfg, err := LoadConfig(repo, other, previous)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Target.Platform != "" {
		t.Errorf("platform %q was carried to a different host; it must be detected again", cfg.Target.Platform)
	}
	// The rest of the recorded layout still travels, which is what makes a
	// bare deploy land in the right place.
	if cfg.Target.RemoteDir != "/opt/otter" {
		t.Errorf("RemoteDir = %q, want the recorded layout", cfg.Target.RemoteDir)
	}

	// The same host keeps its platform, so redeploying does not re-detect.
	same := &Flags{Target: Target{Host: "droplet-amd64"}, set: map[string]bool{}}
	cfg, err = LoadConfig(repo, same, previous)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Target.Platform != "linux/amd64" {
		t.Errorf("platform = %q, want the recorded linux/amd64 for the same host", cfg.Target.Platform)
	}
}

// An explicit --platform always wins, whatever the state says.
func TestExplicitPlatformWins(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repo, "jobs", "one")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte("version: 1\nname: one\nentrypoint: main.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	flags := &Flags{Target: Target{Platform: "linux/arm64"}, set: map[string]bool{}}
	previous := HostDeploy{Host: "h", Target: Target{Host: "h", Platform: "linux/amd64"}}
	cfg, err := LoadConfig(repo, flags, previous)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Target.Platform != "linux/arm64" {
		t.Errorf("platform = %q, want the explicit linux/arm64", cfg.Target.Platform)
	}
}

// The daemon-wide environment file is optional and lives at the repository
// root. Its absence must not break a deployment.
func TestDaemonEnvResolution(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repo, "jobs", "one")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte("version: 1\nname: one\nentrypoint: main.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Absent: no daemon environment, and that is not an error.
	cfg, err := LoadConfig(repo, &Flags{set: map[string]bool{}}, HostDeploy{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DaemonEnv != "" {
		t.Errorf("DaemonEnv = %q, want empty when the file is absent", cfg.DaemonEnv)
	}

	// Present at the default location.
	path := filepath.Join(repo, DaemonEnvFileName)
	if err := os.WriteFile(path, []byte("OTTER_NOTIFY_URL=https://example.test/hook\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(repo, &Flags{set: map[string]bool{}}, HostDeploy{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DaemonEnv != path {
		t.Errorf("DaemonEnv = %q, want %q", cfg.DaemonEnv, path)
	}

	// An explicit path wins, and a relative one resolves against the checkout.
	other := filepath.Join(repo, "daemon-production.env")
	if err := os.WriteFile(other, []byte("OTTER_NOTIFY_ON=failed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(repo, &Flags{DaemonEnv: "daemon-production.env", set: map[string]bool{}}, HostDeploy{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DaemonEnv != other {
		t.Errorf("DaemonEnv = %q, want the explicit %q", cfg.DaemonEnv, other)
	}
}

// A job named "daemon" would write to the same remote path as the
// daemon-wide environment file, so the name is reserved.
func TestDaemonJobNameIsReserved(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "daemon"} {
		dir := filepath.Join(repo, "jobs", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte("version: 1\nname: "+name+"\nentrypoint: main.py\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := LoadConfig(repo, &Flags{set: map[string]bool{}}, HostDeploy{}); err == nil {
		t.Error("a job named 'daemon' was accepted")
	} else if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("error does not explain the reservation: %v", err)
	}

	// Every other name is fine.
	if err := os.RemoveAll(filepath.Join(repo, "jobs", "daemon")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(repo, &Flags{set: map[string]bool{}}, HostDeploy{}); err != nil {
		t.Errorf("a checkout without a 'daemon' job should load: %v", err)
	}
}

// Everything about *reaching* a host belongs to that host. Carrying a port, a
// key path or a detected architecture to a different machine produced a
// missing-file error, a connection to the wrong service, and a binary built for
// the wrong architecture -- all from one polluted state file.
func TestOnlyLayoutTravelsToADifferentHost(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repo, "jobs", "one")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte("version: 1\nname: one\nentrypoint: main.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	previous := HostDeploy{
		Host: "127.0.0.1",
		Target: Target{
			Host:          "127.0.0.1",
			User:          "root",
			Port:          2225,
			IdentityFile:  "container-key",
			Platform:      "linux/arm64",
			RemoteDir:     "/opt/otter",
			ServiceName:   "otterd",
			RunAsUser:     "otter",
			DataDir:       "/opt/otter/workspaces/other-99999999/.otter/data",
			Listen:        "127.0.0.1:7338",
			WorkspaceID:   "99999999-0000-0000-0000-000000000000",
			WorkspaceSlug: "other",
		},
	}

	flags := &Flags{Target: Target{Host: "159.203.184.97"}, set: map[string]bool{}}
	cfg, err := LoadConfig(repo, flags, previous)
	if err != nil {
		t.Fatal(err)
	}

	// Connectivity must not travel.
	if cfg.Target.Port != DefaultSSHPort {
		t.Errorf("Port = %d, want the default %d, not the previous host's", cfg.Target.Port, DefaultSSHPort)
	}
	if cfg.Target.IdentityFile != "" {
		t.Errorf("IdentityFile = %q, want empty; that key belongs to another host", cfg.Target.IdentityFile)
	}
	if cfg.Target.Platform != "" {
		t.Errorf("Platform = %q, want empty so it is detected again", cfg.Target.Platform)
	}

	// The install root travels, because that is the operator's choice.
	if cfg.Target.RemoteDir != "/opt/otter" {
		t.Errorf("RemoteDir = %q, want the recorded layout", cfg.Target.RemoteDir)
	}
	// The workspace does not: it belongs to the host it was created on, so a
	// different machine gets its own directory, unit and port rather than being
	// handed one that may already be taken there.
	if cfg.Target.WorkspaceID == previous.Target.WorkspaceID {
		t.Error("the other host's workspace id was carried over")
	}
	if cfg.Target.Listen == previous.Target.Listen {
		t.Errorf("Listen = %q was carried over; that port may be taken on the new host", cfg.Target.Listen)
	}
	if !strings.HasPrefix(cfg.Target.DataDir, "/opt/otter/workspaces/") {
		t.Errorf("DataDir = %q is not inside the workspaces root", cfg.Target.DataDir)
	}
}

// The shared credentials file is optional, lives at the repository root, and is
// the only environment file a deploy reads. Per-job .env files are
// deliberately ignored: the daemon's environment is a single process
// environment, so they isolated nothing while turning one rotated credential
// into an N-file edit.
func TestSharedEnvResolution(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repo, "jobs", "one")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte("version: 1\nname: one\nentrypoint: main.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A per-job .env exists, and must not be picked up.
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SHOPIFY_CLIENT_ID=per-job\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Absent: no shared file, and that is not an error.
	cfg, err := LoadConfig(repo, &Flags{set: map[string]bool{}}, HostDeploy{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SharedEnv != "" {
		t.Errorf("SharedEnv = %q, want empty; a per-job .env must not be used", cfg.SharedEnv)
	}

	// Present at the default location.
	path := filepath.Join(repo, SharedEnvFileName)
	if err := os.WriteFile(path, []byte("SHOPIFY_CLIENT_ID=shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(repo, &Flags{set: map[string]bool{}}, HostDeploy{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SharedEnv != path {
		t.Errorf("SharedEnv = %q, want %q", cfg.SharedEnv, path)
	}

	// --env-file wins, and a relative path resolves against the checkout.
	other := filepath.Join(repo, "prod.env")
	if err := os.WriteFile(other, []byte("SHOPIFY_CLIENT_ID=prod\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(repo, &Flags{EnvFile: "prod.env", set: map[string]bool{}}, HostDeploy{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SharedEnv != other {
		t.Errorf("SharedEnv = %q, want the explicit %q", cfg.SharedEnv, other)
	}
}

// A missing credential is reported once, naming every job that needs
// it: the file is shared, so the same absence cannot be fixed per job.
func TestMissingSecretsNamesEveryJob(t *testing.T) {
	shared := filepath.Join(t.TempDir(), SharedEnvFileName)
	if err := os.WriteFile(shared, []byte("SHOPIFY_CLIENT_ID=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{SharedEnv: shared, Jobs: []Job{
		{Name: "alpha", Label: "alpha"},
		{Name: "beta", Label: "beta"},
	}}
	missing := cfg.MissingSecrets(map[string][]string{
		"alpha": {"SHOPIFY_CLIENT_ID", "SALESFORCE_CLIENT_SECRET"},
		"beta":  {"SALESFORCE_CLIENT_SECRET"},
	})

	if len(missing) != 1 {
		t.Fatalf("missing = %v, want one entry for the one absent key", missing)
	}
	if !strings.Contains(missing[0], "SALESFORCE_CLIENT_SECRET") {
		t.Errorf("entry does not name the absent key: %q", missing[0])
	}
	for _, name := range []string{"alpha", "beta"} {
		if !strings.Contains(missing[0], name) {
			t.Errorf("entry does not name %s: %q", name, missing[0])
		}
	}
	if strings.Contains(missing[0], "SHOPIFY_CLIENT_ID") {
		t.Errorf("a key present in the shared file was reported missing: %q", missing[0])
	}
}

// With no shared file, the report points at the file that is missing rather
// than at a per-job path that no longer exists.
func TestMissingSecretsPointsAtTheSharedFile(t *testing.T) {
	cfg := Config{Jobs: []Job{{Name: "alpha", Label: "alpha"}}}
	missing := cfg.MissingSecrets(map[string][]string{"alpha": {"SHOPIFY_CLIENT_ID"}})

	if len(missing) != 1 {
		t.Fatalf("missing = %v, want one entry", missing)
	}
	if !strings.Contains(missing[0], SharedEnvFileName) {
		t.Errorf("entry does not name %s: %q", SharedEnvFileName, missing[0])
	}
}

// A state file written before workspaces existed records the flat layout: unit
// `otterd`, data at <remote>/data, port 7337. None of that may leak into the
// workspace that replaces it -- the unit would collide with the old one and the
// data directory would sit outside the workspace.
func TestLegacyStateDoesNotLeakTheFlatLayout(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repo, "jobs", "one")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "otter.yaml"),
		[]byte("version: 1\nname: one\nentrypoint: main.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	legacy := `{"host":"159.203.184.97","target":{"host":"159.203.184.97","user":"root",` +
		`"port":22,"remote_dir":"/opt/otter","service_name":"otterd","run_as_user":"otter",` +
		`"data_dir":"/opt/otter/data","listen":"127.0.0.1:7337","platform":"linux/amd64"}}`
	var state State
	if err := json.Unmarshal([]byte(legacy), &state); err != nil {
		t.Fatal(err)
	}
	previous, ok, err := state.ForHost("159.203.184.97")
	if err != nil || !ok {
		t.Fatalf("legacy state was not readable: ok=%v err=%v", ok, err)
	}

	// A bare deploy: the host comes from the record, so the same-host carry
	// applies -- which is exactly the path that used to leak the flat layout.
	cfg, err := LoadConfig(repo, &Flags{set: map[string]bool{}}, previous)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Target.ServiceName == "otterd" {
		t.Error("the legacy unit name was carried into the workspace")
	}
	if cfg.Target.DataDir == "/opt/otter/data" {
		t.Error("the legacy data directory was carried into the workspace")
	}
	if !strings.HasPrefix(cfg.Target.DataDir, "/opt/otter/workspaces/") {
		t.Errorf("DataDir = %q, want it inside the workspaces root", cfg.Target.DataDir)
	}
	if cfg.Target.Platform != "linux/amd64" {
		t.Errorf("Platform = %q, want the host's detected platform carried", cfg.Target.Platform)
	}
	// A bare deploy must still know which machine it is for.
	if cfg.Target.Host != "159.203.184.97" {
		t.Errorf("Host = %q, want the recorded host carried for a bare deploy", cfg.Target.Host)
	}
}

// A deploy converges, so release retention is on by default: the flag carries a
// bounded window, an explicit value wins, and zero is the documented opt-out.
func TestDeployKeepDefaultsToTheBoundedWindow(t *testing.T) {
	f, err := ParseDeployFlags(nil, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if DefaultKeep != 3 {
		t.Fatalf("DefaultKeep = %d, want the proposed 3", DefaultKeep)
	}
	if f.Keep != DefaultKeep {
		t.Errorf("default keep = %d, want %d", f.Keep, DefaultKeep)
	}

	explicit, err := ParseDeployFlags([]string{"--keep", "10"}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if explicit.Keep != 10 {
		t.Errorf("explicit keep = %d, want 10", explicit.Keep)
	}

	keepAll, err := ParseDeployFlags([]string{"--keep", "0"}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if keepAll.Keep != 0 {
		t.Errorf("keep-all value = %d, want 0", keepAll.Keep)
	}
	if !keepAll.Set("keep") {
		t.Error("an explicit --keep 0 was not recorded as set, so it is indistinguishable from the default")
	}
}

// A negative window is meaningless and would silently become "keep one" inside
// Retain, so it is refused before anything is sent.
func TestDeployRejectsANegativeKeep(t *testing.T) {
	cfg := Config{Keep: -1, ProjectRoot: t.TempDir(), Timeout: time.Minute, Target: DefaultTarget()}
	cfg.Target.Host = "example.test"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a negative keep was accepted")
	}
	if !strings.Contains(err.Error(), "keep") {
		t.Errorf("refusal does not name --keep: %v", err)
	}
}

// CA-08: the caps a deploy writes are explicit, defaulted, and overridable from
// either a flag or the committed config, with the flag winning.
func TestDeployUnitCapsSurface(t *testing.T) {
	newRepo := func(t *testing.T) string {
		t.Helper()
		repo := t.TempDir()
		if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		job := filepath.Join(repo, "jobs", "counter")
		if err := os.MkdirAll(job, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(job, "otter.yaml"),
			[]byte("version: 1\nname: counter\nentrypoint: main.py\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(job, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return repo
	}

	load := func(t *testing.T, repo string, f *Flags) Config {
		t.Helper()
		cfg, err := LoadConfig(repo, f, HostDeploy{})
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		return cfg
	}

	t.Run("defaults", func(t *testing.T) {
		cfg := load(t, newRepo(t), &Flags{set: map[string]bool{}})
		if cfg.MemoryMax != DefaultMemoryMax || cfg.MemoryHigh != DefaultMemoryHigh ||
			cfg.MemorySwapMax != DefaultMemorySwapMax ||
			cfg.CPUQuota != DefaultCPUQuota || cfg.TasksMax != DefaultTasksMax {
			t.Errorf("caps = %q/%q/%q/%q/%q, want the defaults %q/%q/%q/%q/%q",
				cfg.MemoryMax, cfg.MemoryHigh, cfg.MemorySwapMax, cfg.CPUQuota, cfg.TasksMax,
				DefaultMemoryMax, DefaultMemoryHigh, DefaultMemorySwapMax, DefaultCPUQuota, DefaultTasksMax)
		}
		// The unit the deploy would write must actually carry them, and the
		// swap bound must be 0 -- an absent directive leaves the cgroup free to
		// swap past MemoryMax, which is the failure this default exists for.
		unit := UnitFile(cfg.Target, cfg.UnitOptions())
		if !strings.Contains(unit, "\nMemoryMax="+DefaultMemoryMax+"\n") {
			t.Errorf("the rendered unit does not carry the default cap:\n%s", unit)
		}
		if !strings.Contains(unit, "\nMemorySwapMax="+DefaultMemorySwapMax+"\n") {
			t.Errorf("the rendered unit does not carry the default swap bound:\n%s", unit)
		}
		// The default soft cap is the opt-out: with memory.high off, memory.max
		// is what stops a runaway instead of throttling it below the hard cap.
		if cfg.MemoryHigh != CapOff {
			t.Errorf("MemoryHigh = %q, want the default %q", cfg.MemoryHigh, CapOff)
		}
		if strings.Contains(unit, "MemoryHigh=") {
			t.Errorf("the default deploy emits a soft cap:\n%s", unit)
		}
		// An explicit soft cap still reaches the unit: only the default changed.
		withHigh := cfg.UnitOptions()
		withHigh.MemoryHigh = "50%"
		if !strings.Contains(UnitFile(cfg.Target, withHigh), "\nMemoryHigh=50%\n") {
			t.Errorf("an explicit MemoryHigh no longer reaches the unit:\n%s", UnitFile(cfg.Target, withHigh))
		}
	})

	t.Run("flags override", func(t *testing.T) {
		f := &Flags{MemoryMax: "8G", MemoryHigh: "6G", MemorySwapMax: "512M", CPUQuota: "400%", TasksMax: "1024", set: map[string]bool{}}
		cfg := load(t, newRepo(t), f)
		if cfg.MemoryMax != "8G" || cfg.MemoryHigh != "6G" || cfg.MemorySwapMax != "512M" || cfg.CPUQuota != "400%" || cfg.TasksMax != "1024" {
			t.Errorf("flags did not override the defaults: %+v", cfg.UnitOptions())
		}
	})

	t.Run("the config file sets them", func(t *testing.T) {
		repo := newRepo(t)
		config := "memory_max: 2G\nmemory_high: 1G\nmemory_swap_max: 256M\ncpu_quota: 100%\ntasks_max: 128\nread_write_paths:\n  - /srv/scratch\n"
		if err := os.WriteFile(filepath.Join(repo, ConfigFileName), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := load(t, repo, &Flags{set: map[string]bool{}})
		if cfg.MemoryMax != "2G" || cfg.MemoryHigh != "1G" || cfg.MemorySwapMax != "256M" || cfg.CPUQuota != "100%" || cfg.TasksMax != "128" {
			t.Errorf("config file caps did not apply: %+v", cfg.UnitOptions())
		}
		if len(cfg.ReadWritePaths) != 1 || cfg.ReadWritePaths[0] != "/srv/scratch" {
			t.Errorf("config file read_write_paths = %v", cfg.ReadWritePaths)
		}
	})

	t.Run("a flag beats the config file and extends its paths", func(t *testing.T) {
		repo := newRepo(t)
		config := "memory_max: 2G\nread_write_paths:\n  - /srv/committed\n"
		if err := os.WriteFile(filepath.Join(repo, ConfigFileName), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
		f := &Flags{MemoryMax: "8G", ReadWritePaths: stringList{"/srv/scratch"}}
		cfg := load(t, repo, f)
		if cfg.MemoryMax != "8G" {
			t.Errorf("MemoryMax = %q, want the flag's 8G", cfg.MemoryMax)
		}
		if len(cfg.ReadWritePaths) != 2 || cfg.ReadWritePaths[0] != "/srv/committed" || cfg.ReadWritePaths[1] != "/srv/scratch" {
			t.Errorf("ReadWritePaths = %v, want the committed path plus the flag's", cfg.ReadWritePaths)
		}
	})

	t.Run("off is a real opt-out", func(t *testing.T) {
		f := &Flags{MemoryMax: CapOff, set: map[string]bool{}}
		cfg := load(t, newRepo(t), f)
		if cfg.MemoryMax != CapOff {
			t.Errorf("MemoryMax = %q, want the explicit %q", cfg.MemoryMax, CapOff)
		}
		unit := UnitFile(cfg.Target, cfg.UnitOptions())
		if strings.Contains(unit, "MemoryMax=") {
			t.Errorf("an opted-out cap was still emitted:\n%s", unit)
		}
		// Opting one cap out must not opt the swap bound out with it: the
		// swap directive is independent, and its default is a real limit.
		if !strings.Contains(unit, "\nMemorySwapMax="+DefaultMemorySwapMax+"\n") {
			t.Errorf("opting MemoryMax out also dropped the default swap bound:\n%s", unit)
		}
	})

	t.Run("the swap bound accepts zero", func(t *testing.T) {
		// Unlike the other caps, a zero here is a meaningful limit ("no
		// swap"), so it must survive validation and reach the unit.
		for _, value := range []string{"0", "0M"} {
			f := &Flags{MemorySwapMax: value, set: map[string]bool{}}
			cfg := load(t, newRepo(t), f)
			if cfg.MemorySwapMax != value {
				t.Errorf("MemorySwapMax = %q, want %q", cfg.MemorySwapMax, value)
			}
			cfg.ProjectRoot = t.TempDir()
			cfg.Target.Host = "example.test"
			cfg.Timeout = time.Minute
			if err := cfg.Validate(); err != nil {
				t.Errorf("--memory-swap-max %s was refused: %v", value, err)
			}
			if unit := UnitFile(cfg.Target, cfg.UnitOptions()); !strings.Contains(unit, "\nMemorySwapMax="+value+"\n") {
				t.Errorf("unit does not carry MemorySwapMax=%s:\n%s", value, unit)
			}
		}
	})

	t.Run("the rw-path flag is repeatable", func(t *testing.T) {
		f, err := ParseDeployFlags([]string{"--rw-path", "/srv/a", "--rw-path", "/srv/b"}, io.Discard)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(f.ReadWritePaths) != 2 || f.ReadWritePaths[0] != "/srv/a" || f.ReadWritePaths[1] != "/srv/b" {
			t.Errorf("--rw-path collected %v, want both paths", f.ReadWritePaths)
		}
	})
}

// A malformed cap would be written into the unit and only fail on the host, at
// restart, after the deploy claimed success. Each is refused locally.
func TestDeployRejectsMalformedCaps(t *testing.T) {
	base := func() Config {
		target := DefaultTarget()
		target.Host = "example.test"
		return Config{ProjectRoot: t.TempDir(), Timeout: time.Minute, Target: target, Keep: DefaultKeep}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"memory size", func(c *Config) { c.MemoryMax = "lots" }, "memory-max"},
		{"memory suffix", func(c *Config) { c.MemoryMax = "4X" }, "memory-max"},
		{"memory zero", func(c *Config) { c.MemoryMax = "0" }, "memory-max"},
		{"memory zero M", func(c *Config) { c.MemoryMax = "0M" }, "memory-max"},
		{"memory zero G", func(c *Config) { c.MemoryMax = "0G" }, "memory-max"},
		{"memory zero percent", func(c *Config) { c.MemoryMax = "0%" }, "memory-max"},
		{"memory high zero", func(c *Config) { c.MemoryHigh = "0" }, "memory-high"},
		{"memory swap size", func(c *Config) { c.MemorySwapMax = "lots" }, "memory-swap-max"},
		{"memory swap suffix", func(c *Config) { c.MemorySwapMax = "4X" }, "memory-swap-max"},
		{"memory swap negative percent", func(c *Config) { c.MemorySwapMax = "-10%" }, "memory-swap-max"},
		{"cpu quota", func(c *Config) { c.CPUQuota = "half" }, "cpu-quota"},
		{"cpu quota zero", func(c *Config) { c.CPUQuota = "0%" }, "cpu-quota"},
		{"tasks max", func(c *Config) { c.TasksMax = "many" }, "tasks-max"},
		{"tasks max zero", func(c *Config) { c.TasksMax = "0" }, "tasks-max"},
		{"rw path relative", func(c *Config) { c.ReadWritePaths = []string{"srv/scratch"} }, "rw-path"},
		{"rw path in home", func(c *Config) { c.ReadWritePaths = []string{"/home/deploy/scratch"} }, "ProtectHome"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			cfg.MemoryMax, cfg.MemoryHigh, cfg.MemorySwapMax = DefaultMemoryMax, DefaultMemoryHigh, DefaultMemorySwapMax
			cfg.CPUQuota, cfg.TasksMax = DefaultCPUQuota, DefaultTasksMax
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not name %q", err, tc.want)
			}
		})
	}
}
