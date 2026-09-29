# Host recovery implementation plan

Status: proposed. Date: 2026-09-28. No capabilities in this plan are shipped by
writing this document.

## Objective

Recover an Otter runtime's job state, queued work, identities, and
required releases onto a clean replacement host after the original host and its
disk are lost. Preserve ordinary Python execution and the single-daemon model.

The operator supplies a committed recovery checkpoint, replacement-host
configuration, and credentials. Otter restores and validates the runtime before
allowing any job to execute. The original checkout and original data
directory must not be required.

This implements the recovery portion of the [product roadmap](product-roadmap.md).
It is a foundation for replaceable hosts, not automatic failover. Application
artifact storage, multi-node scheduling, distributed leases, filesystem
replication, and zero-loss remote write acknowledgements are outside this change.

## 1. Recovery contract

### First release: complete checkpoints with a finite recovery window

- SQLite remains the authoritative database on the serving host. A recovery
  checkpoint is a consistent database snapshot plus all the artifacts needed to
  interpret and execute its recoverable work.
- A checkpoint is recoverable only after every required object has been uploaded,
  verified, and referenced by its final immutable commit manifest. Local staging
  completion and upload start are not recovery success.
- A restore preserves all state and accepted work represented in that checkpoint.
  Work accepted or completed after its database snapshot may be absent or replayed.
  This is not an RPO-zero feature and does not change live submission acknowledgements.
- Measure exposure using the snapshot time of the newest committed checkpoint,
  not its upload completion time. For an online snapshot, report the capture
  interval and use its start as a conservative recovery boundary. A configured
  backup interval is a target, not a guaranteed RPO during storage outages.
- Recovery time includes download, verification, environment preparation, operator
  validation, and execution release. Publish measured drill results before making
  an RTO commitment; do not invent a universal target.
- Restoring old state can repeat an external effect even when its run appears
  queued or running in the checkpoint. A checkpoint cannot prove what happened
  after it was taken. Preserve run IDs and retry ancestry, explain ambiguity,
  and require job-level idempotency or reconciliation where needed.
- Missed cron occurrences remain skipped, as today. Persisted queued cron runs
  are recovered; do not manufacture cron backfill during restore.
- Automatic execution requires a supported runtime version, compatible platform,
  all required release bytes, and provisioned dependencies/secrets. Missing
  prerequisites leave the restored runtime held with an actionable report.

The first production scope is Linux on the same OS/architecture/libc family as
the source, using the same Otter build and embedded SDK. Cross-platform moves and
runtime upgrades are separate, explicit operations after recovery. Local test
fixtures may run on other currently supported development platforms.

### Schema and format compatibility

The checkpoint records a recovery format version and a SQLite schema version, and
each direction has a different rule:

- A checkpoint whose format or schema is newer than the running Otter build is
  refused before any destination is touched. Downgrade is not a recovery path.
- A checkpoint whose schema is older is migrated forward deterministically as part
  of restore, before the hold is released, and must land on the schema the running
  build expects. Refusing older checkpoints would make every checkpoint stale after
  any schema change, so they are migrated, not rejected.
- The journal records the schema version before and after migration so an
  interrupted restore resumes against a known version.

A restore never runs a checkpoint's own migrations or code against the destination.
Only the running build may migrate the restored database.

### Old-host exclusion

Before releasing execution, the operator must stop or isolate the old host and
disable its supervisor, or establish that it is destroyed. A network timeout is
not evidence that the old host stopped. Record the operator's acknowledgement in
the recovery audit, including how exclusion was established.

The existing data-directory lock is local; it cannot fence a second machine.
Neither a new restore ID nor a rotated Otter API token stops old Python code from
calling external services. Automated takeover must wait for a separately designed
ownership/fencing protocol. Do not present this workflow as automatic failover.

## 2. Current implementation and gaps

