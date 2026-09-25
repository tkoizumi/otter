# Otter Harness implementation plan

Status: proposed; no harness functionality is implemented by this document.
Date: 2026-09-24.

## Objective

Build a development environment in which a coding agent can create, reproduce,
test, and repair Python integrations running on Otter. The first useful outcome
is: take a failed run, construct a reproducible case, repair the integration, and
produce reviewable evidence that the change satisfies the intended behavior.

The harness owns the development loop. Otter remains the execution runtime.
Use existing coding agents through a skill and structured tools initially; a
custom chat interface or model orchestration layer is not required for v1.

Success means an integration author can answer three questions before release:

1. What records and fields will this code change?
2. What happens when requests fail, repeat, or succeed without a response?
3. Which code, environment, inputs, and assertions produced that evidence?

## Starting point and repository boundary

The implementation must build on these existing seams:

| Existing code | Reuse or extension |
| --- | --- |
| `internal/api/`, `internal/cli/` | Integration resolution, run submission, inspection, state access, cancellation |
| `internal/daemon/`, `internal/queue/`, `internal/runs/` | Real execution and retry semantics; release binding on each attempt |
| `internal/release/`, `internal/pyenv/` | Immutable code snapshots, shared-tree layout, prepared Python environments |
| `internal/executor/executor.go` | Child process launch and environment assembly |
| `sdk/python/otter/_launcher.py` | Install instrumentation before integration imports |
| `sdk/python/otter/_*_capture.py` | Transport-specific observation for urllib, requests, and httpx |
| `internal/inspection/` | Sanitized exchanges, capture completeness and omission reasons |
| `internal/state/`, `internal/identity/` | Durable state and instance ownership |

Current constraints are material: Otter does not sandbox Python; capture neither
blocks traffic nor guarantees a replayable recording; current state is not the
state that existed before an old run; retries can repeat external effects.

Follow `CONTRIBUTING.md`: build the harness in a separate `otter-harness`
repository. Keep runtime contracts and SDK hooks here. Vendor clients, connector
knowledge, integration contracts, and business scenarios belong in the harness
or integration projects. This plan is the cross-repository implementation map.

## Architecture decisions

### Separate orchestrator, real runtime

Ship a Go `otter-harness` CLI with versioned JSON output and a separately
packaged Python replay support library. Invoke the installed Otter CLI/API;
do not import Otter's Go `internal` packages or duplicate its scheduler, retry
engine, state service, or release builder.

Each case execution gets a disposable workspace, data directory, identity, and
Otter daemon. Seed state before submission. Preserve that state across the
case's retries and repeated deliveries; discard it between independent cases.
Keep source identity and run IDs as provenance only. Never copy a production
SQLite database, identity marker, credentials file, or active runtime discovery
file into the test workspace.

Disable cron registration and external webhook ingress in the disposable
daemon through an explicit development execution option. Preserve the original
manifest and retry policy. Provide a development-only way to submit a specified
trigger type/body/headers so webhook and cron cases do not silently execute as
manual triggers. These options must not become remote production switches.

### Enforced offline execution

The first supported execution backend is a Linux container, available directly
on Linux and through a compatible container engine on macOS. This dependency
belongs to the harness, not Otter's normal install.

Run the disposable daemon and Python children within the same container with
external networking disabled. Loopback remains available for the scoped Otter
API and scenario services. Run without privilege, host networking, host socket
mounts, or host credentials; use read-only code/dependency mounts and bounded
writable scratch storage. Drop capabilities, enforce process/memory/time limits,
and terminate the entire container on cancellation or deadline.

Prepare the pinned interpreter and dependencies in a separate setup step before
offline execution. Dependency installation must not happen during replay. Record
image, architecture, interpreter, dependency, runtime, and SDK versions in the
result. Report environment differences from production explicitly.

Python transport interception provides useful fixture matching and diagnostics;
the container provides the external network boundary. Unsupported transports,
raw sockets, subprocesses, redirects, and proxy settings must not escape that
boundary. Missing isolation support is an infrastructure error, never a silent
fallback to unrestricted host execution. This is isolation for integration
testing, not a claim of protection from arbitrary hostile kernel exploits.

### Explicit versions and capability negotiation

