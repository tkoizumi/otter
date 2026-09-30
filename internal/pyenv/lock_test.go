package pyenv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLock replaces the job's uv.lock with a body, so a test can express the
// registry a lock records. It must run before Prepare, which hashes the lock
// into the environment identity.
func writeLock(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// lockAgainst is a lock shaped the way uv 0.5.9 writes one, with one registry
// package and the project's own virtual source.
func lockAgainst(registries ...string) string {
	var b strings.Builder
	b.WriteString("version = 1\nrequires-python = \">=3.10\"\n")
	for i, registry := range registries {
		b.WriteString("\n[[package]]\n")
		b.WriteString("name = \"demo" + string(rune('a'+i)) + "\"\n")
		b.WriteString("version = \"0.1.0\"\n")
		b.WriteString("source = { registry = \"" + registry + "\" }\n")
	}
	b.WriteString("\n[[package]]\nname = \"project\"\nversion = \"0.1.0\"\nsource = { virtual = \".\" }\n")
	return b.String()
}

// uv sync --locked refuses a lock cut against one index when preparation is
// configured with another, even when the two serve identical artifacts. That
// refusal must be otter's own, name both URLs and the lock, and arrive before
// any fetch or network check: uv's own message is "exit status 2" and names
// none of the three.
func TestPrepareRefusesALockLockedAgainstADifferentIndex(t *testing.T) {
	clearRouteEnv(t)
	python, pin := localPython(t)
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, pin)
	writeLock(t, dir, lockAgainst("https://pypi.org/simple"))
	uv, logPath := fakeUV(t, data, python, "")

	probe := &probeRecorder{fail: map[string]error{}}
	m := Manager{DataDir: data, Index: "https://mirror.internal/simple", Probe: probe.probe}
	_, err := m.Prepare(context.Background(), dir, "one", uv)
	if err == nil {
		t.Fatal("preparation proceeded against an index the lock does not record")
	}
	if !errors.Is(err, ErrLockIndexMismatch) {
		t.Fatalf("failure is not a lock/index mismatch: %v", err)
	}
	for _, want := range []string{
		"uv.lock",
		"https://pypi.org/simple",        // what the lock records
		"https://mirror.internal/simple", // what preparation is configured with
		"uv lock --default-index",        // the fix that regenerates the lock
		"Omit --index",                   // the fix that always satisfies --locked
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("mismatch error does not mention %q:\n%v", want, err)
		}
	}
	// Nothing was fetched, and the network was never consulted: the lock is a
	// local fact and it is settled first.
	log := readLog(t, logPath)
	if strings.Contains(log, "install") || strings.Contains(log, "sync") {
		t.Errorf("a fetch ran despite the lock refusing the index:\n%s", log)
	}
	if len(probe.seen) != 0 {
		t.Errorf("the network was checked before the lock was: %v", probe.seen)
	}
}

// The lock records the index the environment will be built from, so a
// configured index that matches it is the working case.
func TestLockIndexCheckAcceptsTheIndexTheLockRecords(t *testing.T) {
	clearRouteEnv(t)
	const mirror = "https://mirror.internal/simple"
	python, pin := localPython(t)
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, pin)
	writeLock(t, dir, lockAgainst(mirror))
	uv, logPath := fakeUV(t, data, python, "")

	m := Manager{DataDir: data, Index: mirror, Probe: (&probeRecorder{fail: map[string]error{}}).probe}
	if _, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
		t.Fatalf("a lock cut against the configured index was refused: %v", err)
	}
	log := readLog(t, logPath)
	if !strings.Contains(log, "python install") || !strings.Contains(log, "sync") {
		t.Errorf("preparation did not fetch:\n%s", log)
	}
	if !strings.Contains(log, "--default-index "+mirror) {
		t.Errorf("the configured index did not reach uv:\n%s", log)
	}
}