| Area | Current implementation | Required change |
| --- | --- | --- |
| Database | `internal/database/database.go`: one SQLite database, WAL, `synchronous(NORMAL)`, one connection | Produce a verified standalone snapshot; distinguish local commit durability from remote checkpoint coverage |
| Submission | `internal/daemon/view.go`: run creation and enqueue share a transaction | Preserve this invariant and include acceptance context in the recovery report |
| Completion/retry | `internal/daemon/workers.go`: finishing an attempt and scheduling its retry are separate commits; a terminal-failure notification is sent synchronously between them, delaying the successor | Persist terminal outcome and retry intent/next attempt atomically; emit logs/notifications after that commit, so a notification can never sit between the outcome and its retry |
| Recovery retry policy | `internal/daemon/recovery.go`: an interrupted attempt's retry is decided from the live discovery manifest looked up in the registry | Decide recovery retries from the attempt's bound release manifest, not the manifest discovered today |
| Startup recovery | `internal/daemon/recovery.go`: scans running/queued/retrying runs, with 10,000-row limits; missing queue rows get a new immediate deadline | Paginate to completion, retain original scheduling deadlines, make reconciliation idempotent and fail closed |
| Startup order | `internal/daemon/daemon.go`: discovery and recovery happen in `New`; recovery errors are logged and startup continues | Gate admission and execution — worker pool, triggers, and submission mutators — before they start, while still running discovery and exposing the authenticated inspection API; refuse to release the hold on incomplete reconciliation. Gate execution, not discovery: discovery is how identities are reconciled, and the hold's own status surface lives behind the API server |
| Releases | `internal/release/`: digest-addressed trees and filesystem activation links under `.releases/` | Export required trees, capture activation state, coordinate with activation and pruning, verify content |
| Execution paths | Runs retain absolute `release_source_dir`; workers use it to load the manifest | Resolve executable paths from job ID + release digest + validated relative layout on the new host |
| Identity | `internal/identity/`: UUIDs, generations, canonical paths, `.otter-id`, cross-filesystem operation journal | Restore identities without rediscovery minting replacements; remap paths through a recovery-specific transaction/journal |
| Python | `internal/pyenv/`: environment identity includes interpreter, platform, libc, uv, recipe, and inputs | Rebuild the recorded environment identity; never substitute today's preparation policy silently |
| Pause | `internal/pause/`: suspends cron/webhooks but permits manual runs and queued execution | Add a separate global recovery hold that blocks all execution and admission |
| Operations | `docs/operations.md`: manual SQLite backups; some layout/backup descriptions predate immutable releases | Replace database-only recovery instructions with the complete checkpoint workflow |

These are static code findings, not results from a failure test. In particular,
reproduce the finish/retry crash window before fixing it, and retain that test.

## 3. Checkpoint contents

Back up one runtime as one recovery unit. Do not mix a database from one checkpoint
with active pointers or identity files from another.

| Included | Representation and policy |
| --- | --- |
| Database | Standalone SQLite snapshot containing state, runs, queue deadlines, identities, pause settings, webhook tokens, retained logs/capture, and schema metadata |
| Releases | All active releases and every release needed by queued, retrying, or running attempts; include locally retained rollback releases in v1 for predictable recovery |
| Activation | Explicit job-ID-to-digest map; reconstruct links, rather than copying absolute symlinks |
| Registration | Identity, generation, status, portable path mapping, and the effective registered manifest needed for discovery/scheduling, including jobs with no active release |
| Environment recipes | Per-required-environment recorded policy, input digest, Python pin, uv version, target ABI, and release reference |
| Runtime requirements | Otter build/version, SDK identity, recovery format version, SQLite schema version, platform, supported feature flags |
| Configuration requirements | Allowlisted non-secret daemon settings and names/references for externally supplied secrets and host dependencies |
| Recovery evidence | Capture start/end, source runtime ID, checkpoint ID, counts, byte sizes, hashes, dependency closure, and completeness verdict |

Do not archive arbitrary source workspaces or uncommitted development files.
Capture the daemon's effective registered manifests separately from release
manifests: live trigger/concurrency configuration can differ from a pinned release.
Preserve invalid/retired/suppressed identity records without making them runnable.

Persist the successfully loaded registration configuration and its revision as a
recovery catalog in SQLite, updated under the reload/identity barrier. This lets
offline capture recover the last effective configuration instead of guessing from
possibly edited source files. Populate it on a successful upgraded-daemon load.

