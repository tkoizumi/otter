package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is what a deployment remembers between runs.
//
// It is the only reason `otter deploy` can be idempotent: it records what was
// last pushed to which host so that a second run with no changes can report
// "already up to date" instead of restarting a healthy daemon.
//
// The API token is deliberately not in here. State lives in the repository
// working tree, and a bearer token that grants arbitrary execution is not
// something to leave lying next to the source. It goes in state.secret.json,
// which shares the 0700 state directory.
type State struct {
	// Host is the SSH destination this state describes.
	Host string `json:"host"`
	// Target is the full target of the last successful deploy.
	Target Target `json:"target"`
	// Version is the build version shipped by the last deploy.
	Version string `json:"version"`
	// Revision is a hash of everything that was pushed: both binaries plus the
	// integration and library trees. Equal revisions mean a no-op deploy.
	Revision string `json:"revision"`
	// SecretsRevision hashes the integration env files. It is tracked
	// separately so that rotating a credential restarts the daemon without
	// pretending the code changed.
	SecretsRevision string `json:"secrets_revision,omitempty"`
	// DeployedAt is when that revision landed.
	DeployedAt time.Time `json:"deployed_at"`
}

// secretState holds the token, separately from State.
type secretState struct {
	APIToken string `json:"api_token"`
}

// StateStore reads and writes the state files under .otter/.
type StateStore struct {
	Dir string
}

// NewStateStore returns the store for a repository root.
func NewStateStore(repoRoot string) StateStore {
	return StateStore{Dir: filepath.Join(repoRoot, StateDirName)}
}

func (s StateStore) statePath() string  { return filepath.Join(s.Dir, StateFileName) }
func (s StateStore) secretPath() string { return filepath.Join(s.Dir, "state.secret.json") }

// Load reads the stored state. A missing state file is not an error: it means
// this is the first deploy, and the caller gets a zero State with ok=false.
func (s StateStore) Load() (State, bool, error) {
	var st State

	data, err := os.ReadFile(s.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return st, false, nil
	}
	if err != nil {
		return st, false, fmt.Errorf("read %s: %w", s.statePath(), err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, false, fmt.Errorf("parse %s: %w", s.statePath(), err)
	}
	return st, true, nil
}

// LoadToken reads the stored API token, if any.
func (s StateStore) LoadToken() (string, error) {
	data, err := os.ReadFile(s.secretPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", s.secretPath(), err)
	}
	var sec secretState
	if err := json.Unmarshal(data, &sec); err != nil {
		return "", fmt.Errorf("parse %s: %w", s.secretPath(), err)
	}
	return sec.APIToken, nil
}

// Save writes the state and token atomically with restrictive permissions.
// The directory is created on first use.
func (s StateStore) Save(st State, token string) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", s.Dir, err)
	}
	if err := writeFileAtomic(s.statePath(), mustJSON(st), 0o600); err != nil {
		return err
	}
	if token != "" {
		sec := secretState{APIToken: token}
		if err := writeFileAtomic(s.secretPath(), mustJSON(sec), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// Remove deletes the state files, used by `otter deploy --destroy`.
func (s StateStore) Remove() error {
	for _, path := range []string{s.statePath(), s.secretPath()} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

// writeFileAtomic writes via a temporary file in the same directory so that a
// crash mid-write cannot leave a truncated state file behind.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpName, path, err)
	}
	return nil
}

func mustJSON(v any) []byte {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		// Every value written here is a plain struct of strings, so this is
		// unreachable; panicking beats silently writing a corrupt state file.
		panic(fmt.Sprintf("deploy: cannot encode state: %v", err))
	}
	return append(data, '\n')
}
