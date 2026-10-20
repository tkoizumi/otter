package secretbroker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureCredentialFileMintsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "broker-credentials.json")

	credential, err := EnsureCredentialFile(path, "rt_a")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if credential == "" {
		t.Fatal("minted an empty credential")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %04o, want 0600; this file holds every tenant's credential", mode)
	}

	// The directory holding it is tightened too: a 0755 directory would let any
	// local user list the file even though they cannot read it.
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := dir.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("directory mode = %04o, want no group or other access", mode)
	}
}

// The reader and the writer must agree on the format; a round trip through the
// real reader is the only test that proves it.
func TestEnsureCredentialFileIsReadableByTheBroker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker-credentials.json")
	credential, err := EnsureCredentialFile(path, "rt_a")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	store := NewFileCredentials(path)
	value, ok, err := store.BrokerCredential(context.Background(), "rt_a")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !ok || value != credential {
		t.Errorf("broker read %q, %v; want the minted credential", value, ok)
	}
}

// A re-provision must not mint a second credential. If it did, the tenant's
// environment and the broker's file would disagree and every run would fail
// authentication with a message about a bad token.
func TestEnsureCredentialFileIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker-credentials.json")

	first, err := EnsureCredentialFile(path, "rt_a")
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	second, err := EnsureCredentialFile(path, "rt_a")
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if first != second {
		t.Errorf("a re-provision minted a new credential: %q then %q", first, second)
	}
}

func TestEnsureCredentialFileAddsWithoutDisturbingOthers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker-credentials.json")

	a, err := EnsureCredentialFile(path, "rt_a")
	if err != nil {
		t.Fatalf("ensure a: %v", err)
	}
	b, err := EnsureCredentialFile(path, "rt_b")
	if err != nil {
		t.Fatalf("ensure b: %v", err)
	}
	if a == b {
		t.Error("two runtimes were given the same credential")
	}

	// Both are present afterwards, and rt_a's value is unchanged.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if values["rt_a"] != a || values["rt_b"] != b {
		t.Errorf("file holds %v; want rt_a and rt_b as minted", values)
	}
}

// A writer must not silently accept a file the reader would refuse: doing so
// would leave the host in a state where the broker refuses to start and the
// agent reports success.
func TestEnsureCredentialFileRefusesALooseModeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker-credentials.json")
	writeCredentialFixture(t, path, 0o644, map[string]string{"rt_a": "existing"})

	if _, err := EnsureCredentialFile(path, "rt_b"); err == nil {
		t.Fatal("a group-readable credentials file was accepted for update")
	}
}

func TestEnsureCredentialFileRequiresARuntimeID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker-credentials.json")

	if _, err := EnsureCredentialFile(path, ""); err == nil {
		t.Error("an empty runtime id should be refused")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a refused call should not have written a file")
	}
}

func TestMintCredentialIsUnpredictableAndUsable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		credential, err := MintCredential()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if seen[credential] {
			t.Fatalf("minted a duplicate credential after %d draws", i)
		}
		seen[credential] = true
		if len(credential) < 40 {
			t.Fatalf("credential %q is shorter than 32 bytes of entropy", credential)
		}
	}
}