The catalog is a schema change plus a legacy-absence path, and both are deliverables
rather than assumptions. A legacy installation without a catalog requires an
explicit one-time configuration inventory, recorded as a named artifact, before its
first checkpoint can be called complete. Do not claim to recover an unknown previous
config, and do not let capture infer one from edited source files.

Exclude PID files, sockets, local locks, `.otter/serve` discovery files, transient
run tokens, caches, copied virtual environments, and ordinary application files.
Extract the SDK from the matching binary. Custom SDK overrides must be explicitly
captured with a digest or reported as an unsupported recovery prerequisite.

Credentials are not exported from the daemon's environment. Record required
secret names, then inject values on the replacement host. The database and source
releases can already contain sensitive data, including webhook tokens and captured
payloads: protect the entire checkpoint as sensitive, with private object access,
TLS for remote storage, encryption at rest, restrictive local permissions, and
documented independent access to decryption keys. Do not claim the bundle is
secret-free. Restore reports must not print tokens or secret values.

### Python recovery boundary

V1 reconstructs managed environments from captured lock inputs and the exact
recorded preparation policy. Add an explicit recorded-policy preparation path
to `internal/pyenv`; its current `Prepare` selects the current policy. Validate
the resulting identity and interpreter before publishing readiness.

Package/interpreter downloads remain an external dependency in v1. Record this
in checkpoint inspection and drill the loss of an upstream package: restore must
remain held, not install a newer substitute. Mirroring interpreter distributions,
uv binaries, and locked wheels into recovery storage is a later enhancement for
dependency-independent recovery. Do not copy virtualenvs between host paths.

External Python jobs require operator provisioning of the interpreter,
packages, native tools, and any external files. Inventory declared requirements;
do not pretend to discover arbitrary dynamic imports or filesystem dependencies.
An operator must acknowledge and validate these before execution is released.

## 4. Storage and publication protocol

Add `internal/recovery` for the manifest, inventory, orchestration, verification,
and restore journal. Add a small object-store adapter below it with put/get/head,
paginated list, and delete operations. Start with a local directory backend for
tests/export and an S3 backend for off-host checkpoints. Local-only output is not
host-loss protection. Use a maintained S3 client rather than implementing request
signing, and qualify the first supported service with contract tests. Broader
S3-compatible provider support is earned by the same tests.

Use immutable checkpoint prefixes in the first version:

```text
otter-recovery/<runtime-id>/checkpoints/<checkpoint-id>/
  database.sqlite
  registration.json
  configuration.json
  environments.json
  releases/<job-id>/<release-digest>.tar
  commit.json
```

The commit manifest records SHA-256 and length for every required object, as well
as the embedded release digest and layout version. A release digest and an archive
byte hash serve different purposes; verify both. Object-store ETags are not a
portable content checksum. A trusted bucket is the integrity trust boundary;
hashes detect corruption, not a malicious administrator rewriting the manifest.

Publication sequence:

1. Allocate a unique checkpoint ID and build the complete local staging snapshot.
2. Verify SQLite integrity, foreign keys, identity consistency, and artifact closure.
3. Upload payload objects, with bounded concurrency, streaming, and retry/backoff.
4. Verify remote bytes against the manifest using downloads in v1. Account for
   verification bandwidth and temporary disk in capacity estimates.
5. Publish `commit.json` last, atomically and without overwriting an existing
   checkpoint. On a lost response, read it back and verify before reporting success.
6. Record local status only after remote publication is confirmed. Failure to write
   this local status cannot invalidate an already committed remote checkpoint.

An incomplete prefix is never selectable for restore. A `latest` pointer is an
optional discovery hint, not proof of completeness. Selection lists committed
manifests and inspection checks their closure. If a selected checkpoint is damaged,
report it; do not silently restore an older checkpoint with greater data loss.

Keep payloads per checkpoint initially. Cross-checkpoint deduplication complicates
garbage collection and is deferred until measured storage costs justify it.

## 5. Consistent capture

### Milestone A: offline capture first

Implement and prove stopped-runtime capture before allowing scheduled live backups.
The stopped case is largely solved by stopping the service, not by new locking:
`database.Close` already checkpoints the WAL, so a stopped runtime has a
self-contained main database. Require the service to be stopped and acquire the
existing data-directory lock. Also coordinate every release mutation/prune path with
an artifact lock: the daemon lock alone does not currently protect all filesystem
release operations. The spike budget below belongs to Milestone B; do not spend it
re-proving the stopped case.