// The comparison has to be exactly as strict as uv's in both directions --
// accepting what uv accepts and refusing what uv refuses. Every row here is a
// pair the pinned uv 0.5.9 was measured on with a generated lock and a loopback
// index; the raw transcript is in docs/evidence/phase-0/
// 2026-09-30-p0-08-egress-managed-python.txt (ROUND 3 and ROUND 4).
func TestCanonicalIndexURLFoldsWhatUVFoldsAndNothingElse(t *testing.T) {
	same := [][2]string{
		{"https://pypi.org/simple", "https://pypi.org/simple"},
		{"https://pypi.org:443/simple", "https://pypi.org/simple"},
		{"http://mirror.internal:80/simple", "http://mirror.internal/simple"},
		// uv folds host case, measured.
		{"http://LOCALHOST:8080/simple", "http://localhost:8080/simple"},
		// A bare host is the root index, and uv records the root slash it gives
		// an empty path. Refusing this pair refused a preparation uv accepts.
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/"},
		{"http://mirror.internal", "http://mirror.internal/"},
		// Literal dot segments, measured.
		{"http://mirror.internal/a/../a/simple", "http://mirror.internal/a/simple"},
		{"http://mirror.internal/a/./simple", "http://mirror.internal/a/simple"},
		{"http://mirror.internal/a/b/../simple", "http://mirror.internal/a/simple"},
		{"http://mirror.internal/a/simple/..", "http://mirror.internal/a/"},
		// Percent-encoded dot segments, measured: uv folds /a/%2E%2E/a/simple,
		// /a/.%2e/a/simple and /a/%2e./a/simple to /a/simple.
		{"http://mirror.internal/a/%2E%2E/a/simple", "http://mirror.internal/a/simple"},
		{"http://mirror.internal/a/.%2e/a/simple", "http://mirror.internal/a/simple"},
		{"http://mirror.internal/a/%2e./a/simple", "http://mirror.internal/a/simple"},
		// WHATWG IPv4 short forms, measured: uv folds 127.1, 127.0.1 and
		// 2130706433 to 127.0.0.1.
		{"http://127.1:8080/a/simple", "http://127.0.0.1:8080/a/simple"},
		{"http://127.0.1:8080/a/simple", "http://127.0.0.1:8080/a/simple"},
		{"http://2130706433:8080/a/simple", "http://127.0.0.1:8080/a/simple"},
		{"http://127.0.0.1.:8080/a/simple", "http://127.0.0.1:8080/a/simple"},
	}
	for _, pair := range same {
		if !equalIndexURL(pair[0], pair[1]) {
			t.Errorf("uv treats %q and %q as the same index, and this check does not", pair[0], pair[1])
		}
	}
	different := [][2]string{
		// uv refuses these against --locked; a check that forgave one would
		// wave through a sync uv then rejects.
		{"https://mirror.internal/simple", "https://mirror.internal/simple/"},
		{"http://mirror.internal/simple", "http://mirror.internal/simple?"},
		// Percent escapes are neither decoded nor case-folded.
		{"http://mirror.internal/simple", "http://mirror.internal/sim%70le"},
		{"http://mirror.internal/simple", "http://mirror.internal/%73imple"},
		{"http://mirror.internal/a%2fb", "http://mirror.internal/a%2Fb"},
		// uv does not fold the scheme: it cannot use an upper-case one at all.
		{"http://mirror.internal/simple", "HTTP://mirror.internal/simple"},
		{"https://mirror.internal/simple", "https://mirror.internal:8443/simple"},
		{"http://mirror.internal/simple", "https://mirror.internal/simple"},
		{"https://mirror.internal/simple", "https://other.internal/simple"},
		// An empty segment is a segment: a doubled slash is a different path.
		{"http://mirror.internal/a/simple", "http://mirror.internal/a//simple"},
		// Unmodeled WHATWG corners are refused rather than guessed at.
		{"http://0x7f.0.0.1:8080/simple", "http://127.0.0.1:8080/simple"},
		{"http://0177.0.0.1:8080/simple", "http://127.0.0.1:8080/simple"},
	}
	for _, pair := range different {
		if equalIndexURL(pair[0], pair[1]) {
			t.Errorf("uv treats %q and %q as different indexes, and this check does not", pair[0], pair[1])
		}
	}
	if equalIndexURL("http://mirror.internal/a/simple", "http://mirror.internal/a/simple#frag") {
		t.Error("a fragment was folded away")
	}
}

