package secretbroker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/secrets"
)

func writeCredentialFixture(t *testing.T, path string, mode os.FileMode, values map[string]string) {
	t.Helper()
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, mode); err != nil {
		t.Fatalf("write: %v", err)
	}
	// WriteFile's mode is filtered by the umask, so set it explicitly: a test
	// for a permission refusal must not pass because the umask was strict.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod: %v", err)
	}
}

func TestFileCredentialsLoadsAndLooksUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker-credentials.json")
	writeCredentialFixture(t, path, 0o600, map[string]string{"rt_a": "credential-a"})
	store := NewFileCredentials(path)

	value, ok, err := store.BrokerCredential(context.Background(), "rt_a")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !ok || value != "credential-a" {
		t.Errorf("lookup = %q, %v; want credential-a, true", value, ok)
	}

	// An unknown runtime is absent, not an error: the broker answers 401 to an
	// unknown runtime, and it must not have to distinguish "no file" from
	// "no entry".
	value, ok, err = store.BrokerCredential(context.Background(), "rt_unknown")
	if err != nil {
		t.Fatalf("unknown lookup: %v", err)
	}
	if ok || value != "" {
		t.Errorf("unknown runtime returned %q, %v", value, ok)
	}
}

// A file readable by group or other is every tenant's credential on the host,
// so it is refused before its contents are even parsed.
func TestFileCredentialsRefusesALooseMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "broker-credentials.json")
			writeCredentialFixture(t, path, mode, map[string]string{"rt_a": "credential-a"})
			store := NewFileCredentials(path)

			_, _, err := store.BrokerCredential(context.Background(), "rt_a")
			if err == nil {
				t.Fatalf("mode %04o was accepted", mode)
			}
			if !strings.Contains(err.Error(), "0600") {
				t.Errorf("error does not say what to fix: %v", err)
			}
		})
	}
}

// A malformed file must not echo its own contents: the parse error is a log
// line, and the contents are credentials.
func TestFileCredentialsDoesNotEchoContentsOnAParseError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker-credentials.json")
	if err := os.WriteFile(path, []byte(`{"rt_a": "super-secret-credential-value"`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	store := NewFileCredentials(path)

	_, _, err := store.BrokerCredential(context.Background(), "rt_a")
	if err == nil {
		t.Fatal("a malformed file should be an error")
	}
	if strings.Contains(err.Error(), "super-secret-credential-value") {
		t.Errorf("the error leaked the file's contents: %v", err)
	}
}

// A tenant provisioned after the broker started must be picked up without a
// restart.
func TestFileCredentialsReloadsWhenTheFileChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker-credentials.json")
	writeCredentialFixture(t, path, 0o600, map[string]string{"rt_a": "credential-a"})
	store := NewFileCredentials(path)

	if _, ok, _ := store.BrokerCredential(context.Background(), "rt_b"); ok {
		t.Fatal("rt_b should not exist yet")
	}

	writeCredentialFixture(t, path, 0o600, map[string]string{"rt_a": "credential-a", "rt_b": "credential-b"})

	value, ok, err := store.BrokerCredential(context.Background(), "rt_b")
	if err != nil {
		t.Fatalf("lookup after reload: %v", err)
	}
	if !ok || value != "credential-b" {
		t.Errorf("rt_b = %q, %v after the file changed; want credential-b", value, ok)
	}
}

func TestFileCredentialsBlankValueIsAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker-credentials.json")
	writeCredentialFixture(t, path, 0o600, map[string]string{"rt_a": "   "})
	store := NewFileCredentials(path)

	value, ok, err := store.BrokerCredential(context.Background(), "rt_a")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if ok || value != "" {
		t.Errorf("a blank credential must read as absent, got %q, %v", value, ok)
	}
}

// Starting before any tenant exists is normal, not a failure.
func TestFileCredentialsMissingFileIsEmpty(t *testing.T) {
	store := NewFileCredentials(filepath.Join(t.TempDir(), "absent.json"))

	value, ok, err := store.BrokerCredential(context.Background(), "rt_a")
	if err != nil {
		t.Fatalf("a missing file should not be an error: %v", err)
	}
	if ok || value != "" {
		t.Errorf("got %q, %v from a missing file", value, ok)
	}
}

func TestParseTenants(t *testing.T) {
	registry := `# name subnet gateway
pool1 100.64.10.0/24 100.64.10.1

pool2 100.64.20.0/24 100.64.20.1
`
	tenants, err := ParseTenants(strings.NewReader(registry))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tenants) != 2 {
		t.Fatalf("tenants = %+v, want two", tenants)
	}
	if tenants[1].Name != "pool2" || tenants[1].Gateway != "100.64.20.1" {
		t.Errorf("second tenant = %+v", tenants[1])
	}
}

func TestParseTenantsRefusesMalformedLines(t *testing.T) {
	for name, registry := range map[string]string{
		"two fields":      "pool1 100.64.10.0/24\n",
		"four fields":     "pool1 100.64.10.0/24 100.64.10.1 extra\n",
		"bad subnet":      "pool1 100.64.10.0 100.64.10.1\n",
		"bad gateway":     "pool1 100.64.10.0/24 not-an-ip\n",
		"missing gateway": "pool1 100.64.10.0/24\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTenants(strings.NewReader(registry)); err == nil {
				t.Errorf("ParseTenants accepted %q", registry)
			}
		})
	}
}

// A line number is the difference between a five-second fix and an afternoon,
// so it is asserted rather than left to chance.
func TestParseTenantsNamesTheBadLine(t *testing.T) {
	registry := "pool1 100.64.10.0/24 100.64.10.1\n# comment\npool2 bad 100.64.20.1\n"

	_, err := ParseTenants(strings.NewReader(registry))
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error does not name the line: %v", err)
	}
}

// The broker binds the tenant gateways, so the handler must actually be
// reachable there and stop cleanly when the context ends.
func TestServeAcceptsAndShutsDownCleanly(t *testing.T) {
	broker := New(MapCredentials{"rt_a": runtimeKey}, MapStore{
		"otter/rt_a/job_1": {"TOKEN": "value"},
	}, Options{})

	listeners, err := Listen([]string{"127.0.0.1:0"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listeners[0].Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, listeners, broker, nil) }()

	// The listener is already bound, so the request does not race startup.
	token := mint(t, runtimeKey, secrets.WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_1"})
	body, err := json.Marshal(map[string]string{"runtime_id": "rt_a", "token": token})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://"+addr+PathExchange, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve returned %v on a clean shutdown; a closed listener is expected", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}

	// The port is released, so a restart is not blocked by a lingering listener.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the listener was not released: %v", err)
	}
	_ = listener.Close()
}

// Listen must not leak the listeners it already opened when a later address
// fails, or a restart would find every port still held.
func TestListenClosesWhatItOpenedOnFailure(t *testing.T) {
	first, err := Listen([]string{"127.0.0.1:0"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	held := first[0].Addr().String()

	// Ask for the held port (which fails) alongside a fresh one.
	_, err = Listen([]string{"127.0.0.1:0", held})
	if err == nil {
		t.Fatal("want an error for an address already in use")
	}

	listener, err := net.Listen("tcp", held)
	if err == nil {
		_ = listener.Close()
		t.Error("the first listener was leaked when a later bind failed")
	}
}