Complete or refuse pending identity operations before inventory. Capture effective
registration metadata through a shared exporter, without starting workers,
registering new identities, running jobs, or minting webhook tokens. Refuse
an inconsistent or unresolved registration instead of guessing a mapping.

Create the standalone database through an in-process SQLite snapshot mechanism
supported by the pinned driver. Confine the spike to choosing and proving one path:
the pinned driver already exposes the online backup API (`NewBackup`/`NewRestore`
with `Step`/`Finish`/`Commit`) and supports `VACUUM INTO`, so the question is which
one is simplest to finalize and verify, not whether either exists. Prefer a single
read-transaction statement if it satisfies the consistency, finalization, and
verification requirements; otherwise use the driver's backup API. Do not add a
production dependency on the external `sqlite3` CLI. Do not copy a live main
database after a checkpoint and assume it is consistent. Cleanly finalize/fsync the
destination snapshot before hashing it, and validate that it opens without source
WAL/SHM files.

Resolve the required artifacts from the copied database plus captured activation
and registration metadata. Copy them to staging under the mutation lock and
verify digests. Release local locks before uploading; the operator may restart
the source once the complete local capture is safe. An upload failure must not
extend downtime indefinitely.

### Milestone B: bounded live capture

Use the same manifest and verifier. Add a shared capture protocol covering daemon
reload, identity operations, release activation, release retention, and offline
CLI mutations. Document lock ordering and ensure all mutators participate.

For the initial online implementation, briefly hold new admission/claims and
configuration/identity/artifact mutations while taking the database snapshot and
capturing the matching effective registration/activation state. Serialize database
writes through the existing connection. State calls from running children may
wait; specify a maximum capture duration and fail the capture if it exceeds the
budget. Bound buffered logs/capture; never create an unbounded queue in memory.
This is a backup capture barrier, not the durable recovery hold.

Before releasing the barrier, pin the complete required artifact set against
pruning. Copy pinned immutable artifacts to staging, verify them, then release
pins. Later live submissions can bind other releases; they belong after the
captured boundary and are not part of this checkpoint. A child or external user
modifying supposedly immutable release bytes makes verification fail.

The implementation spike must measure pause duration with a large retained
database. If a full snapshot cannot fit the budget, retain offline capture as
supported and design an incremental online snapshot with equivalent consistency
and pinning evidence before enabling automated backups. Never advertise online
capture based only on a small fixture.

## 6. Restore and execution hold

Proposed command surface; exact parsing should follow existing CLI conventions:

```sh
otter backup create --destination s3://bucket/otter-recovery
otter backup list --source s3://bucket/otter-recovery --json
otter backup inspect <checkpoint-id> --source s3://bucket/otter-recovery
otter backup verify <checkpoint-id> --source s3://bucket/otter-recovery
otter restore <checkpoint-id> --source s3://bucket/otter-recovery \
  --data /var/lib/otter --jobs /opt/otter/jobs
otter recovery status
otter recovery resume --acknowledge-old-host-stopped
```

`backup create` uses the daemon's authenticated capture endpoint when running;
the offline form accepts explicit roots and obtains local locks. `restore` is
offline and must not depend on resolving a live project's API address. Inspection,
listing, and downloading require read access only. Resume is an explicit operator
action, not part of ordinary `otter start` or deployment restart.

Restore sequence:

1. Verify the manifest format and runtime/platform compatibility. Lock the target;
   require new/empty destination roots. Never overwrite an existing runtime by
   default. An interrupted matching restore can resume from its journal.
2. Install a durable restore-in-progress guard before placing executable content.
   Every startup path, including systemd and `otter start`, must honor this guard.
3. Download into staging and verify all objects. Safely extract archives: reject
   traversal, absolute destinations, device files, and escaping links. Preserve
   safe internal symlinks and executable modes required by valid releases.
4. Open the database offline and validate it. Preserve IDs, generations, state,
   pause intent, tokens, timestamps, and retry ancestry. Create a new restore ID
   while retaining the stable runtime ID and source checkpoint provenance.
