package secretbroker

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// credentialBytes is the size of a minted broker credential. It is not a
// password a human types: it only ever travels from the host's files into a
// tenant's environment and back, so the only requirement is that guessing it is
// infeasible.
const credentialBytes = 32

// MintCredential returns a fresh broker credential.
//
// The credential is the HMAC key for one runtime's workload identities, so its
// unpredictability is what stops one tenant forging another's exchange.
func MintCredential() (string, error) {
	buf := make([]byte, credentialBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("secretbroker: mint credential: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// EnsureCredentialFile returns the broker credential for a runtime, minting and
// persisting one when the file does not have it yet.
//
// It is idempotent on purpose. A re-provision of the same runtime must not mint
// a second credential: the tenant's environment and the broker's file would then
// disagree, and every run would fail authentication with a message about a bad
// token rather than about the half-updated pair that caused it.
//
// The file is written 0600 through a temporary file and a rename, so a reader
// never sees a partial document and a crash mid-write cannot leave a
// half-written credential file behind. `FileCredentials` re-reads on change, so
// a broker already running picks the new tenant up without a restart.
func EnsureCredentialFile(path, runtimeID string) (string, error) {
	if runtimeID == "" {
		return "", errors.New("secretbroker: a runtime id is required to store a credential")
	}

	values, err := readCredentialFileForUpdate(path)
	if err != nil {
		return "", err
	}
	if existing := values[runtimeID]; existing != "" {
		return existing, nil
	}

	credential, err := MintCredential()
	if err != nil {
		return "", err
	}
	values[runtimeID] = credential

	if err := writeCredentialFile(path, values); err != nil {
		return "", err
	}
	return credential, nil
}

// readCredentialFileForUpdate reads the file for a read-modify-write, applying
// the same permission rule the reader does: a file this sensitive must not be
// group- or world-readable, and a writer must not silently accept one that is.
func readCredentialFileForUpdate(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("secretbroker: stat credential file: %w", err)
	}
	return loadCredentialFile(path, info)
}

func writeCredentialFile(path string, values map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("secretbroker: prepare credential directory: %w", err)
	}
	raw, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return fmt.Errorf("secretbroker: marshal credentials: %w", err)
	}
	raw = append(raw, '\n')

	// 0600 in the CreateTemp call itself, not a chmod afterwards: between the
	// two there is a window in which the file exists with a wider mode.
	temporary, err := os.CreateTemp(filepath.Dir(path), ".broker-credentials-*")
	if err != nil {
		return fmt.Errorf("secretbroker: create temporary credential file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secretbroker: set credential file mode: %w", err)
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secretbroker: write credential file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("secretbroker: close credential file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("secretbroker: install credential file: %w", err)
	}
	return nil
}
