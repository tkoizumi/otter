# Otter product roadmap

Status: proposed product sequencing. Date: 2026-09-24.

**Make integrations dependable on one machine. Prove people can operate them.
Then help people build them faster and operate them in the cloud.**

Otter's initial customer is a developer or small engineering team running
business integrations written in Python. The first product promise is simple:
deploy ordinary code, leave it running, and understand and recover when it fails.

This roadmap uses readiness gates rather than committed dates. The current
documentation describes substantial runtime functionality; it does not by itself
prove production readiness. Items below mean verify, harden, or fill a demonstrated
gap, not automatically rebuild an existing feature.

## Sequence

| Phase | Customer outcome | Main investment | Exit gate |
| --- | --- | --- | --- |
| 1. Dependable execution | “My work is accounted for, even after a crash.” | Execution, queue, retries, state, releases | Failure and recovery contracts pass repeatable validation |
| 2. Operable runtime | “I can deploy, diagnose, upgrade, and recover it myself.” | Diagnostics, lifecycle, capacity, backups, security | A second operator completes the operating drills without author intervention |
| 3. Runtime validation | “I trust it with an unattended integration.” | Real workloads, support feedback, compatibility | Representative pilots meet the runtime readiness gate |
| 4. Development harness | “I can reproduce a failure and verify a repair before release.” | Isolated scenarios, effects, evidence | A failed integration is repaired and the tested artifact is promoted |
| 5. Managed cloud | “I get the same runtime without maintaining a host.” | Provisioning, isolation, backups, upgrades, access | Managed pilots pass operational and isolation gates |

Phases 1–3 are the foundation. No cloud or harness product implementation starts
before the runtime readiness gate. Customer interviews and paper designs can
continue throughout. Runtime tests and fault injection are foundation work and
must not wait for the developer-facing harness.

Default to the harness before cloud: it builds on runtime diagnostics and can
reduce integration debugging effort. After Phase 3, reverse that order if pilot
evidence shows host operations are the larger adoption blocker. Avoid launching
both tracks at once with a small team.

## 1. Dependable execution

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

## 2. Operable runtime

Make the entire lifecycle usable outside this repository.

- **First success.** Install → initialize → validate → release → run → inspect
  state. Errors explain what failed and the next useful action. Validate this in
  a fresh workspace without source-checkout tooling.
- **Explain a run.** Connect trigger, release, attempt chain, timestamps, failure
  reason, logs, and captured requests. Prioritize the proposed run timeline where
  it reduces diagnosis time. Distinguish missing capture from “no request made.”
  Capture is diagnostic evidence; it is not automatically replayable input.
- **Operate unattended.** Expose worker health, queue age/depth, retry activity,
  storage pressure, and actionable failure signals through CLI/API and documented
  monitoring integration. A custom dashboard is not required for this phase.
- **Recover and maintain.** Verify backup and restore of database plus necessary
  release/environment artifacts, upgrades and migrations, supported rollback,
  retention, secret rotation, and deployment failure recovery. Code rollback does
  not reverse external data changes; database downgrade support must be explicit.
- **Define the trust boundary.** Verify scoped API access, credential handling,
  redaction, and filesystem permissions. Document that trusted Python runs with
  the host user's privileges; process separation is not tenant isolation.
- **Stabilize public contracts.** Specify manifest, SDK, CLI JSON, and API
  compatibility, supported platforms, and deprecation policy. Add extension
  points only when a demonstrated runtime requirement needs them.

Exit evidence: an operator who did not build Otter deploys an integration,
diagnoses a seeded failure, rotates a secret, upgrades, and restores onto a clean
host using published instructions. Record failures and repeat affected drills
after fixes.

## 3. Runtime validation and release gate

Use a small set of real integrations in separately owned integration projects.
Include a scheduled incremental sync, a webhook-driven flow, and a longer
paginated job. Exercise rate limits, repeated delivery, partial completion, and
ambiguous remote outcomes with controlled tests.

The following are proposed acceptance targets, not current results or SLAs:

| Dimension | Required evidence before expansion |
| --- | --- |
| Correctness | All critical fault-matrix cases pass; no unexplained lost accepted work or acknowledged state loss during validation |
| Recovery | Restart, restore, upgrade, and rollback drills pass within recovery objectives agreed before the pilot |
| Unattended operation | At least three representative integrations across at least two independently operated deployments run for 30 consecutive days without a runtime-caused incident requiring manual repair |
| Diagnosis | A non-author identifies the cause and next recovery action for each seeded failure using supported diagnostics |
| Usability | At least two developers independently complete install through first successful run using the documentation |
| Capacity | A published supported-load envelope includes queue latency, resource use, storage growth, and overload behavior |
| Release quality | No open release-blocking correctness, credential-exposure, or upgrade defects; compatibility and operating limits are documented |

Track runtime failures separately from integration bugs and upstream failures.
A successful Python exit alone does not prove correct business effects. Use
independently checked destination outcomes for pilot acceptance. A material
runtime incident restarts the affected reliability observation after the fix.

At the gate review, publish the evidence and remaining limitations, then make an
explicit go/no-go decision. Calendar pressure is not a substitute for passing.
Continue runtime maintenance and regression validation after expansion.

## 4. Development harness

The first harness product closes one loop: **reproduce → repair → verify →
promote**. Start with one transport and one representative integration, then
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

## 5. Managed cloud

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

## Immediate planning backlog

1. Inventory each runtime contract as documented, implemented, tested, or proven
   in operation; name an accountable owner and attach evidence. Start with
   recovery, state, release binding, cancellation, and retry behavior.
2. Turn missing critical evidence into a ranked fault-validation backlog. Fix
   correctness and recoverability gaps before adding convenience features.
3. Complete a single run-diagnosis path and a clean-host restore drill.
4. Select pilot integrations and operators; agree load, recovery objectives,
   incident classification, and measurement before observation begins.
5. Review the readiness scorecard weekly. Keep harness and cloud implementation
   queued until the gate passes, then fund one next product outcome.

Planning basis: [README](../README.md), [architecture](architecture.md),
[operations](operations.md), [security model](security.md), and the proposed
[harness plan](otter-harness-implementation-plan.md). This roadmap is grounded in
those documents; it is not a code audit or a certification of current readiness.
