# Phase 0 task list — Castor dogfood

Status: proposed. Date: 2026-09-29. Derived from
[cloud-alpha-readiness.md](cloud-alpha-readiness.md) §17 Phase 0.

**Goal:** prove that one real Castor job can run safely and unattended on Otter
without Cloud.

**Standing rule (R-19):** every task below is closed by executable or recorded
evidence — a test, or a dated recorded run. A task is not done because a document
says the behavior exists.

**Sizes are first-pass guesses** in focused engineer-days, to be replaced with real
numbers after the first pass (`R-23`). S ≈ 1–2 d, M ≈ 3–5 d, L ≈ 1–2 wk.
P0-14 and P0-16 are elapsed time, not effort.

## Summary

| ID | Task | Kind | Closes | Size | Depends on |
| --- | --- | --- | --- | --- | --- |
| P0-01 | Real crash/kill harness | Blocker | `CA-20`, R-01 | L | — |
| P0-02 | Retry release/environment binding | Blocker | `CA-25`, R-09 | M | — |
| P0-03 | Full-fidelity backup and restore | Blocker | `CA-21`, R-08 | M | — |
| P0-04 | Resource caps in the generated unit | Blocker | `CA-08`, R-10 | S | — |
| P0-05 | Pin the version; no auto-upgrade | Blocker | `CA-22`, R-12 | S | — |
| P0-06 | Deployment-failure recovery | Blocker | `CA-23` | M | P0-03 |
| P0-07 | Provision the Castor host | Prereq | `CA-06`, `CA-09`, `CA-10` | M | — |
| P0-08 | Egress and managed Python | Prereq | `CA-17`, `CA-18` | S | P0-07 |
| P0-09 | Failure notification | Blocker | `CA-30`, R-05 | S | P0-07 |
| P0-10 | Independent liveness detection | Blocker | `CA-31`, R-05 | M | P0-07 |
| P0-11 | Disk retention and thresholds | Blocker | `CA-19`, `CA-33`, R-17 | M | P0-07 |
| P0-12 | Scope Castor's credentials | Prereq | `CA-12` | S | P0-07 |
| P0-13 | Select and harden the job | Blocker | `CA-02`, `CA-40`–`CA-46` | L | — |
| P0-14 | Shadow run beside the Lambda | Blocker | R-02, R-18 | elapsed | P0-13 |
| P0-15 | Cutover | Milestone | — | S | all blockers |
| P0-16 | Operate for 30+ days | Observe | — | elapsed | P0-15 |
| P0-17 | Write the runbook | Observe | `CA-51` | M | P0-16 |
| P0-18 | Produce the ranked gap list | Observe | R-23 | S | P0-16 |

**Critical path:** P0-07 → P0-08 → P0-13 → P0-14 → P0-15 → P0-16 → P0-18. The
runtime-correctness blockers (P0-01…P0-06) run in parallel and must land before
P0-15.

**Rough total.** The guesses above sum to roughly **8–11 weeks of focused work**,
plus 4–6 weeks of elapsed observation. That is higher than the 4–8 week top-down
figure given verbally earlier — the expected direction once work is decomposed.
Treat the decomposed number as the better one, and replace both once P0-01 and
P0-13 are understood; those two dominate the total and carry the most uncertainty.

---

## A. Runtime correctness

These are the tasks that decide whether Otter is what the contract says it is. They
are the reason Phase 0 exists.

### P0-01 — Real crash/kill harness — `CA-20`

**What.** The only real `SIGKILL` test today is in `internal/executor/proc_linux_test.go`,
and it kills a stand-in parent with no daemon and no database
([runtime-contract.md](runtime-contract.md) line 411). `FM-01` and `FM-02` are
marked **Simulated**.

**Deliverable.** A harness that:

1. boots the real daemon against a temporary data directory;
2. submits more than 50 runs, to cross the listing-page boundary the simulated test
   uses;
3. waits until they are genuinely running;
4. `kill -9`s the daemon process;
5. restarts it;
6. asserts every accepted run is either terminal or re-enqueued — none lost, none
   executed twice;