5. Remap canonical job paths into the destination root and regenerate
   `.otter-id` markers and discovery manifests. Materialize a separate writable
   workspace mirror of each active release, preserving its internal relative
   layout, then apply the captured registration manifest there. Do not alter the
   immutable release itself. Use per-identity mirrors to avoid collisions between
   different versions of shared trees; inventory exactly which directories are
   discoverable so nested manifests cannot register unintended jobs. The
   destination therefore has two distinct trees with different owners: the
   per-identity workspace mirror is the discovery/identity/state surface, while the
   release tree resolved by digest is the execution surface. Do not conflate them.
   Reject mapping collisions and paths outside the supported roots with an explicit
   error; never resolve them by flattening paths or minting replacement identities.
   Manifest-only placeholders are insufficient: current validation checks that
   entrypoints and declared Python paths exist. If a captured registration needs
   unpublished source absent from its release, preserve it as blocked and report
   the missing input; never invent source or silently change configuration.
   Jobs with no active release remain non-runnable. Do remapping through a
   recovery-specific journal; ordinary identity move/reset must not invalidate
   queued generations. Keep old paths as provenance.
6. Restore release trees and rebuild activation links by digest. Workers resolve
   pinned releases against the new data root; preserve historical source paths
   as evidence rather than treating them as executable locations. Legacy runs
   without a complete release binding remain blocked for explicit reconciliation.
7. Prepare managed environments under their recorded policies, validate external
   prerequisites, and provision credentials. Never invoke job entrypoints
   as a readiness test. Apply a configuration allowlist; do not inherit the old
   listen address, API token, notification URL, or credential-bearing environment.
   Preserve webhook tokens from the database unless rotation is explicitly chosen.
   The hold is only useful if an operator can reach it, so provision the new host's
   API token and listen address before sealing the data root, and verify that
   `otter recovery status` answers during the hold. A sealed runtime that cannot be
   inspected is indistinguishable from a failed restore. Never revive the old host's
   API token to make the held runtime reachable.
8. Publish the staged data/workspace and record the journal phase with durable
   renames. The two roots cannot be atomically renamed together: keep the startup
   guard until both are installed and verified, and make each phase restartable.
9. Start in recovery hold. Allow authenticated inspection, preparation, and recovery
   actions only. Block manual submissions, webhook admission, cron, worker claims,
   and normal state/identity/release mutations. Preserve the original job
   pause settings independently. Suppress failure notifications caused solely by
   the restoration procedure.
10. Display checkpoint age/coverage, ambiguous work, missing prerequisites, and
    preserved queue counts. Resume only after preflight and old-host exclusion.
    Reconcile interrupted attempts transactionally before any worker or trigger
    starts; recheck readiness after any recovery repair.

Persist the hold across restarts. A partially restored directory, missing release,
failed reconciliation, or unsupported schema must never become an apparently
healthy serving runtime. Liveness may be healthy while execution readiness is
false; surface the precise reason through CLI/API.

### Hold operations

The hold is not a flag file that a second process can bypass. Every startup path
reads it before admitting work, and the inspection surface is defined as narrowly as
the hold itself:

- While held, the API and CLI allow authenticated inspection, preparation, and the
  recovery actions named in the command surface; nothing else. Submission, webhook
  admission, cron, worker claims, and ordinary state/identity/release mutations are
  refused with the hold reason, not with a generic error.
- `otter recovery status` and `otter recovery resume` work from an offline
  invocation and from the held daemon, so recovery does not depend on a live
  project's API address or on the old host's token.
- The hold survives daemon restart, package upgrade, and redeployment. A deploy that
  replaces the binary must not clear it.
- Readiness is reported separately from liveness in both the API and the service
  status, so a supervisor restart loop cannot be mistaken for recovery progress.
- Failure notifications caused solely by the hold or by the restoration procedure
  are suppressed rather than reported as job failures.

## 7. Queue and retry correctness

Make these prerequisite changes independently testable before shipping restore:

- Commit an attempt's terminal outcome and its retry decision together. Prefer
  creating the next attempt and queue entry in the same SQLite transaction;
  enforce uniqueness so repeated recovery cannot create two successors. Emit
  logs/notifications after commit without making them the authority for retries.
