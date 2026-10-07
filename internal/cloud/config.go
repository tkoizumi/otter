// Package cloud is the Otter Cloud client: the credential `otter login`
// stores, the control-plane calls `otter deploy --cloud` makes, and the one
// place the wire protocol is spelled out.
//
// It exists so the CLI stays a thin surface, exactly as it does over
// internal/api: the protocol can be driven against an httptest server with no
// terminal, no network and no real Cloud. A change to the control plane belongs
// here, not in a command function.
package cloud

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultURL is the hosted control plane used when neither a flag, the
// environment, nor a stored login names one.
const DefaultURL = "https://app.runotter.dev"

// The environment overrides. They mirror --cloud and --token so a CI job can
// log in and deploy without a config file, and they take precedence over a
// stored login the way an explicit flag does.
const (
	// URLEnv names the control plane base URL.
	URLEnv = "OTTER_CLOUD_URL"
	// TokenEnv carries the `otk_<id>_<secret>` credential.
	TokenEnv = "OTTER_CLOUD_TOKEN"
	// OperatorEnv names the person a deploy is attributed to, when the local
	// account name is not the right one.
	OperatorEnv = "OTTER_CLOUD_OPERATOR"
)

// ConfigDirName and ConfigFileName locate the stored credential. It lives
// beside the rest of Otter's per-user state, so removing ~/.otter is a
// complete logout and nothing else on the machine has to be told.
const (
	ConfigDirName  = ".otter"
	ConfigFileName = "cloud.json"
)

// Config is what `otter login` persists and every cloud command reads back.
//
// It deliberately holds no derived state: the organization is recorded from
// the verification call so `--status` can answer without the network, but the
// runtimes are always read live, because a runtime assigned since login would
// otherwise be invisible until the next login.
type Config struct {
	CloudURL         string    `json:"cloud_url"`
	Token            string    `json:"token"`
	OrganizationID   string    `json:"organization_id,omitempty"`
	OrganizationName string    `json:"organization_name,omitempty"`
	SavedAt          time.Time `json:"saved_at"`
}

// ErrNotLoggedIn reports that no usable credential is stored. It is a sentinel
// so a caller can tell "nobody has logged in" from "the file is unreadable".
var ErrNotLoggedIn = errors.New("not logged in to Otter Cloud")

// ConfigPath returns the absolute path of the stored credential.
func ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory: %w", err)
	}
	return filepath.Join(home, ConfigDirName, ConfigFileName), nil
}

// LoadConfig reads the stored credential.
//
// A missing file and a blank token both mean "not logged in" and both return
// ErrNotLoggedIn: blanking the token is a logout spelling the CLI once offered,
// and treating it as a corrupt file would turn an old logout into a confusing
// error. The returned Config is still populated on that error, so a caller can
// reuse a stored cloud_url even when the credential is gone.
func LoadConfig() (Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return Config{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, ErrNotLoggedIn
		}
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return cfg, ErrNotLoggedIn
	}
	return cfg, nil
}

// SaveConfig writes the credential, creating ~/.otter when it does not exist.
//
// The file is 0600 and the directory is 0700: the token is a bearer credential
// for a whole organization, so it must not be readable by another local
// account. The mode is set explicitly after writing as well, because
// os.WriteFile only applies its mode on creation and an existing file with a
// looser mode would otherwise keep it.
func SaveConfig(cfg Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	body := append(encoded, '\n')
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("set permissions on %s: %w", path, err)
	}
	return nil
}

// RemoveConfig deletes the stored credential and returns what was removed, so
// `otter logout` can say what it took away. It is idempotent: a machine that
// was never logged in removes nothing and returns the zero Config without
// error.
func RemoveConfig() (Config, error) {
	cfg, _ := LoadConfig() // the zero Config is the right answer when absent
	path, err := ConfigPath()
	if err != nil {
		return cfg, err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return cfg, fmt.Errorf("remove %s: %w", path, err)
	}
	return cfg, nil
}

// Identity is the resolved control plane and credential a command uses.
type Identity struct {
	BaseURL string
	Token   string
	// Stored is the config the answer came from, zero when none was found.
	Stored Config
}

// Resolve works out which control plane and credential a command should use.
//
// Precedence is the explicit flag, then the environment, then the stored
// login, then the hosted default -- for the URL -- and flag, environment,
// stored for the token. Reading the store here rather than in each command is
// what keeps `otter login` and `otter deploy --cloud` from disagreeing about
// which Cloud they mean.
func Resolve(flagURL, flagToken string) (Identity, error) {
	stored, err := LoadConfig()
	if err != nil && !errors.Is(err, ErrNotLoggedIn) {
		return Identity{}, err
	}
	return Identity{
		BaseURL: ResolveBaseURL(flagURL, stored),
		Token:   ResolveToken(flagToken, stored),
		Stored:  stored,
	}, nil
}

// ResolveBaseURL applies the documented precedence for the control-plane URL.
func ResolveBaseURL(flagURL string, stored Config) string {
	for _, candidate := range []string{flagURL, os.Getenv(URLEnv), stored.CloudURL} {
		if value := strings.TrimSpace(candidate); value != "" {
			return strings.TrimRight(value, "/")
		}
	}
	return DefaultURL
}

// ResolveToken applies the documented precedence for the credential. It does
// NOT fall back to the network or to a prompt: a command that needs a token it
// does not have says so, and only `otter login` prompts.
func ResolveToken(flagToken string, stored Config) string {
	for _, candidate := range []string{flagToken, os.Getenv(TokenEnv), stored.Token} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}
	return ""
}

// Operator names the person a deploy is attributed to. The control plane
// records it in the operation so "who promoted this?" has an answer; the flag
// the protocol itself carries is empty when nothing local can name one.
func Operator() string {
	for _, candidate := range []string{os.Getenv(OperatorEnv), os.Getenv("USER"), os.Getenv("LOGNAME")} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}
	return ""
}