Version the project configuration, scenario format, fixture format, result
envelope, and runtime development protocol independently. Add a runtime
capabilities response so the harness can reject unsupported combinations before
launch. Document the minimum compatible Otter version. Existing normal runs
remain unchanged when development options are absent.

## Project artifacts and command surface

Proposed integration project layout:

```text
integration-project/
  integrations/customer-sync/       # existing Otter integration
  harness.yaml                      # integration paths and execution policy
  contracts/customer-sync.yaml      # behavioral requirements and assertion IDs
  cases/customer-sync/
    duplicate-delivery/case.yaml
    duplicate-delivery/initial-state.json
    duplicate-delivery/fixtures.json
  tests/                            # optional project-owned Python assertions
  .otter-harness/                    # ignored run evidence and scratch data
```

Contracts describe matching keys, field ownership, deletion policy, checkpoint
rules, and failure behavior. Each requirement has an ID and links to scenarios.
The harness reports uncovered requirements; a natural-language requirement alone
is not an executable assertion. Do not build a general workflow DSL.

Proposed commands, all new:

```sh
otter-harness init --integration ./integrations/customer-sync
otter-harness doctor
otter-harness case import --runtime dev --run RUN_ID --name rejected-customer
otter-harness case validate rejected-customer
otter-harness test --case rejected-customer --json
otter-harness test --integration customer-sync --json
otter-harness inspect RESULT_ID --json
otter-harness compare BASE_RESULT CANDIDATE_RESULT --json
otter-harness verify-release --evidence RESULT_ID --candidate RELEASE_BUNDLE
```

Use named runtime profiles resolved by the harness; do not accidentally inherit
the current shell's production `OTTER_API_URL`. Read-only import is distinct from
test execution. Every command emits a versioned envelope with status, stable
error codes, artifact paths, coverage, and next actions. Distinguish assertion
failure, invalid case, incomplete evidence, and infrastructure failure. Emit
human diagnostics on stderr when stdout contains JSON.

## Core implementation

### 1. Scenario and fixture engine

A case specifies initial Otter state, trigger deliveries, external-system state,
request/response fixtures or a stateful fake, fault rules, and assertions.
The runner starts the real daemon, seeds state, submits the trigger, waits for
the complete retry chain, then evaluates the final result.

Begin with sequential, bounded JSON HTTP exchanges through `requests` and
`urllib`; add synchronous and asynchronous `httpx` before the v1 release gate.
Advertise coverage per case. Streaming, binary protocols, and arbitrary database
clients are outside initial replay coverage and must fail visibly when needed.

Match on method, normalized URL, selected non-secret headers, canonical JSON
body, and occurrence count. Preserve array order and repeated query parameters.
Ignored fields must be explicit and appear in the report. Ambiguous matches,
unexpected requests, and unused required exchanges fail the case. Never send an
unmatched request to the live destination. Authentication uses synthetic fixture
credentials; do not reconstruct redacted production credentials.

Install replay before imports through a narrow, versioned development hook in
the SDK launcher. Keep replay implementation outside the embedded production
SDK. Production capture remains observational. Avoid stacking conflicting
monkey patches: define adapter ownership and test replay plus capture together.

Support ordering constraints rather than inventing one total order for concurrent
calls. Require an explicitly modeled concurrent case when captures cannot resolve
ordering. Record clock/random dependencies and allow explicit project-level
injection; do not claim whole-program determinism from HTTP replay alone.

### 2. Capture import and missing evidence

Import all pages of the run's exchanges, trigger metadata, retry ancestry, bound
release metadata, and relevant logs through supported APIs. Produce a draft case
and a completeness report. Keep imports in ignored local storage until sanitized
fixtures are deliberately moved into the project.

Classify each missing body, redaction, dropped event, incomplete request, expired
capture, unsupported transport, and unavailable source snapshot. Missing required
information makes the case incomplete. Users or agents may supply synthetic
replacement fixtures, but their provenance must be marked as authored.

Do not seed an old failed run with current production state and call it a
reproduction. V1 requires an explicit initial-state fixture when no historical
snapshot exists. A later opt-in runtime feature can save pre-run state snapshots;
it must account for concurrent runs and must not imply a globally consistent
snapshot of external systems.

