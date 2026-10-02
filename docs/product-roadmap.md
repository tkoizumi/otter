# Otter product roadmap

Status: proposed product sequencing. Date: 2026-09-24.

**Make jobs dependable on one machine. Prove people can operate them.
Then help people build them faster and operate them in the cloud.**

Otter's initial customer is a developer or small engineering team running
business jobs written in Python. The first product promise is simple:
deploy ordinary code, leave it running, and understand and recover when it fails.

This roadmap uses readiness gates rather than committed dates. The current
documentation describes substantial runtime functionality; it does not by itself
prove production readiness. Items below mean verify, harden, or fill a demonstrated
gap, not automatically rebuild an existing feature.

## Release ladder

The phases below carry the release numbers. A phase is not "done" until its exit
gate passes, so a version number here means a phase closed, not a date reached.

| Version | Phase | The release promises |
| --- | --- | --- |
| `v0.2.0` | 1. Dependable execution | Accepted work is never lost, including across a hard kill |
| `v0.3.0` | 2. Operable runtime | A stranger can install, diagnose, upgrade, and restore it from the docs |
| `v0.4.0` | 2. Operable runtime | Manifest, SDK, CLI JSON, and API stop moving |
| `v0.5.0` | 2. Operable runtime | It holds under load, and its limits are published |
| `v0.6.0` | 3. Runtime validation | Real jobs survive real time (release candidate) |
| `v1.0.0` | Freeze | The contract holds, with no new surface |

Phases 4 and 5 are post-1.0 tracks: neither takes a runtime version number until
Phase 3's gate produces the evidence that justifies funding it.

`v1.0.0` adds no features. It is the freeze release: the runtime contract,
compatibility commitment, and supporting evidence are published, and nothing
ships that those documents do not already cover.

## Sequence

| Version | Phase | Customer outcome | Exit gate |
| --- | --- | --- | --- |
| `v0.2.0` | 1. Dependable execution | “My work is accounted for, even after a crash.” | Failure and recovery contracts pass repeatable validation |
| `v0.3.0` | 2. Operable runtime | “I can deploy, diagnose, upgrade, and recover it myself.” | A second operator completes the operating drills without author intervention |
| `v0.4.0` | 2. Operable runtime | “I can build against it without it moving underneath me.” | Compatibility and deprecation policy published, with contract tests |
| `v0.5.0` | 2. Operable runtime | “I know what it will do when it is busy.” | A supported-load envelope is published and overload behaves as documented |
| `v0.6.0` | 3. Runtime validation | “I trust it with an unattended job.” | Representative pilots meet the runtime readiness gate |
| `v1.0.0` | Freeze | “I can depend on this.” | Contract frozen, compatibility commitment published, no open release blockers |
| post-1.0 | 4. Development harness | “I can reproduce a failure and verify a repair before release.” | A failed job is repaired and the tested artifact is promoted |
| post-1.0 | 5. Managed cloud | “I get the same runtime without maintaining a host.” | Managed pilots pass operational and isolation gates |

Phases 1–3 are the foundation. No cloud or harness product implementation starts
before the runtime readiness gate. Customer interviews and paper designs can
continue throughout. Runtime tests and fault injection are foundation work and
must not wait for the developer-facing harness.

Default to the harness before cloud: it builds on runtime diagnostics and can
reduce job debugging effort. After Phase 3, reverse that order if pilot
evidence shows host operations are the larger adoption blocker. Avoid launching
both tracks at once with a small team.

**Amended 2026-10-02 (records `CA-55`).** For the `v0.4.0` + Otter Cloud Phase B
track the sequencing above is superseded: the control plane and a demo UI are
built *in parallel* with the runtime, not after the Phase 3 gate. The runway is
that Castor is a real second consumer, the runtime work those tracks need is the
interface rather than a gate, and UI and projections absorb runtime churn cheaply
while identity and credentials do not. The current sequence, its guardrails, and
the reframed gates are in
[v0.4.0-and-cloud-phase-b-plan.md](v0.4.0-and-cloud-phase-b-plan.md).

## v0.2.0 — Phase 1. Dependable execution

Prioritize correctness over expanding the command surface.

