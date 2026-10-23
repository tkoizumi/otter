// Package replay resends one captured HTTP exchange.
//
// V1 is deliberately narrow (DEC-05): GET only, the original host only, HTTPS
// only, and no editing. The request is reconstructed from the runtime's own
// sanitized copy, secret placeholders are resolved from the live provider, and
// the result is recorded as a new replay document next to the exchange it
// resends. The original exchange is never modified.
package replay

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/secrets"
)

// DefaultMaxResponseBytes bounds the response body a replay stores. It is the
// same bound as capture: a replay is an inspector result, not an archive.
const DefaultMaxResponseBytes = 256 * 1024

// DefaultTimeout bounds one replay. The runtime is sending on behalf of an
// operator who is waiting; a hung destination must not hold the loopback call
// open indefinitely.
const DefaultTimeout = 30 * time.Second

// Refusal means the exchange cannot be replayed as captured. It is a state, not
// a failure: the operator is told which property of the capture prevents an
// exact resend, and nothing is sent.
type Refusal struct {
	Reason string
}

func (r *Refusal) Error() string { return r.Reason }

func refuse(format string, args ...any) *Refusal {
	return &Refusal{Reason: fmt.Sprintf(format, args...)}
}

// Check reports whether an exchange can be replayed exactly as captured.
//
// The order is the operator's order: the most fundamental reason first. Every
// clause is a property of the stored exchange, so this function is pure and can
// be exercised without a network.
func Check(exchange *inspection.Exchange) error {
	if exchange == nil {
		return inspection.ErrNotFound
	}
	if !exchange.Complete {
		return refuse("the exchange was not captured whole, so the original request cannot be reconstructed")
	}
	if exchange.QueryRedacted {
		return refuse("the request URL was redacted, so the original request cannot be reconstructed")
	}
	if exchange.Payloads == "" || exchange.Payloads == "metadata" {
		return refuse("bodies were not captured for this exchange, so there is nothing to resend")
	}

	method := strings.ToUpper(strings.TrimSpace(exchange.Method))
	if method != http.MethodGet {
		return refuse("Otter replay is GET-only in v1; this exchange used %s", method)
	}

	parsed, err := url.Parse(exchange.URL)
	if err != nil || parsed.Host == "" {
		return refuse("the captured URL cannot be parsed")
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return refuse("replay is HTTPS-only; the captured URL uses %q", parsed.Scheme)
	}
	if parsed.User != nil {
		return refuse("the captured URL embeds credentials, which replay will not resend")
	}
	if reason, reserved := reservedDestination(parsed.Hostname()); reserved {
		return refuse("%s", reason)
	}

	if name, ok := requestBoundSignature(exchange.RequestHeaders); ok {
		return refuse("the request is signed by %s, which is bound to the original request and cannot be replayed", name)
	}
	for _, header := range exchange.RequestHeaders {
		if header.Value == inspection.RedactedPlaceholder {
			return refuse("the %s header was redacted, so the original request cannot be reconstructed", header.Name)
		}
	}
	if body := exchange.RequestBody; body != nil {
		switch body.State {
		case inspection.BodyCaptured:
			if len(bytes.TrimSpace(body.JSON)) == 0 {
				return refuse("the captured request body is empty, but the exchange recorded a body")
			}
			if body.Redacted {
				return refuse("the request body was redacted, so the original request cannot be reconstructed")
			}
		case inspection.BodyOmitted:
			if body.BytesObserved > 0 {
				return refuse("the request body was not captured (%s), so the original request cannot be reconstructed", body.Reason)
			}
		}
	}
	return nil
}

