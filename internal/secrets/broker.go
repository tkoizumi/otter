package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// defaultWorkloadTTL bounds how long a minted identity is usable. Short enough
// that a leaked token is not a standing credential, long enough that resolving
// one run's secrets does not need a second exchange.
const defaultWorkloadTTL = 2 * time.Minute

// BrokerProvider resolves a job's secrets from the host broker.
//
// It mints a short-lived token bound to `(runtime_id, job_id)`, exchanges it
// once for that job's secret document, and serves the keys from that document.
// Fetching per run is what makes D2's rotation property real: a rotated value is
// picked up by the next run with no release and no daemon restart. That only
// holds if the caller builds one provider per run — a provider cached across
// runs would pin the old values.
type BrokerProvider struct {
	brokerURL string
	key       string
	identity  WorkloadIdentity

	client *http.Client
	ttl    time.Duration
	now    func() time.Time

	mu     sync.Mutex
	values map[string]string
	loaded bool
}

// BrokerOptions carries the seams a test needs. The zero value is correct.
type BrokerOptions struct {
	Client *http.Client
	TTL    time.Duration
	Now    func() time.Time
}

// NewBrokerProvider builds a provider for exactly one run's identity.
func NewBrokerProvider(brokerURL, key string, identity WorkloadIdentity, opts BrokerOptions) *BrokerProvider {
	p := &BrokerProvider{
		brokerURL: strings.TrimRight(strings.TrimSpace(brokerURL), "/"),
		key:       key,
		identity:  identity,
		client:    opts.Client,
		ttl:       opts.TTL,
		now:       opts.Now,
	}
	if p.client == nil {
		p.client = &http.Client{Timeout: 10 * time.Second}
	}
	if p.ttl <= 0 {
		p.ttl = defaultWorkloadTTL
	}
	if p.now == nil {
		p.now = time.Now
	}
	return p
}

// exchangeRequest is what the runtime presents. The runtime id travels in the
// clear so the broker can find the key to verify against; the token proves
// possession of it. The broker must check that the token's runtime matches this
// field rather than trusting either on its own.
type exchangeRequest struct {
	RuntimeID string `json:"runtime_id"`
	Token     string `json:"token"`
}

type exchangeResponse struct {
	Secrets map[string]string `json:"secrets"`
}

// Get implements Provider. The whole document is fetched once and reused, so a
// job declaring six secrets makes one exchange, not six.
func (p *BrokerProvider) Get(ctx context.Context, key string) (string, error) {
	if err := p.ensureLoaded(ctx); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	value, ok := p.values[key]
	if !ok || strings.TrimSpace(value) == "" {
		// A blank value is a misconfiguration, not a usable secret — the same
		// rule EnvProvider applies.
		return "", fmt.Errorf("%s is not present: %w", key, ErrNotFound)
	}
	return value, nil
}

func (p *BrokerProvider) ensureLoaded(ctx context.Context) error {
	p.mu.Lock()
	if p.loaded {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	values, err := p.exchange(ctx)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.values = values
	p.loaded = true
	return nil
}

func (p *BrokerProvider) exchange(ctx context.Context) (map[string]string, error) {
	if p.brokerURL == "" {
		return nil, errors.New("secrets: no broker URL configured")
	}
	token, err := MintWorkloadToken(p.key, p.identity, p.now(), p.ttl)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(exchangeRequest{RuntimeID: p.identity.RuntimeID, Token: token})
	if err != nil {
		return nil, fmt.Errorf("secrets: marshal exchange: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.brokerURL+"/v1/secrets/exchange", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("secrets: build exchange request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := p.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("secrets: exchange with broker: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		// Deliberately NOT ErrNotFound: a refused exchange is an authorization
		// or configuration failure and must fail the run loudly, rather than
		// being reported as a job that forgot to declare a variable.
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return nil, fmt.Errorf("secrets: broker refused the exchange (%d): %s",
			response.StatusCode, strings.TrimSpace(string(detail)))
	}

	var payload exchangeResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("secrets: decode exchange response: %w", err)
	}
	if payload.Secrets == nil {
		payload.Secrets = map[string]string{}
	}
	return payload.Secrets, nil
}
