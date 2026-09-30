// Package pyenv prepares and resolves opt-in Python environments. Preparation
// never occurs on the run path: the daemon only accepts a ready environment.
package pyenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var exactVersion = regexp.MustCompile(`^3\.[0-9]+\.[0-9]+$`)

// RecipeVersion identifies how an environment is built. Bump it whenever the
// preparation commands, their flags, or the environment layout change: a bump
// must produce a different identity so existing environments are rebuilt
// rather than silently reused.
//
// The egress preflight and the index/mirror passthrough deliberately did not
// bump it, because neither changes what an environment is. With nothing
// configured the commands are byte-for-byte what they were, so every existing
// environment keeps its identity and is reused rather than re-downloading an
// interpreter and its wheels. With something configured, the flags are pinned
// by the lock: `uv.lock` records the registry each package came from, the lock
// is part of the declared inputs, and a configured index that disagrees with it
// is refused before anything is built (see lock.go). A different package route
// is therefore always a different lock, and so a different digest.
const RecipeVersion = "1"

// Spec identifies the declared inputs of a prepared environment.
//
// Digest covers Policy plus Inputs. InputsDigest covers only the declared
// inputs, so a run recorded before a preparation-time policy change (a new uv
// or a new recipe) can still be validated against an environment that was
// legitimately prepared under the older policy.
type Spec struct {
	Job          string `json:"job"`
	Python       string `json:"python"`
	Digest       string `json:"digest"`
	InputsDigest string `json:"inputs_digest"`
	Policy       string `json:"policy"`
}

// Ready is published only after uv has synchronized and the interpreter runs.
type Ready struct {
	Spec
	Interpreter string `json:"interpreter"`
	UVVersion   string `json:"uv_version"`
}

