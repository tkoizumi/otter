# Otter Cloud alpha — risk register

Status: proposed. Date: 2026-09-29. Amended 2026-09-29 after review.
Companion to [cloud-alpha-readiness.md](cloud-alpha-readiness.md).

**Amendment.** This register was written adversarially, and its first revision
enumerated risks without doing the harder job of deciding which ones actually gate
the alpha. The review that followed separated risks that can invalidate the
experiment from operational limitations that are acceptable in an alpha and scale
problems that do not yet exist. That triage is adopted here — see
[What gates, and what does not](#what-gates-and-what-does-not). R-19, R-20 and R-22
are reclassified, R-15 is downgraded, R-23 is added, and the recommendation to defer
the client UI (`CL-06`) is **withdrawn**. The underlying risks are unchanged; their
weight is.

This document is deliberately adversarial. It exists to argue against the plan, so
that the arguments are made now rather than by a client's production incident. It
is not a list of things that will go wrong; it is a list of things that could, and
what would tell us early.

Severity is judged as impact on **this alpha** — 2–5 paying clients plus Castor —
not on the eventual product.

## Summary

| ID | Risk | Severity |
| --- | --- | --- |
| R-01 | The Phase A gate is not real; the platform is built on an unproven execution plane | Critical |
| R-02 | At-least-once retries duplicate writes to NetSuite/Amazon | Critical |
| R-03 | Two products at once with a small team | Critical |
| R-04 | The control plane becomes a credential vault for every runtime | Critical |
| R-05 | A client's job dies silently and nobody learns | Critical |
| R-06 | Celigo and Otter both mutate the same records | High |
| R-07 | The control plane is built against an unfrozen runtime API | High |
| R-08 | Backup restores history but not runnable jobs | High |
| R-09 | A retry runs newer code than its parent | High |
| R-10 | No memory/CPU cap; one job takes a client's host down | High |
| R-11 | Client-visible state diverges from runtime truth | High |
| R-12 | A paying client runs production on a pre-1.0 runtime | High |
| R-13 | Plaintext secrets on the host, and on the operator's machine | High |
| R-14 | Logs and captures leak into the multi-tenant plane | High |
| R-15 | The control plane is a single point of failure for all clients | Low |
| R-16 | Capacity to support 2–5 clients plus Castor does not exist | Medium |
| R-17 | Storage grows unbounded across four different stores | Medium |
| R-18 | "Exit 0" is mistaken for a correct business outcome | Medium |
| R-19 | The alpha's definition of done is satisfiable by inspection | Reclassified — standing rule |
| R-20 | Single-tenant-per-client economics do not improve with scale | Reclassified — benefit at this scale |
| R-21 | "Reasonable" is an undefined reason to stop paying | Medium |
| R-22 | The original problem — the runtime is hard to see — is still unsolved | Reclassified — positioning only |
| R-23 | "Thin" is unquantified; the alpha Cloud has no sizing estimate | High |

Plan coverage below refers to IDs in
[cloud-alpha-readiness.md](cloud-alpha-readiness.md): **Covered**, **Partial**, or
**Not covered**.

## What gates, and what does not

A risk register enumerates. Something else has to decide. This is that decision,
and it is what the rest of this document exists to support.

Phase 0 — the internal Castor dogfood — has an earlier and smaller gate of its own.
The per-risk phase assignment is in
[cloud-alpha-readiness.md](cloud-alpha-readiness.md) §"Risk coverage by phase"; the
list below is the paying-customer gate.

**Gates the first production schedule.** These are the risks capable of
invalidating the experiment, and they are the launch gate:

| Gate | Risk | Task |
| --- | --- | --- |
| Accepted work survives process and host interruption | R-01 | `CA-20` |
| A retry executes its parent's bound release and environment | R-09 | `CA-25` |
| Write-capable jobs have an idempotency strategy | R-02 | `CA-40`–`CA-46` |
| Cloud holds a bounded, least-privilege runtime credential | R-04 | `CL-14`, `CL-21` |
| Failed jobs and dead runtimes are externally detectable | R-05 | `CA-30`, `CA-31`, `CL-07`, `CL-16` |
| A real backup recreates a runnable host | R-08 | `CA-21` |
| Log and capture bodies stay outside Cloud storage | R-14 | `CL-12` |
| The customer runtime is pinned to a known version | R-12 | `CA-22` |
| Host resource and disk exhaustion have basic protection | R-10, R-17 | `CA-08`, `CA-33` |
| The customer accepts the availability and support limits | R-16, R-21 | `CA-50`, `CA-52` |

**Acceptable with simple controls, not gated.** R-07 (freeze the consumed subset
only, do not pull `v0.4.0` forward), R-11 (`requested` / `confirmed` / `failed`
states), R-13 (environment files with correct modes, for 2–5 clients), R-15, R-16.

**Does not drive alpha architecture.** R-20 is a property of the right isolation
choice, not a risk at this scale. R-22 is product positioning, not a gate at any
phase.

**A rule, not a risk.** R-19 — every gate item must cite executable or recorded
evidence — is applied as a standing rule across all of the above, rather than
carried as a risk needing its own mitigation.

**Real, but not gateable.** R-03 and R-23 are not conditions a system can pass.
They are schedule risks, answered by resourcing and by sizing rather than by
engineering, and they are the two most likely to sink this alpha.

---

## Critical

### R-01 — The Phase A gate is not real

**Coverage: Partial (`CA-20`–`CA-25`).**

The plan is sequenced so that runtime reliability (`CA-20`–`CA-25`) comes first and
"gates" the control plane. But nothing enforces that. Phase B has its own
momentum, its own visible progress, and no dependency that fails if Phase A stalls.
The realistic outcome is that Phase A slips quietly while Phase B proceeds, and the
gate becomes a sentence in a document.

Why it matters: Phase A is the only phase whose results can invalidate the whole
plan. If a real `kill -9` loses accepted work, or a retry runs a different release
than its parent, then the control plane is a well-built interface to a runtime that
cannot be trusted with a client's production data. We would have spent the
expensive, hard-to-reverse effort on the wrong layer.

Leading indicator: any Phase B commit lands before `CA-20` produces a passing
real-kill result.

Mitigation: make Phase A's exit a dated, recorded decision — the same shape as the
roadmap's Phase 3 gate — and make that decision *able to be "stop"*. A gate is only
real if a named person owns it and has a date by which they must decide. The review
correctly hardened the gate's content (the conditions listed in
[What gates](#what-gates-and-what-does-not)); what was missing is ownership.
Assigned owner and decision date are recorded in
[cloud-alpha-readiness.md](cloud-alpha-readiness.md) §17.

### R-02 — At-least-once retries duplicate writes to NetSuite/Amazon

**Coverage: Partial (`CA-40`–`CA-46`, and `CA-01` if Otter never writes).**

The runtime contract is explicit: retries may repeat any external effect, and
exactly-once is not promised ([runtime-contract.md](runtime-contract.md) §5.2). The
client's systems of record are NetSuite and Amazon. A retried job that inserts an
order rather than upserting it creates a real duplicate in a real ledger.

This is the most likely way to lose this client, because it is not a visible
failure. The run reports success; the duplicate is discovered later, by an
accountant, in a system we do not control.

Leading indicator: any pilot job that inserts rather than upserts, or whose
idempotency key derives from the Otter run id.

Mitigation: `CA-01` should keep Otter read-only for the first pilot, so the failure
mode is structurally impossible rather than contractually avoided. If Otter must
write, `CA-40`/`CA-41` become release blockers, not checklist items, and the client
should agree the reconciliation procedure for a duplicate.

### R-03 — Two products at once with a small team

**Coverage: Not covered.**

The plan now contains a runtime reliability workstream and a multi-tenant web
platform workstream, running concurrently, on the team that also supports Castor.
The [roadmap](product-roadmap.md) warned about exactly this (lines 56–59).

Why it matters: the failure mode is not that one track is abandoned — it is that
both are 80% done. A runtime that is nearly dependable and a control plane that
nearly isolates tenants is worse than either alone, because it looks shippable.

Leading indicator: nobody is full-time on `CA-20`–`CA-25`; they are interleaved
with control-plane work.

Mitigation: sequence rather than parallelise. Phase A, then Phase B. If that is too
slow, cut scope from Phase B rather than compressing Phase A — but the cut is
*breadth* (everything that does not directly expose or control the runtime), not
the client UI, which the review correctly established is part of the runtime being
usable at all (see R-22). The unaddressed half of this risk is that nobody has
sized the work: see R-23.

### R-04 — The control plane becomes a credential vault for every runtime

**Coverage: Partial (`CL-13`, `CL-14`, `CL-18`).**

To command a runtime, the control plane must authenticate as an administrator of
that runtime. Today that means holding each runtime's `OTTER_API_TOKEN` — an admin
credential that can read all state, trigger any job, and read runs
(`internal/api/server_test.go` distinguishes admin tokens from run-scoped ones).
So the control plane accumulates N powerful credentials, one per client.

This sits awkwardly beside `CL-13` ("the control plane never holds client
secrets"). A runtime admin token is not a *business* secret, but it is a blast
radius across that client's whole runtime. A compromise of the control plane is
therefore a compromise of every client's runtime, which is precisely the outcome
the one-VM-per-tenant design exists to prevent.

Leading indicator: the control plane's database contains a plaintext admin token
per runtime, and nothing bounds what those tokens can do.

Mitigation (adopted): define the runtime credential as a purpose-built,
least-privilege control credential — not the admin token — restricted to the
command surface the gateway actually uses, revocable per runtime (`CL-14`), and
never able to read business secrets.

This is **a new runtime feature, not a configuration change.** The runtime today
issues an admin token, per-run tokens and webhook tokens, and nothing purpose-built
for a gateway (`internal/api/server.go`). Scoping it is runtime work on the critical
path for Phase B and must be estimated as such (`CL-21`). Treat control-plane
compromise as a first-class threat model, not a hardening afterthought.

### R-05 — A client's job dies silently and nobody learns

**Coverage: Partial (`CA-30`, `CA-31`, `CL-07`, `CL-16`).**

Notification is best-effort by design: a bounded in-process retry, no durable
outbox ([notify.go](../internal/notify/notify.go) line 9). If the daemon is down,
or the retry window passes during a blip, the alert is gone. The plan adds
control-plane heartbeat monitoring, which is the right fix — but it also makes the
control plane the only thing watching, and the control plane can be down too.

Why it matters: the value proposition is "leave it running and understand when it
fails." An alpha where a client's scheduled sync fails on Friday and is discovered
on Monday has not delivered the product.

Leading indicator: the first week of production has no alert traffic at all —
either nothing failed, or nothing is reaching anyone.

Mitigation: keep two independent paths (`CA-30` runtime-side, `CA-31`
control-plane-side), monitor the control plane on a path that does not depend on
itself (`CL-16`), and test alert delivery as a drill, including the case where the
webhook endpoint is down for an hour.

---

## High

### R-06 — Celigo and Otter both mutate the same records

**Coverage: Covered (`CA-01`).**

Two systems writing the same NetSuite records with no arbitration is a correctness
defect shipped on purpose. The plan addresses it, but the risk is that `CA-01` is
treated as a formality and approved as "yes, both may write, we'll be careful."

Leading indicator: the ownership matrix is vague about who owns *writes*, or names
two owners for one object.

Mitigation: read-only for the first pilot. Make widening to writes a separate,
explicitly-argued decision with the client.

### R-07 — The control plane is built against an unfrozen runtime API

**Coverage: Not covered.**

`v0.4.0` is the release that freezes the manifest schema, Python SDK, CLI JSON and
the **HTTP API** ([product-roadmap.md](product-roadmap.md) lines 124–146). It has
not shipped. The control plane will bind tightly to that API — job listing, run
listing, run detail, logs, trace, capture metadata, and every control command.

Why it matters: this inverts the normal dependency. Instead of the runtime holding
still while clients build on it, the control plane starts depending on an interface
that is explicitly still moving. Every runtime API change then forces a
control-plane change, and the fleet — at 2–5 runtimes on possibly different
versions — must be served by one control plane.

Leading indicator: the first control-plane change made purely because a runtime
refactor moved an endpoint or reshaped a JSON envelope.

Mitigation: freeze the subset of the runtime API the control plane consumes, before
Phase B starts. That is a smaller, earlier version of `v0.4.0` — a written
compatibility promise covering only the endpoints the gateway uses. Also define a
runtime↔cloud version compatibility policy: the plan has a `runtime version` field
but no statement of which control plane serves which runtime versions.

### R-08 — Backup restores history but not runnable jobs

**Coverage: Covered (`CA-21`).**

The documented backup copies `otter.db` only ([operations.md](operations.md) lines
399–441), while releases live on disk and job identity is a `.otter-id` marker in
the source directory ([security.md](security.md) lines 468–479). The plan now
explicitly demands more than SQLite. The residual risk is that `CA-21` is proven on
a workspace the test itself created, not on a real client's host with real releases
and real prepared environments.

Leading indicator: the restore drill passes in CI but has never been run against a
backup taken from a live client VM.

Mitigation: run the restore drill once, early, against Client A's real host into a
throwaway VM — before the client has data worth losing. Do it while it is cheap.

### R-09 — A retry runs newer code than its parent

**Coverage: Covered (`CA-25`).**

The retry binding is implemented but unproven, and stated as an explicit
non-guarantee ([runtime-contract.md](runtime-contract.md) lines 296–312). If a
deploy lands between attempt 1 and its retry, the retry may execute different code
against the same durable state — a class of bug that is very hard to diagnose from
the client's side.

Leading indicator: any client production release deployed while runs are queued or
retrying.

Mitigation: `CA-25` before any client schedule is enabled, and a rule that client
releases are deployed during quiet windows, not while work is in flight.

### R-10 — No memory/CPU cap; one job takes a client's host down

**Coverage: Covered (`CA-08`).**

The manifest bounds `timeout` and `concurrency` only. Nothing bounds memory or CPU
([manifest-reference.md](manifest-reference.md) lines 84, 237). A leaking Python
job takes the host, the daemon, every other job on that runtime, and the control
plane's view of it.

Leading indicator: the first `MemoryMax` is set at the systemd level only, with no
per-job distinction — one greedy job starves its neighbours on the same host.

Mitigation: `CA-08` proves a runaway process is killed and the daemon survives.
Note that with one client per VM this is contained to that client, which is an
argument for keeping one-tenant-per-host even when it is inconvenient.

### R-11 — Client-visible state diverges from runtime truth

**Coverage: Partial (`CL-05`, `CL-11`).**

The plan is careful that the *runtime* is authoritative and the cloud projection is
disposable. The unaddressed inverse: users make decisions on the projection. If a
client clicks Pause and the command is lost, the UI shows paused while the job
keeps running. If the projection lags, an operator diagnosing an incident reads a
state that was true thirty seconds ago — or worse, reads "succeeded" for a run that
has since been retried.

Leading indicator: the UI shows a control as applied before the runtime confirms
it.

Mitigation: control commands are requests with explicit `requested` and `confirmed`
states, and the UI shows the difference. `CL-11`'s idempotency keys are the
mechanism; the state machine is the missing piece.

### R-12 — A paying client runs production on a pre-1.0 runtime

**Coverage: Partial (`CA-22`).**

The runtime is pre-1.0 and the contract freeze is `v0.4.0`. Carrying a client's
production means every subsequent runtime release is a decision, and the plan does
not say who makes it, how it is tested against that client's jobs, or how a client
is pinned to a version.

Leading indicator: a client runtime is upgraded to get a feature the client did not
ask for.

Mitigation: pin each client runtime to a known-good version. Upgrade deliberately,
one client at a time, after `CA-22` passes against that client's job. Never
auto-upgrade a client's production runtime.

### R-13 — Plaintext secrets on the host, and on the operator's machine

**Coverage: Partial (`CA-12`, `CA-15`).**

There is no built-in secret backend; `SecretProvider` is an extension point
([manifest-reference.md](manifest-reference.md) lines 284–285). Credentials live in
a `0600` `EnvironmentFile`, and `otter deploy` writes them there from the operator's
machine — so the operator's laptop holds every client's Amazon, NetSuite and Celigo
credentials ([deploy/config.go](../internal/deploy/config.go) line 160).

For one client on one host this is a defensible alpha posture. It stops being
defensible the moment there are five, because the credential concentration moves to
the least-governed machine in the system.

Leading indicator: the deploy machine's `.otter` state or shell history contains
client credentials, and rotation requires a re-deploy.

Mitigation: record the decision explicitly (`CA-15`), keep the deployment state
directory out of backups and off shared machines, and treat the operator laptop as
in scope for `CA-13` rotation. Plan the secret backend before client three, not
client five.

### R-14 — Logs and captures leak into the multi-tenant plane

**Coverage: Covered (`CL-12`).**

"Cloud may store a projection for UI responsiveness and search" is ambiguous, and
the wrong reading centralises every client's run logs and HTTP captures — order
payloads, customer PII, and near-credential data — in one multi-tenant store. That
would make the control plane the single richest target in the system and undercut
the whole one-VM-per-tenant rationale ([security.md](security.md) line 115).

Leading indicator: a control-plane table contains log lines or payload bodies; or a
"search runs by payload" feature request arrives before tenancy is settled.

Mitigation: `CL-12` as written — metadata and status cross the boundary, bytes do
not, the UI proxies on demand. Make it a tested property: assert a client's log
bytes are absent from control-plane storage and backups.

### R-23 — "Thin" is unquantified

**Coverage: Not covered. Added on review.**

Neither this register nor its companion puts a size on the alpha. The review's
principal defence is that the Cloud stays *thin*, and as a direction that is right —
but nothing makes the claim falsifiable.

The enumerated alpha surface is a multi-tenant web application: users,
organizations, memberships, per-runtime credential lifecycle, an outbound command
protocol with idempotency and an acknowledgement state machine, heartbeat,
projections, and an on-demand proxy for logs and captures. On top of that sit the
runtime-reliability gate, reproducible provisioning, and the Castor migration.

Why it matters: R-03 is the risk most likely to sink this alpha, and it cannot be
assessed without a number. "Thin" that means six engineer-weeks and "thin" that
means six engineer-months are different plans, and they disagree about whether the
gate is even reachable.

Leading indicator: `CL-06` is estimated without a UI design; the schedule is
expressed only as a phase order.

Mitigation: size the alpha Cloud per workstream, in engineer-weeks, with the scoped
runtime credential (`CL-21`) counted as runtime work rather than a control-plane
detail. The sizing is what makes R-03 answerable — see
[cloud-alpha-readiness.md](cloud-alpha-readiness.md) §17.

---

## Medium

### R-15 — The control plane is a single point of failure for all clients

**Coverage: Covered (`CL-15`, `CL-16`). Severity downgraded to Low on review.**

Execution continues when the control plane is down — that is the design, and it is
right. This is largely *reduced by the architecture* rather than merely tolerated:
schedules keep running, durability stays local, and client data stays on the
runtime. The temporary loss is management and visibility, which is precisely why
execution must not depend on Cloud. Control-plane high availability can wait until
the cost of Cloud downtime justifies it.

The residual is small but real: break-glass is documented and never rehearsed, so
it is discovered to be broken during the outage.

Leading indicator: no one has opened a client runtime over SSH since it was
provisioned.

Mitigation: exercise break-glass once per client during onboarding, while it is a
drill rather than an emergency.

### R-16 — Capacity to support 2–5 clients plus Castor does not exist

**Coverage: Partial (`CA-50`, `CA-51`).**

The plan commits to on-call, incident ownership, response windows and per-client
runbooks. It does not say who. If that is one person, the client is buying a
service with a bus factor of one, and a simultaneous Castor migration makes it
worse.

Leading indicator: the on-call rota has one name on it.

Mitigation: name the person and the response window honestly in the client
agreement (`CA-50`), even if it is a single name and business hours. An honest
narrow commitment is worth more than an implied broad one.

### R-17 — Storage grows unbounded across four different stores

**Coverage: Partial (`CA-19`, `CA-33`).**

Four things grow: the SQLite database, release snapshots, prepared Python
environments and the uv cache, and HTTP capture rows. Retention exists for run logs
and captures; releases prune on deploy but only with a keep window; environment GC
is "deliberately deferred" ([managed-python.md](managed-python.md) line 480).

Leading indicator: disk usage on a client host grows monotonically across a month
with no retention event in the logs.

Mitigation: `CA-33` with an explicit alert threshold per store, and a disk-fill
drill — because the failure mode is not "slow", it is "the daemon cannot write".

### R-18 — "Exit 0" is mistaken for a correct business outcome

**Coverage: Covered (`CA-46`).**

A job can exit zero having written nothing, or having written the wrong thing. The
plan requires independent verification of expected effects, which is right, but it
is the item most likely to be quietly dropped when schedules are turned on under
time pressure.

Leading indicator: the pilot's acceptance evidence is run statuses rather than
destination-system queries.

Mitigation: make the pilot's definition of success a destination-side assertion —
"the order appears once in NetSuite" — not a run status.

### R-19 — The alpha's definition of done is satisfiable by inspection

**Coverage: Reclassified on review — a standing rule, not a risk.**

The observation was right: the plan has many MUST items whose "done when" phrasing
a reader could satisfy by reading code ("isolation exists", "secrets are scoped",
"redaction verified"). But that is not something to *mitigate*; it is a rule to
*apply*, and it needs no project or owner of its own.

**Standing rule: every gate item must cite executable or recorded evidence** — a
test, or a dated recorded run. A behavior is not proven because a document says it
exists. This mirrors the runtime contract's evidence statuses (`Real` / `Simulated`
/ `Partial` / `None`).

Leading indicator: a gate line ticked with a file reference instead of a test or a
recorded run.

### R-20 — Single-tenant-per-client economics do not improve with scale

**Coverage: Not covered. Reclassified on review: a benefit at alpha scale, not a
risk.**

This was over-weighted. One VM per client does mean flat per-client infrastructure
cost, but at 2–5 clients that is not a problem to solve — it buys stronger
isolation, simpler debugging, simpler resource accounting, a smaller blast radius,
easier recovery, and easier offboarding. Optimizing marginal cost before
establishing product value is backwards.

The original severity implied an architectural objection. There is none.

What survives is a pricing note, not a risk: do not price the alpha by analogy with
multi-tenant SaaS, where marginal cost approaches zero. Price it as a managed
service with a floor that covers a dedicated host plus a share of control-plane
overhead, and state that scale efficiency arrives only with the out-of-scope
shared-runtime architecture.

Leading indicator: pricing set by analogy with multi-tenant SaaS.

### R-21 — "Reasonable" is an undefined reason to stop paying

**Coverage: Not covered.**

The engagement is conditional on the arrangement being "reasonable", which is not a
measurable term. Meanwhile the plan invests in a control plane whose cost is
largely sunk before the first invoice.

Leading indicator: the agreement contains no acceptance criteria, no minimum term,
and no defined exit.

Mitigation: get a commitment that survives the alpha — a minimum term, or payment
for a defined first milestone (e.g. one job in production for 30 days with alerts).
Convert a subjective condition into a milestone before the expensive phases start.

### R-22 — The original problem is still unsolved

**Coverage: Reclassified on review — product positioning, not a risk at any phase
gate.**

This risk originally carried the recommendation to defer the client UI (`CL-06`).
**That recommendation is withdrawn.** For a remotely hosted runtime, a management
surface is not a second product — it is part of the runtime being usable. The
SSH-and-CLI path is adequate for the developer building Otter and is not an
adequate interface for a customer operator, least of all a customer whose current
tool is Celigo, a point-and-click iPaaS. The original framing assumed a
developer-shaped user, which does not describe this client.

What survives is the observation both documents now share: the runtime's compelling
claim is "I `kill -9` it mid-run and nothing accepted is lost", and no harness
demonstrates it (`CA-20`). A dashboard makes the product easier to *try*, not
easier to *understand*.

Leading indicator: a demo that opens with the dashboard and never shows a crash.

**Not a Phase 0 blocker.** Castor does not need a canonical sales demo before a
scheduled import can move safely. `CA-20` is built because crash safety must be
proven; its value as a demo is a bonus, not a readiness requirement.

Mitigation (positioning only): keep `CA-20` as the canonical demo — a real kill, on
camera, in sixty seconds — and ensure the UI *exposes runtime behaviour* rather
than becoming an unrelated visual workflow product.

---

## Accepted risks

Consciously not mitigated for the alpha, and worth stating to the client:

- **No high availability.** A host failure takes that client's runtime down until a
  manual restore (`CA-21`). RTO is bounded by how fast a human runs the drill.
- **No control-plane SLA.** Outages stop visibility, not execution (`CL-15`).
- **Best-effort notification.** Mitigated by a second path, not eliminated (R-05).
- **No per-job memory or CPU limits below the host level** beyond `CA-08`'s
  systemd caps.
- **Pre-1.0 interfaces.** The runtime API may change until `v0.4.0`; each client is
  pinned and upgraded deliberately (R-07, R-12).
- **macOS and Windows hosts.** The alpha host is Linux, which is also where the only
  real child-lifetime guarantee exists (`Pdeathsig`).

## Tripwires

Signals that should trigger a re-plan rather than a status update:

1. `CA-20` produces a result that contradicts the contract. Stop; fix the runtime.
2. Any Phase B work lands before `CA-20` passes (R-01).
3. A pilot job inserts rather than upserts (R-02).
4. The control plane holds a runtime's full admin token (R-04).
5. A runtime API change forces a control-plane change before the API subset is
   frozen (R-07).
6. A client's restore drill has never been run against that client's real backup
   (R-08).
7. Nobody is named on-call, or the rota has one name (R-16).
8. Disk on a client host grows monotonically with no retention activity (R-17).
9. A Phase 0 blocker is stated as something only observable after cutover — the
   gate has gone circular (R-01).
10. The client agreement has no acceptance criteria (R-21).

## The five changes that reduce the most risk

Most of this register cannot be closed by writing code; it shrinks by making
different sequencing decisions.

1. **Make Phase A stoppable.** A recorded, dated decision that can be "stop" (R-01,
   R-03).
2. **Keep Otter read-only for the first pilot.** Structurally removes duplicate
   writes instead of contractually avoiding them (R-02, R-06).
3. **Freeze the control-plane subset of the runtime API before Phase B.** A small,
   early `v0.4.0` (R-07, R-12).
4. **Give the control plane a least-privilege runtime credential, not the admin
   token.** Confines a control-plane compromise to the gateway's command surface
   (R-04).
5. **Size the alpha Cloud, and name the owner and date that make the gate real.**
   R-01 and R-03 are the two risks most likely to sink this alpha, and neither is
   closed by code — one needs ownership, the other needs a number (R-01, R-03,
   R-23).

## Related

- [cloud-alpha-readiness.md](cloud-alpha-readiness.md) — the tasks these risks
  attach to.
- [runtime-contract.md](runtime-contract.md) — the guarantees and non-guarantees
  behind R-02, R-09 and R-18.
- [product-roadmap.md](product-roadmap.md) — the gate that R-01 risks hollowing out.
- [security.md](security.md) — the runtime trust model that R-04 and R-14 extend.
