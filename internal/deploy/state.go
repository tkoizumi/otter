package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// State is what a project remembers across deploys: one record per host it has
// been deployed to.
//
// It used to hold a single host, which was correct only while a project could
// belong to one machine. A project can now own a workspace on several hosts, so
// the record is keyed by host and carries the workspace it landed in.
//
// The API token is deliberately not in here. State lives in the project working
// tree, and a bearer token that grants arbitrary execution is not something to
// leave lying next to the source. It goes in state.secret.json, which shares
// the 0700 state directory.
type State struct {
	Deploys map[string]HostDeploy `json:"deploys,omitempty"`
}

// HostDeploy is one project's record for one host.
type HostDeploy struct {
	// Host is the SSH destination this record describes.
	Host string `json:"host"`
	// Target is the full target of the last successful deploy, including the
	// workspace it owns there.
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

	// Bindings relates each deployed integration's label to the identity the
	// destination runtime assigned it. The destination mints its own identity
	// -- local and remote ids are independent -- so this record is the only
	// place the two are related, and it is what lets a later deploy tell
	// "same instance, new code" from "new instance" without guessing.
	Bindings []Binding `json:"bindings,omitempty"`
}

// Binding is one destination registration as recorded by a deploy.
type Binding struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	Path string `json:"path"`
}

// UnmarshalJSON reads both the current shape and the single-host shape written
// by earlier versions, so upgrading otter does not turn an existing project
// into a first-time deploy and lose the host it belongs to.
func (s *State) UnmarshalJSON(data []byte) error {
	var probe struct {
		Deploys map[string]HostDeploy `json:"deploys"`
		// Legacy flat fields.
		Host            string    `json:"host"`
		Target          Target    `json:"target"`
		Version         string    `json:"version"`
		Revision        string    `json:"revision"`
		SecretsRevision string    `json:"secrets_revision"`
		DeployedAt      time.Time `json:"deployed_at"`
		Bindings        []Binding `json:"bindings"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	if len(probe.Deploys) > 0 {
		s.Deploys = probe.Deploys
		return nil
	}
	if probe.Host != "" {
		s.Deploys = map[string]HostDeploy{probe.Host: {
			Host:            probe.Host,
			Target:          probe.Target,
			Version:         probe.Version,
			Revision:        probe.Revision,
			SecretsRevision: probe.SecretsRevision,
			DeployedAt:      probe.DeployedAt,
			Bindings:        probe.Bindings,
		}}
	}
	return nil
}

// ForHost returns the record for an SSH destination. An empty host means "the
// only one", which is what makes a bare `otter deploy` after the first one go
// to the same machine; several hosts without one named is ambiguous rather than
// a guess.
func (s State) ForHost(host string) (HostDeploy, bool, error) {
	host = hostOnly(host)
	if host != "" {
		rec, ok := s.Deploys[host]
		return rec, ok, nil
	}
	switch len(s.Deploys) {
	case 0:
		return HostDeploy{}, false, nil
	case 1:
		for _, rec := range s.Deploys {
			return rec, true, nil
		}
	}
	return HostDeploy{}, false, fmt.Errorf(
		"this project has deployed to %d hosts (%s); name one with --host",
		len(s.Deploys), joinSorted(s.Hosts()))
}

// Put stores a record under its host.
func (s *State) Put(rec HostDeploy) {
	if s.Deploys == nil {
		s.Deploys = map[string]HostDeploy{}
	}
	s.Deploys[rec.Host] = rec
}

// Delete forgets one host.
func (s *State) Delete(host string) {
	delete(s.Deploys, host)
}

// Hosts lists the recorded hosts, sorted.
func (s State) Hosts() []string {
	hosts := make([]string, 0, len(s.Deploys))
	for host := range s.Deploys {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

// hostOnly drops the login user from an SSH destination, so "root@host" and a
// stored "host" compare equal.
func hostOnly(dest string) string {
	for i := 0; i < len(dest); i++ {
		if dest[i] == '@' {
			return dest[i+1:]
		}
	}
	return dest
}

func joinSorted(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += ", "
		}
		out += item
	}
	return out
}

// secretState holds the API tokens, one per host and workspace.
//
// A token belongs to one daemon, and a daemon belongs to one workspace on one
// host, so the key is both. The legacy single-token field is still read so an
// existing checkout keeps the token the host already knows.
type secretState struct {
	Tokens   map[string]string `json:"tokens,omitempty"`
	APIToken string            `json:"api_token,omitempty"`
}

// tokenKey names one daemon's token.
func tokenKey(host, workspaceID string) string { return host + "/" + workspaceID }

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

// LoadToken reads the stored API token for one host and workspace.
func (s StateStore) LoadToken(host, workspaceID string) (string, error) {
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
	if token := sec.Tokens[tokenKey(host, workspaceID)]; token != "" {
		return token, nil
	}
	// A token stored before tokens were keyed still belongs to this checkout's
	// only daemon.
	return sec.APIToken, nil
}

// Save writes the state and, when one was resolved, this daemon's token
// atomically with restrictive permissions. The directory is created on first
// use.
func (s StateStore) Save(st State, token, host, workspaceID string) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", s.Dir, err)
	}
	if err := writeFileAtomic(s.statePath(), mustJSON(st), 0o600); err != nil {
		return err
	}
	if token == "" {
		return nil
	}
	// Preserve every other daemon's token rather than replacing the file.
	sec := secretState{Tokens: map[string]string{}}
	if data, err := os.ReadFile(s.secretPath()); err == nil {
		_ = json.Unmarshal(data, &sec)
		if sec.Tokens == nil {
			sec.Tokens = map[string]string{}
		}
	}
	sec.Tokens[tokenKey(host, workspaceID)] = token
	sec.APIToken = ""
	return writeFileAtomic(s.secretPath(), mustJSON(sec), 0o600)
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
		// The values here are plain structs of strings, numbers and times.
		panic(fmt.Sprintf("marshal state: %v", err))
	}
	return append(data, '\n')
}
