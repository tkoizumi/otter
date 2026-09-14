# Managed Python Environments

Otter can own the Python interpreter and the dependencies an integration runs
with, instead of depending on whatever `python3` a host happens to have. This
is opt-in, per integration, and additive: an integration that does not ask for
it behaves exactly as before.

The feature exists because a host you do not control is a host you cannot
diagnose. When Otter owns the environment, "it behaved differently yesterday"
has an answer in the run record.

## What changes, and what does not

| | External (default) | Managed (opt-in) |
| --- | --- | --- |
| Interpreter | `python.executable`, or `python3` from `PATH` | Prepared under `<data dir>/python`, selected per environment |
| Dependencies | Yours to provision | Installed from `uv.lock` into the environment |
| When resolved | Every run | Once, when the run is submitted |
| Isolation | Shared with the host | Private to the integration |
| Reproducibility | Whatever the host has today | Identified by a digest recorded on every run |

Everything else is unchanged: the same Go process supervision, output capture,
timeouts, cancellation, retries, state and logs.

## Choosing a mode

| `python.mode` | `python.executable` | Result |
| --- | --- | --- |
| `external` (or omitted) | omitted | Legacy behavior: `python3` from `PATH`. |
| `external` (or omitted) | set | Use that interpreter verbatim. Nothing is prepared. |
| `managed` | must be omitted | Use only a prepared environment. |
| `managed` | set | **Rejected.** The two ways of choosing an interpreter conflict. |

A lock file alone never changes how an integration runs. Opting in is explicit,
so adding `pyproject.toml` to an existing integration cannot silently move it
onto a different interpreter.

## Required files

```
my-integration/
├── otter.yaml
├── main.py
├── .python-version     exact CPython patch version, e.g. 3.13.5
├── pyproject.toml      declares requires-python and dependencies
└── uv.lock             the locked resolution
```

- `.python-version` must be an exact patch version. `3.13` is rejected: a
  floating minor version would make the environment's identity depend on the
  day it was built.
- `python.path` works in managed mode. Each declared directory is captured into
  the release at the same relative depth, so a path like `../../lib/python`
  resolves inside the snapshot exactly as it does in the checkout. Shared code
  that can be published as a package is better declared in `pyproject.toml`,
  because then it is locked and versioned with everything else.

## Releases

A managed integration does not execute its source tree. It executes an
**immutable release**: a snapshot of the integration plus the shared code it
declares, copied under the data directory and addressed by a digest of its
contents.

```sh
otter release <integration>          # stage, prepare and activate
otter release --list <integration>   # what is staged, and which one is active
```

`otter deploy` runs this for every managed integration automatically, before the
daemon restarts.

### Why

Without a snapshot, three things can go wrong:

- A run started during a deploy imports a mix of old and new modules.
- A retry executes different code than the attempt it is retrying.
- "It behaved differently yesterday" has no answer, because the code that ran
  is gone.

A release fixes all three. The digest is recorded on every run, so a run
identifies the exact snapshot it executed.

### The layout is mirrored, not flattened

```
<data dir>/.releases/<integration>/<release-digest>/
├── integrations/<integration>/   the snapshot, discovered as usual
├── lib/                          shared code at the same relative depth
└── otter-release.json            digest, environment, source, timestamp
```

Mirroring the checkout is what lets a manifest keep `python.path: [../../lib]`
verbatim: the release root plays the part of the repository root, so every
relative path resolves the same way before and after activation. Nothing is
rewritten to release an integration.

The activation link lives at `<data dir>/.releases/active/<integration>`,
outside the integrations tree, so `otter deploy` can keep syncing sources
normally without touching what is currently being served.

### Ordering

Release is stage → prepare → activate, and each step fails safely:

- **Staging** copies into a temporary directory and renames it into place, so a
  release directory either exists complete or not at all.
- **Preparation** runs against the staged snapshot, not the live tree, so what
  is validated is what will run.
- **Activation** is a single atomic `rename` of a symlink. A failure in any
  earlier step leaves the previous release active.

Redeploying unchanged inputs stages nothing new: identical inputs produce an
identical digest, so an existing release is reused.

### Traceability, not gating

A release records the git revision it was built from, and whether the working
tree was dirty at the time:

```
shopify-to-salesforce: release 7f060c540d85 (git 59b3e55, working tree dirty)
note: shopify-to-salesforce was released from a working tree with uncommitted changes
```

```
* 7f060c540d85  2026-09-14 03:16:59  env=7a83e4a641bb git=59b3e55+dirty  /Users/...
```

Releasing and committing are deliberately independent. A release is a build
step; a commit is a history step. Requiring a clean tree would break the
edit → release → run → fix loop, and it would not buy the guarantee it looks
like it buys: a commit says nothing about which branch reached production or
which revision a host is actually serving.

