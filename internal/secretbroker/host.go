package secretbroker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// FileCredentials reads per-runtime broker credentials from a JSON object
// mapping runtime id to credential.
//
// This is the host's copy of the credentials the provisioner also puts in each
// tenant's environment. It is the only thing that lets the broker tell one
// tenant's request from another's, so it is treated as the most sensitive file
// on the host after the pool token.
//
// Two properties are enforced rather than assumed:
//
//   - The file must not be readable by group or other. It holds a credential for
//     every tenant on the host, so a world-readable copy is every tenant's
//     secrets one `cat` away.
//   - No credential value ever appears in an error or a log line. Errors name
//     the path and the problem, never the contents.
//
// The file is re-read when it changes, so a tenant provisioned after the broker
// started does not need the broker restarted.
type FileCredentials struct {
	path string

	mu      sync.Mutex
	values  map[string]string
	modTime time.Time
	size    int64
	loaded  bool
}

// NewFileCredentials builds a store over path. Nothing is read until the first
// lookup, so a broker can start before the file exists.
func NewFileCredentials(path string) *FileCredentials {
	return &FileCredentials{path: path}
}

// BrokerCredential implements Credentials.
func (f *FileCredentials) BrokerCredential(_ context.Context, runtimeID string) (string, bool, error) {
	if err := f.refresh(); err != nil {
		return "", false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.values[runtimeID]
	if !ok || strings.TrimSpace(value) == "" {
		// A blank credential is a misconfiguration, not a usable one: it would
		// verify nothing rather than everything, and must not read as "present".
		return "", false, nil
	}
	return value, true, nil
}

// refresh reloads the file when it has changed.
func (f *FileCredentials) refresh() error {
	info, err := os.Stat(f.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No file yet is not an error: the host may have no tenants.
			return nil
		}
		return fmt.Errorf("secretbroker: stat credential file: %w", err)
	}

	f.mu.Lock()
	unchanged := f.loaded && info.ModTime().Equal(f.modTime) && info.Size() == f.size
	f.mu.Unlock()
	if unchanged {
		return nil
	}

	values, err := loadCredentialFile(f.path, info)
	if err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.values = values
	f.modTime = info.ModTime()
	f.size = info.Size()
	f.loaded = true
	return nil
}

func loadCredentialFile(path string, info os.FileInfo) (map[string]string, error) {
	// The permission check comes first, so a badly-permissioned file is refused
	// even when its contents happen to parse.
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return nil, fmt.Errorf(
			"secretbroker: %s is mode %04o; it holds a credential for every tenant on this host and must be 0600",
			path, mode)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("secretbroker: read credential file: %w", err)
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		// The error names the path and the offset, never the bytes.
		return nil, fmt.Errorf("secretbroker: %s is not a JSON object of runtime id to credential: %w", path, err)
	}
	if values == nil {
		values = map[string]string{}
	}
	return values, nil
}

// Tenant is one line of the pool's tenant registry.
type Tenant struct {
	Name    string
	Subnet  string
	Gateway string
}

// ParseTenants reads the registry the provisioner maintains: one
// `name subnet gateway` line per tenant, with blank lines and `#` comments
// ignored.
//
// The broker listens on the GATEWAY addresses rather than on a wildcard. Each
// tenant reaches its own bridge gateway, so binding there means the broker is
// reachable from the tenant networks and from nowhere else — a wildcard bind on
// an EC2 host would also listen on the public interface.
func ParseTenants(r io.Reader) ([]Tenant, error) {
	var out []Tenant
	scanner := bufio.NewScanner(r)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 3 {
			return nil, fmt.Errorf("tenant registry line %d: want `name subnet gateway`, got %d field(s)", line, len(fields))
		}
		tenant := Tenant{Name: fields[0], Subnet: fields[1], Gateway: fields[2]}
		if _, _, err := net.ParseCIDR(tenant.Subnet); err != nil {
			return nil, fmt.Errorf("tenant registry line %d: subnet %q is not a CIDR", line, tenant.Subnet)
		}
		if ip := net.ParseIP(tenant.Gateway); ip == nil {
			return nil, fmt.Errorf("tenant registry line %d: gateway %q is not an IP address", line, tenant.Gateway)
		}
		out = append(out, tenant)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("secretbroker: read tenant registry: %w", err)
	}
	return out, nil
}

// Listen binds one listener per address. Binding is separate from serving so a
// caller can fail before it is ready to accept traffic, and so a test can hand
// in its own listeners.
func Listen(addrs []string) ([]net.Listener, error) {
	listeners := make([]net.Listener, 0, len(addrs))
	for _, addr := range addrs {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return nil, fmt.Errorf("secretbroker: listen on %s: %w", addr, err)
		}
		listeners = append(listeners, listener)
	}
	return listeners, nil
}

// Serve runs handler on every listener until ctx is cancelled, then closes them
// and returns.
//
// A closed listener is the expected way this ends, so it is not reported as a
// fault: the alternative is a broker that logs an error every time it restarts.
func Serve(ctx context.Context, listeners []net.Listener, handler http.Handler, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// The exchange carries a runtime id and a token and returns a small
		// document; anything slow is a problem, not a workload.
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	done := make(chan error, len(listeners))
	for _, listener := range listeners {
		go func(l net.Listener) {
			err := server.Serve(l)
			if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				done <- err
				return
			}
			done <- nil
		}(listener)
	}

	select {
	case <-ctx.Done():
	case err := <-done:
		if err != nil {
			_ = server.Close()
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	// Shutdown waits for in-flight exchanges and closes every listener Serve was
	// given, so no goroutine is left holding one. The buffered channel means none
	// of them blocks on the way out.
	return nil
}