- Store the authoritative next-eligible time on the run or durable retry intent,
  as well as the queue index. Reconciliation must not turn a delayed retry into
  immediate work merely because its queue row is missing. Define a conservative,
  inspectable policy for legacy rows whose original deadline cannot be recovered.
- For an attempt marked running in the snapshot, preserve the interrupted attempt
  and record host-recovery provenance. Create a new attempt only when its bound
  release's retry policy permits it. If attempts are exhausted, retain an explicit
  interrupted failure for operator action; never silently discard it.
- Use the bound release manifest for recovery retry policy, not a newer discovery
  manifest. An interrupted attempt is decided by the policy it actually ran under,
  and that policy is the one recorded with its pinned release. Pin its code and
  environment through every successor attempt.
- Preserve queued/retrying run IDs, metadata, release bindings, generations, and
  future deadlines. Overdue queued work is eligible after resume; future work waits.
- Paginate recovery with a stable cursor until all relevant rows are processed.
  Do not mutate a paginated set using offsets that can skip rows. The current
  `runs` filter offers only offset-based paging, so this requires a keyset query
  on `(queued_at, id)` (or an equivalent immutable ordering) before recovery may
  loop. Treat re-queueing a row that stays in `queued`/`retrying` as a case the
  cursor must survive without skipping or double-processing. Recovery errors block
  execution rather than merely logging and continuing.

No local transaction makes a remote business operation exactly once. The recovery
report must call out the entire uncertain interval after capture, not just attempts
that were running at capture time.

## 8. Scheduling, retention, and operations

After online capture meets its gate, add an optional daemon backup schedule with
one in-flight job, bounded staging space, retry backoff/jitter, and no overlap.
Storage failure leaves the runtime serving under its existing local durability
contract, while status exposes increasing recovery exposure. A strict admission
policy tied to backup freshness is a separate future feature.

Expose last attempted/successful checkpoint, snapshot boundary, upload completion,
duration, bytes, last verification, recovery exposure, and sanitized error details.
Use existing monitoring/notification plumbing for stale or failed protection;
do not send a success alert every interval.

Use explicit retention counts/age and retain multiple verified checkpoints. A
failed backup never triggers deletion of the last good one. Initially make pruning
an explicit operator operation with a preview. Separate cleanup of uncommitted
prefixes from committed checkpoints; only reap abandoned uploads after a documented
grace period and confirmation that no active job owns them. Coordinate restore
readers and pruning, or require maintenance exclusion in v1. Do not delete a
checkpoint halfway through its restore. Bucket lifecycle rules must respect the
same retention contract.

Update deploy to support provisioning a replacement without starting execution,
injecting secrets, and honoring the recovery hold. It must not rsync over recovered
identity markers, replace active release selections, or auto-run `release` from a
new checkout during recovery. Regenerate service files for the destination host.

## 9. Implementation sequence and exit gates

| Milestone | Primary code surfaces | Exit evidence |
| --- | --- | --- |
| 1. Recovery invariants | `internal/runs`, `queue`, `daemon/workers.go`, `daemon/recovery.go`, `migrations/` | Kill-point tests prove atomic retry decisions, preserved deadlines, stable pagination, and fail-closed startup |
| 2. Portable artifact inventory | `internal/release`, `identity`, `pyenv`, daemon registration catalog (table + legacy inventory artifact), new `internal/recovery`, `migrations/` | Full closure resolves from captured data, absolute execution paths are removed as authority, identity remapping and recorded-policy preparation pass |
| 3. Offline checkpoint/restore | `database`, `datalock`, `cli`, `daemon`, `api`, `recovery` | Destroyed-source local drill restores IDs, queue, releases, and held execution into different roots |
| 4. Remote committed checkpoints | Recovery storage adapter, configuration and CLI | Real S3 upload/verify/download drill succeeds; interrupted uploads never appear recoverable |
| 5. Live capture and scheduling | Daemon capture barrier, release/identity mutators, retention | Race/fault suite passes; capture pause and disk usage fit a published workload envelope |
| 6. Operator recovery workflow | `internal/deploy`, operations/API/CLI docs, smoke drills | A second operator recovers a lost Linux host using only the checkpoint, documented provisioning inputs, and credentials |

