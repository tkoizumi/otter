package secrets

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWorkloadTokenRoundTrips(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	id := WorkloadIdentity{RuntimeID: "rt_1", JobID: "job_1"}

	token, err := MintWorkloadToken("runtime-one-secret", id, now, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	got, err := VerifyWorkloadToken("runtime-one-secret", token, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got != id {
		t.Errorf("identity = %+v, want %+v", got, id)
	}
}

// The property D2 names: a compromised tenant must not be able to present
// anything that resolves a sibling's secrets on the same host.
func TestWorkloadTokenIsRejectedForASiblingRuntime(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	id := WorkloadIdentity{RuntimeID: "rt_a", JobID: "job_shared"}

	token, err := MintWorkloadToken("runtime-a-secret", id, now, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if _, err := VerifyWorkloadToken("runtime-b-secret", token, now); !errors.Is(err, ErrWorkloadToken) {
		t.Errorf("sibling runtime verify error = %v, want ErrWorkloadToken", err)
	}
}

// The job id is signed, so a token minted for one job cannot be widened to
// another by editing the payload.
func TestWorkloadTokenRejectsAWidenedJobID(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	token, err := MintWorkloadToken("key", WorkloadIdentity{RuntimeID: "rt_1", JobID: "job_1"}, now, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	encoded, mac, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("token %q has no separator", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims workloadClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	claims.JobID = "job_2" // the sibling's job
	widened, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	forged := base64.RawURLEncoding.EncodeToString(widened) + "." + mac

	if _, err := VerifyWorkloadToken("key", forged, now); !errors.Is(err, ErrWorkloadToken) {
		t.Errorf("widened token verify error = %v, want ErrWorkloadToken", err)
	}
}

func TestWorkloadTokenExpiryIsExclusive(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	token, err := MintWorkloadToken("key", WorkloadIdentity{RuntimeID: "rt", JobID: "job"}, now, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// One second before expiry the token still works.
	if _, err := VerifyWorkloadToken("key", token, now.Add(59*time.Second)); err != nil {
		t.Errorf("verify one second before expiry: %v", err)
	}
	// At expiry it does not: the boundary is exclusive, not inclusive.
	if _, err := VerifyWorkloadToken("key", token, now.Add(time.Minute)); !errors.Is(err, ErrWorkloadToken) {
		t.Errorf("verify at expiry error = %v, want ErrWorkloadToken", err)
	}
	if _, err := VerifyWorkloadToken("key", token, now.Add(2*time.Minute)); !errors.Is(err, ErrWorkloadToken) {
		t.Errorf("verify after expiry error = %v, want ErrWorkloadToken", err)
	}
}

func TestWorkloadTokenMintRejectsAnIncompleteAudience(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := map[string]WorkloadIdentity{
		"no runtime": {JobID: "job"},
		"no job":     {RuntimeID: "rt"},
		"neither":    {},
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := MintWorkloadToken("key", id, now, time.Minute); !errors.Is(err, ErrWorkloadToken) {
				t.Errorf("mint error = %v, want ErrWorkloadToken", err)
			}
		})
	}
}

func TestWorkloadTokenMintRejectsNoKeyAndNoTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	id := WorkloadIdentity{RuntimeID: "rt", JobID: "job"}

	if _, err := MintWorkloadToken("  ", id, now, time.Minute); !errors.Is(err, ErrWorkloadToken) {
		t.Errorf("empty key mint error = %v, want ErrWorkloadToken", err)
	}
	if _, err := MintWorkloadToken("key", id, now, 0); !errors.Is(err, ErrWorkloadToken) {
		t.Errorf("zero ttl mint error = %v, want ErrWorkloadToken", err)
	}
}

func TestWorkloadTokenVerifyRejectsMalformed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for name, token := range map[string]string{
		"empty":         "",
		"no separator":  "abcdef",
		"empty payload": ".abcdef",
		"empty mac":     "abcdef.",
		"not base64":    "!!!!.????",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyWorkloadToken("key", token, now); !errors.Is(err, ErrWorkloadToken) {
				t.Errorf("verify(%q) error = %v, want ErrWorkloadToken", token, err)
			}
		})
	}
}
