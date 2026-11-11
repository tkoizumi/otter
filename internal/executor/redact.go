package executor

import (
	"sort"
	"strings"
)

// redacted marks where a secret value was replaced in captured output.
const redacted = "[redacted]"

// minRedactable is the shortest value worth replacing.
//
// A one-character value would turn every line into confetti, and a "secret" that
// short is not one. Eight is the point below which the substitution does more harm
// (unreadable logs, a false sense that redaction is complete) than good.
const minRedactable = 8

// secretRedactor replaces exact secret VALUES in a line of captured output.
//
// WHY THIS EXISTS. A job can print anything, including the credential it was
// given. The runtime knows those values — they are the ones it injected from the
// manifest's `secrets` list — so it is the only place that can keep them out of
// the run log, which is readable by every session in the workspace and by
// `otter logs`.
//
// WHAT IT IS NOT. It is not a guarantee. A job can transform a value (base64, a
// substring, a hash) and defeat an exact-match redactor; and a value shorter than
// minRedactable is left alone on purpose. It is a backstop for the ordinary
// mistake — printing the environment, echoing a token, a traceback that includes
// headers — not a containment boundary. The boundary is that the credential is
// never written to a file, never returned by an API and never given to the
// generated code except as an environment variable.
//
// Values come only from the run's DECLARED secrets, never from the whole
// environment: redacting unrelated variables would hide ordinary output and teach
// operators that the logs lie.
type secretRedactor struct {
	values []string
}

// newSecretRedactor builds a redactor from the values the manifest declares.
//
// environment is the resolved run environment; names are the manifest's `secrets`.
// A declared name that the environment does not carry is skipped: there is
// nothing to redact, and its absence is reported elsewhere as a missing secret.
func newSecretRedactor(environment map[string]string, names []string) *secretRedactor {
	values := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		value := environment[name]
		if len(value) < minRedactable || seen[value] {
			continue
		}
		seen[value] = true
		values = append(values, value)
	}
	if len(values) == 0 {
		return nil
	}
	// Longest first, so a value that contains another value cannot be left
	// half-replaced (the shorter one would break the longer match).
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return &secretRedactor{values: values}
}

// Redact returns the line with every known secret value replaced.
func (r *secretRedactor) Redact(line string) string {
	if r == nil || line == "" {
		return line
	}
	for _, value := range r.values {
		if strings.Contains(line, value) {
			line = strings.ReplaceAll(line, value, redacted)
		}
	}
	return line
}
