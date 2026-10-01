# Phase 0, P0-01 to P0-08: what we can now stand behind

This is a before-and-after of **guarantees**, not features. A guarantee here means
a claim we can defend with evidence from a real machine, plus an honest statement
of where that claim stops. A guarantee without its limit is marketing, so every
entry below names what it still does **not** cover.

Two conventions:

- **Before** is not "we had nothing". It is what the code and docs let us assert
  at the time — including the cases where we were asserting something untrue.
- **Evidence** points at a recorded transcript, not at a passing test name.

The eight tasks are the *integrity* set: they are about a machine that runs
unattended without losing work, silently diverging, or escaping its box. The
tasks that remain (P0-09 onward) are the *noticing* set — whether anyone finds
out when something goes wrong, and whether the job itself is right. Both matter;
they are different kinds of confidence, and it is worth not confusing them.

## The eight at a glance

| | Guarantee | Before | Now |
| --- | --- | --- | --- |
| P0-01 | A killed daemon loses no accepted run and runs none twice | Crash safety was **simulated**; the only real kill test had no daemon and no database | Real daemon, 50+ runs across the page boundary, `kill -9`, restart, every accepted run accounted for |
| P0-02 | A retry runs the release and environment it was submitted against | Contract stated this as an **explicit non-guarantee** | Promoted to a guarantee, with tests for the release, the environment digest, and a prune in between |
| P0-03 | A backup restores into a working runtime on a clean host | The documented backup copied **`otter.db` only** | Full-fidelity backup and a drill that restores onto a clean second host and runs a job there |
| P0-04 | A runaway job dies instead of taking the host with it | **No caps existed at all** in the generated unit | Caps emitted, defaults corrected twice, proven on the real host: killed in 1.4s, cgroup never swapped |
| P0-05 | The runtime version does not move on its own | Pre-1.0 runtime with no recorded pin and no upgrade procedure | Pin recorded, procedure written, no unattended upgrade path |
| P0-06 | A deploy that fails midway leaves the previous release serving | Untested; the observation period explicitly exercises upgrades | Injected mid-deploy failure leaves the known-good release active |
| P0-07 | The host is provisioned reproducibly and exposes nothing it should not | Provisioning was a stack nobody had booted; the assertion had three wrong expectations | One script from empty VM to running daemon; external scan finds only 22/tcp; permissions asserted, not read |
| P0-08 | Managed Python is prepared before any schedule runs, and the host's egress is real | No offline bundle; preparation fetches at runtime; egress never verified | Egress preflight fails before any fetch; **CPython 3.13.1 prepared on arm64** before schedules were enabled |

## P0-01 — A killed daemon loses no accepted run and runs none twice

**Guarantee.** If `otterd` is `SIGKILL`ed while runs are in flight, then after
restart every accepted run is either terminal or re-enqueued. None is lost, none
executes twice.

**Before.** The only real `SIGKILL` test in the tree killed a stand-in parent
process with no daemon and no database. The two failure modes this is meant to
rule out — `FM-01` and `FM-02` — were marked **Simulated** in the runtime
contract. In other words: we were making a crash-safety claim in a document, and
the thing that backed it did not include the component whose crash mattered. The
listing-page boundary (more than 50 runs) had never been crossed either.

**What enforces it now.** `internal/daemon/crash_harness*_test.go`: boot the real
daemon against a temporary data directory, submit more than 50 runs so the
listing page boundary is crossed, wait until they are genuinely running, `kill -9`
the daemon, restart it, and assert the accounting.

**Evidence.** Recorded run: `internal/daemon` `ok 33.170s`, having accepted **80
runs** against the real daemon. `runtime-contract.md` now carries `FM-01` as
**Real (Linux)** and `FM-02` as **Real (atomicity)** — the retry-binding half of
`FM-02` was still open there as `OT-011`, and closing it is P0-02's job. On macOS
the no-duplicate half remains a documented non-guarantee, because there is no
parent-death signal to hook; so "Real" here means Linux.

**What it does not guarantee.** This is a killed *process* in a test environment,
not a power loss or a corrupt data directory. The exactly-once half is
Linux-only; on macOS the no-duplicate property is a stated non-guarantee rather
than a tested one, and the recorded evidence says so rather than reporting a
pass.

## P0-02 — A retry runs the release and environment it was submitted against

