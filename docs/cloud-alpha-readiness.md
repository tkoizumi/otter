# Otter Cloud alpha — readiness and 2–5 client hosting plan

Status: proposed. Date: 2026-09-29. Amended 2026-09-29: Phase 0 added.

Phase 0 reinstates the hand-provisioned single-tenant runtime — the arrangement the
first draft proposed — for the internal Castor workload only. The control plane
remains the plan for paying clients. See §17.

Scope: first paying clients plus Castor production workloads.
Target scale: 2–5 external clients, plus Castor.

## Goal

The alpha should prove that Otter can safely operate production Python jobs for a
small number of clients through a hosted web interface.

This is not the final large-scale Otter Cloud architecture.

The target architecture is:

**multi-tenant control plane + isolated single-tenant runtimes**

Each client gets its own Otter runtime environment and host boundary. Otter Cloud
provides authentication, organizations, runtime registration, UI, remote control,
and fleet monitoring. `otterd` remains a single-tenant runtime and does not need a
tenant model.

For the first 2–5 clients:

- Client A → dedicated VM → `otterd`
- Client B → dedicated VM → `otterd`
- Castor → dedicated runtime → `otterd`

The runtime remains responsible for execution, scheduling, retries, durable state,
releases, and run history. The control plane manages access and operations around
those runtimes.

## Relationship to product-roadmap.md

This plan **deviates deliberately** from [product-roadmap.md](product-roadmap.md),
which states that no cloud product implementation starts before the Phase 3
runtime readiness gate (line 51) and places managed cloud post-1.0 (line 49). It
also contradicts the roadmap's advice to default to the development harness first
and to avoid running both tracks with a small team (lines 56–59).

That deviation is a conscious decision, and the reason it is defensible is
**Castor**: a second, internal consumer of the control-plane API means the
control plane is not built speculatively for one client. This should be recorded
as a dated decision rather than left as a contradiction between two documents.
Roadmap amendment is tracked as `CA-55`.

**Phase A is a real gate, not a formality.** Phases B–F spend heavily on a control
plane and UI whose value depends on the runtime actually being dependable. If
Phase A fails — a real daemon `kill -9` loses accepted work, a retry runs the wrong
release, a backup cannot restore — then stop and fix the runtime. Do not build the
control plane on top of an unproven execution plane.

## What we can promise — and must not

The client's jobs write to systems of record. This is the part that cannot be
fixed after the fact.

| Claim | Status | Source |
| --- | --- | --- |
| Accepted work survives a crash once the code is `v0.2.0` | Contracted, but only **simulated** — no real `SIGKILL` test exists | [runtime-contract.md](runtime-contract.md) lines 411–441 |
| Retries may repeat external effects | **Explicit non-guarantee** | [runtime-contract.md](runtime-contract.md) §5.2 |
| Exactly-once execution | **Not promised.** At-least-once with a durable audit trail | [runtime-contract.md](runtime-contract.md) line 326 |
| A retry runs its parent's bound release and environment | **Explicit non-guarantee** today (`OT-011`); promoted to MUST as `CA-25` | [runtime-contract.md](runtime-contract.md) lines 296–312 |
| Failure notification is delivered | **Best-effort**, bounded in-process retry, no durable outbox | [notify.go](../internal/notify/notify.go) line 9 |
| Documented backup captures everything needed to restore | **Database-only** as documented; releases and job identity live outside it | `CA-21` |
| Control-plane availability | **No SLA in the alpha.** Control-plane outage must not stop execution (`CL-15`) | This plan |

The client-facing promise:

> Otter runs your Python on a schedule or trigger, keeps a durable audit trail of
> every attempt, and never silently drops accepted work. It does **not** promise
> exactly-once execution. Your jobs must be safe to run twice.

That is a design constraint the pilot jobs must satisfy, agreed in writing
(`CA-03`), not a footnote.

---

# 1. Client scope and execution contract

## CA-01 — Define the system ownership boundary — MUST

For every system and object the pilot touches, document which system owns writes.

For the initial Amazon / NetSuite / Celigo client, explicitly define whether Celigo
or Otter owns: orders, items, prices, reconciliation, exceptions.

Otter and Celigo must not independently mutate the same business records without
an explicit arbitration design.

Safest alpha shape: **Otter reads from both systems and writes to neither** —
reconciliation, exception handling and reporting. A pilot that observes cannot
double-write; widening to writes is a separate, later decision.

**Done when:** a one-page ownership matrix is approved by the client.

## CA-02 — Select narrow pilot jobs — MUST

Initial jobs should be small in scope, idempotent, replayable, one-directional
where practical, and broken into bounded work units. Avoid making the first
production deployment a full-system migration or synchronization.

**Done when:** every pilot job has a documented input, output, business effect,
replay behavior, and owner.

## CA-03 — Document at-least-once execution — MUST

Otter does not promise exactly-once execution. Client job code must tolerate
duplicate execution and ambiguous external outcomes.

**Done when:** this behavior is documented in the client agreement and
acknowledged before production use.

## CA-04 — Classify failures in advance — SHOULD

Agree the routing for runtime faults vs job bugs vs upstream failures
(Celigo / Amazon / NetSuite). Track them separately; they have different owners
and different fixes.

**Done when:** each class has a named first responder and a diagnostic path.

## CA-05 — Agree data handling and capture policy — MUST

With a control plane displaying client data, this matters more than before.

HTTP capture stores request and response bodies for seven days by default
([security.md](security.md) lines 435–437). Order payloads carry customer PII.

**Done when:** every job handling PII declares `capture: metadata` or
`capture: off` (or the deployment sets `--capture-default`), the retention window
is agreed with the client, and `CL-12` guarantees the control plane does not
become a second copy.

---

# 2. Cloud identity and tenancy

## CL-01 — Add organizations and users — MUST

Minimum control-plane entities: `User`, `Organization`, `OrganizationMembership`,
`Runtime`, `RuntimeCredential`.