What is worth having is the ability to trace a deployed release back to a
commit, which is what the metadata provides. Enforcing a clean tree is a job for
whatever deploys to production, not for the build. Out of a git repository
entirely, the field is simply absent and a release still works.

### Binding

A managed run binds to the active release **when it is submitted**, and the
recorded snapshot is what execution uses. Activating a newer release therefore
cannot move a queued or retried attempt onto different code, and a run keeps
working while a deploy is in progress. An attempt whose snapshot has been
removed fails with an explicit error instead of silently running something else.

Integrations that do not set `python.mode: managed` are unaffected: they execute
their source tree exactly as before, and no release is created for them.

### Retention

`otter release --keep N` removes inactive releases beyond `N`, never touching
the active release, never touching one referenced by queued or running work, and
always keeping one release to roll back to. Retention is off unless you ask for
it; every release is kept by default, and `otter release --list` shows what is
consuming disk.

### Rollback

Activate an older release by name:

```sh
otter release --list my-integration          # find the digest
```

Rolling back means pointing the active link at a previous digest. The previous
release is still on disk unless retention removed it, and the runs that used it
are still recorded.

## Layout on disk

Everything derived lives under the data directory and can be deleted without
losing integration state:

```
<data dir>/
├── otter.db                          integration state, runs, logs
├── otter.lock                        exclusive ownership of this data directory
├── tools/uv/uv                       vendored uv, used only for preparation
├── python/<version>/                 shared interpreter installations
├── environments/<digest>/            one prepared environment per identity
│   ├── bin/python                    the interpreter runs use
│   └── otter-ready.json              readiness marker; absence means "not ready"
├── cache/uv/                         uv's download cache
├── .releases/                        immutable source snapshots
│   ├── <integration>/<digest>/       mirrored checkout: integrations/ + lib/
│   └── active/<integration>          symlink to the release being served
└── sdk/python/                       the embedded Otter SDK
```

The database and the environments are independent. `otter.db` holds the
watermarks; deleting `environments/` costs a re-preparation, nothing more.

## Environment identity

An environment is identified by a digest over:

- **The declared inputs** — the integration name, the exact Python pin, and the
  contents of `.python-version`, `pyproject.toml` and `uv.lock`.
- **The preparation policy** — the environment recipe version, the uv version,
  the target OS and architecture, the libc variant, and the explicit
  dependency-selection and installation policy.

Two consequences worth knowing:

1. **Changing any input builds a new environment.** The old one is left in
   place, so a run that was already submitted keeps working.
2. **Changing the toolchain builds a new environment too.** A uv upgrade, or
   the recipe version changing between Otter releases, produces a different
   identity and therefore a rebuild on the next preparation. That is
   deliberate: a different toolchain is a different environment, and silently
   reusing the old one would hide it.

Runs record the identity they were submitted against, so a retry after a
dependency or toolchain change still resolves the environment its parent
selected rather than moving to the new one.

## Preparing

```sh
otter release shopify-to-salesforce             # stage, prepare, activate
otter prepare                                   # every managed integration
otter prepare shopify-to-salesforce             # just one
otter prepare --integrations ./integrations --data /var/lib/otter
```

For a local run, `otter release` is the one you want: the daemon executes the
**active release**, so a managed integration that has never been released has
nothing to run. The daemon says so explicitly rather than falling back:

```
managed integration shopify-to-salesforce has no active release;
run otter release shopify-to-salesforce before submitting runs
```

`otter prepare` on its own is still useful: it builds and validates the
environment without creating a release, which is what you want when diagnosing a
dependency problem.

`make sync-release` wraps the release step for the local workflow, so a typical
session is:

```sh
make build
make sync-release      # after changing code or dependencies
make sync-up           # terminal 1
make sync-run          # terminal 2
```

Preparation:

1. Validates the manifest, the Python pin, the project metadata and the lock.
2. Resolves the environment identity and takes an advisory lock on it.
3. Reuses an existing ready environment when the identity matches — this is
   the common case and costs nothing.
4. Otherwise installs the pinned interpreter into `python/`, creates the
   environment, and installs the locked production dependencies.
5. Verifies the interpreter reports the pinned version, then publishes the
   readiness marker.

It is safe to run repeatedly, safe to interrupt, and safe to run while the
daemon is serving. A interrupted preparation leaves no readiness marker; the
next run rebuilds the incomplete directory under the lock. A ready environment
is never overwritten, and completed environments are never renamed, because
installed console scripts contain absolute paths.

Preparation constraints, all deliberate:

- No automatic lock updates. A stale `uv.lock` is an error, not something to
  silently resolve.