**Guarantee.** A run that fails and is retried executes the release snapshot it
was *submitted* against — not whatever is active when the retry happens — and,
under managed Python, the same prepared environment digest. A pending backlog
keeps its digest even if a release prune runs in between.

**Before.** `planRetry` copied `release_digest` and `release_source_dir`, but no
test referenced `ReleaseSourceDir` at all, and the runtime contract stated the
behaviour as an **explicit non-guarantee**. The failure this allows is quiet and
nasty: a retry silently running different code from the attempt it is retrying,
which makes a run's history a lie about what produced it.

**What enforces it now.** `retry_binding_test.go` plus `recoveryRetryPolicy` in
`recovery.go`: submit against release A, fail, activate B, let the retry run, and
assert it ran **A's** snapshot and A's environment digest — with a prune in the
window to prove the backlog keeps its digest.

**Evidence.** Four tests, six cases, merged in `7b990bc`. The contract's §5.1 is
promoted from non-guarantee to guarantee, and `OT-011` — the retry-binding half
that `FM-02` had left open — is closed.

**What it does not guarantee.** It does not promise that the *same environment*
can be reconstructed if its inputs are gone — the test keeps the backlog's
digest, but a pruned environment cannot be re-synthesised from a digest alone.
That is a P0-03 concern (what a backup must contain) rather than a retry one.

## P0-03 — A backup restores into a working runtime on a clean host

**Guarantee.** A backup taken from a **live, serving** runtime restores onto a
clean second host of the same shape, and the restored instance serves, keeps its
job identity, and runs a job from the restored release and environment.

**Before.** The documented backup copied `otter.db` and nothing else. Releases
live on disk under the data directory; job identity is a `.otter-id` marker
inside the **source** directory. So the documented procedure restored history
whose releases were missing — a restore that looks successful and produces an
instance that cannot run anything. The gap was in the operations document, which
is the worst place for it: it would be followed exactly, by someone who needed it
to work.

**What enforces it now.** Three things, and the third is the one that matters:

1. A written definition of what a complete backup contains — database, release
   snapshots, prepared environments, job sources including `.otter-id`.
2. `operations.md` corrected to match.
3. A drill (`scripts/drill/backup-restore.sh`, clean-host mode) that takes a hot
   backup, transfers it to a genuinely empty second host, restores, starts the
   daemon, and asserts.

**Evidence.** HW-7, **passed twice** on two real hosts
([record](evidence/phase-0/2026-09-30-p0-03-hw7-clean-host.txt)). Restore verified
3986 files against `MANIFEST.sha256` before writing; identity survived by marker
hash; a new run executed from the restored release and environment; and the
cross-host check — the source runtime does not know that run id — is what proves
the run happened on the second machine.

**What it does not guarantee.** The drill states its own limits, and they are
real: **secrets are excluded from the backup by design**, so a restore supplies
no credentials; interpreters were not proven to travel across architectures; and
`otter prepare` rebuilding `python/` or `cache/uv/` from scratch is explicitly not
proven. Also, the drill's new contract test has a spelling hole in one of its own
detectors (recorded as a follow-up) — the shipped fix is sound, the guard is not.

## P0-04 — A runaway job dies instead of taking the host with it

**Guarantee.** A deliberately runaway job is OOM-killed by its own cgroup; the
daemon survives, keeps its PID, does not restart, and the host keeps answering.
The failure stays *inside the unit* rather than becoming a host-wide thrash.

**Before.** The generated unit emitted `User=`, `ExecStart=`, `Restart=always`
and friends, and **nothing else**. There was no `MemoryMax`, no `CPUQuota`, no
`TasksMax`, anywhere in the deploy code. A runaway job could take the whole
machine down, and nothing in the manifest bounded it — the manifest bounds
`timeout` and `concurrency`, which are job-level, not host-level.

This is also the entry where the fix itself was wrong twice, which is the most
useful thing in this document:

- **Attempt 1** added `MemoryMax` and `MemoryHigh`. On the real host the runaway
  was **never killed**: `memory.current` does not count swap, so with the cgroup's
  swap unbounded the kernel reclaimed into the swapfile instead of killing,
  `memory.current` stayed below `memory.max`, the swapfile filled to 100%, and
  `otterd` — sharing the cgroup — stopped answering its API.