Every runtime belongs to exactly one organization. Every web/API request must be
authorized against the organization.

**Done when:** users from Organization A cannot view or operate any resource owned
by Organization B — proven by cross-tenant authorization tests, not by inspection.

## CL-02 — Add runtime registration — MUST

Each `otterd` instance must have a stable runtime ID, organization ownership, an
authentication credential, runtime version, last heartbeat, and connection status.
A runtime may only authenticate as itself.

**Done when:** a newly provisioned runtime registers and reconnects after daemon
or host restart.

## CL-03 — Isolate execution by client — MUST

Each client gets a separate VM or equivalent strong isolation boundary. Do not
host unrelated customers' arbitrary Python jobs inside one shared runtime.

**Done when:** each external tenant has its own runtime host, filesystem, SQLite
database, Python environments, secrets, and resource limits.

## CL-14 — Runtime credential lifecycle — MUST

Per-runtime credentials need issuance, rotation, and **revocation**. A leaked
runtime credential must not let an attacker impersonate a client's runtime to the
control plane or reach another organization's data.

**Done when:** revoking a runtime credential immediately stops that runtime
authenticating, and rotation is exercised by a drill.

## CL-21 — Scope the runtime's control credential — MUST

**Runtime work, not control-plane work.** To command a runtime, Cloud must
authenticate to it. Today the only credential with that authority is the runtime's
admin token (`OTTER_API_TOKEN`), which also reads all state and reaches business
data. If Cloud holds one admin token per runtime, a control-plane compromise
defeats the one-VM-per-tenant isolation (`R-04`).

The runtime currently issues an admin token, per-run tokens and webhook tokens, and
nothing purpose-built for a gateway (`internal/api/server.go`). A least-privilege
control credential is therefore **a new runtime capability**, on the critical path
for Phase B, and must be estimated as runtime work rather than a control-plane
detail.

**Done when:** a runtime can issue a credential limited to the gateway's command
surface, that credential cannot read business secrets, and it is independently
revocable per runtime.

## CL-17 — Cloud-side authorization is not runtime trust — MUST

A runtime is authenticated, but authenticated is not authorized. Every command
the cloud sends must be checked against the runtime's organization, and every
projection the runtime reports must be scoped to its own organization. A
compromised runtime must not be able to write into another tenant's projection.

**Done when:** a tampered runtime claiming another organization's IDs is rejected
and the attempt is audited.

---

# 3. Runtime ↔ Cloud communication

## CL-04 — Add an authenticated outbound control connection — MUST

The runtime initiates an outbound authenticated connection to Otter Cloud.
Routine operations must not require inbound public access to the Otter API.

Loopback-only access plus SSH tunneling
([target.go](../internal/deploy/target.go) lines 415–422) remains appropriate for
administrative recovery, but is insufficient for browser-driven client operations.

The control channel should support: list jobs, run job, pause job, resume job,
retrieve run information, retrieve logs, retrieve trace, and retrieve HTTP capture
metadata where policy allows.

Runtime-to-cloud events should include: heartbeat, job changed, run accepted, run
started, run succeeded, run failed, runtime health changed.

**Done when:** a runtime behind an inbound-deny firewall can be operated entirely
from the Otter Cloud UI.

## CL-05 — Keep runtime state authoritative — MUST

Otter Cloud must not become a second scheduler, queue, or execution state machine.
The runtime remains authoritative for scheduled work, accepted work, retries,
execution state, durable run state, releases, and `ctx.state`.

Cloud may store a projection for UI responsiveness, search, fleet status,
alerting, and analytics. Loss of the projection must not change execution
correctness.

**Done when:** cloud metadata can be deleted and reconstructed without corrupting
runtime execution state — and the reconstruction path is a tested procedure, not
an assertion.

## CL-22 — Add dynamic schedules — MUST

*Runtime work, not control-plane work*, and not in the first draft. `CL-04`'s
command list does not include scheduling, and schedules are manifest-only today:
changing one means editing a released file and running `otter reload`. A control
plane cannot manage a schedule it cannot create, and a UI cannot offer "every
fifteen minutes for this dataset" without a per-occurrence payload.

The runtime must gain first-class schedules: create, update, pause, resume and
remove without a reload, keyed by durable job identity, carrying a payload for
each occurrence, with manifest-declared schedules still reconciling
declaratively and neither owner silently overwriting the other. One occurrence
must produce at most one run across restarts and duplicate commands.

The design is [dynamic-schedules-design.md](dynamic-schedules-design.md).

**Done when:** a runtime behind an inbound-deny firewall can be given a new
schedule by the control plane, the occurrence fires with its payload, a restart
neither replays nor drops it, a duplicate command produces no second schedule,
and `otter reload` leaves an API-created schedule untouched.

## CL-23 — Make the runtime API a supported remote surface — MUST

*Runtime work, not control-plane work.* The API is loopback-only by convention,
not by necessity, and the artifacts that govern it disagree: the daemon permits a
non-loopback bind when a token is set, `otter deploy` refuses a wildcard address
and plans the API as loopback with `ssh -L`, and `assert-host-permissions.sh`
fails any non-loopback listen address.

Loopback is standing in for authorization. The credential that guards the API is a
single static admin token with full control-plane authority — including
`POST /v1/jobs/{id}/runs`, which executes arbitrary code — and no read-only or
per-job variant ([security.md](security.md)). An operator who needs the API from
another host, another container, or a client's backend has no supported path but a
tunnel.

An operator running Otter on their own host owns that decision. The runtime makes
the safe configuration the default and the reachable configuration possible,
rather than refusing on the operator's behalf.

Deliverables:

- Operator credentials gain scopes — control (run, pause, schedule), read (jobs,
  runs, logs, state), admin — with multiple named tokens, rotation and revocation.
  `CL-21` scopes the credential *Cloud* holds; this scopes the operator's.