// cgnatPrefix is 100.64.0.0/10, the carrier-grade NAT range RFC 6598 carves out
// of 100.64.0.0/8. netip does not call it private, and a pooled runtime that
// reaches a CGNAT address is reaching the provider's own network rather than the
// public internet, so replay refuses it the way it refuses RFC 1918.
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// reservedDestination reports why a captured URL's host must not be resent, and
// false when replay may target it.
//
// This is the runtime half of the §11 "private-range block"
// (cloud/docs/pooled-capture-replay-plan.md). A replay sends with the tenant's
// live credentials, so it must not be usable to poke a loopback listener, the
// link-local metadata service at 169.254.169.254, a neighbouring container on a
// private range, or an internal single-label name.
//
// What is covered: a host that is an IP literal — after IPv4-in-IPv6 addresses
// are unmapped, so ::ffff:169.254.169.254 is refused exactly like its IPv4 form —
// in loopback, link-local, private, carrier-grade NAT, unspecified or multicast
// space; the reserved names "localhost" and "*.localhost"; any single-label
// name, which is the classic internal-name shape; and a multi-label all-numeric
// host that is not a canonical IP literal (the inet_aton short forms 127.1 and
// 169.254.1, whose meaning depends on the resolver). Name comparison is
// case-insensitive and tolerates a trailing dot.
//
// What is NOT covered, deliberately: name resolution. A public name that resolves
// to a private address is allowed, because the destination is pinned to the
// captured URL (D7 — the operator never chooses a host) and the tenant's own job
// already reached that address. Resolving here would put a DNS lookup inside a
// pure guard and would still be defeated by a name that resolves differently at
// send time. Multi-label internal names (localhost.localdomain,
// metadata.google.internal) are likewise not caught by the name rule: they fail
// only if the environment resolves them to one of the literals above.
func reservedDestination(host string) (string, bool) {
	name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if name == "" {
		return "", false
	}
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return fmt.Sprintf("replay target %q is a reserved name, which replay will not send to", name), true
	}
	if addr, err := netip.ParseAddr(name); err == nil {
		addr = addr.Unmap()
		switch {
		case addr.IsLoopback():
			return fmt.Sprintf("replay target %s is a loopback address, which replay will not send to", addr), true
		case addr.IsLinkLocalUnicast():
			return fmt.Sprintf("replay target %s is a link-local address, which replay will not send to", addr), true
		case addr.IsMulticast(), addr.IsLinkLocalMulticast():
			return fmt.Sprintf("replay target %s is a multicast address, which replay will not send to", addr), true
		case addr.IsUnspecified():
			return fmt.Sprintf("replay target %s is an unspecified address, which replay will not send to", addr), true
		case addr.IsPrivate():
			return fmt.Sprintf("replay target %s is a private address, which replay will not send to", addr), true
		case cgnatPrefix.Contains(addr):
			return fmt.Sprintf("replay target %s is a carrier-grade NAT address, which replay will not send to", addr), true
		}
		return "", false
	}
	if !strings.Contains(name, ".") {
		return fmt.Sprintf("replay target %q is a single-label name, which replay will not send to", name), true
	}
	// A multi-label numeric host that is not a canonical IP literal is the
	// inet_aton short form (127.1, 169.254.1, 0x7f.0.0.1), whose meaning depends
	// on the resolver and includes loopback and link-local addresses. No real TLD
	// is all-numeric, so the last label decides it: refuse rather than guess.
	if last := name[strings.LastIndexByte(name, '.')+1:]; last != "" && strings.Trim(last, "0123456789") == "" {
		return fmt.Sprintf("replay target %q is a numeric host, not an IP literal, and replay will not send to it", name), true
	}
	return "", false
}

// Logger is the one log event a replay emits. It matches logging.Logger, and
// the interface keeps this package from depending on the daemon's logger.
type Logger interface {
	Error(event string, err error, kv ...any)
}

// Engine resends captured exchanges.
type Engine struct {
	// Store holds the capture record and receives the replay document.
	Store *inspection.Store
	// Secrets resolves the placeholders in the captured template.
	Secrets secrets.Provider
	// Client sends the request. The zero value uses a bounded default client
	// that does not follow redirects (a redirect is a different destination,
	// which v1 does not allow).
	Client *http.Client
	// Now and MaxResponseBytes are test seams; the zero values are correct.
	Now              func() time.Time
	MaxResponseBytes int64
	Logger           Logger
}

// Result is one recorded replay.
type Result struct {
	ReplayID        string
	RunID           string
	RequestID       string
	ResolvedSHA256  string
	StatusCode      *int
	ResponseBody    *inspection.BodyDescriptor
	DurationTotalMS int64
	TransportError  string
}

