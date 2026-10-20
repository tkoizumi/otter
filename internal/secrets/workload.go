package secrets

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrWorkloadToken is returned for every rejected workload-identity token. The
// specific reason is wrapped, but callers should treat this as one failure so a
// caller cannot probe which check rejected it.
var ErrWorkloadToken = errors.New("workload token rejected")

// workloadTokenVersion is inside the signed payload, so a token minted under an
// older format cannot be replayed against a newer verifier.
const workloadTokenVersion = 1

// WorkloadIdentity is the audience a secret request is bound to.
//
// D2 (`otter-platform/docs/decisions.md`) requires the binding to name the
// runtime *and* the job. A token that is merely short-lived is not enough: a
// compromised tenant could present it for a sibling's secrets on the same host,
// which is the failure this type exists to prevent.
type WorkloadIdentity struct {
	RuntimeID string
	JobID     string
}

func (w WorkloadIdentity) validate() error {
	if strings.TrimSpace(w.RuntimeID) == "" {
		return fmt.Errorf("%w: no runtime id", ErrWorkloadToken)
	}
	if strings.TrimSpace(w.JobID) == "" {
		return fmt.Errorf("%w: no job id", ErrWorkloadToken)
	}
	return nil
}

type workloadClaims struct {
	Version   int    `json:"v"`
	RuntimeID string `json:"rt"`
	JobID     string `json:"job"`
	ExpiresAt int64  `json:"exp"`
}

// MintWorkloadToken returns a short-lived token bound to id and signed with the
// runtime's broker credential.
//
// The credential is the per-runtime secret the provisioner already gives a
// tenant (see `internal/agent/secret.go`): it is scoped to one runtime and is
// stored only as a hash for that runtime's assignment, so a sibling on the same
// host cannot present it. Possession of the key is therefore what proves which
// runtime is asking, and the signed job id is what bounds the ask to one job.
func MintWorkloadToken(key string, id WorkloadIdentity, now time.Time, ttl time.Duration) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("%w: no signing key", ErrWorkloadToken)
	}
	if err := id.validate(); err != nil {
		return "", err
	}
	if ttl <= 0 {
		return "", fmt.Errorf("%w: ttl must be positive", ErrWorkloadToken)
	}
	claims := workloadClaims{
		Version:   workloadTokenVersion,
		RuntimeID: id.RuntimeID,
		JobID:     id.JobID,
		ExpiresAt: now.Add(ttl).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("workload token: marshal claims: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + signWorkload(key, encoded), nil
}

// VerifyWorkloadToken checks a token's signature and expiry and returns the
// identity it names.
//
// It deliberately says nothing about whether that job belongs to that runtime.
// The broker owns that check against the durable assignment; this function must
// not appear to replace it.
func VerifyWorkloadToken(key, token string, now time.Time) (WorkloadIdentity, error) {
	if strings.TrimSpace(key) == "" {
		return WorkloadIdentity{}, fmt.Errorf("%w: no signing key", ErrWorkloadToken)
	}
	encoded, mac, ok := strings.Cut(token, ".")
	if !ok || encoded == "" || mac == "" {
		return WorkloadIdentity{}, fmt.Errorf("%w: malformed", ErrWorkloadToken)
	}
	// Constant time: a byte-by-byte comparison would leak the MAC.
	if !hmac.Equal([]byte(mac), []byte(signWorkload(key, encoded))) {
		return WorkloadIdentity{}, fmt.Errorf("%w: bad signature", ErrWorkloadToken)
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return WorkloadIdentity{}, fmt.Errorf("%w: bad payload encoding", ErrWorkloadToken)
	}
	var claims workloadClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return WorkloadIdentity{}, fmt.Errorf("%w: bad payload", ErrWorkloadToken)
	}
	if claims.Version != workloadTokenVersion {
		return WorkloadIdentity{}, fmt.Errorf("%w: unsupported version %d", ErrWorkloadToken, claims.Version)
	}
	id := WorkloadIdentity{RuntimeID: claims.RuntimeID, JobID: claims.JobID}
	if err := id.validate(); err != nil {
		return WorkloadIdentity{}, err
	}
	if !now.Before(time.Unix(claims.ExpiresAt, 0)) {
		return WorkloadIdentity{}, fmt.Errorf("%w: expired", ErrWorkloadToken)
	}
	return id, nil
}

func signWorkload(key, payload string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
