// Package secretbroker is the host side of D2's secret exchange.
//
// A pooled tenant's otterd mints a short-lived workload identity bound to
// `(runtime_id, job_id)` with its host-local broker credential and presents it
// here; the broker verifies it, checks the audience, and returns that job's
// secret document. The AWS credential that reads the backend never leaves the
// host, and the control plane is not in the path at all (CL-13).
//
// Two properties this package owns, and nothing else does:
//
//   - The namespace is built in exactly one place, from validated identifiers,
//     so a crafted runtime id or job id cannot escape its segment and read a
//     sibling's document.
//   - A refusal is indistinguishable from "no such runtime". An unauthenticated
//     caller learns nothing about which runtimes exist.
package secretbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/tkoizumi/otter/internal/secrets"
)

// ErrNotFound is returned by a Store for a namespace that does not exist.
var ErrNotFound = errors.New("secret document not found")

// Credentials resolves the host-local broker credential for a runtime.
//
// It returns ok=false for an unknown runtime rather than an error: the broker
// treats "unknown" and "bad credential" identically on the wire.
type Credentials interface {
	BrokerCredential(ctx context.Context, runtimeID string) (credential string, ok bool, err error)
}

// Store reads one namespace's secret document.
type Store interface {
	Document(ctx context.Context, namespace string) (map[string]string, error)
}

// Jobs reports whether a job belongs to a runtime. It is optional; a nil Jobs
// leaves the namespace prefix as the only boundary.
//
// It exists because the namespace bounds a runtime to its own subtree but does
// not by itself stop one job of a tenant reading a sibling job's secrets of the
// same tenant. D2 asks for the binding to name the job, so a broker that can
// answer the question should.
type Jobs interface {
	HasJob(ctx context.Context, runtimeID, jobID string) (bool, error)
}

// Broker serves the exchange endpoint.
type Broker struct {
	credentials Credentials
	store       Store
	jobs        Jobs
	logf        func(format string, args ...any)
	now         func() time.Time

	// maxSkew tolerates a small clock difference between the tenant and the
	// host when checking expiry.
	maxSkew time.Duration
}

// Options carries the seams a test needs. The zero value is correct.
type Options struct {
	Jobs    Jobs
	Now     func() time.Time
	MaxSkew time.Duration
	// Logf records a backend lookup failure.
	//
	// The response to such a failure is deliberately generic -- the store's error
	// text can name S3 paths and account ids, and a tenant must not see them --
	// which makes this the only place the cause appears. Without it, a broker that
	// cannot reach Secrets Manager at all is indistinguishable from one that can
	// reach it and find nothing, and both look like a missing secret.
	Logf func(format string, args ...any)
}

// New builds a broker.
func New(credentials Credentials, store Store, opts Options) *Broker {
	b := &Broker{
		credentials: credentials,
		store:       store,
		jobs:        opts.Jobs,
		logf:        opts.Logf,
		now:         opts.Now,
		maxSkew:     opts.MaxSkew,
	}
	if b.now == nil {
		b.now = time.Now
	}
	return b
}

// maxBody bounds a request body. The request carries only a runtime id and a
// token, so anything larger is a malformed or hostile caller.
const maxBody = 8 << 10

// PathExchange is the one route the broker serves. It is spelled here and in
// the runtime's client; a change to either is a protocol change.
const PathExchange = "/v1/secrets/exchange"

// Namespace is the single place the backend path is built.
//
// Both identifiers are validated as single safe segments. This is a correctness
// boundary and not a style preference: the runtime id is what bounds a tenant
// to its own subtree, and a value containing a separator would escape it.
func Namespace(runtimeID, jobID string) (string, error) {
	if err := validSegment("runtime", runtimeID); err != nil {
		return "", err
	}
	if err := validSegment("job", jobID); err != nil {
		return "", err
	}
	return "otter/" + runtimeID + "/" + jobID, nil
}

// tenantSegment is the reserved job-id slot that holds a runtime's shared
// secrets. Reserved rather than merely conventional: validSegment refuses a
// leading underscore for every identity segment, so no job and no runtime can be
// named into this space and have the shared tier answer as though it were theirs.
const tenantSegment = "_tenant"

// TenantNamespace is where a tenant's shared secrets live: one copy for every job
// on the runtime, which is what makes a rotation one write instead of one per job.
//
// A job's own namespace wins on any name it declares, so sharing is a default and
// not a ceiling: a job that needs a different value still gets one.
func TenantNamespace(runtimeID string) (string, error) {
	if err := validSegment("runtime", runtimeID); err != nil {
		return "", err
	}
	return "otter/" + runtimeID + "/" + tenantSegment, nil
}

