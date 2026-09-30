package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P0-12's first claim is "names only": a manifest declares the name of a
// secret, never its value. These tests pin how far that is *enforced* rather
// than merely documented, because the two are not the same thing.
//
// `secrets` decodes into []string and every entry must match envNameRegexp, so
// the shape decides it:
//
//   - a value written as `NAME=value`, or a `secrets:` mapping of name to
//     value, cannot be a name and is refused (the first two tests below);
//   - a value that is itself spelled like an environment variable name --
//     letters, digits and underscores, starting with a letter or underscore --
//     is byte-for-byte indistinguishable from a name and is accepted
//     (the third test). That is a real limitation, not a guarantee, and the
//     manifest reference says so.

func secretManifest(list string) string {
	return "version: 1\nname: castor-sync\nentrypoint: main.py\nsecrets:\n" + list
}

// TestManifestRefusesASecretValueWrittenInTheList covers the paste-a-value
// mistake the task's evidence bar names: `SHOPIFY_TOKEN=shpat_...` in the
// `secrets` list. It is rejected at validate time, before anything deploys or
// runs, because `=` is not part of an environment variable name.
func TestManifestRefusesASecretValueWrittenInTheList(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{"a NAME=value assignment", "SHOPIFY_TOKEN=shpat_0123456789abcdef"},
		{"a value with a hyphen", "shpat-0123456789abcdef"},
		{"a value with dots", "shpat.0123456789abcdef"},
		{"a value with a space", `"Bearer sk_live_0123"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			touch(t, filepath.Join(dir, "main.py"))
			path := writeManifest(t, dir, secretManifest("  - "+tc.entry+"\n"))

			// Load only: the value must not even survive parsing as if it
			// were a name, and the error must name the offending entry.
			_, err := LoadAndValidate(path)
			if err == nil {
				t.Fatalf("manifest accepted %q as a secret name; a value must be refused", tc.entry)
			}

			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("error = %v (%T), want a *ValidationError from Validate", err, err)
			}
			if !strings.Contains(err.Error(), "secrets[0]") ||
				!strings.Contains(err.Error(), "not a valid environment variable name") {
				t.Errorf("error = %q, want it to name secrets[0] and say why", err.Error())
			}
		})
	}
}

// TestManifestRefusesASecretValueMapping covers the other way a value reaches
// the field: YAML that maps the name to the value, which is what someone
// writing `env:` by muscle memory produces. `secrets` is a list of names, so
// the decoder refuses the mapping outright -- there is no path by which a value
// keyed by its own name is read and then silently ignored.
func TestManifestRefusesASecretValueMapping(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "main.py"))
	path := writeManifest(t, dir, `version: 1
name: castor-sync
entrypoint: main.py
secrets:
  SHOPIFY_TOKEN: shpat_0123456789abcdef
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("a `secrets:` mapping was accepted; it must fail at parse time")
	}
	var verr *ValidationError
	if errors.As(err, &verr) {
		t.Errorf("error = %v, want a parse failure rather than a validation failure", err)
	}
	if !strings.Contains(err.Error(), "cannot unmarshal") {
		t.Errorf("error = %q, want the decoder's type error", err.Error())
	}
}

// TestASecretValueShapedLikeANameIsAccepted records the boundary of the
// guarantee, so it cannot be mistaken for enforcement that does not exist.
//
// `shpat_0123456789abcdef` is a plausible credential value and it passes
// validation, because it is also a valid environment variable name: the
// manifest language has no way to tell the two apart. Nothing here fails if
// the credential is real -- the run simply resolves an environment variable by
// that name and fails with "not available" if the environment lacks it. The
// defence is documentary and procedural (keep values in the `0600` environment
// file), not syntactic.
func TestASecretValueShapedLikeANameIsAccepted(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "main.py"))
	path := writeManifest(t, dir, secretManifest("  - shpat_0123456789abcdef\n"))

	m, err := LoadAndValidate(path)
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v; an identifier-shaped value cannot be "+
			"rejected syntactically, so this test pins the documented limitation", err)
	}
	if len(m.Secrets) != 1 || m.Secrets[0] != "shpat_0123456789abcdef" {
		t.Errorf("Secrets = %v, want the single entry kept verbatim", m.Secrets)
	}
}

// TestSecretValidationDoesNotConsultTheDaemonEnvironment pins why a missing
// secret is a *run* failure rather than a manifest failure: a manifest that
// names a secret validates whether or not the daemon can resolve it, so the
// absence is discovered at resolution time -- before Python starts, as the
// daemon's own test asserts. If validation ever started resolving names, a
// deploy would begin refusing manifests it must accept, and this test fails.
func TestSecretValidationDoesNotConsultTheDaemonEnvironment(t *testing.T) {
	t.Setenv("OTTER_TEST_PRESENT_SECRET", "shpat_0123456789abcdef")
	if err := os.Unsetenv("OTTER_TEST_ABSENT_SECRET"); err != nil {
		t.Fatalf("unset: %v", err)
	}

	dir := t.TempDir()
	touch(t, filepath.Join(dir, "main.py"))
	path := writeManifest(t, dir, secretManifest(
		"  - OTTER_TEST_PRESENT_SECRET\n  - OTTER_TEST_ABSENT_SECRET\n"))

	m, err := LoadAndValidate(path)
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v; a name that the environment does not "+
			"hold must still validate, because resolution happens at run time", err)
	}

	// The names are kept verbatim and no value was merged in from the
	// environment the validator happened to run in.
	if len(m.Secrets) != 2 {
		t.Fatalf("Secrets = %v, want both declared names", m.Secrets)
	}
	for _, name := range m.Secrets {
		if strings.Contains(name, "shpat") {
			t.Errorf("Secrets = %v, want names only; a value leaked from the environment", m.Secrets)
		}
	}
}