Captured responses can reproduce a historical sequence. They cannot establish
how an external system would respond to different code or prove idempotency.
Use stateful fakes or sandbox accounts for those assertions.

### 3. Stateful fakes, faults, and effects

Supply a small generic record-store fake in harness tests; vendor-specific fakes
remain project or connector packages. Its ledger tracks actual simulated record
mutations independently of logs emitted by the integration.

Initial faults: 429 with Retry-After, transient 5xx, malformed response, timeout
before commit, timeout after commit, and process termination at a named boundary.
Use barriers or acknowledged hooks rather than timing sleeps. Fault consumption
and fake-system state persist across retries. Repeated delivery also preserves
destination state so duplicate behavior can be asserted.

Separate requested effects, committed fake effects, verified sandbox effects,
and integration-reported events. An HTTP 200 or a self-reported success is not
proof of the final destination state. Report field-level differences only when
before/after records are available; otherwise report request differences and the
missing evidence.

Start semantic tracing with structured log fields such as record ID, operation,
decision, and correlation ID. Add a dedicated SDK event API only if logs prove
insufficient. Traces explain decisions; independent assertions determine pass
or fail.

### 4. Assertions and evidence

Built-in assertions cover terminal outcome across retries, expected request
counts, final state values, state advancement constraints, record counts,
uniqueness by external key, and unchanged fields. Optional Python assertions run
inside the same isolation boundary and read evidence artifacts, without live
credentials or network access.

Persist one result manifest plus bounded supporting artifacts: case/fixture/
contract hashes, release digest, dependency/environment identity, runtime and SDK
versions, attempt IDs, assertion outcomes, effect ledger, state diff, redacted
logs, transport coverage, and explicit limitations. Avoid storing credential
values or credential hashes. Apply redaction to triggers, state, assertions, and
custom logs as well as HTTP. Configure retention and size limits for local
evidence; sensitive state should be minimized or replaced with synthetic data.

Assertions may require raw synthetic data inside the container; sanitize exports
without changing the assertion inputs. Do not let missing or redacted comparison
values become an automatic pass. Agent-authored assertion changes must be shown
alongside code changes so weakening the expected behavior is visible in review.

### 5. Agent workflow

Publish a portable skill with the CLI schema, integration patterns, fixture
repair rules, and the workflow: inspect contract, reproduce, edit, test, compare,
prepare release evidence. The agent uses the same commands as a human. An MCP
facade is optional after CLI behavior stabilizes; it must not contain a second
implementation of the harness.

The agent may autonomously edit local code and run offline scenarios within its
assigned task. Live credentials, target environment, record scope, and allowed
operations come from explicit persisted policy. Runtime enforcement must not
depend on skill prose. API docs, captured responses, and logs are untrusted data,
not permission to expand execution scope.

### 6. Release evidence and promotion

Test a staged immutable candidate. Do not activate the production release to run
tests. Add supported release export/import and digest verification contracts if
needed rather than copying private release directories from another process.
Preserve shared-tree placement and runtime digest semantics across the bundle.

`verify-release` checks the candidate code digest and environment identity against
the evidence. Modified code, fixtures, assertions, or dependencies invalidate the
relevant evidence. Record platform differences; identical source alone does not
prove identical Linux/macOS behavior. The promotion adapter uses existing Otter
release/deploy mechanisms and must verify the artifact that will execute, rather
than rebuild from a mutable working tree after approval.

V1 produces local evidence and a verification command, not cryptographic trust
in agent-generated reports. Enforcing a production gate requires a trusted CI
runner and artifact verification. Activation rollback does not undo external
data changes; data repair remains an explicit operation with its own evidence.

## Delivery sequence and acceptance gates