- **Explicit execution semantics.** Define trigger acceptance, durable enqueue,
  claim, execution, terminal state, cancellation, and retry transitions. Document
  missed cron occurrences and duplicate webhook delivery. An accepted request
  must resolve to durable work or an explicit, inspectable failure.
- **Crash and restart recovery.** Verify daemon death, child death, interrupted
  shutdown, and restart during retry backoff. Account for queued work and active
  attempts; verify descendants cannot continue unnoticed after cancellation or
  recovery on supported platforms.
- **Durable state with honest guarantees.** Verify acknowledged writes survive
  supported crash scenarios. Specify concurrent state-update behavior. External
  API effects and local checkpoints are not one transaction: retries can repeat
  effects, so document idempotency and recovery patterns without promising
  exactly-once execution.
- **Reproducible releases.** Verify queued runs and retries retain their bound
  code and environment. Exercise shared libraries, managed dependencies,
  activation, rollback, and retention of artifacts still needed by pending work.
  Distinguish external Python's guarantees from managed Python's guarantees.
- **Bounded behavior.** Define queue admission, concurrency, retry, payload, log,
  capture, and storage limits. Exercise database contention, disk exhaustion,
  large output, and overload; failures must be visible and recovery documented.

Exit evidence: a versioned runtime contract and automated fault matrix covering
these cases, with no unresolved critical correctness failures. Performance targets
must name hardware, workload, and supported load; establish a baseline before
choosing latency and throughput thresholds.

## v0.3.0 — Phase 2. Operable runtime

Make the entire lifecycle usable outside this repository.

This version is the first half of Phase 2: the runtime becomes diagnosable,
recoverable, and maintainable by someone who did not build it. The contract and
capacity halves of Phase 2 follow as `v0.4.0` and `v0.5.0`, so the exit evidence
below covers the phase as a whole rather than this version alone. The
stabilize-public-contracts work from this phase is specified under `v0.4.0`.

- **First success.** Install → initialize → validate → release → run → inspect
  state. Errors explain what failed and the next useful action. Validate this in
  a fresh workspace without source-checkout tooling.
- **Explain a run.** Connect trigger, release, attempt chain, timestamps, failure
  reason, logs, and captured requests. Prioritize the proposed run timeline where
  it reduces diagnosis time. Distinguish missing capture from “no request made.”
  Capture is diagnostic evidence; it is not automatically replayable input.
- **Operate unattended.** Expose worker health, queue age/depth, retry activity,
  storage pressure, and actionable failure signals through CLI/API and documented
  monitoring job. A custom dashboard is not required for this phase.
- **Recover and maintain.** Verify backup and restore of database plus necessary
  release/environment artifacts, upgrades and migrations, supported rollback,
  retention, secret rotation, and deployment failure recovery. Code rollback does
  not reverse external data changes; database downgrade support must be explicit.
- **Define the trust boundary.** Verify scoped API access, credential handling,
  redaction, and filesystem permissions. Document that trusted Python runs with
  the host user's privileges; process separation is not tenant isolation.

Exit evidence: an operator who did not build Otter deploys a job,
diagnoses a seeded failure, rotates a secret, upgrades, and restores onto a clean
host using published instructions. Record failures and repeat affected drills
after fixes.

## v0.4.0 — Phase 2. Contract freeze

The runtime stops being a moving target for anyone building against it. This is
still Phase 2 work: the point is not new surface, it is making the existing
surface a promise.

- **Freeze the interfaces.** Manifest schema, Python SDK, CLI JSON, and the HTTP
  API each get a written compatibility and deprecation policy. A breaking change
  needs a versioned path and a deprecation window, not a patch release.
- **Version the machine-readable output.** Stable, versioned JSON envelopes for
  the commands and endpoints scripts consume, so automation is not built on an
  unversioned shape.
- **State the platform contract.** Name the supported OS and architectures, and
  mark the rest unsupported rather than leaving them implicit. "Unsupported but
  documented" is a valid answer; "undocumented" is not.
- **State the honest limits.** Publish what is guaranteed and what is not: state
  update concurrency, retry effect duplication, downgrade support, and transport
  security. These belong in the contract, because a guarantee discovered by a
  user in production is a defect report.

Exit evidence: the compatibility and deprecation policy is published, contract
tests pin each frozen interface, and every documented limit matches observed
behavior.