// Replay re-sends one exchange and records the result as a replay document.
//
// A transport failure is a recorded outcome, not an error: the replay happened
// (the request left the process), and the operator needs to see what came back
// or why nothing did. The error return is reserved for a refusal before anything
// was sent, and for a failure to record a replay that was sent.
func (e *Engine) Replay(ctx context.Context, exchange *inspection.Exchange, jobID string) (*Result, error) {
	if err := Check(exchange); err != nil {
		return nil, err
	}
	if e.Store == nil {
		return nil, fmt.Errorf("replay: no inspection store is configured")
	}
	if e.Secrets == nil {
		return nil, fmt.Errorf("replay: no secret provider is configured")
	}

	started := e.now()
	replayID, err := newReplayID()
	if err != nil {
		return nil, err
	}

	headers, err := e.resolveHeaders(ctx, exchange.RequestHeaders)
	if err != nil {
		return nil, err
	}
	body, err := e.resolveBody(ctx, exchange.RequestBody)
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, exchange.URL, bytes.NewReader(body))
	if err != nil {
		return nil, refuse("the captured request cannot be reconstructed: %v", err)
	}
	request.Header = headers
	if body != nil && request.Header.Get("Content-Type") == "" {
		if exchange.RequestBody != nil && exchange.RequestBody.ContentType != "" {
			request.Header.Set("Content-Type", exchange.RequestBody.ContentType)
		}
	}
	// The digest covers the request that is actually sent: method, URL, the
	// resolved headers and the resolved body. A body-only digest would be the
	// hash of the empty string for every GET, which would audit nothing.
	resolvedSHA := requestDigest(request.Method, exchange.URL, request.Header, body)

	result := &Result{
		ReplayID:       replayID,
		RunID:          exchange.RunID,
		RequestID:      exchange.RequestID,
		ResolvedSHA256: resolvedSHA,
	}

	response, sendErr := e.client().Do(request)
	if sendErr != nil {
		result.TransportError = sendErr.Error()
	} else {
		defer response.Body.Close()
		code := response.StatusCode
		result.StatusCode = &code
		result.ResponseBody = e.captureResponse(response)
	}
	result.DurationTotalMS = e.now().Sub(started).Milliseconds()

	record := &inspection.Replay{
		ReplayID:        replayID,
		RunID:           exchange.RunID,
		RequestID:       exchange.RequestID,
		JobID:           jobID,
		OccurredAt:      started,
		Complete:        true,
		Method:          http.MethodGet,
		URL:             exchange.URL,
		RequestHeaders:  exchange.RequestHeaders,
		RequestBody:     exchange.RequestBody,
		StatusCode:      result.StatusCode,
		ResponseBody:    result.ResponseBody,
		TransportError:  result.TransportError,
		DurationTotalMS: &result.DurationTotalMS,
		ResolvedSHA256:  resolvedSHA,
	}
	if err := e.Store.RecordReplay(ctx, record); err != nil {
		// The request was sent. Report the response to the operator, but do not
		// pretend the replay was recorded: the caller turns this into a warning,
		// and the sink has no replay document to link.
		if e.Logger != nil {
			e.Logger.Error("replay_not_recorded", err,
				"run_id", exchange.RunID, "request_id", exchange.RequestID, "replay_id", replayID)
		}
		return result, fmt.Errorf("replay: the request was sent but not recorded: %w", err)
	}
	return result, nil
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

func (e *Engine) client() *http.Client {
	if e.Client != nil {
		return e.Client
	}
	return &http.Client{
		Timeout: DefaultTimeout,
		// A redirect is a different request to a destination the operator did
		// not choose. V1 pins the original host, so the 3xx is the result.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (e *Engine) maxResponseBytes() int64 {
	if e.MaxResponseBytes > 0 {
		return e.MaxResponseBytes
	}
	return DefaultMaxResponseBytes
}

// resolveHeaders rebuilds the request's headers. Hop-by-hop headers are dropped
// because Go owns them; a placeholder is resolved from the live provider.
func (e *Engine) resolveHeaders(ctx context.Context, pairs []inspection.HeaderPair) (http.Header, error) {
	headers := make(http.Header, len(pairs))
	for _, pair := range pairs {
		name := strings.TrimSpace(pair.Name)
		if name == "" || hopByHop(name) {
			continue
		}
		value := pair.Value
		if secret, ok := secretPlaceholder(value); ok {
			resolved, err := e.Secrets.Get(ctx, secret)
			if err != nil {
				return nil, refuse("the %s header needs secret %s, which is not available for replay", name, secret)
			}
			value = resolved
		} else if strings.Contains(value, "{{secret:") {
			return nil, refuse("the %s header contains a secret reference that cannot be resolved", name)
		}
		headers.Add(name, value)
	}
	return headers, nil
}

// resolveBody returns the resolved body bytes and their digest. The placeholder
// form is a whole JSON string, so replacing it with a JSON-encoded value keeps
// the document valid even when the secret contains quotes or control characters.
func (e *Engine) resolveBody(ctx context.Context, body *inspection.BodyDescriptor) ([]byte, error) {
	if body == nil || body.State != inspection.BodyCaptured {
		return nil, nil
	}
	raw := body.JSON
	var firstErr error
	resolved := jsonPlaceholderPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		if firstErr != nil {
			return match
		}
		parts := jsonPlaceholderPattern.FindSubmatch(match)
		if parts == nil {
			return match
		}
		secret := string(parts[1])
		value, err := e.Secrets.Get(ctx, secret)
		if err != nil {
			firstErr = refuse("the request body needs secret %s, which is not available for replay", secret)
			return match
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			firstErr = fmt.Errorf("replay: encode secret %s: %w", secret, err)
			return match
		}
		return encoded
	})
	if firstErr != nil {
		return nil, firstErr
	}
	if bytes.Contains(resolved, []byte("{{secret:")) {
		return nil, refuse("the request body contains a secret reference that cannot be resolved")
	}
	return resolved, nil
}