- No inherited user-level uv configuration.
- No development dependencies.
- No builds from source: the first release installs wheels only, so a
  dependency without a wheel for the target fails clearly instead of trying to
  compile.
- No integration credentials reach the installer. Package-index credentials are
  configured separately, through the environment.

### uv

Preparation needs `uv`, and Otter does not require a global installation. The
lookup order is:

1. `--uv <path>`, if given.
2. `<data dir>/tools/uv/uv` — a vendored copy.
3. `uv` on `PATH`.

`otter deploy` runs `otter prepare` on the host automatically, before the
daemon restarts, as the service account. A failed preparation stops the deploy
and leaves the previous deployment serving.

## What a managed run gets

Managed children run with a deliberately constructed environment rather than
inheriting the daemon's:

- The prepared environment's interpreter, invoked directly. Otter never runs
  the integration through `uv run`; preparation and execution stay separate so
  the daemon supervises exactly one process.
- `PYTHONHOME`, `PYTHONUSERBASE`, `VIRTUAL_ENV` and the host's `PYTHONPATH`
  removed, and user site-packages disabled.
- The SDK prepended to the import path ahead of everything else.
- The environment's `bin` directory prepended to `PATH`, so tools the
  integration launches resolve to the matching interpreter.
- `NO_PROXY` extended with `127.0.0.1`, `localhost` and `::1`, so the SDK's
  state and log calls to the daemon never go through an outbound proxy.
- Only the declared `env` values and resolved `secrets`, plus a small
  allow-list of host variables: process basics (`HOME`, `LANG`, `TZ`, `TMPDIR`),
  proxy settings, `SSL_CERT_FILE`, and the operational knobs an integration
  reads (`DRY_RUN`, `PAGE_SIZE`, `RUN_BUDGET_SECONDS`, `SYNC_ADDRESS`,
  `OVERLAP_SECONDS`, `MAX_PAGES_PER_RUN`, `SALESFORCE_BATCH_SIZE`,
  `SHOPIFY_SORT_KEY`).

  That allow-list is deliberately an explicit list rather than a prefix match,
  so it stays auditable: the daemon's world does not become the child's, and no
  credential crosses sideways. The cost is that a *new* knob has to be added to
  it, which is why a setting that used to work can silently stop arriving. If a
  managed integration ignores a variable you set, this list is the first thing
  to check -- `otter.yaml`'s `env:` block always works and is the better home
  for anything an integration needs to read.

A run whose environment is missing or does not match its recorded identity
fails with a clear error and **does not fall back to host Python**.

## Diagnosing

Every managed run records its Python mode, the environment digest, the resolved
interpreter version, and the SDK version. They appear in `otter run-status`:

```
python mode:   managed
python:        3.13.5
environment:   4b1f0c9e2a7d3f81...
sdk:           0.1.0
```

and in `otter inspect <integration>`, which reports `managed (prepared
environment)` instead of an executable path.

The first thing to check when a managed integration will not start is whether
its environment is ready for the identity currently on disk:

```sh
otter prepare <integration>     # prints the identity, or prepares it
```

## Guarantees

For a managed integration:

- A run executes the snapshot it was submitted against, not the live tree.
- A retry executes the same snapshot as the attempt it retries.
- A deploy cannot rewrite the code an in-flight attempt is using.
- Every run records the release digest and the environment digest that ran it.
- A failed stage or preparation leaves the active release serving.

## Limits in this release

- **Platforms:** glibc Linux on x86-64 and ARM64. musl/Alpine is identified and
  refused rather than half-supported.
- **Not every patch version is downloadable everywhere.** uv fetches from
  python-build-standalone, and its catalogue differs per platform. Check before
  pinning:

  ```sh
  uv python list --all-versions | grep '<your pin>'
  ```

  `otter prepare` fails with "No download found for request" when the pin is not
  obtainable, which is a clear failure rather than a silent fallback.
- **No system packages.** If a dependency needs a shared library the host does
  not have, preparation fails with the installer's error. Otter does not install
  OS packages.
- **No automatic cleanup.** Old environments and old interpreters are retained.
  Disk usage grows with each distinct identity; GC is deliberately deferred
  until environments are known to be unreferenced by queued or running work.
- **No offline bundle yet.** Preparation fetches the interpreter and wheels. On
  a host with no egress, provision `tools/uv/uv`, `python/` and a populated
  `cache/uv/` out of band, or point uv at an internal mirror. A complete offline
  bundle is planned separately.

## What this does not promise

Managed environments make the Python side reproducible. They do not make
delivery exactly-once. Retries and crash recovery can still run an integration
more than once, so an integration must be idempotent — upsert by an external
ID, as the shipped Shopify-to-Salesforce example does.