- **Attempt 2** added `MemorySwapMax=0` but left `MemoryHigh=60%`. Swap was now
  bounded, but the soft cap throttled and reclaimed rather than killing: the job
  sat pinned just above `memory.high` for ten minutes, holding the only worker,
  until its own 600s manifest timeout ended it — `oom_kill 0` throughout.

**What enforces it now.** `MemoryMax=75%`, `MemorySwapMax=0`, `CPUQuota=200%`,
`TasksMax=512`, and **no `MemoryHigh`** — the defaults are complementary and
neither is sufficient alone.

**Evidence.** HW-3 on the real host
([record](evidence/phase-0/2026-09-30-p0-04-hw3-unit-caps-fixed.txt)): `oom_kill 1`,
`memory.peak` exactly `memory.max`, `swap.current 0`, killed in **1.417 s** against
a 600s timeout, `MainPID` unchanged, `NRestarts 0`, no survivor, `/health` 200.

**What it does not guarantee.** The drill pins the `MemoryHigh` default but still
passes `-memory-swap-max` explicitly, so `DefaultMemorySwapMax` is held by the Go
tests and not by the drill — a regression would be caught in CI, not in this
window. The container kernel used offline differs from the host's, and the caps
are sized for a `t4g.micro`, not for a larger machine.

## P0-05 — The runtime version does not move on its own

**Guarantee.** The host runs a recorded, pinned version, and a deploy does not
change it without an explicit decision. There is a written upgrade procedure.

**Before.** The runtime is pre-1.0 and the contract freeze (`v0.4.0`) has not
shipped. No pin was recorded for the Castor host and no upgrade procedure was
written.