7. asserts startup refuses when recovery cannot complete (already covered by
   `TestStartupFailClosedOnIncompleteRecovery`).

**Evidence.** The harness runs in CI and its output is recorded. `runtime-contract.md`
Appendix A moves `FM-01`/`FM-02` from **Simulated** to **Real**.

**Note.** This is the largest single item and the one most likely to find a real
defect. Budget for fixing what it finds, not just writing it.

### P0-02 — Retry release and environment binding — `CA-25`

**What.** `planRetry` (`internal/daemon/workers.go`) copies `release_digest` and
`release_source_dir` and resolves the parent's environment, but no test references
`ReleaseSourceDir` at all, and the contract states this as an explicit
non-guarantee ([runtime-contract.md](runtime-contract.md) lines 296–312).

**Deliverable.**

- Submit a run bound to release A; let it fail.
- Activate release B.
- Let the retry execute.
- Assert the retry ran **A's** snapshot, and — under managed Python — A's
  environment digest.
- A pending backlog keeps its digest through a release prune.

**Evidence.** The test passes; `runtime-contract.md` §5.1 is promoted from
non-guarantee to guarantee.

### P0-03 — Full-fidelity backup and restore — `CA-21`

**What.** The documented backup copies `otter.db` only
([operations.md](operations.md) lines 399–441). Releases live on disk under the
data directory, and job identity is a `.otter-id` marker inside the **source**
directory ([security.md](security.md) lines 468–479). A database-only backup
restores history whose releases are missing.

**Deliverable.**

1. Define exactly what a complete backup contains: database, release snapshots,
   prepared Python environments (or the recipe that reconstructs them), job source
   directories including `.otter-id`.
2. Correct the documented procedure in `operations.md`.
3. Drill: take a backup from a live Castor-like runtime, restore onto a **clean
   host**, and assert run history, durable state, releases, identity, and a
   *runnable* job all survive.

**Evidence.** A drill script plus its recorded output, against a real host — not a
synthetic CI workspace.

### P0-04 — Resource caps in the generated unit — `CA-08`

**What.** `UnitFile` ([render.go](../internal/deploy/render.go) lines 27–63) emits
`User=`, `ExecStart=`, `Restart=always` and friends, and nothing else. There is no
`MemoryMax`, `CPUQuota` or `TasksMax` anywhere in the deploy code, and the manifest
bounds only `timeout` and `concurrency`
([manifest-reference.md](manifest-reference.md) lines 84, 237).

**Deliverable.** Emit host-level caps in the generated unit, sized for the Castor
workload, and document how to size them.

**Evidence.** A deliberately runaway Python job is killed; `otterd` survives and the
host stays responsive.

### P0-05 — Pin the version; no auto-upgrade — `CA-22`

**What.** The runtime is pre-1.0 and the contract freeze (`v0.4.0`) has not shipped.

**Deliverable.**

- Record the pinned runtime version for the Castor host.
- A written upgrade procedure: one runtime at a time, P0-03 backup first, quiet
  window, post-upgrade smoke.
- Confirm no unattended upgrade path exists.

**Evidence.** The procedure is written and the pin is recorded; a deploy does not
move the version without an explicit decision.

### P0-06 — Deployment-failure recovery — `CA-23`

**What.** Not on the original blocker list, but the observation period explicitly
exercises upgrades, so a mid-deploy failure must not leave the runtime broken.

**Deliverable.** Inject a failure midway through a deploy; the previous known-good
release remains active and serving.

**Evidence.** Recorded drill output.

---

## B. Host

### P0-07 — Provision the Castor host — `CA-06`, `CA-09`, `CA-10`

**What.** One VM, `otterd`, loopback API, SSH tunnel — the architecture in
[security.md](security.md).

**Deliverable.** One script from an empty Linux VM to a running daemon:

- unprivileged `otter` service account with a `nologin` shell (`otter deploy`
  already does this — [render.go](../internal/deploy/render.go) line 137);