Ship milestones 1–4 as manual recoverable-host support if useful; scheduled
protection requires milestone 5. Do not label manual backups automatic protection.
Assign concrete RPO/RTO targets for the pilot workload before milestone 6.

## 10. Validation matrix

Tests should assert persisted outcomes and independently checked external effects,
not only successful command exits. Use local fake services for job effects
and a real S3 bucket for the storage qualification drill.

| Fault or scenario | Required result |
| --- | --- |
| Host disk deleted after a committed checkpoint | Clean host restores snapshot state, queue, identities, pause intent, and every required release without the original checkout |
| Queued run bound to old release; new release active | Queue executes its old digest; new submissions use the captured active digest |
| Retry backoff spans backup/restore | Original deadline survives; one successor attempt exists |
| Crash after attempt failure, before retry creation | Atomic transition prevents a lost retry or recovery completes persisted intent |
| More than 10,000 queued/interrupted attempts | Every relevant row is reconciled exactly once; none skipped by pagination |
| Fake upstream commits before child/host dies | Replay ambiguity is reported; idempotency fixture prevents duplicate business effect |
| Release activation, pruning, identity move, or reload races capture | Checkpoint is internally consistent or capture fails; never commits missing/mismatched artifacts |
| Capture exceeds its declared barrier budget under load | Barrier is released, capture fails with a bounded error, no checkpoint is committed, and serving continues undisturbed |
| Upload interrupted or acknowledgement lost | No incomplete checkpoint selected; committed manifest can be recognized by readback |
| Corrupt DB/archive, missing payload, unsafe archive path | Verification fails before execution or destination publication |
| Checkpoint written by a newer format or schema | Refused before any destination is touched; no partial restore and no downgrade attempt |
| Checkpoint schema older than the running build | Migrated forward deterministically before the hold is lifted; journal records the before/after version |
| Restore interrupted between directory publications | Restart resumes the journal or stays held; workers never observe a partial restore |
| Restore under different absolute roots | Same identities/generations and release digests; execution resolves only destination paths |
| Two identities map to the same destination path or outside the supported roots | Explicit collision error; no path flattening and no new identity minted |
| Missing uv/wheel/interpreter, ABI mismatch, missing secret | Held with exact prerequisite; no policy substitution and no entrypoint execution |
| Old host reachable or exclusion unconfirmed | Resume requires the explicit exclusion acknowledgement; documentation states that it is not technical fencing |
| Backup store unavailable, local staging disk full | Bounded failure, no false success, prior checkpoints intact, serving behavior matches documented contract |
| Runtime reboot while recovery hold is active | Hold persists; no manual/cron/webhook/queued execution escapes it |
| Restore damaged latest checkpoint | Explicit error and older-checkpoint selection; no silent fallback |
| Ordinary deploy targets recovered workspace | Hold and recovered release/identity choices preserved; deployment cannot accidentally release work |

The final drill records source workload, snapshot boundary, accepted run IDs,
independent destination effects, recovered queue/state checksums, release digests,
time to inspection/readiness/resume, and all external provisioning dependencies.
Recovery succeeds only when those checks pass, not when the daemon merely starts.

## 11. Documentation and follow-on decisions

Update `README.md`, `docs/architecture.md`, `docs/operations.md`,
`docs/deploy.md`, `docs/managed-python.md`, and `docs/api-reference.md` alongside
implementation. Remove the implication that one database file contains all
recoverable runtime assets and the unsafe live-checkpoint-then-copy recipe.
Explain ordinary local application files are outside runtime recovery coverage.

Keep two future decisions separate:

1. **Dependency-independent restore:** mirror exact runtime/toolchain/package
   artifacts so recovery does not require upstream downloads.
2. **Zero-loss remote durability and automatic failover:** define a remotely
   durable commit boundary, log replication, ownership epochs, fencing, and
   recovery of acknowledged tails. This is a different protocol, not a smaller
   backup interval, and requires a new design and fault model.

The immediate deliverable is a complete, verifiable, operator-controlled recovery
path whose limits are visible before the host is lost.