- TLS is either supported directly or covered by a documented reverse-proxy
  pattern. The daemon serves plain HTTP today.
- `otter deploy` accepts a non-loopback or wildcard bind behind an explicit opt-in
  and a warning, instead of refusing it.
- `assert-host-permissions.sh` asserts the *configured* posture — loopback, or a
  token with an expected bind or CIDR — rather than hardcoding loopback, and its
  recorded Phase 0 evidence is re-run.
- [security.md](security.md)'s blanket prohibition and `CL-04`'s inbound-access
  wording are reconciled with the capability.

**Done when:** a runtime on a private network, behind TLS, is operated from
another host by a client holding a read-only token that cannot execute code, and
the host assertion passes on that configuration without weakening the loopback
default.

## CL-11 — Make control commands idempotent — MUST

*Not in the first draft; this is a correctness gap.*

The control channel is at-least-once, exactly like job execution. A "Run now" sent
over a flaky connection, retried by the gateway, or replayed after a reconnect can
enqueue the same run twice.

**Done when:** every control command carries a caller-derived idempotency key, the
runtime deduplicates on it, and a duplicate "Run now" produces exactly one run.

## CL-12 — Keep logs and captures on the runtime — MUST

*Not in the first draft; this is an isolation and compliance decision.*

"Cloud may store a projection" is ambiguous, and the wrong reading puts every
client's run logs and HTTP captures into one multi-tenant store. Those payloads
carry order data and PII (`CA-05`), and centralising them makes the control plane
the single richest target in the system.

**Decision:** the control plane stores metadata and status. Run logs, traces and
HTTP captures stay on the runtime and are **proxied on demand** over the control
channel, never persisted in the multi-tenant plane.

**Done when:** a client's log and capture bytes are absent from control-plane
storage and backups, and the UI still shows them.

## CL-15 — Define break-glass access and control-plane outage behavior — MUST

If the client can only operate through the UI, the control plane becomes a single
point of failure for visibility.

**Done when:** it is documented and tested that (a) a control-plane outage does not
stop scheduled execution, and (b) an operator can still inspect and control a
runtime over SSH + CLI during that outage.

---

# 4. Client web interface

## CL-06 — Build the minimum client UI — MUST

The first paying client must be able to operate Otter without SSH or the CLI.

Minimum pages:

**Jobs** — job name, enabled/paused state, schedule or trigger, latest run, latest
success, latest failure, runtime status.

**Runs** — run ID, job, state, start time, end time, duration, retry relationship.

**Run detail** — stdout, stderr, structured logs, trace/timeline, error
information, HTTP request metadata where capture policy permits it.

**Controls** — Run now, Pause, Resume. Deployment controls may remain
operator-only during the first alpha.

**Done when:** a client can inspect normal operation and diagnose an ordinary
failed run using only the web application.

**Sequencing note.** The UI is **not deferred.** For a remotely hosted runtime, a
management surface is not a second product — it is part of the runtime being usable
at all. SSH and the CLI are adequate for the developer building Otter and are not
an adequate interface for a customer operator, least of all a customer whose
current tool is Celigo, a point-and-click iPaaS (`R-22`).

The earlier recommendation to defer `CL-06` until a client asked is withdrawn. What
stands is the ordering: `CL-01`–`CL-05`, `CL-07` and `CL-11`–`CL-21` have a consumer
whether or not a client ever opens a browser — Castor does (`CL-10`) — so build the
API and registry first, then the UI against a proven API and observed usage rather
than guessed requirements.

The scope cut is therefore **breadth, not the UI**: everything that does not
directly expose or control the runtime waits. See Phase B/C in §17.

---

# 5. Host provisioning and hardening

