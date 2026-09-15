// Package notify reports failed runs to an operator-controlled endpoint.
//
// It exists so a failure stops being something you discover by looking. The
// daemon is the only component that sees every run reach a terminal state --
// cron, manual, webhook, retry, timeout, shutdown -- so it is the only place
// that can report uniformly. Integrations keep the job of deciding *whether*
// they failed; the runtime keeps the job of telling someone.
//
// Delivery is deliberately best-effort. A bounded in-process retry covers a
// transient endpoint problem, and nothing covers the daemon dying before the
// request is sent. That is stated here rather than implied, because an alerting
// path that quietly drops alerts is worse than none: it is trusted.
package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/otter-runtime/otter/internal/config"
	"github.com/otter-runtime/otter/internal/logging"
)

// Defaults for delivery.
const (
	defaultTimeout    = 5 * time.Second
	defaultAttempts   = 3
	defaultRetryDelay = 2 * time.Second
)

// Payload is what a failure reports. It carries enough to act on without
// opening the host.
type Payload struct {
	Integration string `json:"integration"`
	RunID       string `json:"run_id"`
	Status      string `json:"status"`
	Attempt     int    `json:"attempt"`
	Error       string `json:"error,omitempty"`
	// Detail is the integration's own last log line, which is usually the
	// most informative field: it is the difference between "process exited
	// with code 1" and the actual reason.
	Detail     string `json:"detail,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	// Release identifies the source snapshot that ran, so a failure can be
	// tied to a specific build.
	Release string `json:"release,omitempty"`
	Host    string `json:"host,omitempty"`
}

// Notifier delivers payloads according to a configuration.
type Notifier struct {
	cfg    config.NotifyConfig
	client *http.Client
	log    *logging.Logger

	// attemptDelay lets a test compress the retry backoff. It is the delay
	// between attempts and takes no other meaning.
	attemptDelay time.Duration
	attempts     int
}

// Option adjusts a Notifier at construction.
type Option func(*Notifier)

// WithRetryPolicy sets the number of delivery attempts and the delay between
// them. Tests use it to compress the backoff.
func WithRetryPolicy(attempts int, delay time.Duration) Option {
	return func(n *Notifier) {
		if attempts > 0 {
			n.attempts = attempts
		}
		n.attemptDelay = delay
	}
}

// New builds a Notifier. A nil logger is tolerated so the notifier can be used
// from tests and one-off tools.
func New(cfg config.NotifyConfig, log *logging.Logger, opts ...Option) *Notifier {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	n := &Notifier{
		cfg:          cfg,
		client:       &http.Client{Timeout: timeout},
		log:          log,
		attempts:     defaultAttempts,
		attemptDelay: defaultRetryDelay,
	}
	for _, opt := range opts {
		opt(n)
	}
	return n
}

// Enabled reports whether any notification will be attempted.
func (n *Notifier) Enabled() bool { return n != nil && n.cfg.Enabled() }

// Wants reports whether a terminal status should be reported.
func (n *Notifier) Wants(status string) bool {
	return n.Enabled() && n.cfg.Matches(status)
}

// Send delivers one payload.
//
// It returns an error only for diagnostics: callers must never let a
// notification change a run's outcome, so the result is logged and dropped.
func (n *Notifier) Send(ctx context.Context, payload Payload) error {
	if !n.Enabled() {
		return nil
	}

	body, err := n.body(payload)
	if err != nil {
		return fmt.Errorf("notify: encode payload: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= n.attempts; attempt++ {
		if err := n.post(ctx, body); err != nil {
			lastErr = err
			if attempt < n.attempts {
				select {
				case <-ctx.Done():
					// The daemon is shutting down; stop trying rather than
					// holding the worker open.
					return ctx.Err()
				case <-time.After(n.attemptDelay):
				}
			}
			continue
		}
		if n.log != nil {
			n.log.Debug("notification_sent",
				"integration", payload.Integration,
				"run_id", payload.RunID,
				"status", payload.Status,
				"attempt", attempt)
		}
		return nil
	}

	return fmt.Errorf("notify: delivery failed after %d attempt(s): %w", n.attempts, lastErr)
}

func (n *Notifier) post(ctx context.Context, body []byte) error {
	// The context bounds this attempt; the client timeout is a backstop for a
	// server that accepts the connection and then stalls.
	reqCtx, cancel := context.WithTimeout(ctx, n.client.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, n.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Drain so the connection can be reused, and to avoid leaving the server
	// blocked writing a response nobody reads.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("endpoint returned %s", resp.Status)
	}
	return nil
}

// RedactedURL is the configured endpoint with any embedded secret removed.
func (n *Notifier) RedactedURL() string {
	if n == nil {
		return ""
	}
	return n.cfg.RedactedURL()
}
