// Package identity owns the durable identity of an integration instance.
//
// An integration is not identified by its manifest name or by its directory:
// both are things an operator edits or a `cp -r` duplicates. Identity is a
// UUID minted once by the runtime, recorded in the registry, and mirrored into
// a `.otter-id` marker inside the source directory so that the registry can
// recognise the same tree after a move and can refuse a marker that was
// copied from somewhere else.
//
// The package is deliberately independent of the daemon: discovery, the CLI,
// the release system and the deployment system all need to reason about
// identity, and none of them should import the daemon to do it.
package identity

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ManifestFileName is the integration manifest filename. It lives here as well
// as in internal/config because discovery produces identity observations, and
// a shared constant is what keeps the two packages from drifting.
const ManifestFileName = "otter.yaml"

// MaxIDLength bounds an identifier. UUID text is 36 bytes; the slack exists
// for migrated legacy identifiers, which are the old manifest names.
const MaxIDLength = 128

// ErrInvalidID reports a value that cannot be an identifier.
var ErrInvalidID = errors.New("identity: invalid integration id")

// ID is an opaque integration instance identifier.
//
// IDs are opaque strings, not UUID values: migration preserves the legacy
// manifest name as the identifier so that existing state, history, tokens and
// releases keep working without a single rename. Code must never parse an ID,
// only compare and store it.
type ID string

// String renders the identifier.
func (id ID) String() string { return string(id) }

// IsZero reports whether the identifier is empty.
func (id ID) IsZero() bool { return id == "" }

// Mint returns a fresh identifier. It is a UUID v4: collisions are
// astronomically unlikely, and callers that persist must still handle the
// unique-constraint violation rather than assume uniqueness.
func Mint() (ID, error) {
	value, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("identity: mint id: %w", err)
	}
	return ID(value.String()), nil
}

// Parse validates a stored or presented identifier.
//
// The accepted grammar is deliberately conservative because the same value
// travels through file paths, URL segments and environment variables: bounded
// length, no whitespace, no path separators, no traversal, no control
// characters. Legacy identifiers -- manifest names such as `counter` or
// `shopify-to-erp` -- satisfy it, which is what makes migration rename-free.
func Parse(raw string) (ID, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidID)
	}
	if len(raw) > MaxIDLength {
		return "", fmt.Errorf("%w: %d bytes exceeds the %d byte limit", ErrInvalidID, len(raw), MaxIDLength)
	}
	if raw == "." || raw == ".." {
		return "", fmt.Errorf("%w: %q is a path traversal", ErrInvalidID, raw)
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return "", fmt.Errorf("%w: %q contains %q, which is not allowed", ErrInvalidID, raw, string(c))
		}
	}
	return ID(raw), nil
}

// MustParse is Parse for values already known to be valid, such as those read
// back from the registry. It panics on invalid input and exists only for
// tests and constants.
func MustParse(raw string) ID {
	id, err := Parse(raw)
	if err != nil {
		panic(err)
	}
	return id
}

// ParseMarkerBody reads the identifier out of a marker file body.
//
// A marker is one identifier and one trailing newline. Anything else --
// extra lines, leading or trailing spaces, a partially written file -- is
// rejected rather than trimmed, because a marker that is almost right is a
// file we do not understand and must not act on.
func ParseMarkerBody(body []byte) (ID, error) {
	text := string(body)
	text = strings.TrimSuffix(text, "\n")
	if strings.ContainsAny(text, "\r\n") {
		return "", fmt.Errorf("%w: marker must contain exactly one line", ErrInvalidID)
	}
	if text != strings.TrimSpace(text) {
		return "", fmt.Errorf("%w: marker must not contain leading or trailing whitespace", ErrInvalidID)
	}
	return Parse(text)
}

// MarkerBody renders the exact bytes stored in a marker file.
func MarkerBody(id ID) []byte {
	return []byte(id.String() + "\n")
}
