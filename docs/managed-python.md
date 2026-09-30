# Managed Python Environments

Otter can own the Python interpreter and the dependencies a job runs
with, instead of depending on whatever `python3` a host happens to have. This
is opt-in, per job, and additive: a job that does not ask for
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
| Isolation | Shared with the host | Private to the job |
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

A lock file alone never changes how a job runs. Opting in is explicit,
so adding `pyproject.toml` to an existing job cannot silently move it
onto a different interpreter.

## Required files

```
my-job/
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
  the release at the depth it has relative to the **release base** — the
  closest common ancestor of the jobs discovery root, the
  job and every captured tree. A path like `../../lib/python` from
  `jobs/<name>` therefore resolves inside the snapshot exactly as it does
  in the checkout. Shared code that can be published as a package is better
  declared in `pyproject.toml`, because then it is locked and versioned with
  everything else.

  Three shared-tree rules are enforced at release time, before a snapshot is
  written:

  - **A declared tree that is missing is an error.** It is never silently
    skipped: a release that dropped it would import the live copy here and fail
    on the host.
  - **An absolute `python.path` is unsupported and refused.** It exists on the
    machine that releases and is a missing directory everywhere else, which is
    exactly the failure that is impossible to see locally. A tree that shares no
    ancestor below the filesystem root with the job has no relative
    placement either, and is refused for the same reason.
  - **Symlinks are preserved only when they resolve inside a captured tree.**
    An internal link keeps working because every captured path is placed
    relative to the same base. A link that resolves anywhere else — including an
    absolute target — is refused: locally it would import live code the snapshot
    pretends to contain, and on the host it would dangle.

## Releases

Every job executes an **immutable release** rather than its source
tree: a snapshot of the job plus the shared code it declares, copied
under the data directory and addressed by a digest of its contents. A release
is required before anything runs, for external and managed Python alike, and an
edit is not live until the job is released again.

```sh
otter release                        # the job in the working directory
otter release <job>          # by name, from anywhere in the workspace
otter release --all                  # every job in the workspace
otter release --list <job>   # what is staged, and which one is active
```

`otter deploy` runs this for every job automatically, before the daemon
restarts.

What managed Python adds to a release is the environment: a pinned interpreter
and a locked dependency set, prepared before activation. An external
job's release pins the code and nothing else -- it still runs the
interpreter its manifest names. The rest of this document is about that managed
half.

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
<data dir>/.releases/<job>/<release-digest>/
├── jobs/<job>/   the snapshot, discovered as usual
├── lib/                          shared code at the same relative depth
└── otter-release.json            placement, digest, environment, source, timestamp
```

The job and every shared tree land relative to **one base**: the
closest common ancestor of the jobs discovery root, the
job directory and every captured tree. The release root plays the part
of that base, so every relative path resolves the same way before and after
activation and nothing is rewritten to release a job.

One rule covers every workspace shape:

| Workspace | Job | Shared tree |
| --- | --- | --- |
| `<root>/jobs/<name>` + `<root>/lib/python` | `jobs/<name>` | `lib/python` |
| `<root>/<name>` + `<root>/lib/python` | `<name>` | `lib/python` |
| `<root>/group/<name>` + `<root>/group/lib/python` | `group/<name>` | `group/lib/python` |
| Deploy: `/opt/otter/jobs/<name>` + `/opt/otter/lib/python` | `jobs/<name>` | `lib/python` |

Deploy is the case that fixes the upper bound of the base. It releases with
`--jobs /opt/otter/jobs` while shared code lives at
`/opt/otter/lib/python`, so the base is `/opt/otter`, not the discovery root —
with the discovery root as the base there would be no way to spell
`../lib/python` inside the release.

The recorded placement is part of the release digest, together with each
shared tree's destination name. Moving `lib` to `vendor`, or a job from
`jobs/<name>` to `<name>`, changes every relative import the job
performs, so it produces a different release rather than reusing one.

Releases staged before the placement was recorded default to
`jobs/<name>`, so a snapshot already on disk keeps working after an
upgrade.

The activation link lives at `<data dir>/.releases/active/<job>`,
outside the jobs tree, so `otter deploy` can keep syncing sources
normally without touching what is currently being served.

### Ordering

Release is stage → validate → prepare → activate, and each step fails safely:

- **Staging** copies into a temporary directory and renames it into place, so a
  release directory either exists complete or not at all.
- **Validation** loads the snapshot's *own* manifest and resolves its
  `python.path` entries against the snapshot. This is the check that tells "the
  path exists on the machine that released" apart from "the release captured the
  tree the path names"; an absolute path passes the first and fails here.
- **Preparation** runs against the staged snapshot, not the live tree, and the
  snapshot manifest — not the live one — decides whether it runs at all. What is
  validated is what will run.
