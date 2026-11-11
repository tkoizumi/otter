package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/secrets"
)

// TestRunResolvesSecretsFromTheBrokerPerRun is the runtime half of D2: a run
// resolves its declared secrets by exchanging an audience-bound workload
// identity with the host broker, rather than reading the daemon's environment.
//
// The fake broker performs the checks the real one must perform -- verify the
// token, require the token's runtime to match the runtime claimed -- so this
// exercises the contract and not only the client.
func TestRunResolvesSecretsFromTheBrokerPerRun(t *testing.T) {
	const brokerKey = "host-local-broker-credential"

	var (
		mu   sync.Mutex
		seen []secrets.WorkloadIdentity
	)
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/secrets/exchange" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			RuntimeID string `json:"runtime_id"`
			Token     string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		id, err := secrets.VerifyWorkloadToken(brokerKey, req.Token, time.Now())
		if err != nil {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		if id.RuntimeID != req.RuntimeID {
			http.Error(w, "audience mismatch", http.StatusForbidden)
			return
		}
		mu.Lock()
		seen = append(seen, id)
		mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]any{
			"secrets": map[string]string{"BROKERED_TOKEN": "from-the-broker"},
		})
	}))
	defer broker.Close()

	root := t.TempDir()
	writeJob(t, root, "brokered", `
version: 1
name: brokered
entrypoint: main.py
timeout: 30
secrets:
  - BROKERED_TOKEN
`, `
import os
# Compare rather than print: the value must reach the process, and the captured
# log must not carry it. Asserting the literal text would now pass for the wrong
# reason -- it is replaced before it is stored.
print("TOKEN_OK" if os.environ["BROKERED_TOKEN"] == "from-the-broker" else "TOKEN_WRONG")
`)

	// The static provider is empty, so the only way this run can succeed is by
	// resolving through the broker.
	d := newDaemon(t, root, "", secrets.NewStaticProvider(map[string]string{}), func(cfg *config.DaemonConfig) {
		cfg.SecretBroker = config.SecretBrokerConfig{
			URL:        broker.URL,
			RuntimeID:  "rt_1",
			Credential: brokerKey,
		}
	})
	startDaemon(t, d)

	runID, err := d.SubmitRun(context.Background(), "brokered", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	view := awaitTerminal(t, d, runID)
	if view.Run.Status != runs.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", view.Run.Status, view.Run.ErrorString())
	}

	stdout := strings.Join(logMessages(t, d, runID, runs.StreamStdout), "\n")
	// The job printed the verdict, not the value, so this proves injection without
	// depending on the value surviving into a log -- which it must not.
	if !strings.Contains(stdout, "TOKEN_OK") {
		t.Errorf("stdout = %q, want the process to have observed the brokered value", stdout)
	}
	// And the captured log must not contain it: the redactor replaces declared
	// secret values before they are stored.
	if strings.Contains(stdout, "from-the-broker") {
		t.Errorf("the brokered value reached the log: %q", stdout)
	}

	// The identity the broker verified names this runtime and this run's job --
	// not merely "some run".
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("broker exchanges = %d, want 1", len(seen))
	}
	if seen[0].RuntimeID != "rt_1" {
		t.Errorf("broker saw runtime %q, want rt_1", seen[0].RuntimeID)
	}
	if seen[0].JobID != view.Run.JobID {
		t.Errorf("broker saw job %q, want the run's job %q", seen[0].JobID, view.Run.JobID)
	}
}

// With no broker configured the daemon-wide provider is used unchanged, so an
// ordinary deployment resolves exactly as it did before.
func TestSecretsForRunFallsBackWithoutABroker(t *testing.T) {
	fallback := secrets.NewStaticProvider(map[string]string{"A": "1"})
	d := &Daemon{secrets: fallback}

	if got := d.secretsForRun(&runs.Run{JobID: "job_1"}); got != secrets.Provider(fallback) {
		t.Errorf("without a broker, secretsForRun = %T, want the daemon provider", got)
	}
}

// The provider is built per run, never shared. Sharing one would pin the values
// the first run saw and silently break D2's rotation property.
func TestSecretsForRunBuildsAFreshBrokerProviderPerRun(t *testing.T) {
	d := &Daemon{
		secrets: secrets.NewStaticProvider(nil),
		cfg: config.DaemonConfig{SecretBroker: config.SecretBrokerConfig{
			URL:        "http://127.0.0.1:9000",
			RuntimeID:  "rt_1",
			Credential: "key",
		}},
	}

	first := d.secretsForRun(&runs.Run{JobID: "job_1"})
	second := d.secretsForRun(&runs.Run{JobID: "job_1"})

	if _, ok := first.(*secrets.BrokerProvider); !ok {
		t.Fatalf("provider = %T, want *secrets.BrokerProvider", first)
	}
	if first == second {
		t.Error("secretsForRun returned the same provider twice; it must be built per run")
	}
}