- data directory `0700` owned by the service account; environment file `0600`
  **root**-owned, inside a `0700` root-owned directory. The daemon reads it
  through systemd's `EnvironmentFile=`, which runs as root before dropping to
  `User=otter`, so the service account — and therefore any job it runs — cannot
  read or rewrite its own credentials. *(Corrected 2026-09-30: this line said the
  file was owned by the service account. That is the weaker posture, it is not
  what `otter deploy` does, and an independent verifier confirmed the
  implementation in P0-12 — root-owned `0600` under `umask 077` in a `0700`
  directory. The permission assertion that encoded the old wording was
  corrected rather than the host.)*
- systemd unit installed and enabled;
- **no public API ingress.** `otter deploy` already refuses a wildcard bind
  ([target.go](../internal/deploy/target.go) lines 415–422); verify it on the real
  host;
- restricted SSH.

**Evidence.** An external port scan shows only the explicitly approved ports; a
permission assertion test passes, rather than the docs being read.

**Optional but cheap:** add the `security.md` hardening directives
(`NoNewPrivileges`, `ProtectSystem`, `ProtectHome`, `PrivateTmp`, `ReadWritePaths`)
— see "Deliberately not in Phase 0" below.

### P0-08 — Egress and managed Python — `CA-17`, `CA-18`

**What.** Managed Python has **no offline bundle**: preparation fetches the
interpreter and wheels ([managed-python.md](managed-python.md) line 482), and not
every patch version is downloadable everywhere (line 466).

**Deliverable.** Confirm outbound egress to PyPI / `python-build-standalone` and to
Castor's sources, and run `otter prepare` during setup — never at the first
scheduled run. Prime `cache/uv/` (`CA-19`).

**Evidence.** `otter prepare` succeeds on the host for the pinned interpreter before
any schedule is enabled.

---

## C. Observability

### P0-09 — Failure notification — `CA-30`

**What.** `OTTER_NOTIFY_URL` supports JSON, Slack, Discord and Teams. Only failures
notify ([deploy.md](deploy.md) line 283).

**Deliverable.** Configure a channel a human actually reads.

**Evidence.** A deliberately failing run posts to that channel.

### P0-10 — Independent liveness detection — `CA-31`

**What.** Delivery is "deliberately best-effort" with a bounded in-process retry and
no durable outbox ([notify.go](../internal/notify/notify.go) line 9). If the daemon
is down, it cannot report its own death. Phase 0 has **no control plane**, so this
must be external to the runtime.

**Deliverable.** A minute-level external healthcheck against `/health` (cron,
uptime service, or systemd watchdog) that alerts when the check stops — a
dead-man's-switch rather than a positive check.

**Evidence.** Kill the daemon and confirm an alert arrives without anyone watching a
terminal.

### P0-11 — Disk retention and thresholds — `CA-19`, `CA-33`

**What.** Four things grow: `otter.db`, release snapshots, prepared environments and
`cache/uv/`, and capture rows. Retention for runs and logs landed after `v0.2.0`, in
the `v0.3.0` line of work, but it is **opt-in**
([daemon.go](../internal/config/daemon.go) line 40): `--capture-retention`,
`--log-retention`, `--run-retention`. Releases prune on deploy with a keep window;
environment GC is "deliberately deferred"
([managed-python.md](managed-python.md) line 480).

**Deliverable.** Enable all three retention windows for Castor — they default to
retain-forever — and set alert thresholds covering all four stores. Include a
disk-fill drill: the failure mode is not "slow", it is "the daemon cannot write".

**Evidence.** The alert fires at a test threshold; retention demonstrably prunes.

---

## D. Secrets

### P0-12 — Scope Castor's credentials — `CA-12`

**What.** Manifests declare secret **names**; values are read from the daemon
environment ([manifest-reference.md](manifest-reference.md) lines 272–283). A
missing secret fails the run **before** Python starts, and is not retried.

**Deliverable.** Castor's credentials in the `0600` environment file; the manifest
declares only the names it needs.