- **Activation** is a single atomic `rename` of a symlink, and it re-validates
  the snapshot and, for a managed release, that its environment is ready. A
  failure in any step leaves the previous release active.

Redeploying unchanged inputs stages nothing new: identical inputs produce an
identical digest, so an existing release is reused.

### Upgrading

Every job must be released once after upgrading Otter. The digest format
is versioned, and a new implementation deliberately does not reuse a snapshot
laid out by an older one, even when the inputs look identical. A deploy prunes
inactive snapshots to `--keep` (default 3), but the previous release is the
newest inactive one, so a rollback to a pre-upgrade release still works.
`otter deploy` performs the re-release for every job as part of the deploy.

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

A run binds to the active release **when it is submitted**, and the recorded
snapshot is what execution uses. Activating a newer release therefore cannot
move a queued or retried attempt onto different code, and a run keeps working
while a deploy is in progress. An attempt whose snapshot has been removed fails
with an explicit error instead of silently running something else.

That is true of every job. Managed mode binds one thing more: the
interpreter and the dependency set, recorded on the run at submission, so a
later dependency change cannot move a queued or retried attempt onto a different
environment either.

The execution settings come from the **bound release's** manifest, never from the
live one. Editing `python.mode` in the source tree does not change how an
already-staged release runs, and does not affect a rollback to it: the snapshot
is what executes, so the snapshot is what describes it.

### Retention

`otter release --keep N` removes inactive releases beyond `N`, never touching
the active release, never touching one referenced by queued, running or
retrying work, and always keeping one release to roll back to. A prune refuses
to run when the run registry cannot be read, so an unknown pin set never means
an empty one. `otter release` keeps every release unless you ask for `--keep`;
`otter deploy` prunes with a default of `--keep 3`, because a deploy is where
convergence is expected. `otter release --list` shows what is consuming disk.

### Rollback

Activate an older release by digest prefix, which is the way `--list` prints
them:

```sh
otter release --list my-job                   # find the digest
otter release --activate 3c850cfa6c9c my-job  # point the active link at it
```

Rolling back means pointing the active link at a previous digest. The previous
release is still on disk unless retention removed it, and the runs that used it
are still recorded. Running attempts are unaffected: they keep executing the
snapshot they bound to.

Activation checks the target before it switches: the release's snapshot manifest
must still resolve, and a managed release's environment must still be ready. A
rollback to a broken or unprepared snapshot is refused with the command that
fixes it (`otter prepare`), and the release that is currently active keeps
serving.

## Layout on disk

Everything derived lives under the data directory and can be deleted without
losing job state:

```
<data dir>/
├── otter.db                          job state, runs, logs
├── otter.lock                        exclusive ownership of this data directory
├── tools/uv/uv                       vendored uv, used only for preparation
├── python/<version>/                 shared interpreter installations
├── environments/<digest>/            one prepared environment per identity
│   ├── bin/python                    the interpreter runs use
│   └── otter-ready.json              readiness marker; absence means "not ready"
├── cache/uv/                         uv's download cache
├── .releases/                        immutable source snapshots
│   ├── <job>/<digest>/       mirrored checkout: jobs/ + lib/
│   └── active/<job>          symlink to the release being served
└── sdk/python/                       the embedded Otter SDK
```

The database and the environments are independent. `otter.db` holds the
watermarks; deleting `environments/` costs a re-preparation, nothing more.

## Environment identity

An environment is identified by a digest over:

- **The declared inputs** — the job's durable identity (not its label:
  environments are keyed by identity, so renaming a manifest reuses the
  environment it already built), the exact Python pin, and the contents of
  `.python-version`, `pyproject.toml` and `uv.lock`.
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

The **fetch route** — `--index` and `--python-mirror` — is deliberately not part
of the identity. It chooses how the same pinned artifacts are reached, not what
they are: the lock pins every dependency by hash, and uv verifies the
interpreter download against the hash in the catalogue it carries. Folding the
route in would also break the two places that re-resolve an identity without
ever being told it — the daemon binds a run at submission, and
`otter release --activate` re-resolves on a rollback — so a release prepared
through a mirror would come back as "not prepared" the moment either of them
looked at it.

Environments are content-addressed and may be shared by several jobs, so
`otter delete` never removes them: it purges only what the identity exclusively
owns. Reclaiming environment disk is a separate, deliberate operation.

## Preparing

```sh
otter release shopify-to-salesforce             # stage, prepare, activate
otter prepare                                   # every managed job
otter prepare shopify-to-salesforce             # just one
otter prepare --jobs ./jobs --data /var/lib/otter
```

For a local run, `otter release` is the one you want: the daemon executes the
**active release**, so a job that has never been released has nothing
to run -- managed or not. The daemon says so explicitly rather than falling
back:

