package cloud

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// privateHome points HOME at a temporary directory, so the credential tests
// never touch a developer's real ~/.otter.
func privateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestConfigRoundTripIsPrivate(t *testing.T) {
	home := privateHome(t)
	saved := Config{
		CloudURL:         "https://cloud.example",
		Token:            "otk_1_secret",
		OrganizationID:   "org_1",
		OrganizationName: "Acme",
		SavedAt:          time.Now().UTC().Truncate(time.Second),
	}
	if err := SaveConfig(saved); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	path, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if want := filepath.Join(home, ConfigDirName, ConfigFileName); path != want {
		t.Errorf("ConfigPath = %q, want %q", path, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %04o, want 0600", perm)
	}

	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.CloudURL != saved.CloudURL || got.Token != saved.Token ||
		got.OrganizationID != saved.OrganizationID || got.OrganizationName != saved.OrganizationName {
		t.Errorf("round trip = %+v, want %+v", got, saved)
	}
	if !got.SavedAt.Equal(saved.SavedAt) {
		t.Errorf("SavedAt = %s, want %s", got.SavedAt, saved.SavedAt)
	}
}

func TestLoadConfigWithoutAFileIsNotLoggedIn(t *testing.T) {
	privateHome(t)
	if _, err := LoadConfig(); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("err = %v, want ErrNotLoggedIn", err)
	}
}

func TestRemoveConfigIsIdempotent(t *testing.T) {
	privateHome(t)
	if err := SaveConfig(Config{CloudURL: "https://cloud.example", Token: "otk_1_secret", OrganizationName: "Acme"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	removed, err := RemoveConfig()
	if err != nil {
		t.Fatalf("RemoveConfig: %v", err)
	}
	if removed.OrganizationName != "Acme" {
		t.Errorf("removed = %+v, want the stored config", removed)
	}
	path, _ := ConfigPath()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config still exists after removal: %v", err)
	}
	again, err := RemoveConfig()
	if err != nil {
		t.Fatalf("second RemoveConfig: %v", err)
	}
	if again != (Config{}) {
		t.Errorf("second removal returned %+v, want the zero config", again)
	}
}

func TestResolvePrecedence(t *testing.T) {
	privateHome(t)
	stored := Config{CloudURL: "https://stored.example", Token: "otk_stored"}

	t.Setenv(URLEnv, "https://env.example")
	t.Setenv(TokenEnv, "otk_env")

	if got := ResolveBaseURL("https://flag.example", stored); got != "https://flag.example" {
		t.Errorf("flag URL = %q", got)
	}
	if got := ResolveBaseURL("", stored); got != "https://env.example" {
		t.Errorf("env URL = %q", got)
	}
	t.Setenv(URLEnv, "")
	if got := ResolveBaseURL("", stored); got != "https://stored.example" {
		t.Errorf("stored URL = %q", got)
	}
	if got := ResolveBaseURL("", Config{}); got != DefaultURL {
		t.Errorf("default URL = %q, want %q", got, DefaultURL)
	}

	if got := ResolveToken("otk_flag", stored); got != "otk_flag" {
		t.Errorf("flag token = %q", got)
	}
	if got := ResolveToken("", stored); got != "otk_env" {
		t.Errorf("env token = %q", got)
	}
	t.Setenv(TokenEnv, "")
	if got := ResolveToken("", stored); got != "otk_stored" {
		t.Errorf("stored token = %q", got)
	}
	// The trailing slash on a URL is normalized away, so a stored value that
	// has one cannot produce a double slash in a path.
	if got := ResolveBaseURL("https://flag.example/", Config{}); got != "https://flag.example" {
		t.Errorf("normalized URL = %q", got)
	}
}