// The measured matrix, through the decision preparation actually makes: every
// pair uv accepts is accepted, every pair uv refuses is refused. This is what
// notices an over-refusal returning -- a change that folds more than uv does
// makes the accepting rows fail.
func TestLockIndexDecisionMatchesUV(t *testing.T) {
	clearRouteEnv(t)
	const host = "http://mirror.internal:8080"
	cases := []struct {
		name       string
		recorded   string
		configured string
		uvAccepts  bool
	}{
		{"identical", host + "/simple", host + "/simple", true},
		{"bare host against uv's root-slash lock", "http://127.0.0.1:8080/", "http://127.0.0.1:8080", true},
		{"host case", host + "/simple", "http://MIRROR.internal:8080/simple", true},
		{"literal dot segments", host + "/simple", host + "/a/../simple", true},
		{"percent-encoded dot segments", host + "/simple", host + "/a/%2E%2E/simple", true},
		{"IPv4 short form", "http://127.0.0.1:8080/simple", "http://127.1:8080/simple", true},
		{"IPv4 integer form", "http://127.0.0.1:8080/simple", "http://2130706433:8080/simple", true},
		{"trailing slash on a non-empty path", host + "/simple", host + "/simple/", false},
		{"empty query", host + "/simple", host + "/simple?", false},
		{"percent-decoded unreserved", host + "/simple", host + "/sim%70le", false},
		{"percent-escape hex case", host + "/simple%2f", host + "/simple%2F", false},
		{"scheme case", host + "/simple", "HTTP://mirror.internal:8080/simple", false},
		{"different port", host + "/simple", "http://mirror.internal:8081/simple", false},
		{"different host", host + "/simple", "http://other.internal:8080/simple", false},
		{"doubled slash", host + "/a/simple", host + "/a//simple", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeInputs(t, dir, "3.13.5")
			writeLock(t, dir, lockAgainst(tc.recorded))
			m := Manager{DataDir: t.TempDir(), Index: tc.configured}
			err := m.checkLockIndex(dir)
			if tc.uvAccepts && err != nil {
				t.Errorf("uv accepts %q against a lock recording %q, and preparation refuses it: %v",
					tc.configured, tc.recorded, err)
			}
			if !tc.uvAccepts && err == nil {
				t.Errorf("uv refuses %q against a lock recording %q, and preparation accepts it",
					tc.configured, tc.recorded)
			}
		})
	}
}

// The spellings uv's parser folds away are accepted end to end, so a
// preparation that would have worked is not stopped by the comparison.
func TestConfiguredIndexSpellingsUVAcceptsAreAccepted(t *testing.T) {
	clearRouteEnv(t)
	cases := []struct{ configured, recordedByUV string }{
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/"},
		{"http://127.0.0.1:8080/a/../a/simple", "http://127.0.0.1:8080/a/simple"},
		{"http://127.0.0.1:8080/a/%2E%2E/a/simple", "http://127.0.0.1:8080/a/simple"},
		{"http://127.1:8080/a/simple", "http://127.0.0.1:8080/a/simple"},
		{"http://2130706433:8080/a/simple", "http://127.0.0.1:8080/a/simple"},
		{"http://mirror.internal", "http://mirror.internal/"},
	}
	for _, tc := range cases {
		python, pin := localPython(t)
		dir, data := t.TempDir(), t.TempDir()
		writeInputs(t, dir, pin)
		writeLock(t, dir, lockAgainst(tc.recordedByUV))
		uv, _ := fakeUV(t, data, python, "")
		m := Manager{DataDir: data, Index: tc.configured, Probe: (&probeRecorder{fail: map[string]error{}}).probe}
		if _, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
			t.Errorf("a lock uv would record for %q was refused: %v", tc.configured, err)
		}
	}
}

// A lock cut with two sources needs both supplied. uv reports the missing one as
// a resolution failure ("not found in the package registry"), so preparation
// refuses it first and names the source and the variable that supplies it.
func TestMultiIndexLockNeedsEveryRecordedSource(t *testing.T) {
	clearRouteEnv(t)
	const (
		a = "http://127.0.0.1:8080/a/simple"
		b = "http://127.0.0.1:8080/b/simple"
	)
	python, pin := localPython(t)
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, pin)
	writeLock(t, dir, lockAgainst(a, b))
	uv, logPath := fakeUV(t, data, python, "")
	probe := &probeRecorder{fail: map[string]error{}}

	m := Manager{DataDir: data, Index: a, Probe: probe.probe}
	_, err := m.Prepare(context.Background(), dir, "one", uv)
	if err == nil {
		t.Fatal("a lock recording a source the route does not supply was accepted")
	}
	if !errors.Is(err, ErrLockIndexMismatch) {
		t.Fatalf("failure is not an otter-level route refusal: %v", err)
	}
	for _, want := range []string{"uv.lock", b, "UV_EXTRA_INDEX_URL=" + b, "omit --index"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q:\n%v", want, err)
		}
	}
	if log := readLog(t, logPath); strings.Contains(log, "install") || strings.Contains(log, "sync") {
		t.Errorf("a fetch ran despite the refusal:\n%s", log)
	}
}