```
job shopify-to-salesforce has no active release;
run otter release shopify-to-salesforce before submitting runs
```

`otter prepare` on its own is still useful for a managed job: it builds
and validates the environment without staging a release, which is what you want
when diagnosing a dependency problem. It says so and does nothing for an
external one, which has no environment to prepare.

A typical session, using an installed Otter in your own project:

```sh
otter release <job>   # after changing code or dependencies
otter start --detach          # the runtime
otter run <job>       # queue a run and follow it
otter stop
```

Preparation:

1. Validates the manifest, the Python pin, the project metadata and the lock.
2. Resolves the environment identity and takes an advisory lock on it.
3. Reuses an existing ready environment when the identity matches — this is
   the common case and costs nothing. A reused environment is never checked
   against the network: it is already built.
4. Otherwise checks that the endpoints it needs are reachable (below), then
   installs the pinned interpreter into `python/`, creates the environment, and
   installs the locked production dependencies.
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
- No inherited user-level uv configuration. The index and the interpreter
  source are passed to uv as explicit flags instead (see
  [Reaching the endpoints](#reaching-the-endpoints-mirrors-and-the-egress-preflight)),
  which is also why a mirror does not need a config file.
- No development dependencies.
- No builds from source: the first release installs wheels only, so a
  dependency without a wheel for the target fails clearly instead of trying to
  compile.
- No job credentials reach the installer. Package-index credentials are
  configured separately, through the environment.

### Reaching the endpoints: mirrors and the egress preflight

Preparation downloads, so it needs a route to two places: the package index the
locked dependencies come from, and python-build-standalone, which is where the
pinned interpreter comes from. Neither has an offline bundle (see the limits
below), and on a host with no route the failure arrives in the middle of a
fetch, wearing whatever disguise that fetch was under — a DNS error, a TLS
timeout, or uv's `No download found for request` when the *catalogue* has no
build for the pin and the platform.

`otter prepare` and `otter release` therefore ask the network question first,
before anything is fetched:

```
$ otter prepare --jobs ./jobs --data /var/lib/otter
otter: egress preflight ok for environment 4b1f0c9e2a7d: package index https://pypi.org/simple/, managed Python downloads https://github.com/indygreg/python-build-standalone/releases/download
my-job: Python 3.13.1, environment 4b1f0c9e2a7d
```

The check is on the **route**, not on the download. An endpoint that answers at
all has been reached, so the 404 that the root of a download path returns and
the 401 a private mirror returns before it is authenticated both count, while a
DNS failure, a refused connection, or a timeout does not. Proxy and TLS settings
(`HTTPS_PROXY`, `NO_PROXY`, `SSL_CERT_FILE`) are honoured, because uv honours
them.

A failure names the endpoint, the pin, and the platform, and says what to do
about each:

```
otter: prepare my-job: egress preflight failed for CPython 3.13.1 on linux/arm64:
managed Python downloads is unreachable (https://github.com/.../releases/download):
dial tcp: lookup github.com: no such host
hint: preparation fetches the managed interpreter and the locked dependencies, and has
no offline bundle. Check the host's route and proxy (HTTPS_PROXY and NO_PROXY are
honoured), point preparation at an internal mirror with --index and --python-mirror,
provision python/ and cache/uv/ out of band, or run with --skip-egress-check when the
host is deliberately air-gapped and already primed.
```

That separation is the point of the check. When the preflight passes and the
interpreter install then fails with `No download found for request`, the network
has been ruled out and what is left is the platform/catalogue problem: not every
patch version is published for every architecture, uv decides that from the
catalogue it carries, and the fix is a pin that is published or a host of an
architecture that is. Otter says so in the error, and names the platform and what
to compare against:

```
hint: the egress preflight passed, so this is not a network failure:
python-build-standalone has no CPython 3.13.1 build for linux/arm64. Not every patch
version is published for every platform; check the catalogue with
`uv python list --all-versions`, or prepare on a host whose architecture publishes
this pin (linux/amd64 is the fallback when arm64 does not).
```

#### Pointing preparation at a mirror

```sh
otter prepare --index https://mirror.internal/simple \
              --python-mirror https://mirror.internal/python-build-standalone
```

- `--index` replaces the default package index (uv's `--default-index`) rather
  than adding to it, so a host with no route to PyPI is never asked to reach it.
- `--python-mirror` is the **root** the interpreter archives live under: uv
  appends the release tag and the file name to it exactly as it does to its own
  default, so the value is a mirror of
  `https://github.com/indygreg/python-build-standalone/releases/download`.
- `--egress-endpoint <url>`, repeatable, adds an endpoint the job itself needs —
  its API, say — to the check. Otter cannot discover those, so they are
  configured; they are checked, never fetched from.
- `otter release` accepts the same flags, and `otter deploy` passes them to the
  release it runs on the host, which is how a host that reaches its
  dependencies through a mirror is deployed without any uv configuration file.

They are per-invocation flags and not uv configuration files. Package-index
*credentials* still come from the environment, as they always have.

The check itself follows what uv will use, not only what was passed on the
command line: with neither flag set, `UV_DEFAULT_INDEX` (or its deprecated
`UV_INDEX_URL`) and `UV_PYTHON_INSTALL_MIRROR` are honoured, because uv reads
them too. An endpoint set only in the environment is therefore checked, and is
not mistaken for "no route to PyPI". The additional-source variables
(`UV_INDEX`, `UV_EXTRA_INDEX_URL`, `UV_FIND_LINKS`) are not followed: they add
sources to the default index rather than replacing it, so the default index is
still what answers whether the host can reach an index at all.

`--skip-egress-check` turns the check off, for the one case that needs it: a
host that is deliberately air-gapped but already primed — the interpreter is
under `python/` and `cache/uv/` is warm — where a probe would refuse a
preparation that would otherwise have succeeded from what is already on disk.
Nothing else should skip it: a host that can reach its endpoints gains nothing
by not being checked, and the check costs one request per endpoint per
environment that actually has to be built.

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
  the job through `uv run`; preparation and execution stay separate so
  the daemon supervises exactly one process.
- `PYTHONHOME`, `PYTHONUSERBASE`, `VIRTUAL_ENV` and the host's `PYTHONPATH`
  removed, and user site-packages disabled.
- The SDK prepended to the import path ahead of everything else.
- The environment's `bin` directory prepended to `PATH`, so tools the
  job launches resolve to the matching interpreter.
- `NO_PROXY` extended with `127.0.0.1`, `localhost` and `::1`, so the SDK's
  state and log calls to the daemon never go through an outbound proxy.
- Only the declared `env` values and resolved `secrets`, plus a small
  allow-list of host variables: process basics (`HOME`, `LANG`, `TZ`, `TMPDIR`),
  proxy settings, `SSL_CERT_FILE`, and the operational knobs a job
  reads (`DRY_RUN`, `PAGE_SIZE`, `RUN_BUDGET_SECONDS`, `SYNC_ADDRESS`,
  `OVERLAP_SECONDS`, `MAX_PAGES_PER_RUN`, `SALESFORCE_BATCH_SIZE`,
  `SHOPIFY_SORT_KEY`).

  That allow-list is deliberately an explicit list rather than a prefix match,
  so it stays auditable: the daemon's world does not become the child's, and no
  credential crosses sideways. The cost is that a *new* knob has to be added to
  it, which is why a setting that used to work can silently stop arriving. If a
  managed job ignores a variable you set, this list is the first thing
  to check -- `otter.yaml`'s `env:` block always works and is the better home
  for anything a job needs to read.

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

and in `otter inspect <job>`, which reports `managed (prepared
environment)` instead of an executable path.

The first thing to check when a managed job will not start is whether
its environment is ready for the identity currently on disk:

```sh
otter prepare <job>     # prints the identity, or prepares it
```

## Guarantees

For every job:

- A run executes the snapshot it was submitted against, not the live tree.
- A retry executes the same snapshot as the attempt it retries.
- A deploy cannot rewrite the code an in-flight attempt is using.
- Every run records the release digest that ran it.
- A run's Python mode is taken from the release it bound to, not from the live
  manifest, so editing the source cannot change a released run's settings.
- A failed stage, a failed validation, a failed preparation or a failed
  activation leaves the active release serving.

Additionally, for a managed job:

- Every run also records the environment digest it ran on.
- A failed preparation leaves the active release serving.

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
  obtainable, which is a clear failure rather than a silent fallback. Because
  the egress preflight has already answered the network question by then, that
  error is also *diagnosed*: it names the pin and the target platform and says
  that no endpoint was unreachable
  ([Reaching the endpoints](#reaching-the-endpoints-mirrors-and-the-egress-preflight)).
- **No system packages.** If a dependency needs a shared library the host does
  not have, preparation fails with the installer's error. Otter does not install
  OS packages.
- **No automatic cleanup.** Old environments and old interpreters are retained.
  Disk usage grows with each distinct identity; GC is deliberately deferred
  until environments are known to be unreferenced by queued or running work.
- **No offline bundle yet.** Preparation fetches the interpreter and wheels. On
  a host with no egress, provision `tools/uv/uv`, `python/` and a populated
  `cache/uv/` out of band, or point preparation at an internal mirror with
  `--index` and `--python-mirror`. A complete offline bundle is planned
  separately.

## What this does not promise

Managed environments make the Python side reproducible. They do not make
delivery exactly-once. Retries and crash recovery can still run a job
more than once, so a job must be idempotent — upsert by an external
ID, as the shipped Shopify-to-Salesforce example does.
