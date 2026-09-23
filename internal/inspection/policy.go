// Package inspection implements Otter's bounded HTTP request inspection: the
// capture policy, the wire contract the Python SDK submits, redaction, and the
// SQLite records that `otter requests` reads back.
//
// Capture is diagnostic. Every rule here exists to keep a recording bounded,
// attributable and safe to store: a failure to record must never change what an
// integration does, and a stored value must never contain a credential.
package inspection

import (
	"fmt"
	"time"
)

// Policy is how much detail a run records about its outgoing HTTP traffic.
type Policy string

const (
	// PolicyOff records nothing and installs no instrumentation.
	PolicyOff Policy = "off"
	// PolicyMetadata records request summaries and never a payload.
	PolicyMetadata Policy = "metadata"
	// PolicyFull additionally records permitted headers and bounded JSON bodies.
	PolicyFull Policy = "full"
)

// DefaultPolicy is what an ordinary run records. Payload capture is opt-in
// because bodies routinely carry credentials.
const DefaultPolicy = PolicyMetadata

// AllPolicies lists every valid policy, for validation and help text.
func AllPolicies() []Policy { return []Policy{PolicyOff, PolicyMetadata, PolicyFull} }

// ParsePolicy resolves a caller-supplied capture level. An empty value means the
// default, which is how `otter run` treats a missing --capture flag.
func ParsePolicy(s string) (Policy, error) {
	if s == "" {
		return DefaultPolicy, nil
	}
	p := Policy(s)
	if !p.Valid() {
		return "", fmt.Errorf("invalid capture policy %q: use one of off, metadata, full", s)
	}
	return p, nil
}

// Valid reports whether p is a known policy.
func (p Policy) Valid() bool {
	switch p {
	case PolicyOff, PolicyMetadata, PolicyFull:
		return true
	default:
		return false
	}
}

// Enabled reports whether the policy records anything at all. An unknown policy
// is not enabled, so an unresolvable value can never switch instrumentation on.
func (p Policy) Enabled() bool { return p.Valid() && p != PolicyOff }

// CapturesBodies reports whether payload capture is permitted.
func (p Policy) CapturesBodies() bool { return p == PolicyFull }

// String makes a Policy usable anywhere a string is expected.
func (p Policy) String() string { return string(p) }

// Contract versions.
const (
	// SchemaVersion identifies the persisted record shape and the wire payload
	// shape. A submission that does not match is rejected rather than guessed at.
	SchemaVersion = 1
	// PolicyVersion identifies the redaction and omission rules. It is recorded
	// per run because those rules are expected to tighten over time, and a
	// reviewer needs to know which rules produced an old recording.
	PolicyVersion = 1
)

// Adapter and coverage names.
const (
	// AdapterURLLib is the only transport adapter this milestone implements.
	AdapterURLLib = "urllib"
	// Coverage names what the installed adapters can actually observe. It is
	// deliberately narrower than "all HTTP": custom overrides that bypass the
	// base opener, subprocesses, requests, httpx and raw sockets are not covered.
	Coverage = "urllib"
)

// Limits bounds capture so a diagnostic can never grow without bound. The same
// values are enforced in the SDK and again on ingestion: the SDK keeps a run
// from producing unbounded work, and the server keeps a buggy or hostile client
// from persisting it.
type Limits struct {
	// MaxBodyBytes is the largest request or response body buffered per message.
	MaxBodyBytes int64
	// MaxRunBytes is the largest total persisted capture payload per run.
	MaxRunBytes int64
	// MaxRunRequests is the largest number of request records per run.
	MaxRunRequests int
	// MaxHeaderPairs and MaxHeaderBytes bound a single header block.
	MaxHeaderPairs int
	MaxHeaderBytes int
	// MaxURLLength bounds a sanitized URL.
	MaxURLLength int
	// MaxCallSiteLength bounds the recorded file/function/line string.
	MaxCallSiteLength int
	// MaxBatchEvents bounds one ingestion request.
	MaxBatchEvents int
	// MaxErrorTextBytes bounds sanitized transport-error text.
	MaxErrorTextBytes int
	// MaxRequestIDLength bounds an SDK-generated request id.
	MaxRequestIDLength int
	// FlushDeadline bounds how long the SDK will wait to deliver capture on
	// shutdown. It is a hard ceiling: dropping capture is always preferable to
	// delaying or altering an integration's exit.
	FlushDeadline time.Duration
}

// DefaultLimits returns the centrally defined limits. Every component reads them
// from here so the SDK, ingestion and retention cannot drift apart.
func DefaultLimits() Limits {
	return Limits{
		MaxBodyBytes:       256 * 1024,
		MaxRunBytes:        10 * 1024 * 1024,
		MaxRunRequests:     1000,
		MaxHeaderPairs:     100,
		MaxHeaderBytes:     16 * 1024,
		MaxURLLength:       4096,
		MaxCallSiteLength:  512,
		MaxBatchEvents:     100,
		MaxErrorTextBytes:  1024,
		MaxRequestIDLength: 128,
		FlushDeadline:      2 * time.Second,
	}
}