## v0.5.0 — Phase 2. Capacity and bounded behavior

Define what happens when the runtime is busy, and publish it.

- **Bound admission.** Queue depth, payload, log, capture, and storage limits are
  explicit, with visible behavior when a limit is reached. Backpressure or
  refusal must be inspectable, never silent.
- **Publish a supported-load envelope.** Queue latency, resource use, storage
  growth, and overload behavior, measured against named hardware and workload.
- **Exercise the edges.** Database contention, disk exhaustion, large output, and
  sustained overload. Failures must be visible and recovery documented.

Exit evidence: the supported-load envelope is published, overload behaves as
documented, and the performance targets name the hardware and workload they were
measured on.

## v0.6.0 — Phase 3. Runtime validation and release gate

Use a small set of real jobs in separately owned job projects.
Include a scheduled incremental sync, a webhook-driven flow, and a longer
paginated job. Exercise rate limits, repeated delivery, partial completion, and
ambiguous remote outcomes with controlled tests.

The following are proposed acceptance targets, not current results or SLAs:

| Dimension | Required evidence before expansion |
| --- | --- |
| Correctness | All critical fault-matrix cases pass; no unexplained lost accepted work or acknowledged state loss during validation |
| Recovery | Restart, restore, upgrade, and rollback drills pass within recovery objectives agreed before the pilot |
| Unattended operation | At least three representative jobs across at least two independently operated deployments run for 30 consecutive days without a runtime-caused incident requiring manual repair |
| Diagnosis | A non-author identifies the cause and next recovery action for each seeded failure using supported diagnostics |
| Usability | At least two developers independently complete install through first successful run using the documentation |
| Capacity | A published supported-load envelope includes queue latency, resource use, storage growth, and overload behavior |
| Release quality | No open release-blocking correctness, credential-exposure, or upgrade defects; compatibility and operating limits are documented |

Track runtime failures separately from job bugs and upstream failures.
A successful Python exit alone does not prove correct business effects. Use
independently checked destination outcomes for pilot acceptance. A material
runtime incident restarts the affected reliability observation after the fix.

At the gate review, publish the evidence and remaining limitations, then make an
explicit go/no-go decision. Calendar pressure is not a substitute for passing.
Continue runtime maintenance and regression validation after expansion.

## post-1.0 — Phase 4. Development harness

The first harness product closes one loop: **reproduce → repair → verify →
promote**. Start with one transport and one representative job, then
expand from demonstrated demand.

1. Run the real Otter runtime in a disposable, enforced offline environment with
   seeded state and controlled triggers.
2. Reproduce a concrete failure using fixtures, stateful fake services, and fault
   injection. Incomplete captured evidence must be reported as incomplete.
3. Check intended effects and state against independently specified assertions,
   including duplicate delivery and timeout after a remote commit.
4. Let an existing coding agent use the same tools as a human to repair Python
   and produce a reviewable change with test evidence.
5. Bind evidence to code, dependencies, environment, fixtures, and assertions;
   verify that the immutable artifact promoted is the artifact tested.

Exit gate: a seeded duplicate-write or checkpoint bug is reproduced, repaired
without weakening expectations, and verified before promotion. Track false
passes, correct repairs, reproduction success, and time to reviewable evidence.

Keep the harness independently versioned in its own repository. Add narrowly
scoped runtime capabilities only as this slice requires them. The existing
[harness implementation plan](otter-harness-implementation-plan.md) supplies
technical detail; its milestones begin after the runtime readiness gate here.
Defer custom agent orchestration, broad connector coverage, live production
writes, and autonomous data repair.

## post-1.0 — Phase 5. Managed cloud

Start with managed operation of the proven runtime for one trusted team per
isolated deployment. Preserve the same Python, manifests, SDK, release identity,
and execution semantics used locally.

The first paid-value hypothesis is removing host maintenance: provision a runtime,
deploy a release, supply secrets, inspect runs, receive failure notifications,
and get managed backups and upgrades. Validate willingness to pay before broad
platform investment.

Cloud-specific requirements include authenticated access and roles, tested tenant
boundaries, secret lifecycle, quotas and resource limits, audit records, recovery,
upgrade orchestration, usage visibility, and support procedures. Keep these in
the managed service where possible; avoid a second execution engine.