// An additive source the lock already records is a working configuration, not a
// mismatch: a lock cut with `--default-index A --extra-index-url B` records both,
// `--default-index A` alone then exits 1, and the variable supplies the missing
// recorded source so uv exits 0. Refusing it would refuse a preparation uv
// performs.
func TestAdditiveSourceIsAcceptedWhenTheLockRecordsIt(t *testing.T) {
	const (
		a = "http://127.0.0.1:8080/a/simple"
		b = "http://127.0.0.1:8080/b/simple"
	)

	for _, name := range []string{"UV_INDEX", "UV_EXTRA_INDEX_URL"} {
		t.Run(name, func(t *testing.T) {
			clearRouteEnv(t)
			t.Setenv(name, b)
			python, pin := localPython(t)
			dir, data := t.TempDir(), t.TempDir()
			writeInputs(t, dir, pin)
			writeLock(t, dir, lockAgainst(a, b))
			uv, logPath := fakeUV(t, data, python, "")
			probe := &probeRecorder{fail: map[string]error{}}

			m := Manager{DataDir: data, Index: a, Probe: probe.probe}
			if _, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
				t.Fatalf("a source the lock records was refused: %v", err)
			}
			// Both sources are part of the route, so both are checked.
			want := []string{a, b}
			if len(probe.seen) < len(want) || probe.seen[0] != want[0] || probe.seen[1] != want[1] {
				t.Errorf("probed %v, want the configured index and the added source first: %v", probe.seen, want)
			}
			if log := readLog(t, logPath); !strings.Contains(log, "sync") {
				t.Errorf("preparation did not sync:\n%s", log)
			}
		})
	}

	// A find-links lock records the directory as a source, relative to the lock
	// file; uv syncs from it with or without the variable set.
	t.Run("UV_FIND_LINKS recorded as a relative path", func(t *testing.T) {
		clearRouteEnv(t)
		python, pin := localPython(t)
		dir, data := t.TempDir(), t.TempDir()
		writeInputs(t, dir, pin)
		writeLock(t, dir, lockAgainst("../wheels"))
		t.Setenv("UV_FIND_LINKS", filepath.Join(filepath.Dir(dir), "wheels"))
		uv, _ := fakeUV(t, data, python, "")
		m := Manager{DataDir: data, Probe: (&probeRecorder{fail: map[string]error{}}).probe}
		if _, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
			t.Fatalf("a recorded find-links directory was refused: %v", err)
		}
	})
}

func TestLockIndexCheckFollowsTheUVEnvironment(t *testing.T) {
	const mirror = "https://mirror.internal/simple"

	t.Run("matching environment index", func(t *testing.T) {
		clearRouteEnv(t)
		t.Setenv("UV_DEFAULT_INDEX", mirror)
		python, pin := localPython(t)
		dir, data := t.TempDir(), t.TempDir()
		writeInputs(t, dir, pin)
		writeLock(t, dir, lockAgainst(mirror))
		uv, _ := fakeUV(t, data, python, "")
		m := Manager{DataDir: data, Probe: (&probeRecorder{fail: map[string]error{}}).probe}
		if _, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
			t.Fatalf("a lock cut against UV_DEFAULT_INDEX was refused: %v", err)
		}
	})

	t.Run("mismatching environment index", func(t *testing.T) {
		clearRouteEnv(t)
		t.Setenv("UV_DEFAULT_INDEX", mirror)
		python, pin := localPython(t)
		dir, data := t.TempDir(), t.TempDir()
		writeInputs(t, dir, pin)
		writeLock(t, dir, lockAgainst("https://pypi.org/simple"))
		uv, _ := fakeUV(t, data, python, "")
		m := Manager{DataDir: data, Probe: (&probeRecorder{fail: map[string]error{}}).probe}
		_, err := m.Prepare(context.Background(), dir, "one", uv)
		if !errors.Is(err, ErrLockIndexMismatch) {
			t.Fatalf("a lock that disagrees with UV_DEFAULT_INDEX was accepted: %v", err)
		}
	})
}

// With no index configured, uv installs from the URLs the lock records and
// accepts a lock cut against a mirror. Refusing that here would turn a working
// preparation into a failure, so the check must stay quiet.
func TestLockIndexCheckIsSilentWhenNoIndexIsConfigured(t *testing.T) {
	clearRouteEnv(t)
	python, pin := localPython(t)
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, pin)
	writeLock(t, dir, lockAgainst("https://mirror.internal/simple"))
	uv, logPath := fakeUV(t, data, python, "")

	m := Manager{DataDir: data, Probe: (&probeRecorder{fail: map[string]error{}}).probe}
	if _, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
		t.Fatalf("a mirror-cut lock was refused although nothing was configured: %v", err)
	}
	log := readLog(t, logPath)
	if strings.Contains(log, "--default-index") {
		t.Errorf("an unconfigured preparation passed an index to uv:\n%s", log)
	}
	if !strings.Contains(log, "sync") {
		t.Errorf("preparation did not sync:\n%s", log)
	}
}