The [hardening checklist](security.md#hardening-checklist) in `security.md`
(lines 439–466) is accurate but **descriptive**: `otter deploy` does not implement
most of it. `UnitFile` writes `User=`, `Group=`, `WorkingDirectory=`, `ExecStart=`,
two `EnvironmentFile=` lines, `Restart=always`, `KillSignal=` and `TimeoutStopSec=`
— and nothing else ([render.go](../internal/deploy/render.go) lines 37–62).

## CA-06 — Reproducible host provisioning — MUST

One automated process that takes a fresh Linux VM to a functioning, registered
Otter runtime: service account, directories, binaries, systemd, firewall, security
updates, runtime credentials, cloud registration, monitoring, Python tooling.

**Done when:** an empty VM becomes a registered working runtime without manual
host configuration.

## CA-07 — Enforce systemd hardening — MUST

Generate and verify `NoNewPrivileges`, `ProtectSystem`, `ProtectHome`,
`PrivateTmp`, `ReadWritePaths`, and relevant process restrictions.

**Done when:** the generated unit contains them and a drill asserts each, rather
than the checklist being prose.

## CA-08 — Enforce resource limits — MUST

Protect the daemon and host from runaway Python jobs: memory, CPU, process/task
count. The manifest has `timeout` and `concurrency` only
([manifest-reference.md](manifest-reference.md) lines 84, 237) — no memory or CPU
bound — so this is host-level (`MemoryMax`, `MemoryHigh`, `MemorySwapMax`,
`CPUQuota`, `TasksMax`). `MemorySwapMax=0` is part of the bound, not a detail:
with the cgroup's swap unbounded the hard cap never binds and the runaway is
paged out instead of killed.

**Done when:** a deliberately runaway Python process is killed without killing
`otterd` or making the host unavailable.

## CA-09 — Verify filesystem permissions — MUST

Verify at runtime: the Otter data directory is private to the service account,
credential/environment files are mode `0600`, and files are owned by the expected
service account.

**Done when:** provisioning tests assert these permissions.

## CA-10 — Prevent unintended inbound access — MUST

Client runtime machines must not expose the Otter API publicly. Normal
communication is outbound to the control plane. Administrative SSH may remain
under restricted policy.

**Done when:** an external network scan confirms only explicitly approved ports
are reachable.

---

# 6. Python environment and egress

## CA-17 — Verify required outbound egress — MUST

The runtime host must reach all required dependency and business endpoints:
Python runtime/tooling downloads, package indexes, Amazon APIs, NetSuite, Celigo,
and Otter Cloud.

There is **no offline bundle yet**: preparation fetches the interpreter and wheels
([managed-python.md](managed-python.md) line 482).

**Done when:** egress is proven from the real host to each endpoint.

## CA-18 — Prove managed Python preparation on the production host — MUST

Run `otter prepare` during deployment rather than waiting for the first scheduled
production run. Not every patch version is downloadable everywhere
([managed-python.md](managed-python.md) line 466).

**Done when:** the exact pinned interpreter and job environment are prepared
successfully before enabling schedules.

## CA-19 — Prime the uv cache — SHOULD

Keep `cache/uv/` warm so a cold host does not fetch the toolchain at first run
([managed-python.md](managed-python.md) line 274). Environment disk grows with
each distinct job identity and GC is deliberately deferred (line 480), which is a
second reason `CA-33` exists.

---

# 7. Secrets and credentials

## CA-11 — Provision runtime/API credentials securely — MUST

Generate strong credentials; store only in appropriately protected locations.
Record owner, purpose, runtime association, and rotation procedure.

## CA-12 — Scope client secrets per job — MUST

Manifests reference only the **names** of secrets a job needs. Secret values never
appear in manifests or release artifacts
([manifest-reference.md](manifest-reference.md) lines 272–285).

## CA-13 — Test secret rotation — MUST

Prove behavior for queued work, running children, new processes, and daemon
restart.

## CA-14 — Verify real-world redaction — MUST

Test with realistic client payloads and credentials. Secrets must not appear in
stdout/stderr, structured logs, HTTP capture, error summaries, **or cloud
projections** — the last is new with the control plane.

**Done when:** seeded credentials cannot be found in retained client-visible or
operator-visible telemetry.

## CA-15 — Decide the secret backend — MUST

There is **no built-in secret backend** today; `SecretProvider` is the extension
point ([manifest-reference.md](manifest-reference.md) lines 284–285). For the
alpha, plaintext `0600` `EnvironmentFile` on an isolated host is defensible — but
say so explicitly, and note that `otter deploy` writes those values from the
operator's machine, so it holds every client's credentials too
([deploy/config.go](../internal/deploy/config.go) line 160).

**Done when:** the decision and its rationale are recorded, and `CA-13` proves
rotating through it.

## CA-16 — Rotate SSH keys and runtime credentials on a schedule — SHOULD

## CL-13 — Keep client secrets out of the control plane — MUST

The control plane must never hold, display, or proxy a client's business
credentials. Secrets reach jobs through the runtime's environment, and the control
plane only ever handles secret **names**.

**Done when:** an operator with full control-plane access cannot read any client
secret, and this is a designed property rather than an accident.

---

# 8. Runtime reliability evidence

The following runtime drills remain mandatory, and they gate the control plane
(see §17 Phase A).

## CA-20 — Real daemon kill/recovery drill — MUST

Start real work, `kill -9` the actual daemon, restart it, and verify every accepted
unit of work either reaches a correct terminal state or is re-enqueued.

The existing evidence is simulated: `FM-01`/`FM-02` are `Simulated`, and the only
real `SIGKILL` test kills a stand-in with no daemon and no database
([runtime-contract.md](runtime-contract.md) line 411). Insufficient for a
production hosting claim.

**Done when:** the real-kill harness passes and `runtime-contract.md`'s matrix
moves from Simulated to Real.

## CA-21 — Complete backup and restore drill — MUST

Back up everything required to restore a runtime to a new machine — more than
SQLite if correctness depends on releases, source identity, Python environments
or reconstructable environment metadata, runtime identity, and durable state.

The documented procedure copies `otter.db` only ([operations.md](operations.md)
lines 399–441), while release snapshots live on disk under the data directory and
job identity is a `.otter-id` marker inside the **source directory**
([security.md](security.md) lines 468–479).

**Done when:** a backup from Host A restores onto clean Host B and historical
runs, state, releases, and runnable jobs remain intact. Fix the documented
procedure to match.

## CA-22 — Upgrade and downgrade-refusal drill — MUST

Prove a supported upgrade succeeds, an unsupported downgrade is safely rejected,
and existing durable state remains valid.

## CA-23 — Deployment-failure recovery drill — MUST

Inject failure midway through deployment. The previous known-good release must
remain operational.

## CA-24 — Fresh-host release artifact drill — MUST

A clean VM must become operational using published release artifacts only. No
source checkout or locally built binary. `scripts/smoke.sh` currently uses a
locally built binary.

## CA-25 — Retry release/environment binding — MUST

Promoted from SHOULD: a retry running newer code than its parent is not something
to hand a paying client.

**Done when:** activate a new release between attempt 1 and its retry and prove the
retry executes the parent's release snapshot and environment. Promote the
guarantee in `runtime-contract.md` §5.1 only when this passes.

## CA-26 — Script the drills — SHOULD

Make them `make drill` subcommands that assert their own outcome and exit non-zero
with failing evidence, so a second operator can run them without interpreting
prose ([v0.3.0-release-plan.md](archive/v0.3.0-release-plan.md) WS6).

## CA-56 — Validate crash recovery per client host — SHOULD

The provisioning workflow runs recovery validation before enabling schedules.
Do this on the client's real host **before** schedules are enabled, not against a
live production runtime.

---

# 9. Monitoring and alerting

## CA-30 — Deliver job failure notifications — MUST

Configure a human-visible failure channel for every production client: Slack,
Teams, a webhook into the client's system, or an internal support alert.

## CA-31 — Detect runtime disappearance externally — MUST

Do not rely on `otterd` to report its own death. Delivery is best-effort with a
bounded in-process retry and no durable outbox
([notify.go](../internal/notify/notify.go) line 9) — if the daemon is down or a
webhook blips, the alert is gone.

The control plane tracks heartbeat and alerts when it disappears. Keep `CA-30` as
well: two independent paths, because the control plane itself can be down.

## CL-07 — Central runtime health view — MUST

Display runtime online/offline status, version, last heartbeat, disk usage, daemon
health, and latest job activity. Operators get one view across all runtimes.

## CL-16 — Monitor and back up the control plane — MUST

*Not in the first draft.* The control plane is now a single point of failure for
visibility and alerting for every client at once.

**Done when:** it has its own health monitoring, its own alerting (on a path that
does not depend on itself), and its own tested backup and restore.

## CA-32 — Expose queue age and depth — SHOULD

`/health` reports counts only (`OT-004`). Add queue age, per-job depth, and
last-success freshness so a stuck job is visible before it becomes a support
ticket, and so `CL-07` has something meaningful to show.

## CA-33 — Monitor storage growth — MUST

Monitor filesystem usage, SQLite size, release artifacts, Python
environment/cache growth, and log/capture retention. Define retention and alert
thresholds before enabling unattended jobs.

## CA-34 — Document incident log access — SHOULD

`otter trace`, `otter requests`, and run logs, including truncation bounds and the
capture-state distinction between "nothing recorded" and "nothing requested".

---

# 10. Control-plane security

*New workstream. Section 5 hardens the runtime host; nothing yet hardens the
control plane.*

## CL-18 — Harden the control plane — MUST

It is internet-facing, multi-tenant, and can command every client's runtime. It is
the highest-value target in the system.

Minimum: session management, MFA, rate limiting, CSRF protection, secure password
reset, and dependency and image patching.

**Done when:** the control plane passes the same kind of checklist `security.md`
applies to the runtime.

## CL-19 — Audit every control action — MUST

Every command issued to a runtime, every login, and every membership change is
recorded with actor, organization, target, and outcome.

**Done when:** "who paused Client A's job at 03:14" is answerable from the audit
log, and the log is append-only from the application's perspective.

## CL-20 — Rate-limit and bound control commands — SHOULD

A compromised or buggy UI must not be able to enqueue thousands of runs across a
client's runtime. Bound command rate per organization, and surface the bound in
the UI rather than failing silently.

---

# 11. Job correctness requirements

Responsibilities of both Otter and the job implementation.

## CA-40 — Use stable external IDs — MUST

Writes should be upserts or otherwise safely address existing entities. Avoid
blind inserts where replay could create duplicates.

## CA-41 — Use business-derived idempotency keys — MUST

Derive from stable business input, not from Otter run IDs.

## CA-42 — Restrict stateful checkpoint jobs to safe concurrency — MUST

Use `concurrency: 1` for non-atomic read/modify/write through `ctx.state`.

## CA-43 — Use bounded replayable units — MUST

Checkpoint at small boundaries: one page, one batch, one bounded business unit —
not one enormous synchronization transaction.

## CA-44 — Handle ambiguous remote writes — MUST

A network error does not prove the remote system failed to commit. Reconcile
remote state before replaying uncertain effects.

## CA-45 — Respect upstream rate limits — MUST

Bound concurrency, retries, exponential backoff, and request rate. A retry storm
must not amplify an upstream outage.

## CA-46 — Verify business outcomes independently — MUST

A successful exit code is not proof of a correct integration. Where practical,
verify expected effects by querying the destination system.

`CA-44` and `CA-46` are the difference between a demo and a production pilot. A
job that returns zero having written nothing, or that double-writes an order, will
be found by the client, not by us.

---

# 12. Castor migration

Castor uses the same cloud/runtime architecture as external clients.

## CL-08 — Create a dedicated Castor runtime — MUST

Its own isolated runtime. Do not special-case it inside the control plane.

## CL-09 — Move one scheduled Castor import to Otter — MUST

Select one existing EventBridge/Lambda scheduled import and migrate it end to end:
normal scheduling, release flow, secrets, run history, alerts, and cloud control
plane.

**Done when:** the workload runs in production through Otter and no longer depends
on Lambda/EventBridge for its execution schedule.

## CL-10 — Make Castor consume the normal Otter Cloud API — MUST

Where Castor needs Otter status or controls, use the same API the frontend uses,
not an internal shortcut. This makes Castor an early production consumer of the
public control-plane abstraction — and it is the reason the control plane is
worth building before a second external client exists.

---

# 13. Operations and support

## CA-50 — Define incident ownership — MUST

For every client: who receives alerts, who investigates, expected response window,
and the client escalation path.

## CA-51 — Create a client runbook — MUST

Pause jobs, inspect failures, inspect logs/traces, retry work, roll back releases,
rotate credentials, restore the runtime, contact the client.

## CA-52 — Agree recovery objectives — SHOULD

State the RPO and RTO actually committed to, following from `CA-21`. Without this
the client will assume better than we deliver.

## CA-53 — Agree volume and capacity — SHOULD

Expected runs per day, payload sizes, storage growth. Informs `CA-33`, VM sizing,
and `CL-20`. Also state the per-client concurrency ceiling (`--workers` defaults
to CPU count, capped at 8).

## CA-55 — Amend product-roadmap.md — MUST

Record the dated decision to run this track ahead of the Phase 3 gate and the
harness, and why Castor changes the calculus. Two documents must not silently
disagree.

---

# 14. Provisioning workflow for each client

1. Create organization.
2. Create runtime identity.
3. Provision dedicated VM.
4. Apply OS/systemd hardening.
5. Configure outbound network access.
6. Install published Otter release.
7. Register runtime with Otter Cloud.
8. Verify cloud heartbeat.
9. Configure client secrets.
10. Run `otter prepare`.
11. Deploy pilot release.
12. Run smoke job.
13. Run crash/recovery validation where appropriate.
14. Configure disk/runtime monitoring.
15. Configure failure alerts.
16. Enable schedules.
17. Give client Cloud access.

May initially be operator-triggered. Self-service provisioning is not required.

---

# 15. Web/control-plane architecture

```text
                    Otter Cloud
       ┌────────────────────────────────┐
       │ Authentication                 │
       │ Organizations                  │
       │ Runtime registry               │
       │ Jobs / run projections         │
       │ Runtime command gateway        │
       │ Fleet health / alerts          │
       │ Web UI                         │
       └───────────────┬────────────────┘
                       │
             outbound authenticated
                runtime connections
                       │
        ┌──────────────┼───────────────┐
        │              │               │
    Client A        Client B         Castor
      VM              VM              VM
    otterd           otterd          otterd
    SQLite           SQLite          SQLite
    Python           Python          Python
```

The control plane is multi-tenant. The execution plane is not.

Metadata and status cross the boundary. **Logs, traces and captures do not**
(`CL-12`).

---

# 16. Explicitly out of scope

Do not block the 2–5 client alpha on: shared multi-tenant runtime workers,
Kubernetes, distributed scheduling, multi-region execution, automatic cross-host
failover, runtime autoscaling, billing automation, connector marketplace, workflow
canvas, sophisticated enterprise RBAC, customer self-service VM provisioning,
automatic runtime placement, fully automated backup scheduling, shared Python
execution hosts.

These may become useful once operating experience demonstrates the need.

---

# 17. Recommended implementation sequence

## Phase 0 — Castor dogfood (internal, non-paying)

**Purpose.** Absorb the risk a paying client would otherwise absorb. Run one real
Castor scheduled import on one dedicated runtime, operated by us, with no control
plane.

**What "managed" means here.** We operate the host: `otterd` on a dedicated VM,
loopback API, SSH tunnel, CLI — the architecture in [security.md](security.md).
This is not a product and it does not need one. `CL-08` still applies in spirit:
give Castor its own runtime and operate it exactly as a client's, so the runbook
written here is the runbook used later.

**Phase 0 does not need `CA-55`.** It operates the runtime; it does not build
cloud. The roadmap deviation that `CA-55` records begins when Phase B starts.

**In scope.** `CA-20`, `CA-25`, `CA-21`, `CA-08`, `CA-30`, `CA-31`, `CA-22`,
`CA-19`, `CA-33`, `CA-40`–`CA-46`, `CA-02`, plus the viability prerequisites
below. `CA-51` (runbook) is written during the observation period.

**Out of scope.** Every `CL-*` except `CL-08` in spirit. `CL-06` — the operator is
the runtime's author, so CLI and logs are sufficient. `CL-10` — a scheduled import
needs no Otter API; the integration surface is the data, not the API. Also out:
`CA-01` and `CA-05` (client-specific ownership and data policy),
`CA-11`–`CA-16` beyond the practices Castor's own credentials require, and
`CA-50`/`CA-52`/`CA-53` (no commercial agreement).

**First import.** Choose one that is scheduled rather than event-driven (so cron and
backlog behavior get exercised), watermark- or checkpoint-shaped (so `ctx.state` is
used for real), upsert-shaped or idempotent at the destination, and verifiable from
outside Otter.

### Pre-cutover blockers

These gate the cutover. Each is something that could make the migration unsafe.

| Blocker | Task | Closes |
| --- | --- | --- |
| A real crash/kill test proves accepted work survives | `CA-20` | R-01 |
| Retries are bound to the original release and environment | `CA-25` | R-09 |
| Backup + restore produces a runnable job with state and releases intact | `CA-21` | R-08 |
| A runaway process cannot take the runtime host down indefinitely | `CA-08` | R-10 |
| A deliberate failure reaches a human with nobody watching the terminal | `CA-30`, `CA-31` | R-05 |
| Runtime version is pinned; no automatic upgrades | `CA-22` | R-12 |
| Disk growth has retention, monitoring, and an alert threshold | `CA-19`, `CA-33` | R-17 |
| The Castor job is safe to retry, externally verifiable, and its business outcome is checked independently of exit status | `CA-40`–`CA-46` | R-02, R-18 |
| Job ownership, replay behavior, and business effect are documented | `CA-02` | R-02 |
| The job has run in shadow mode beside the existing Lambda long enough to compare results | — | R-02, R-18 |

**Viability prerequisites.** Not safety blockers, but the job cannot start without
them: reproducible host provisioning and no public API ingress (`CA-06`, `CA-09`,
`CA-10`), outbound egress and a successful `otter prepare` (`CA-17`, `CA-18`), and
scoped secrets for the job (`CA-12`).

### Post-cutover observation — 30 days

Evidence goals, not gates. Gating the migration on these would be circular: you
cannot prove 30 days of operation before you migrate, and you cannot migrate until
you have proven 30 days.

- 30+ days unattended with no runtime-caused incident.
- Observed operational burden.
- Storage-growth data.
- Upgrades and restarts exercised in normal operation.
- A refined runbook (`CA-51`).
- A written, ranked list of operational gaps.
- Those gaps become the control plane's requirements document.

### Standing rule

**Every Phase 0 blocker must have executable or recorded evidence.** A blocker is
not closed because a document says the behavior exists; it is closed by a test or a
dated recorded run. This is `R-19`, promoted from a risk to a rule
([cloud-alpha-risks.md](cloud-alpha-risks.md) R-19).

### Phase 0 in one line

**Phase 0 proves that one real Castor job can run safely and unattended on Otter
without Cloud.** Cutover is gated by crash safety, retry correctness,
recoverability, bounded host failure, alerting, pinned releases, storage
protection, and application-level correctness. The following 30 days are not
another readiness gate; they are the experiment that tells us what Cloud actually
needs to provide.

**The sequence:** prove runtime → migrate Castor → observe operations → derive
Cloud requirements → build Cloud for clients.

**Timing.** Read the observation evidence after four to six weeks, not six months.
Phase B must be started backwards from the client's target date; a longer Castor
soak does not improve the plan, it only delays Phase B.

## Risk coverage by phase

`Phase 0` is the Castor dogfood above. `Client` is everything a paying customer
requires. `Carried` means Phase 0 already closed it and the client phase only
re-verifies it on the client's own host.

| Risk | Phase 0 | Client | Where it is addressed |
| --- | --- | --- | --- |
| R-01 The gate is not real | **Yes** | Carried | Phase 0 *is* this gate; ownership and date in Phase A below |
| R-02 Duplicate external writes | **Partial** — the migrated job must be idempotent | **Yes** — plus the client's systems of record | `CA-40`–`CA-46`; `CA-01` for the client |
| R-03 Two products, one team | No — Phase 0 is a single track | **Yes** | Phase B onward; sizing below |
| R-04 Control plane credential vault | No — no control plane | **Yes** | `CL-14`, `CL-21` |
| R-05 Silent failure | **Yes** | **Yes** — per-client channels | `CA-30`, `CA-31`; `CL-07`, `CL-16` |
| R-06 Celigo and Otter both write | N/A | **Yes** | `CA-01` |
| R-07 Unfrozen runtime API | No — nothing consumes it | **Yes** | Freeze the consumed subset before Phase B |
| R-08 Backup restores runnable jobs | **Yes** | **Yes** — re-run per host | `CA-21` |
| R-09 Retry runs newer code | **Yes** | Carried | `CA-25` |
| R-10 No memory/CPU cap | **Yes** | Carried | `CA-08` |
| R-11 Client-visible state diverges | No — the CLI reports runtime truth directly | **Yes** | `CL-05`, `CL-11` |
| R-12 Pre-1.0 in production | **Yes** — pin, no auto-upgrade | Carried | `CA-22` |
| R-13 Plaintext secrets | **Partial** — Castor's own credentials, same practice, lower stakes | **Yes** — fleet-scale handling | `CA-12`–`CA-16` |
| R-14 Logs and captures centralised | No — single tenant | **Yes** | `CL-12` |
| R-15 Control plane is a SPOF | No — no control plane | **Yes**, once it exists | `CL-15`, `CL-16` |
| R-16 Support capacity | **Partial** — you are on call for yourself | **Yes** — named owner and window | `CA-50`, `CA-51` |
| R-17 Storage growth | **Yes** | Carried | `CA-19`, `CA-33` |
| R-18 "Exit 0" ≠ correct outcome | **Yes** — verify Castor's destination | **Yes** | `CA-46` |
| R-19 Done by inspection | **Rule**, not a blocker | **Rule** | Every blocker cites a test or a dated recorded run |
| R-20 Single-tenant economics | No — reclassified, not a risk | Pricing note only | — |
| R-21 "Reasonable" | No — no commercial agreement | **Yes** | Milestone-based commitment |
| R-22 Runtime is hard to see | No — product positioning | Positioning only | Not a migration or onboarding gate |
| R-23 "Thin" is unquantified | **Partial** — Phase 0 produces the first real numbers | **Yes** — the control-plane sizing | Sizing below |

**This table is the answer to "what must be true before Castor, and what can wait."**
Twelve of the twenty-three risks are Phase 0 work, wholly or partly. Eight are
genuinely client-phase — the control plane, tenancy, the UI, the Celigo boundary,
and the commercial terms. R-19 is a standing rule rather than a risk, and R-20 and
R-22 need no Phase 0 action at all. That split is why Phase 0 is cheap, and why it
is worth doing first.

**R-03, R-04, R-07, R-11, R-14, R-15, R-20 and R-21 stay out of Phase 0.** Each
becomes relevant only when the Cloud product is built or sold, and none of them can
make a Castor migration unsafe.

## Phase A — preserve the runtime guarantees

`CA-20`, `CA-21`, `CA-22`, `CA-23`, `CA-24`, `CA-25`, `CA-07`, `CA-08`.

Establishes that Otter itself behaves correctly under failure. **This is a gate.**
A failure here stops Phases B–F.

Phase 0 satisfies this gate for the Castor runtime. The client track re-runs the
host-dependent parts — `CA-21`, `CA-24`, and the `CA-06`–`CA-10` host checks — on
each client's host; the runtime-level items (`CA-20`, `CA-25`) carry forward.

**Gate ownership.** A gate is only real if a named person owns it and has a date by
which they must decide. The Phase A gate is owned by **⟨name⟩**, with a go/no-go
decision due **⟨date⟩**, recorded here with its evidence. "Stop" is a valid
outcome: if `CA-20` shows lost accepted work, or `CA-25` shows a retry running the
wrong release, the correct response is to fix the runtime, not to proceed to
Phase B.

**Sizing.** The alpha Cloud must be estimated per workstream, in engineer-weeks,
before Phase B starts, with `CL-21` counted as runtime work. Until that number
exists, "thin" is an assertion and `R-03` is unanswerable — whether the gate is
reachable at all depends on it (`R-23`).

## Phase B — build the minimal control plane

`CL-01`, `CL-02`, `CL-04`, `CL-05`, `CL-07`, plus `CL-11`, `CL-12`, `CL-14`,
`CL-17`, `CL-18`–`CL-23` — including `CL-21`, the runtime-side control credential,
`CL-22`, dynamic schedules, and `CL-23`, the reachable and scoped API, all three of
which are runtime work on this phase's critical path.

At this point Cloud can securely operate one runtime. Ship the API before any UI:
Castor is the first consumer (`CL-10`).

## Phase C — build the client UI

`CL-06`: jobs, runs, run detail, logs, run now, pause/resume.

Ships after Phase B, against a proven API. Let Castor drive Phase B first, then
build the UI to requirements observed from real usage rather than guessed ones. Add
no broader product features — the UI exists to expose and control the runtime, not
to become a workflow product.

## Phase D — automate one-host-per-client deployment

`CA-06`–`CA-18`, plus cloud registration, smoke tests, health validation, alerts,
disk monitoring. Result: a repeatable client onboarding process.

## Phase E — Castor scale-out and API consumption

Castor already migrated in Phase 0. This phase moves *additional* Castor workloads
onto the platform and, where Castor needs Otter status or controls inside its own
UI, points it at the control-plane API (`CL-10`).

`CL-10`'s requirement is "the same API the frontend uses, not an internal
shortcut" — not "the control-plane API specifically". Once `CL-23` makes the
runtime API a supported, scoped, remote surface, a single-tenant Castor backend
consuming it directly is an ordinary client relationship rather than a shortcut,
and `CL-10` is satisfied by the runtime API until a Cloud frontend exists to share
it. Whichever surface is chosen, Castor's **backend** holds the credential, never
the browser. The decision is recorded in
[product-roadmap.md](product-roadmap.md#recorded-decisions).

## Phase F — onboard the first external client

`CA-01`–`CA-05`, `CA-40`–`CA-46`, `CA-50`, `CA-51`, production alerts, client Cloud
account. Only then enable unattended production schedules.

---

# 18. Alpha exit gate

## Before the first paying client's production schedule

Phase 0 enables an unattended Castor schedule under the smaller gate in §17's
[Risk coverage by phase](#risk-coverage-by-phase). This gate applies to a paying
client's workload.

Enabling an unattended schedule for a paying client requires evidence of the
following. These are the risks capable of invalidating the experiment
([cloud-alpha-risks.md](cloud-alpha-risks.md) §"What gates, and what does not").
The full exit gate below is reached later.

1. Accepted work survives process and host interruption (`CA-20`).
2. A retry executes its parent's bound release and environment (`CA-25`).
3. Write-capable jobs have an idempotency strategy — or the first pilot is
   read-only (`CA-01`, `CA-40`–`CA-46`).
4. Cloud uses a bounded, least-privilege runtime credential (`CL-14`, `CL-21`).
5. Failed jobs and dead runtimes are externally detectable (`CA-30`, `CA-31`,
   `CL-07`, `CL-16`).
6. A real backup recreates a runnable host (`CA-21`).
7. Log and capture bodies remain outside Cloud storage (`CL-12`).
8. The client runtime is pinned to a known version (`CA-22`).
9. Host resource and disk exhaustion have basic protection (`CA-08`, `CA-33`).
10. The client accepts the alpha's availability and support limitations (`CA-50`,
    `CA-52`).

## Full alpha exit gate

Ready for the first 2–5 paying clients when all of the following are true:

- Every client has an isolated runtime host.
- One shared control plane securely separates organizations, proven by
  cross-tenant authorization tests.
- Clients can log in and see only their own jobs and runs.
- Clients can inspect logs and run status.
- Clients can run, pause, and resume jobs from the browser.
- Runtime hosts require no public Otter API ingress.
- Runtimes maintain authenticated outbound connectivity to Cloud.
- A runtime disappearing generates an external alert, and the control plane has
  its own monitoring and backup.
- Control commands are idempotent: a duplicated "Run now" yields one run.
- Client logs and captures are absent from control-plane storage and backups.
- Control-plane outage does not stop execution, and break-glass access is tested.
- No client secret is readable from the control plane.
- A fresh VM can be provisioned reproducibly.
- Managed Python preparation succeeds before jobs are enabled.
- Secrets are scoped and redaction is verified against realistic payloads.
- A real daemon `kill -9` loses no accepted work.
- Backup restores onto a clean host with required state and releases intact.
- A runaway Python process cannot take down the runtime host.
- Retries preserve their parent's release/environment binding.
- Client jobs are designed for at-least-once execution.
- Ambiguous remote writes are reconciled before retry.
- Business outcomes are independently validated where necessary.
- Incident ownership, recovery objectives, and a client runbook exist.
- At least one Castor scheduled production import runs through the same
  architecture.
- `product-roadmap.md` records the decision to run this track first (`CA-55`).

The objective of this alpha:

> **Operate a small fleet of isolated Otter runtimes through one hosted control
> plane, giving clients enough visibility and control to safely run production
> Python jobs while preserving Otter's simple single-host runtime model.**

This should provide enough real usage to determine which parts of a larger Otter
Cloud architecture are genuinely necessary before building shared execution
infrastructure or large-scale orchestration.

---

## Open questions for the client

1. Which object types must Otter read, and which (if any) must it write?
2. What is the business key for an order, item and price — does it exist on both
   sides of the Celigo flow?
3. What is the current failure mode of the Celigo flows, and what breaks when they
   fail? That gap is what Otter is being bought to close.
4. Where should failure alerts go, and who is on the other end?
5. How long may order payloads be retained on our host?
6. Does the client actually want a browser UI, or would a weekly report and an
   alert channel serve them for the first months? This decides whether `CL-06`
   blocks their onboarding.

## Related

- [cloud-alpha-risks.md](cloud-alpha-risks.md) — the adversarial risk register for
  this plan, reconciled with it on 2026-09-29.
- [product-roadmap.md](product-roadmap.md) — Phase 3 gate and post-1.0 Phase 5;
  superseded for this track by `CA-55`.
- [v0.3.0-release-plan.md](archive/v0.3.0-release-plan.md) — WS6 covers `CA-20`–`CA-26`.
- [runtime-contract.md](runtime-contract.md) — the guarantees behind §"What we can
  promise".
- [security.md](security.md) — the runtime hardening checklist behind
  `CA-07`–`CA-09`, and the model `CL-18` must mirror for the control plane.
- [operations.md](operations.md) — the backup procedure behind `CA-21`.
- [managed-python.md](managed-python.md) — preparation and cache behavior behind
  `CA-17`–`CA-19`.