**Evidence.** The manifest contains no values; a run with a missing secret fails
cleanly and repeatedly.

---

## E. The Castor job

### P0-13 — Select and harden the job — `CA-02`, `CA-40`–`CA-46`

**What.** The job is where a duplicate write becomes a real business problem, and no
runtime guarantee prevents it ([runtime-contract.md](runtime-contract.md) §5.2).

**Deliverable.**

- Pick an import that is **scheduled** (not event-driven, so cron and backlog
  behavior get exercised), **watermark- or checkpoint-shaped** (so `ctx.state` is
  used for real), **upsert-shaped or idempotent** at the destination, and
  **verifiable from outside Otter**.
- Document input, output, business effect, replay behavior, and owner.
- Apply: upsert by a stable external id; idempotency key from business input, not
  the run id; `concurrency: 1` around `ctx.state` checkpoints; bounded work units
  (one page, not one sync); reconcile ambiguous remote writes before replaying;
  bound concurrency, retries and backoff; verify the outcome by querying the
  destination.

**Evidence.** A destination-side assertion — the record appears exactly once — not a
run status.

---

## F. Cutover

### P0-14 — Shadow run beside the Lambda

**What.** The first cutover should not also be the first test.

**Deliverable.** Run the Otter job alongside the existing Lambda long enough to
compare results, and diff the destinations.

**Evidence.** A recorded comparison, with every difference explained.

### P0-15 — Cutover

**Deliverable.** Disable the Lambda schedule; enable Otter's.

**Evidence.** The workload runs on Otter and no longer depends on
Lambda/EventBridge for its schedule. Every blocker above has its recorded evidence.

---

## G. Observation — after cutover

Not gates. This is the experiment.

### P0-16 — Operate for 30+ days

30+ days unattended with no runtime-caused incident. Exercise upgrades and restarts
in normal operation. Collect storage-growth data and observed operational burden.

### P0-17 — Write the runbook — `CA-51`

Pause jobs, inspect failures, read logs and traces, retry work, roll back releases,
rotate credentials, restore the runtime. Written while operating, so it reflects
what actually happens.

### P0-18 — Produce the ranked gap list

A written, ranked list of the operational gaps a paying client would have hit. **This
is Phase 0's most important output** — it is the control plane's requirements
document, and it is what Phase B is designed from.

---

## Deliberately not in Phase 0

Recorded so they do not leak in. None of these can make a Castor migration unsafe.

- **`CA-07` systemd hardening** — not a blocker for an internal host, but it is
  cheap to add while writing P0-07's provisioning script, and much cheaper to do
  once than to retrofit. Recommended, optional.
- **`CA-14` redaction verification** — cheap and worth doing anyway; not a blocker
  while captures stay on a single-tenant host.
- **`CA-11`, `CA-13`, `CA-15`, `CA-16`** — beyond the practices Castor's own
  credentials require.
- **`CA-24` fresh-host from a published archive** — Castor's host is provisioned by
  hand; this is a client-phase onboarding requirement.
- **`CA-01`, `CA-05`, `CA-50`, `CA-52`, `CA-53`** — client-specific ownership, data
  policy, and commercial terms.
- **Every `CL-*` except `CL-08` in spirit.** `CL-06` (the UI) is unnecessary when the
  operator is the runtime's author. `CL-10` is deferred to Phase E so Castor is not
  pointed at an API that is about to be superseded.
- **Risks R-03, R-04, R-07, R-11, R-14, R-15, R-20, R-21** — see
  [cloud-alpha-readiness.md](cloud-alpha-readiness.md) §"Risk coverage by phase".

## Related

- [cloud-alpha-readiness.md](cloud-alpha-readiness.md) — the plan this executes.
- [cloud-alpha-risks.md](cloud-alpha-risks.md) — the risks each task closes.
- [runtime-contract.md](runtime-contract.md) — the evidence statuses P0-01 and
  P0-02 move.
- [operations.md](operations.md) — the backup procedure P0-03 corrects.
