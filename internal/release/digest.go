package release

import (
	"fmt"
	"strings"
)

// NormalizeDigest accepts a release digest in either the bare-hex form the store
// is keyed by or the `sha256:<hex>` form the control plane speaks, and returns
// the bare lowercase hex.
//
// The two spellings name the same release, and refusing one of them turned a
// promotion into "no job has release sha256:...": the control plane sent the
// canonical prefixed form and the runtime compared it, character for character,
// against a directory named by bare hex. Normalising at the boundary is what
// makes the digest an address rather than a spelling.
func NormalizeDigest(d string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(d))
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) != 64 {
		return "", fmt.Errorf("release: %q is not a release digest", d)
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return "", fmt.Errorf("release: %q is not a release digest", d)
		}
	}
	return s, nil
}