| Milestone | Concrete deliverable | Acceptance gate |
| --- | --- | --- |
| M0: contracts and feasibility | Harness scaffold, schemas, capability handshake, isolation spike | Execute one temporary integration through real Otter in an offline container; unsupported host setup fails clearly |
| M1: isolated scenarios | Disposable daemon, trigger injection, state seeding, immutable candidate, JSON results | Two cases cannot share state; a case's retries do share state; cron cannot fire; cancellation kills descendants; production workspace is unchanged |
| M2: replay and import | Adapter hooks, strict fixture matching, paginated capture importer | Imported complete JSON case runs offline; missing state/body yields incomplete; unmatched calls fail; raw sockets/subprocesses cannot reach external hosts |
| M3: behavioral verification | Stateful fake, fault rules, assertions, effect/state diff | Timeout-after-commit reproduces a duplicate bug; fixing idempotency passes without changing expectations; crash recovery preserves checkpoint semantics |
| M4: agent and release loop | Portable skill, inspect/compare tools, verified release bundle | Agent repairs seeded failures using tools; a code or dependency edit invalidates evidence; tested bundle is the bundle promoted |
| M5: live development | Connection profiles, schema discovery, brokered live reads, sandbox execution | Operation-level policy, scoped credentials, pagination and record budgets are enforced independently of generated code |

M0–M4 define v1. Complete an end-to-end vertical slice in M1–M3 before expanding
the adapter matrix. Do not build a custom UI before that slice demonstrates a
useful repair loop. M5 follows v1; production writes and automated repair are
separate later milestones.

Suggested merge sequence:

1. Harness schemas, CLI skeleton, doctor, and container backend spike.
2. Runtime development capabilities, trigger/scheduling controls, launcher hook.
3. Harness isolated runner, state seeding, results, and cleanup.
4. Strict replay for initial transports and capture import.
5. Stateful effects, failure injection, and assertion engine.
6. httpx parity, coverage reporting, and failure diagnostics.
7. Runtime release bundle contract and harness evidence verification.
8. Portable agent skill and end-to-end repair evaluation.

Each change ships its own contract tests and documentation. Keep runtime and
harness releases independently versioned, with explicit compatibility checks.

## Later capabilities

Connection profiles should identify account, environment, scopes, credential
reference, and supported operations. Store discovered schemas and API knowledge
with provenance and timestamps. Refresh them explicitly; do not assume that
custom fields or permissions are constant.

For live-read mode, route traffic through an operation-aware broker outside the
offline execution boundary. Allow declared API operations, not merely GET
requests: GraphQL queries may use POST and nominal reads may have side effects.
The broker holds credentials, validates redirects/hosts, bounds requests and
records, and denies undeclared operations. Simulate destination writes. Sandbox
mode uses separate accounts and verifies actual destination records after runs.

Only after these work should a UI expose the record timeline, contract coverage,
fixture editor, field-level changes, and release evidence. Defer a connector
marketplace, broad OAuth service, enterprise control plane, workflow canvas,
arbitrary protocol replay, and automatic production data repair.

## Validation and operating limits

Runtime changes use the repository's existing temporary-workspace fixtures,
`make test`, `make lint`, and affected smoke/cross-platform checks. Add race tests
where new daemon controls or snapshots introduce concurrent state. Harness tests
use local fake services only; no vendor credentials are required in CI.

The release gate includes duplicate delivery, partial batch, pagination, rate
limit, rejected field, timeout-after-commit, crash-before-checkpoint, secret
redaction, unexpected traffic, and incompatible-runtime cases. Cover both
decorated entrypoints and direct `Context` usage, including import-time HTTP.

Exercise isolation on supported Linux architectures and the macOS container
workflow. Test outbound IPv4/IPv6, DNS, proxy variables, subprocess access, host
mount access, and cancellation. Test archive path traversal and escaping symlinks
for imported fixtures and release bundles. Bound case duration, retries, request
counts, logs, artifacts, and disk use.

Agent evaluation uses seeded defects and independently specified assertions.
Measure successful reproductions, correct repairs, false passes, attempts to
weaken assertions, and time to reviewable evidence. These are evaluation metrics,
not promised performance targets until a baseline exists.

## Decisions to revisit after the first vertical slice

- Whether supported transport hooks are sufficient or a local replay proxy is
  needed for additional SDKs.
- Which real integrations justify maintained stateful connector fakes.
- Whether opt-in pre-run state capture is worth its storage and concurrency cost.
- Which Linux production environments should have first-class image parity.
- Whether evidence inspection needs a local web UI before broader connection work.

The first release is complete when a developer can reproduce a failed case,
observe incorrect effects, repair ordinary Python, pass independently defined
scenarios, and verify the resulting release artifact without touching production.