Exit gate: managed pilots demonstrate provisioning, access isolation, restore,
upgrade recovery, and useful incident notification against agreed service
objectives. Measure deployment success, operator effort, incident rate, retention,
and cost per active deployment before widening availability.

Defer shared multi-tenant workers, distributed scheduling, multi-region operation,
a workflow canvas, and a connector marketplace until customer requirements and
measured limits justify them.

## v1.0.0 — Freeze

`v1.0.0` is not a feature release. By this point the phases above have passed
their gates, and the only work left is to state the commitment and publish the
evidence behind it.

- **Publish the evidence bundle.** The versioned runtime contract, the passing
  fault matrix, the pilot results, the supported-load envelope, and the known
  limitations, in one place a reader can check.
- **State the compatibility commitment.** How long interfaces are supported, what
  counts as a breaking change, and how deprecation is announced.
- **Ship nothing new.** If a capability is not already covered by the frozen
  contract and the evidence, it waits for a post-1.0 release.
- **Confirm no open release blockers.** No unresolved correctness,
  credential-exposure, or upgrade defect at the moment of tagging.

Exit gate: the phase 3 gate review has made an explicit go decision, and the
published contract matches a release that passed the fault matrix. Calendar
pressure is not a substitute for passing.

## Recorded decisions

### 2026-10-01 — The runtime API is a product surface, and schedules are runtime state

**Decision.** The runtime API is a first-class surface for a self-hosted,
single-tenant deployment — not only an internal transport for the CLI and a future
control plane. An operator may reach it remotely with a scoped credential, and
gating that access is the operator's decision rather than a blanket refusal in the
tooling. Schedules become runtime state that the API and the CLI can create,
change and remove, not only a manifest plus `otter reload`.

**Why now.** Two drivers arrived together. Castor's ingestion cannot migrate to a
design that is not close to the end state, and for a single tenant the end state
*is* the runtime API: dynamic schedules (`CL-22`) plus a reachable, scoped API
(`CL-23`). Neither is multi-tenant Cloud work, so neither needs the Phase B gate.
The interim alternative — a tick job claiming a database queue and mirroring sync
status back into Castor — would be discarded at cutover.

**Consequences.**

- `CL-22` and `CL-23` are pulled forward of the Phase B gate and belong to the
  Phase 2 "Operable runtime" track, where `v0.4.0` already promises that the API
  stops moving.
- The scheduler becomes store-backed. The invasive half of that change lands
  before Castor's job is live, while the host runs only paused stand-ins.
- Three artifacts that currently disagree about bind addresses
  (`internal/config/daemon.go`, `internal/deploy/target.go`,
  `scripts/assert-host-permissions.sh`) are reconciled, and `P0-07`'s recorded
  host evidence is re-run rather than amended.
- [cloud-alpha-readiness.md](cloud-alpha-readiness.md) Phase E's "control-plane
  API rather than the runtime API" is reconciled with `CL-10`'s actual wording.
- The interim Castor scheduling shim is cancelled. The durable Castor fixes —
  lease, single-active-run index, cancel, shared import core — proceed regardless,
  because they improve the Lambda path too.

**Design.** [dynamic-schedules-design.md](dynamic-schedules-design.md).

## Immediate planning backlog

1. Inventory each runtime contract as documented, implemented, tested, or proven
   in operation; name an accountable owner and attach evidence. Start with
   recovery, state, release binding, cancellation, and retry behavior.
2. Turn missing critical evidence into a ranked fault-validation backlog. Fix
   correctness and recoverability gaps before adding convenience features.
3. Complete a single run-diagnosis path and a clean-host restore drill.
4. Select pilot jobs and operators; agree load, recovery objectives,
   incident classification, and measurement before observation begins.
5. Review the readiness scorecard weekly. Keep harness and cloud implementation
   queued until the gate passes, then fund one next product outcome.

Planning basis: [README](../README.md), [architecture](architecture.md),
[operations](operations.md), [security model](security.md), and the proposed
[harness plan](otter-harness-implementation-plan.md). This roadmap is grounded in
those documents; it is not a code audit or a certification of current readiness.
The version numbers above assign each phase to a release; they do not imply that
a phase's items are implemented or tested today.
