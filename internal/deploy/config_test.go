package deploy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestApplyDefaultsDerivesDataDir(t *testing.T) {
	target := Target{Host: "h", RemoteDir: "/srv/otter"}
	target.ApplyDefaults()

	if target.DataDir != "/srv/otter/data" {
		t.Errorf("DataDir = %q, want /srv/otter/data", target.DataDir)
	}
	if target.ServiceUnit() != "otterd.service" {
		t.Errorf("ServiceUnit = %q, want otterd.service", target.ServiceUnit())
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

// A limited deploy must name exactly one integration, must refuse an unknown
// one, and must be flagged so the push protects the integrations it is not
// carrying.
func TestIntegrationFilter(t *testing.T) {
	newRepo := func(t *testing.T) string {
		t.Helper()
		repo := t.TempDir()
		if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"alpha", "beta"} {
			dir := filepath.Join(repo, "integrations", name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte("name: "+name+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return repo
	}

	load := func(t *testing.T, repo string, f *Flags) (Config, error) {
		t.Helper()
		return LoadConfig(repo, f, State{})
	}

	t.Run("no filter deploys everything", func(t *testing.T) {
		repo := newRepo(t)
		cfg, err := load(t, repo, &Flags{set: map[string]bool{}})
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Integrations) != 2 {
			t.Errorf("deployed %v, want both integrations", cfg.Integrations)
		}
		if cfg.Limited {
			t.Error("an unfiltered deploy reported itself as limited")
		}
	})

	t.Run("a named integration is the only one", func(t *testing.T) {
		repo := newRepo(t)
		cfg, err := load(t, repo, &Flags{Integration: "beta", set: map[string]bool{}})
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Integrations) != 1 || cfg.Integrations[0] != "beta" {
			t.Errorf("deployed %v, want just beta", cfg.Integrations)
		}
		if !cfg.Limited {
			t.Error("a single-integration deploy should be marked limited")
		}
	})

	t.Run("an unknown integration is an error", func(t *testing.T) {
		repo := newRepo(t)
		_, err := load(t, repo, &Flags{Integration: "gamma", set: map[string]bool{}})
		if err == nil {
			t.Fatal("an unknown integration was accepted")
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
	dir := filepath.Join(repo, "integrations", "one")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte("name: one\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	previous := State{
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
	dir := filepath.Join(repo, "integrations", "one")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "otter.yaml"), []byte("name: one\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	flags := &Flags{Target: Target{Platform: "linux/arm64"}, set: map[string]bool{}}
	previous := State{Host: "h", Target: Target{Host: "h", Platform: "linux/amd64"}}
	cfg, err := LoadConfig(repo, flags, previous)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Target.Platform != "linux/arm64" {
		t.Errorf("platform = %q, want the explicit linux/arm64", cfg.Target.Platform)
	}
}