// UnmarshalJSON preserves readiness markers written before the job rename.
func (r *Ready) UnmarshalJSON(data []byte) error {
	type current Ready
	var wire struct {
		current
		LegacyJob string `json:"integration"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*r = Ready(wire.current)
	if r.Job == "" {
		r.Job = wire.LegacyJob
	}
	return nil
}

// Manager owns the derived environments under a data directory.
//
// The zero value of every field but DataDir is the documented default: no
// configured index or mirror, the default endpoints, and the egress preflight
// enabled. Preparation settings describe the *route* to the same hash-pinned
// content rather than the content itself, so they are deliberately absent from
// the environment identity: an environment prepared through a mirror must stay
// valid when the mirror is later unreachable, and `otter release --activate`
// and the daemon both re-resolve that identity without ever being told which
// mirror prepared it.
type Manager struct {
	DataDir string

	// Index is the package index dependencies are resolved and synced from.
	// Empty keeps whatever the lock records, which uv installs from when no
	// index is configured. It is passed to uv explicitly, as `--default-index`,
	// because preparation runs uv with configuration discovery disabled
	// (UV_NO_CONFIG=1): the flag needs no config file, and no inherited
	// user-level uv configuration can redirect the fetch.
	//
	// It replaces the default index rather than being an additional one, and it
	// has to be the index `uv.lock` records: `uv sync --locked` re-resolves
	// against the configured index and refuses when the result would change the
	// lock, so preparation refuses the disagreement itself, before fetching
	// (see lock.go).
	Index string

	// PythonMirror is the root managed interpreter downloads come from. Empty
	// keeps uv's default (DefaultPythonMirror). It is passed as `--mirror`,
	// which uv composes as <mirror>/<release-tag>/<file>, so the value is the
	// directory the release assets live under.
	PythonMirror string

	// ExtraEndpoints are further endpoints the preflight must reach, typically
	// the APIs the jobs themselves call. The runtime cannot discover those, so
	// they are configured; they are checked but never fetched from by
	// preparation.
	ExtraEndpoints []string

	// SkipEgressCheck turns the preflight off. It exists for a host that is
	// deliberately air-gapped and already primed -- its interpreter, its wheel
	// cache and its environment are provisioned out of band -- where a probe
	// would refuse preparation that would otherwise have succeeded from what
	// is already on disk.
	SkipEgressCheck bool

	// Probe performs one preflight request. Nil means HTTPProbe, which is the
	// only part of preparation that touches the network before a fetch; a test
	// injects a stub here.
	Probe Prober

	// Logf receives preparation progress, one line per step worth narrating.
	// Nil is legal and means silent, and a caller that wants the preflight to
	// be visible rather than only audible on failure supplies one.
	Logf func(format string, args ...any)
}

// logf reports preparation progress when a sink was supplied. A nil sink is
// legal: preparation is not narration, and a test should not need one.
func (m Manager) logf(format string, args ...any) {
	if m.Logf == nil {
		return
	}
	m.Logf(format, args...)
}

func (m Manager) root() (string, error) {
	if m.DataDir == "" {
		return "", errors.New("python data directory is empty")
	}
	return filepath.Abs(m.DataDir)
}

// Resolve hashes the lock, project metadata, exact Python pin, job
// identity, target platform and every preparation-policy input. The directory
// is never shared across targets.
//
// uvVersion is part of the identity because a different uv resolves and
// installs differently; pass "" when uv is unavailable and the caller is only
// reproducing an identity it already recorded.
func (m Manager) Resolve(dir, job, uvVersion string) (Spec, error) {
	if job == "" {
		return Spec{}, errors.New("job name is empty")
	}
	var inputs [][]byte
	for _, name := range []string{".python-version", "pyproject.toml", "uv.lock"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return Spec{}, fmt.Errorf("read %s: %w", name, err)
		}
		inputs = append(inputs, body)
	}
	version := strings.TrimSpace(string(inputs[0]))
	if !exactVersion.MatchString(version) {
		return Spec{}, fmt.Errorf(".python-version must pin an exact CPython patch version (for example 3.13.5), got %q", version)
	}

	policy := buildPolicy(job, version, targetPlatform(), libcVariant, uvVersion)

	// The inputs digest deliberately excludes the policy so that a recorded run
	// stays valid across a preparation-time tooling change.
	inputsHasher := sha256.New()
	for _, s := range []string{"otter-pyenv-inputs-v1", job, version} {
		_, _ = inputsHasher.Write([]byte(s + "\x00"))
	}
	for _, input := range inputs {
		_, _ = inputsHasher.Write(input)
		_, _ = inputsHasher.Write([]byte{0})
	}

	hasher := sha256.New()
	for _, s := range []string{"otter-pyenv-v2", policy, hex.EncodeToString(inputsHasher.Sum(nil))} {
		_, _ = hasher.Write([]byte(s + "\x00"))
	}

	return Spec{
		Job:          job,
		Python:       version,
		Digest:       hex.EncodeToString(hasher.Sum(nil)),
		InputsDigest: hex.EncodeToString(inputsHasher.Sum(nil)),
		Policy:       policy,
	}, nil
}

// buildPolicy is the canonical description of everything outside the declared
// inputs that changes what gets prepared: the recipe, the pinned interpreter
// and its exact version, the target ABI, and the dependency-selection and
// installation policy.
func buildPolicy(job, python, platform, libc, uvVersion string) string {
	return strings.Join([]string{
		"recipe=" + RecipeVersion,
		"uv=" + uvVersion,
		"py=" + python,
		"target=" + platform,
		"libc=" + libc,
		// The preparation policy, stated explicitly rather than implied by the
		// command line, so changing any of it changes the identity.
		"deps=lock",
		"dev=exclude",
		"build=wheel-only",
		"project=no-install",
		"python=managed-only",
		"config=none",
		// The index is deliberately recorded as "default" whatever the caller
		// configured, and it does not need recording: uv treats the index as
		// part of the lock -- every registry package carries the URL it came
		// from -- the lock's contents are in the declared inputs above, and a
		// configured index that disagrees with a recorded one is refused
		// before anything is built (lock.go). A different package route is
		// therefore always a different lock, and so a different digest, while
		// the two callers that re-resolve this identity -- the daemon at
		// submission, `otter release --activate` on a rollback -- are never
		// told which mirror prepared it. Folding the URL into the policy
		// instead would make both of them look for an environment preparation
		// never built.
		"index=default",
		"job=" + job,
	}, ";")
}

// targetPlatform is the host the environment is prepared for. It is recorded
// rather than inferred so a copied data directory is never mistaken for a
// compatible one.
func targetPlatform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// libcVariant distinguishes glibc from musl targets, which cannot share a
// prepared interpreter. It is resolved once at package load.
var libcVariant = detectLibc()

func (m Manager) envDir(spec Spec) (string, error) {
	root, err := m.root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "environments", spec.Digest), nil
}

// GetReady validates the publication marker and the interpreter before a run.
//
// spec normally comes from the run record, so it carries the identity that was
// resolved when the run was submitted. The local policy is deliberately not
// recomputed here: a queued run must keep working after a preparation-time
// tooling change, because its environment was already prepared and is still on
// disk.
func (m Manager) GetReady(spec Spec) (Ready, error) {
	if len(spec.Digest) != 64 || spec.Job == "" || spec.Python == "" {
		return Ready{}, errors.New("run has no valid managed Python environment identity")
	}
	dir, err := m.envDir(spec)
	if err != nil {
		return Ready{}, err
	}
	body, err := os.ReadFile(filepath.Join(dir, "otter-ready.json"))
	if err != nil {
		return Ready{}, fmt.Errorf("managed Python environment %s is not prepared; run otter prepare: %w", spec.Digest[:12], err)
	}
	var ready Ready
	if err := json.Unmarshal(body, &ready); err != nil {
		return Ready{}, fmt.Errorf("invalid managed Python readiness marker: %w", err)
	}
	if !matches(spec, ready) {
		return Ready{}, errors.New("managed Python readiness marker does not match the requested environment")
	}
	if info, err := os.Stat(ready.Interpreter); err != nil || info.IsDir() {
		return Ready{}, fmt.Errorf("managed Python interpreter is missing: %s", ready.Interpreter)
	}
	if filepath.Dir(filepath.Dir(ready.Interpreter)) != dir ||
		filepath.Base(ready.Interpreter) != "python" {
		return Ready{}, fmt.Errorf("managed Python interpreter %s is outside its environment", ready.Interpreter)
	}
	return ready, nil
}

// matches confirms that a readiness marker describes the requested identity.
func matches(want Spec, ready Ready) bool {
	return ready.Job == want.Job &&
		ready.Python == want.Python &&
		ready.Digest == want.Digest
}

// ResolveCurrent returns the identity a fresh preparation would produce for
// this host right now, resolving the uv version because it is part of the
// policy. It is only for preparation and for binding a new run; execution uses
// the identity the run recorded instead.
func (m Manager) ResolveCurrent(ctx context.Context, dir, job string) (Spec, error) {
	return m.currentIdentity(ctx, dir, job, "")
}

// ResolveCurrentAt is ResolveCurrent with an explicit uv path, for callers that
// manage the toolchain location themselves.
func (m Manager) ResolveCurrentAt(ctx context.Context, dir, job, uvPath string) (Spec, error) {
	return m.currentIdentity(ctx, dir, job, uvPath)
}

// currentIdentity returns the identity a fresh preparation would produce. The
// uv version is part of the policy, so it is resolved here; a missing uv does
// not fail identity resolution, it is recorded as "absent" so an already
// prepared environment stays reusable.
func (m Manager) currentIdentity(ctx context.Context, dir, job, uvPath string) (Spec, error) {
	version := uvVersionOf(ctx, m.UVPath(uvPath))
	return m.Resolve(dir, job, version)
}

// uvCandidate locates a vendored uv.
//
// Two layouts are searched: one inside the data directory, and the deploy
// layout, where the data directory is a sibling of the install root's tools
// directory. Both spellings resolve to the same binary, which matters because
// uv is part of the environment identity: a daemon and a `otter release` that
// disagreed about which uv they found would compute different environment
// digests for the same job.
func (m Manager) uvCandidate() (string, bool) {
	root, err := m.root()
	if err != nil {
		return "", false
	}

	// A vendored uv is looked for in the data directory and in the directories
	// that hold it, because where `tools/uv` sits depends on where the data
	// directory is:
	//
	//	<install root>/data        + <install root>/tools/uv
	//	<workspace>/.otter/data    + <workspace>/tools/uv
	//
	// The nearest one wins. Both spellings have to resolve to the same binary,
	// because uv is part of the environment identity: a daemon and a deploy that
	// disagreed about which uv they found would compute different digests for
	// the same job, and every run would look for an environment that
	// preparation never created.
	var dirs []string
	dirs = append(dirs, filepath.Join(root, "tools", "uv"))
	ancestor := root
	for depth := 0; depth < 3; depth++ {
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			break
		}
		ancestor = parent
		dirs = append(dirs, filepath.Join(ancestor, "tools", "uv"))
	}

	for _, dir := range dirs {
		for _, name := range []string{"uv", "uv.exe"} {
			candidate := filepath.Join(dir, name)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, true
			}
		}
	}
	return "", false
}

// UVPath returns the uv executable preparation should use.
//
// A copy vendored under the data directory always wins, even over an explicit
// path. The daemon and `otter release` must resolve uv identically: uv is part
// of the preparation policy and therefore of the environment digest, so two
// different answers would produce two different environments for the same
// job, and a run would look for an environment preparation never
// created.
func (m Manager) UVPath(explicit string) string {
	if candidate, ok := m.uvCandidate(); ok {
		return candidate
	}
	if explicit != "" {
		return explicit
	}
	return "uv"
}

// resolveUV locates the uv executable, preferring a vendored copy. A missing
// uv is only an error when something actually has to be built: an environment
// that is already prepared must stay reusable on a host where the toolchain
// has since been removed.
func resolveUV(uvPath string) (string, error) {
	if uvPath == "" {
		uvPath = "uv"
	}
	found, err := exec.LookPath(uvPath)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrUVMissing, uvPath)
	}
	return found, nil
}

// ErrUVMissing reports that preparation was attempted without a uv binary.
var ErrUVMissing = errors.New("uv is required for preparation and was not found")

// uvVersionOf returns the uv version, or the placeholder "absent" when uv
// cannot be located. The placeholder still participates in the policy: a host
// that gains uv later produces a different environment identity rather than
// silently reusing one built without it.
func uvVersionOf(ctx context.Context, uvPath string) string {
	uv, err := resolveUV(uvPath)
	if err != nil {
		return "absent"
	}
	out, err := exec.CommandContext(ctx, uv, "--version").Output()
	if err != nil {
		return "absent"
	}
	return strings.TrimSpace(string(out))
}

// RecordedIdentity rebuilds the identity a previous resolution recorded.
//
// The daemon uses this on the run path. It never invokes uv or re-derives the
// policy, because a queued run must keep resolving to the environment it was
// submitted against even after the toolchain changes.
func RecordedIdentity(job, python, digest, policy string) Spec {
	return Spec{Job: job, Python: python, Digest: digest, Policy: policy}
}

// Prepare downloads the pinned interpreter and syncs locked production
// dependencies. A crashed preparation leaves no readiness marker; retrying
// safely reconstructs the incomplete directory under an advisory lock.
func (m Manager) Prepare(ctx context.Context, dir, job, uvPath string) (Ready, error) {
	spec, err := m.currentIdentity(ctx, dir, job, uvPath)
	if err != nil {
		return Ready{}, err
	}
	envDir, err := m.envDir(spec)
	if err != nil {
		return Ready{}, err
	}
	root, err := m.root()
	if err != nil {
		return Ready{}, err
	}
	if err := os.MkdirAll(filepath.Dir(envDir), 0o700); err != nil {
		return Ready{}, err
	}
	lock, err := acquireLock(filepath.Join(filepath.Dir(envDir), spec.Digest+".lock"))
	if err != nil {
		return Ready{}, err
	}
	defer lock.Close()
	if ready, err := m.GetReady(spec); err == nil {
		return ready, nil
	}
	// An existing marker with invalid contents is an integrity error. Never
	// delete that environment; an older queued run might still reference it.
	if _, err := os.Stat(filepath.Join(envDir, "otter-ready.json")); err == nil {
		return Ready{}, fmt.Errorf("environment %s has an invalid readiness marker", spec.Digest[:12])
	}
	if err := os.RemoveAll(envDir); err != nil {
		return Ready{}, fmt.Errorf("remove incomplete environment: %w", err)
	}
	if err := os.MkdirAll(envDir, 0o700); err != nil {
		return Ready{}, err
	}
	// Reuse happens above; reaching this point means the environment must be
	// built, which genuinely requires uv.
	uv, err := resolveUV(m.UVPath(uvPath))
	if err != nil {
		return Ready{}, err
	}
	versionOut, err := exec.CommandContext(ctx, uv, "--version").Output()
	if err != nil {
		return Ready{}, fmt.Errorf("read uv version: %w", err)
	}
	// The lock decides where packages come from, so settle its disagreement
	// with a configured index here, locally and before the network check: uv
	// would otherwise refuse this at `sync` with a bare exit 2 that names
	// neither the lock nor the index.
	if err := m.checkLockIndex(dir); err != nil {
		return Ready{}, err
	}
	// Ask the network question before the fetch, so a host with no route fails
	// as a route problem rather than as whatever uv was doing when it noticed.
	// A skipped check is recorded: it is what stops a later interpreter failure
	// from being misreported as a platform/patch-version problem.
	egressChecked := false
	if !m.SkipEgressCheck {
		if err := m.checkEgress(ctx, dir, spec); err != nil {
			return Ready{}, err
		}
		egressChecked = true
		m.logf("egress preflight ok for environment %s: %s\n", spec.Digest[:12], describeEgress(m.endpoints(dir)))
	}
	baseEnv := append(os.Environ(), "UV_PYTHON_INSTALL_DIR="+filepath.Join(root, "python"), "UV_CACHE_DIR="+filepath.Join(root, "cache", "uv"), "UV_PROJECT_ENVIRONMENT="+envDir, "UV_PYTHON_PREFERENCE=only-managed", "UV_NO_CONFIG=1")
	installArgs := []string{"python", "install", "--install-dir", filepath.Join(root, "python")}
	if mirror := strings.TrimSpace(m.PythonMirror); mirror != "" {
		installArgs = append(installArgs, "--mirror", mirror)
	}
	install := exec.CommandContext(ctx, uv, append(installArgs, spec.Python)...)
	install.Env = baseEnv
	if out, err := install.CombinedOutput(); err != nil {
		return Ready{}, installError(spec, err, strings.TrimSpace(string(out)), egressChecked)
	}
	syncArgs := []string{"sync", "--locked", "--no-install-project", "--no-build", "--no-config", "--no-python-downloads", "--python", spec.Python, "--python-preference", "only-managed"}
	if index := strings.TrimSpace(m.Index); index != "" {
		// --default-index, not --index: the configured index replaces PyPI
		// instead of joining it, so a host with no route to PyPI is not asked
		// to reach it.
		syncArgs = append(syncArgs, "--default-index", index)
	}
	sync := exec.CommandContext(ctx, uv, syncArgs...)
	sync.Dir, sync.Env = dir, baseEnv
	if out, err := sync.CombinedOutput(); err != nil {
		return Ready{}, fmt.Errorf("sync %s: %w: %s", job, err, strings.TrimSpace(string(out)))
	}
	interpreter := filepath.Join(envDir, "bin", "python")
	verify := exec.CommandContext(ctx, interpreter, "-c", "import sys; print('.'.join(map(str, sys.version_info[:3])))")
	verify.Env = []string{"PATH=" + os.Getenv("PATH"), "PYTHONNOUSERSITE=1"}
	out, err := verify.Output()
	if err != nil {
		return Ready{}, fmt.Errorf("verify interpreter: %w", err)
	}
	if strings.TrimSpace(string(out)) != spec.Python {
		return Ready{}, fmt.Errorf("prepared Python version %s does not match pin %s", strings.TrimSpace(string(out)), spec.Python)
	}
	ready := Ready{Spec: spec, Interpreter: interpreter, UVVersion: strings.TrimSpace(string(versionOut))}
	body, err := json.MarshalIndent(ready, "", "  ")
	if err != nil {
		return Ready{}, err
	}
	tmp := filepath.Join(envDir, "otter-ready.json.tmp")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return Ready{}, err
	}
	if err := os.Rename(tmp, filepath.Join(envDir, "otter-ready.json")); err != nil {
		return Ready{}, err
	}
	return ready, nil
}