func (e *Engine) captureResponse(response *http.Response) *inspection.BodyDescriptor {
	limit := e.maxResponseBytes()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return &inspection.BodyDescriptor{State: inspection.BodyOmitted, Reason: inspection.ReasonIncomplete}
	}
	contentType := strings.TrimSpace(response.Header.Get("Content-Type"))
	descriptor := &inspection.BodyDescriptor{
		State:         inspection.BodyCaptured,
		ContentType:   contentType,
		BytesObserved: int64(len(body)),
		JSON:          body,
	}
	if int64(len(body)) > limit {
		descriptor.State = inspection.BodyOmitted
		descriptor.Reason = inspection.ReasonOversized
		descriptor.JSON = nil
		return descriptor
	}
	if !looksLikeJSON(contentType, body) {
		descriptor.State = inspection.BodyOmitted
		descriptor.Reason = inspection.ReasonUnsupportedContent
		descriptor.JSON = nil
		return descriptor
	}
	out := e.Store.SanitizeBody(descriptor)
	return out
}

func looksLikeJSON(contentType string, body []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "json") {
		return true
	}
	trimmed := bytes.TrimSpace(body)
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

func hopByHop(name string) bool {
	switch strings.ToLower(name) {
	case "host", "content-length", "connection", "transfer-encoding",
		"proxy-connection", "keep-alive", "upgrade", "te", "trailer",
		// Let Go negotiate and decode the response; forwarding the captured
		// value would hand the engine compressed bytes it cannot parse.
		"accept-encoding":
		return true
	}
	return false
}

var (
	placeholderPattern     = regexp.MustCompile(`^\{\{secret:([A-Za-z_][A-Za-z0-9_]*)\}\}$`)
	jsonPlaceholderPattern = regexp.MustCompile(`"\{\{secret:([A-Za-z_][A-Za-z0-9_]*)\}\}"`)
)

func secretPlaceholder(value string) (string, bool) {
	match := placeholderPattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return "", false
	}
	return match[1], true
}

// requestBoundSignature names a header that makes the request unreplayable as
// captured: the signature covers the original timestamp, body or host, so a
// resend would be refused by the destination (or, worse, accepted for the wrong
// reason).
func requestBoundSignature(headers []inspection.HeaderPair) (string, bool) {
	for _, header := range headers {
		name := strings.ToLower(strings.TrimSpace(header.Name))
		value := strings.ToUpper(strings.TrimSpace(header.Value))
		switch name {
		case "authorization":
			if strings.HasPrefix(value, "AWS4-HMAC-SHA256") ||
				strings.HasPrefix(value, "HMAC ") ||
				strings.HasPrefix(value, "SIGNATURE ") {
				return header.Name, true
			}
		case "x-amz-date", "x-amz-content-sha256", "x-amz-credential",
			"x-amz-signature", "x-amz-security-token",
			"x-hub-signature", "x-hub-signature-256",
			"x-signature", "x-content-signature":
			return header.Name, true
		}
	}
	return "", false
}

// requestDigest is the audit digest of one resolved request. Header names are
// sorted so two runs of the same request produce the same digest; values are
// hashed, never stored.
func requestDigest(method, rawURL string, headers http.Header, body []byte) string {
	var buf bytes.Buffer
	buf.WriteString(strings.ToUpper(method))
	buf.WriteByte('\n')
	buf.WriteString(rawURL)
	buf.WriteByte('\n')
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, value := range headers[name] {
			buf.WriteString(strings.ToLower(name))
			buf.WriteString(": ")
			buf.WriteString(value)
			buf.WriteByte('\n')
		}
	}
	buf.WriteByte('\n')
	buf.Write(body)
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

func newReplayID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("replay: generate replay id: %w", err)
	}
	return "replay_" + hex.EncodeToString(random), nil
}
