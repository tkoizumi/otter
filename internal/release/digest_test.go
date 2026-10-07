package release

import (
	"strings"
	"testing"
)

// The two spellings name the same release. Comparing them as strings is what
// produced "no job has release sha256:...".
func TestNormalizeDigestUnifiesTheSpellings(t *testing.T) {
	bare := strings.Repeat("f68f24", 10) + "abcd" // 64 hex chars
	if len(bare) != 64 {
		t.Fatalf("fixture digest is %d chars, want 64", len(bare))
	}
	for _, in := range []string{bare, "sha256:" + bare, "SHA256:" + bare, "  " + bare + "  "} {
		got, err := NormalizeDigest(in)
		if err != nil {
			t.Fatalf("NormalizeDigest(%q): %v", in, err)
		}
		if got != bare {
			t.Errorf("NormalizeDigest(%q) = %q, want %q", in, got, bare)
		}
	}

	for _, bad := range []string{"", "sha256:", "nope", bare[:63], bare + "a", "sha256:" + "z" + bare[:63]} {
		if _, err := NormalizeDigest(bad); err == nil {
			t.Errorf("NormalizeDigest(%q) should have been refused", bad)
		}
	}
}