// Only registry sources have an index to disagree with. A project that depends
// on path, git or URL sources must not be refused for a missing registry.
func TestLockIndexCheckIgnoresNonRegistrySources(t *testing.T) {
	clearRouteEnv(t)
	python, pin := localPython(t)
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, pin)
	writeLock(t, dir, `version = 1
requires-python = ">=3.10"

[[package]]
name = "local"
version = "0.1.0"
source = { virtual = "." }

[[package]]
name = "from-path"
version = "0.1.0"
source = { directory = "../from-path" }

[[package]]
name = "from-edit"
version = "0.1.0"
source = { editable = "../from-edit" }

[[package]]
name = "from-git"
version = "0.1.0"
source = { git = "https://example.invalid/from-git?rev=abc#abc" }

[[package]]
name = "from-url"
version = "0.1.0"
source = { url = "https://example.invalid/from-url-0.1.0-py3-none-any.whl" }
`)
	uv, _ := fakeUV(t, data, python, "")
	m := Manager{DataDir: data, Index: "https://mirror.internal/simple", Probe: (&probeRecorder{fail: map[string]error{}}).probe}
	if _, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
		t.Fatalf("a lock with no registry source was refused: %v", err)
	}
}

// A lock the runtime cannot make sense of must not become a refusal of its own:
// uv still owns that error, and preparation must not block on a parse failure.
func TestLockIndexCheckToleratesALockItCannotRead(t *testing.T) {
	clearRouteEnv(t)
	python, pin := localPython(t)
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, pin)
	writeLock(t, dir, "this is not a lock file\nregistry = \n")
	uv, _ := fakeUV(t, data, python, "")
	m := Manager{DataDir: data, Index: "https://mirror.internal/simple", Probe: (&probeRecorder{fail: map[string]error{}}).probe}
	if _, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
		t.Fatalf("an unreadable lock was turned into a refusal: %v", err)
	}
}

// The package endpoints the preflight checks are the ones uv will fetch from.
// With nothing configured that means the registries the lock records, not PyPI:
// a mirror-cut lock on a PyPI-less host is a preparation that works, and
// checking PyPI would refuse it.
func TestPreflightChecksTheRegistriesTheLockRecords(t *testing.T) {
	clearRouteEnv(t)
	python, pin := localPython(t)
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, pin)
	writeLock(t, dir, lockAgainst("https://mirror-a.internal/simple", "https://mirror-b.internal/simple"))
	uv, _ := fakeUV(t, data, python, "")

	probe := &probeRecorder{fail: map[string]error{}}
	m := Manager{DataDir: data, Probe: probe.probe}
	if _, err := m.Prepare(context.Background(), dir, "one", uv); err != nil {
		t.Fatal(err)
	}
	want := []string{"https://mirror-a.internal/simple", "https://mirror-b.internal/simple", DefaultPythonMirror}
	if len(probe.seen) != len(want) {
		t.Fatalf("probed %v, want %v", probe.seen, want)
	}
	for i := range want {
		if probe.seen[i] != want[i] {
			t.Errorf("probed %v, want %v", probe.seen, want)
		}
	}
	for _, seen := range probe.seen {
		if seen == DefaultPackageIndex {
			t.Errorf("PyPI was checked although uv fetches from the lock's registries: %v", probe.seen)
		}
	}
}

// The lock/index check is about building an environment, not about serving one
// that is already built. A ready environment must stay reusable even when the
// current invocation configures an index that would not have produced it.
func TestReadyEnvironmentIsNotBlockedByAnIndexMismatch(t *testing.T) {
	clearRouteEnv(t)
	python, pin := localPython(t)
	dir, data := t.TempDir(), t.TempDir()
	writeInputs(t, dir, pin)
	writeLock(t, dir, lockAgainst("https://pypi.org/simple"))
	uv, _ := fakeUV(t, data, python, "")

	probe := &probeRecorder{fail: map[string]error{}}
	manager := Manager{DataDir: data, Probe: probe.probe}
	ready, err := manager.Prepare(context.Background(), dir, "one", uv)
	if err != nil {
		t.Fatal(err)
	}
	probe.seen = nil

	mismatched := Manager{DataDir: data, Index: "https://mirror.internal/simple", Probe: probe.probe}
	again, err := mismatched.Prepare(context.Background(), dir, "one", uv)
	if err != nil {
		t.Fatalf("a ready environment was refused because of the current index: %v", err)
	}
	if again.Digest != ready.Digest {
		t.Errorf("reuse resolved a different environment: %s != %s", again.Digest, ready.Digest)
	}
	if len(probe.seen) != 0 {
		t.Errorf("reuse consulted the network: %v", probe.seen)
	}
}
