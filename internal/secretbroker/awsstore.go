package secretbroker

import (
	"context"
	"strings"

	"github.com/tkoizumi/otter/internal/awssecrets"
)

// AWSStore adapts the Secrets Manager client to the broker's Store seam: one
// job's whole document, keyed by secret name.
//
// It lives here rather than beside the AWS client because it is the piece that
// must satisfy this package's contract, including its `ErrNotFound`. An adapter
// elsewhere would compile, return its own not-found error, and surface "this job
// has no secrets" to the operator as a 500 — which is exactly what happened
// before this moved.
//
// The backend holds one secret per (runtime, job, name) — see D4's reasoning:
// per-name resources are what let the CONTROL PLANE hold a write-only
// credential. The read side pays for that with a list and a batch-get, which
// this type absorbs so the broker does not have to know how the values are
// stored.
type AWSStore struct {
	Client *awssecrets.Client
}

// Document returns every secret under namespace, keyed by its own name.
//
// A namespace with no secrets is ErrNotFound rather than an empty document, so
// the broker answers 404 and a run fails with "no secrets are configured for
// this job" instead of starting with an empty environment.
func (s *AWSStore) Document(ctx context.Context, namespace string) (map[string]string, error) {
	if namespace == "" {
		return nil, ErrNotFound
	}
	// The trailing separator is what stops job "job_1" from matching the secrets
	// of "job_10".
	prefix := namespace + "/"

	names, err := s.Client.ListSecretNames(ctx, prefix)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, ErrNotFound
	}

	values, err := s.Client.BatchGetSecretValues(ctx, names)
	if err != nil {
		return nil, err
	}

	out := make(map[string]string, len(values))
	for full, value := range values {
		// Belt and braces: the list was already filtered by prefix, but a backend
		// that returns a name outside it must not have that value attributed to
		// this job.
		name := strings.TrimPrefix(full, prefix)
		if name == full || name == "" {
			continue
		}
		// Anything that is not a plain identifier is dropped rather than
		// returned. The write-through path only ever creates environment-variable
		// names, so this is a backstop -- but a name carrying a separator would
		// otherwise reach the broker's document, and the two ends should agree
		// about what a secret name can be.
		if !validSecretName(name) {
			continue
		}
		out[name] = value
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// validSecretName mirrors the rule the manifest and the control plane's
// write-through both apply: an environment variable name.
func validSecretName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