**What enforces it now.** The pin is recorded in the ledger
([Pinned runtime version](phase-0-status.md#pinned-runtime-version)); the upgrade
procedure is in `operations.md` — one runtime at a time, a P0-03 backup first, a
quiet window, a post-upgrade smoke. No code path updates the runtime; nothing
polls for releases.

**Evidence.** The pin and the procedure, plus HW-1 deploying exactly
`v0.3.0-rc1`. A version label comes from the CLI binary's build-time stamp, which
is why deploys rebuild with an explicit `VERSION` — a lesson learned by shipping
a `-dirty` build to the host once.

**What it does not guarantee.** The *upgrade drill itself* has not been run. We
have a procedure and a pin; we have not exercised moving between versions on the
host under load. That belongs with the v0.3.0 work.

## P0-06 — A deploy that fails midway leaves the previous release serving

**Guarantee.** A failure injected partway through a deploy leaves the previous
known-good release active and serving.

**Before.** Untested. This was not on the original blocker list, but the
observation period explicitly exercises upgrades, so a mid-deploy failure that
leaves a broken runtime would be discovered at the worst possible time.

**What enforces it now.** `scripts/drill/deploy-failure.sh` against a
containerised SSH/systemd deploy target: inject the failure, assert the previous
release is still the active one.

**Evidence.** Recorded drill output, merged `30f4fd9`.

**What it does not guarantee.** The deploy target is a container, not the real
host, and the drill requires a **committed** working tree (it asserts
`git status --porcelain` is unchanged), so it cannot be run against a dirty
checkout. It proves the recovery path, not that every possible partial failure is
recoverable.

## P0-07 — The host is provisioned reproducibly and exposes nothing it should not

**Guarantee.** One script takes an empty Linux VM to a running daemon with an
unprivileged service account, a systemd unit, a loopback-or-private API, restricted
SSH, and a filesystem posture you can *assert* rather than read about.

**Before.** Provisioning was a stack nobody had booted. Two things were wrong
with that, and both were found the moment it ran:

- **The machine did not come up correctly, twice.** First boot aborted because an
  `sshd` reload was not tolerated under `set -e` (socket-activated sshd on Ubuntu
  24.04) — which meant no swap and no provisioning report, silently. The fix for
  that introduced the second failure: `sshd -t` needs `/run/sshd`, absent on a
  first boot. Both are the same class of mistake, and neither was visible from
  reading the template.
- **The assertion had three wrong expectations.** Once it ran against a real
  deployed workspace it failed 5 of 26 checks, and three of those failures were
  the *assertion*, not the host: it read `service_name`/`data_dir` when the
  deployer writes `unit` and derives the data directory; it demanded the
  credential env files be service-account-owned when root-owned `0600` is both
  what is implemented and the stronger posture; and it treated `otter.db` at
  `644` as a failure when the `0700` directory is the actual containment.

**What enforces it now.** `scripts/provision.sh` and
`scripts/assert-host-permissions.sh`, with two fixture suites, plus a rebuilt
host, plus the `otter-platform` stack fixes (swap before any risky step, a
tolerated sshd reload, `/run/sshd` created before validation).

**Evidence.** HW-8 ([record](evidence/phase-0/2026-09-30-p0-07-hw8-assertion.txt)):
an external full-range scan finds **only 22/tcp**, and the permission assertion
exits 0 on the real host having read the sandbox directives back from the
**loaded** unit, the real `workspace.json`, the real file modes and the real
listener set. The host reaches `cloud-init: done` with no errors.

**What it does not guarantee.** One of the script's 33 failure statements has no
test, and removing it makes a security-relevant assertion — the loopback bind
check — silently disappear while all 48 cases stay green. Eleven of
`provision.sh`'s 27 `die` paths (probe and transport failures) are likewise
unpinned. The assert harness gives a **false red** on a test machine that has a
real `ss` on `PATH`. And CA-10's external `nmap` scan is a separate step: this
script cannot perform it, so its failure message naming CA-09/CA-10 over-names.

## P0-08 — Managed Python is prepared before any schedule runs

**Guarantee.** The host can reach what it needs to prepare an environment, and
preparation happens **during setup, before any schedule is enabled** — never at
the first scheduled run.

**Before.** Managed Python had no offline bundle: preparation fetched the
interpreter and wheels at deploy time, and not every patch version is
downloadable everywhere. Egress was never verified on the host. The failure this
allows is the worst-timed one: the first scheduled run of the night discovers the
host cannot reach PyPI.

**What enforces it now.** A prepare-time egress preflight
(`internal/pyenv/egress.go`) that fails **before any fetch**, with a hint; the
`--index`, `--python-mirror`, `--egress-endpoint` and `--skip-egress-check`
controls for hosts that need them; and preparation run as part of setup.

**Evidence.** HW-2 on the real host: **CPython 3.13.1 prepared for both jobs on
arm64** — which is the concrete answer to the documented worry about
architecture coverage.

**What it does not guarantee.** Three deliberately conservative refusals ship with
this branch: an unrecorded additive index source is refused even where uv would
accept it, and a Unicode IDNA host is refused. Both have a remedy that terminates
(omit the variable), and both are labelled as deliberate in a test table so a
future improvement is an edit rather than a surprise. Two negative tests that
would have caught adjacent regressions were lost in a refactor and have not been
replaced. And the preflight proves reachability *at preparation time* — a host
whose egress changes later is not re-checked.

## The through-line

Three of these eight found that the guarantee we thought we had was false:

- **P0-03**: the documented backup restored history without releases.
- **P0-04**: there were no resource caps at all, and the first fix was wrong on
  the real host in a way that made things worse — swap filled and the daemon
  starved.
- **P0-07**: the provisioning stack aborted on first boot, twice, and the
  assertion that was supposed to verify the host had three wrong expectations.

In every one of those cases, reading the code and the docs would have told us we
were fine. The value came from running the thing on a real machine, which is why
the evidence for this half is transcripts from real hosts rather than green test
names. The single most expensive lesson: three independent verification rounds on
P0-03 never reached past its own preflight gate, while the first real execution
reached it in 19 seconds and found a one-word bug.

## What these eight do **not** buy

They are integrity guarantees: no lost work, no double execution, no silent
divergence from what a run claims, no resource escape, no unrecoverable deploy,
no unaccounted host, no surprise network. They say nothing about whether anyone
**finds out** when something goes wrong, or whether the job is **correct**:

| Still not guaranteed | Task |
| --- | --- |
| That a failed run reaches a human | P0-09 |
| That a dead daemon is noticed at all | P0-10 |
| That disk exhaustion is caught before it bites | P0-11 |
| That Castor's real credential **values** are deployed, scoped and rotatable | P0-12 (mechanism merged) + HW-5 (values) |
| That the actual job does the right thing | P0-13 |
| That a shadow run matches the Lambda, or that cutover is safe | P0-14, P0-15 |
| That any of it holds for 30 days | P0-16 to P0-18 |

A machine that keeps every promise above and tells nobody when it fails is still
a machine you cannot leave alone. That is what the second half is for.