func validSegment(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s id is empty", name)
	}
	if len(value) > 128 {
		return fmt.Errorf("%s id is longer than 128 characters", name)
	}
	if value == "." || value == ".." {
		return fmt.Errorf("%s id must not be a path traversal", name)
	}
	// Underscore-prefixed segments belong to the broker, not to a caller. Without
	// this, a job literally named "_tenant" would be served the tenant's shared
	// document as its own.
	if value[0] == '_' {
		return fmt.Errorf("%s id must not begin with an underscore: that space is reserved", name)
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("%s id contains an unusable character %q", name, r)
		}
	}
	return nil
}

type exchangeRequest struct {
	RuntimeID string `json:"runtime_id"`
	Token     string `json:"token"`
}

type exchangeResponse struct {
	Secrets map[string]string `json:"secrets"`
}

// ServeHTTP implements the exchange endpoint.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != PathExchange {
		writeError(w, http.StatusNotFound, "unknown route")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "the exchange is a POST")
		return
	}

	var req exchangeRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if req.RuntimeID == "" || req.Token == "" {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	ctx := r.Context()
	credential, ok, err := b.credentials.BrokerCredential(ctx, req.RuntimeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "credential lookup failed")
		return
	}
	if !ok || credential == "" {
		// Indistinguishable from a bad token on purpose.
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	identity, err := secrets.VerifyWorkloadToken(credential, req.Token, b.now().Add(-b.maxSkew))
	if err != nil {
		// The reason is not echoed: it would tell a caller which check failed.
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	// The claim selects the key; the token proves possession of it. Neither
	// field is trusted on its own.
	if identity.RuntimeID != req.RuntimeID {
		writeError(w, http.StatusForbidden, "audience mismatch")
		return
	}

	namespace, err := Namespace(identity.RuntimeID, identity.JobID)
	if err != nil {
		writeError(w, http.StatusForbidden, "unusable identity")
		return
	}

	if b.jobs != nil {
		member, err := b.jobs.HasJob(ctx, identity.RuntimeID, identity.JobID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "job lookup failed")
			return
		}
		if !member {
			writeError(w, http.StatusForbidden, "that job does not belong to this runtime")
			return
		}
	}

	shared, err := TenantNamespace(identity.RuntimeID)
	if err != nil {
		writeError(w, http.StatusForbidden, "unusable identity")
		return
	}

	// Two lookups, one answer: the tenant's shared document first, the job's own
	// names over it. The tenant segment is derived from the token's runtime id and
	// never from the request, so this does not widen D2's audience -- a job can
	// read its own tenant's shared secrets and no others.
	//
	// `ErrNotFound` from either is "nothing there", not a failure: a job with no
	// shared secrets and no names of its own is still the 404 below, which is the
	// message that says so.
	document := map[string]string{}
	for _, name := range []string{shared, namespace} {
		found, lookupErr := b.store.Document(ctx, name)
		if lookupErr != nil {
			if errors.Is(lookupErr, ErrNotFound) {
				continue
			}
			// The backend's error text can name paths and account ids, so it is not
			// returned -- but it is the whole of the diagnosis, so it must be recorded.
			if b.logf != nil {
				b.logf("pool broker: reading %s: %v", name, lookupErr)
			}
			writeError(w, http.StatusInternalServerError, "secret lookup failed")
			return
		}
		for key, value := range found {
			document[key] = value
		}
	}
	if len(document) == 0 {
		writeError(w, http.StatusNotFound, "no secrets are configured for this job")
		return
	}
	writeJSON(w, http.StatusOK, exchangeResponse{Secrets: document})
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// MapCredentials is an in-memory Credentials, for a host that keeps the
// per-runtime credentials in memory and for tests.
type MapCredentials map[string]string

// BrokerCredential implements Credentials.
func (m MapCredentials) BrokerCredential(_ context.Context, runtimeID string) (string, bool, error) {
	value, ok := m[runtimeID]
	return value, ok, nil
}

// Keys returns the sorted runtime ids this broker knows, which is what an
// operator surface can show without disclosing any credential.
func (m MapCredentials) Keys() []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// MapStore is an in-memory Store, for a host that holds documents itself and
// for tests.
type MapStore map[string]map[string]string

// Document implements Store.
func (m MapStore) Document(_ context.Context, namespace string) (map[string]string, error) {
	document, ok := m[namespace]
	if !ok {
		return nil, ErrNotFound
	}
	return document, nil
}

// StoreFunc adapts a function to Store.
type StoreFunc func(ctx context.Context, namespace string) (map[string]string, error)

// Document implements Store.
func (f StoreFunc) Document(ctx context.Context, namespace string) (map[string]string, error) {
	return f(ctx, namespace)
}
