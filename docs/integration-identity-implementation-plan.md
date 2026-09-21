# Integration identity: implementation handoff

Status: proposed implementation specification, September 21, 2026.

## 1. Assignment and scope

Implement the identity redesign described here in the Go daemon, CLI, Python SDK, release system, deployment system, database, tests, and documentation. This document is self-contained and supersedes earlier proposals to infer moves from copied UUIDs or to preserve an old identity when a different marker appears at the same path.

Do not add an ID to `otter.yaml`. Do not derive durable identity from a manifest name, directory basename, content digest, or filesystem inode. Do not implement automatic state-preserving move detection. Do not introduce a distributed registry or a general-purpose workflow engine.

Before edits, read applicable AGENTS.md instructions, inspect the current working tree, and verify the referenced implementation points; this plan is based on repository inspection at the date above. Preserve unrelated changes. Use temporary workspaces for tests. Do not reset, delete, migrate, or resolve collisions in the user's actual `.otter` data as part of implementation or validation.

Deliver working implementation, meaningful tests, documentation, and a final summary of changes, checks, and remaining limitations. Implement all phases; intermediate phases are not an acceptable final state.

## 2. Problem and required invariants

Today `config.Integration.ID` is the manifest name. It selects database state, history, queues, webhook tokens, releases, environments, and child-process identity. Discovery marks a later duplicate invalid but returns no error. Local path-based release resolution can independently select that same name and activate another directory's code against existing state.

Required invariants:

1. Each registered instance has an immutable, never-reused ID. Names are mutable, non-unique labels.
2. One source path has at most one current owner. Every durable artifact belongs to an ID, never to a label.
3. Copying source to a new path creates a fresh ID and state, including when the original disappears before scanning.
4. Replacing a registered directory with source having no marker or a different valid marker retires the old instance and creates a fresh one, including across daemon downtime.
5. A marker copied from B into A never grants A access to B's state. A replacement never silently keeps A's old state either.
6. Editing only `name` preserves ID and state. Two active instances may share a label; bare-label lookup then errors.
7. Only an explicit `move` operation transfers an existing path binding while preserving identity.
8. Retired, deleting, or deleted identities cannot receive new work or accept integration-originated state writes. Outstanding credentials cannot defeat retirement.
9. Discovery errors are not evidence of deletion. No partially observed scan changes ownership.
10. Legacy state/history survives migration unchanged unless the operator explicitly requests purge later.
11. Deployment preserves destination identity and never imports local identity.

Explicit boundary: replacing a directory with an exact backup containing the SAME marker at the SAME registered path cannot be distinguished from continuity. It keeps its identity unless explicitly reset. Do not promise universal detection of delete/recreate. This design detects replacement when the marker is missing or differs.

## 3. Final identity and marker contract

### Registry

Add a dedicated identity package (suggested `internal/identity`) with persistence, reference resolution, reconciliation planning, and lifecycle operations. Keep it separate from the daemon's in-memory execution registry.

Suggested persistent records:

- `integration_instances`: `id` primary key, `name`, `canonical_path` (current or last source path), `status`, `generation`, timestamps, optional retirement reason.
- Status values: `pending`, `active`, `retired`, `deleting`, `deleted`. Manifest validity and temporary execution blocking are separate from ownership status.
- `integration_paths`: canonical path primary key, current owner ID or null, suppression flag/reason. This table reserves paths during pending operations and suppresses deleted sources. Keep historical paths in instance/operation records; do not let history impose uniqueness on future instances.
- `identity_operations`: operation ID, kind, phase, affected IDs/paths, expected marker observations, chosen new ID where applicable, and structured operation-specific recovery data.
- Identity initialization/migration metadata separate from SQL schema version.

Exact table names can follow repository conventions. Preserve the semantic constraints. The ID primary key already ensures uniqueness; do not add a redundant unique index on active IDs. Ensure concurrent attempts cannot reserve the same path.

Use the existing `github.com/google/uuid` dependency to mint UUIDs. Retry an ID collision against all instance rows, including tombstones. Treat IDs as opaque strings throughout the runtime because migrated IDs retain their old names.

### `.otter-id`

An ordinary hidden file inside each registered source directory:

```text
0195a7c2-8e31-7b64-9f02-6dcb482ea510\n
```

The example denotes one identifier plus a real newline, not a literal backslash-n. Newly minted values may use UUID v4. No name, path, timestamp, or credentials are needed in the file.

- The registry authority writes it. It is not a user-authored manifest field.
- A migrated marker may contain a legacy ID such as `counter`; parsing must accept safe legacy identifiers without interpreting them as labels.
- Define and share a bounded, single-line, path-safe ID parser; reject empty, oversized, multi-line, traversal, separator-containing, or otherwise malformed values.
- Read via no-follow/regular-file checks. Do not read or overwrite through marker symlinks. Reject suspicious hard-linked markers where detectable. Check the opened object, not only an earlier pathname check.
- Write through a temporary file in the same directory followed by atomic replacement, with appropriate flushes and restrictive permissions compatible with the runtime owner.
- Add `.otter-id` to the repository ignore rules and generated project ignore rules. Gitignore is convenience, not a trust control.
- Exclude markers and marker temporary files from release copying/hashing and deployment staging/revision calculation.
- A matching existing marker can be read from a read-only directory. Any operation requiring a new marker must fail clearly if it cannot write one. There is no path-only registration fallback.
- Never write a marker into a release snapshot or runtime working directory merely because execution occurs there.

## 4. Discovery and reconciliation semantics

Refactor discovery to return manifest observations and explicit filesystem errors. It must not mint IDs or write files. Distinguish absent directories, directories without manifests, malformed manifests, unreadable manifests, and unreadable directories.

Duplicate labels are valid in the final design. Remove the current duplicate-name invalidation once every operational identity path uses the registry. `validate` reports duplicate labels informationally and lists paths; malformed manifests, bad markers, and incomplete scans remain errors. Validation never creates registrations or rewrites markers.

Build a reconciliation plan from one registry snapshot and the scan observations. Sort only for stable diagnostics, never to choose an identity winner. Examine known registered paths directly as needed; absence from a manifest list does not prove a directory was deleted.

| Observation | Registry condition | Action |
|---|---|---|
| Existing path, matching marker | Active owner | Preserve ID; refresh label and validity |
| Existing path, missing marker | Active owner | Retire old ID, cancel its authority, register fresh ID |
| Existing path, different valid marker | Active owner | Same replacement behavior: retire old ID and mint fresh; never adopt presented ID |
| Existing path, malformed/symlink/unreadable marker | Any | Block affected execution and report error; no reassignment |
| New path, valid manifest, absent marker | No owner/suppression | Mint and persist fresh ID |
| New path, valid manifest, valid copied marker | No owner/suppression | Mint fresh ID and replace copied marker, even if old owner is absent |
| Known path, matching marker, invalid or missing manifest | Active owner | Preserve identity, disable new execution; report invalid configuration |
| Known source directory definitively absent | Active owner | Retire; keep history and payload data |
| Retired path reappears | No active owner | Fresh registration; never revive retired identity from its marker |
| Suppressed path remains or reappears | Deleted/suppressed | Report suppressed; do not auto-register |
| New path with invalid/missing manifest | None | Report invalid where appropriate; do not register |

For a known path with invalid/missing manifest AND a marker mismatch, do not keep executing the old registration. Block it and defer creating a replacement until the manifest is valid. For definitive directory disappearance, retire regardless of its previous manifest validity.

An incomplete scan makes no persistent reconciliation changes and writes no markers. Report every failed path. Temporarily block admissions for registrations whose source cannot be verified or is directly observed to conflict; unaffected existing registrations may continue. This execution gate is distinct from retirement and clears after successful verification. Revalidate marker/path ownership before admitting live-source work, so postponing reconciliation cannot run replacement code against old state.

Filesystem traversal is not atomic. Recheck affected paths/markers before each planned operation, abort/retry if observations changed, and never overwrite an unexpected marker encountered during recovery. Do not claim safety against arbitrary hostile same-user filesystem races; document the trust boundary.

Copies discovered together each receive new IDs. No UUID grouping, absent-original heuristic, inode comparison, name match, or content comparison can adopt old state.

## 5. Single writer, locking, and recovery

Extract/reuse the daemon's current workspace/data-directory ownership lock into a shared package. The daemon holds it for its lifetime. A CLI requiring offline mutation must acquire exactly that same lock; if a daemon owns it, send an authenticated API request instead. API failure must not trigger a second direct writer. Verify non-Unix ownership behavior and fail unsupported mutation modes clearly.

The daemon serializes identity-changing operations using a service-level mutex, with database transactions for durable state. Establish and document lock ordering relative to queue/worker/registry locks. Do not hold SQLite transactions during process termination, environment preparation, or network transfers.

SQL and filesystem updates are not one transaction. Use narrowly scoped, recoverable operations:

1. Reserve affected paths and persist intent, expected observations, and allocated IDs in SQLite.
2. Disable affected admissions and invalidate old authority where applicable.
3. Perform marker write, move, or cleanup using the journaled inputs.
4. Recheck observations and commit final bindings/status in SQLite.
5. Mark complete; publish the in-memory view and schedules.

Recovery runs before normal reconciliation, scheduling, queue claims, or serving mutating APIs. If intent is persisted but a marker is not written, resume only when its expected precondition still holds. If the new marker is already written, recognize the journaled ID and finish rather than minting another. Unexpected external changes leave the operation blocked with a useful diagnostic; do not overwrite them blindly or resurrect retired authority.

On startup, fence interrupted executions using existing recovery mechanisms. Handle surviving child processes using verified process ownership; never kill an unrelated PID merely because it was recorded before a crash.

## 6. Resolution, API, execution, and SDK

Use typed references internally: label, path, or ID. Suggested user syntax:

```sh
otter run counter
otter run ./integrations/counter
otter run id:counter
otter run id:550e8400-e29b-41d4-a716-446655440000
```

- Bare labels select exactly one active owner, even if that owner's manifest is currently invalid (then explain the invalidity). Never prefer an ID match over a label match implicitly.
- Ambiguous labels return a conflict listing all candidate IDs and paths and usable explicit references.
- Path references resolve source registrations, not `manifest.name`. Accept `.` and explicit manifest paths consistently with existing CLI behavior.
- Unknown paths error for run/release; registration is explicit or happens through a successful scan.
- Resolve missing historical paths for administrative deletion from recorded bindings when unambiguous; do not require `EvalSymlinks` to succeed on a deleted source. Explicit ID remains the reliable historical reference.
- Relative paths are interpreted against the caller's working directory. Across a remote API, do not silently canonicalize a local directory as a server path; use typed server-path references or prefer label/ID. Deployment constructs destination paths on the remote host.
- Root containment must use path-aware checks, not string prefixes. Reject source aliases through symlinks outside the configured root. Avoid following directory symlink cycles. Detect same-file aliases where practical; do not promise bind-mount or network-filesystem identity equivalence.

Preserve existing ID-based resource routes for SDK state access. Add an admin-only resolver and lifecycle endpoints using structured JSON; CLI convenience resolution happens through these. Do not make every existing `/integrations/{id}` route guess whether its segment is a label or ID. Define 404 for no match, 409 for ambiguity/lifecycle conflicts, and validation errors consistently with existing API conventions.

Pass resolved ID and generation through submission, queue, run records, worker claims, retries, and run tokens. New executor request fields must explicitly contain integration ID and label; stop constructing `OTTER_INTEGRATION_ID` from `Manifest.Name`. Reserved runtime environment variables must not be overridden by manifest or extra environment inputs.

Check current active status and generation when claiming work, scheduling retries, resolving run credentials, and mutating state. Carry token generation through the authorization boundary into state mutation; an authentication check followed by an unguarded write has a race. Use a shared transaction/conditional write for the generation/status check and state mutation. Admin inspection of historical data may remain available, but integration credentials must be revoked on retirement.

Add `OTTER_INTEGRATION_NAME`. Update Python context exposure if appropriate; keep state addressing based on ID. Existing run IDs/log APIs remain stable. Add name snapshots to new runs and name fields to integration API views. Do not invent historical labels when they are unavailable; the old integration key can supply the legacy label during migration.

## 7. Lifecycle command behavior

### `otter register <path>`

Register valid source through the authority; idempotent when the active binding and marker match. Explicit registration clears a deletion suppression and allocates a fresh ID. Never adopt a supplied marker into old state. Show resulting ID, name, and path.

### `otter reset <ref>`

Resolve and journal the target before edits. Retire its old ID, revoke credentials, stop/settle running work, cancel queued/retrying work, and issue a fresh ID at the same path. Preserve old data/history for explicit inspection or later deletion. Report old and new IDs. Do not implement reset as simply unlinking `.otter-id` and waiting for another scan. Malformed marker repair should be possible through explicit path/ID reset after validating source and confirming it is a regular safe target.

### `otter delete <ref>`

Transition to deleting, block admissions/retries, revoke tokens, settle workers/log sinks, and purge state, queue records, run logs, run history, webhook tokens, owned releases/active links, and exclusively owned prepared environments. Enumerate all resources first. Do not delete shared Python runtimes, shared caches, SDK installations, or unrelated environment files. Use ownership metadata rather than broad filesystem globs.

Retain a minimal deleted-ID tombstone and path suppression, while leaving source files untouched. Only suppress a path still owned by the deleted ID: deleting a retired A must not suppress a newer B now at A's old path. Preserve its marker as inert source metadata if safe; a future explicit register replaces it. Repeated delete by explicit ID succeeds. Cleanup failures remain deleting and resume after restart. No late worker/log callback may recreate purged rows.

### `otter move <ref> <destination>`

Preserve identity through a journaled same-filesystem directory rename. Require matching source marker, absent/unreserved destination, allowed root, and no nested registered integrations for the initial implementation. Reject cross-filesystem operations before mutation where detectable and handle EXDEV without committing the binding.

Quiesce the integration, cancel queued work with a clear reason, settle execution, invalidate old generations, reserve both paths, rename, update binding, and restore scheduling. The marker and ID remain unchanged. Crash recovery inspects old/new paths and the journal rather than inferring moves during ordinary scanning. Reject ambiguous recovery states.

Plain shell `mv` is intentionally treated as disappearance plus fresh registration. No separate `new-id` command is needed; use reset. Defer automatic moves, cross-filesystem copy-and-delete, and general backup resurrection/import commands.

## 8. Releases and Python environments

Update `internal/cli/ref.go`, `release.go`, `prepare.go`, `internal/release/*`, and `internal/pyenv/*` to use authoritative IDs.

- Replace independent manifest-name resolution in `integrationTargets` and all `--source` branches. A source override must match the resolved source binding; it cannot stage arbitrary source under an existing ID.
- Resolve `(ID, generation, source path)` before staging. Check marker/binding before capture and again before activation. Stage in an isolated temporary location. If reset/delete/move intervenes, refuse activation; do not fall back to name.
- Offline staging/activation owns the workspace lock; online staging goes through the authority or an authority-controlled operation. The existing direct CLI writer must not remain an unguarded alternate route.
- Release storage remains `.releases/<id>/<digest>` and `.releases/active/<id>`. Metadata records ID and source provenance; add schema/version fields if needed.
- Exclude `.otter-id` from both copying and hashing. Keep code digests independent of marker contents. Metadata identity and directory ownership remain validated.
- Distinguish current source binding from historical release source. Authorized move must not invalidate rollback to an intact release owned by the same ID. Existing queued/retry snapshot semantics must be preserved except where lifecycle cancellation explicitly cancels them.
- Prepare environments using ID, not label or directory basename. Audit policy/digest inputs and existing recorded identities. Deletion must identify exclusive environment ownership reliably.
- Prevent retention/cleanup from deleting resources pinned by active operations or executions.

## 9. Deployment integration

Current deploy selection returns directory names and passes them to remote release as names. Separate these concepts explicitly: local relative source path, destination relative source path, label, destination ID.

Required behavior:

1. Local Stage excludes markers and marker temporary files. Revision calculation ignores them.
2. Remote rsync excludes/protects destination `.otter-id` from both transfer and deletion. Verify actual filter semantics with integration tests; exclusion in local Stage alone is insufficient with `--delete`.
3. A destination missing an integration gets a fresh remote registration. An existing destination registration persists through code updates and label changes. Local and remote UUIDs need not match.
4. Remote release resolves destination paths or explicit IDs, never a directory basename masquerading as a name.
5. Partial deploy does not delete or retire other integrations.
6. Runtime owner must be able to write new markers in the deployed source tree; update permissions narrowly, not by recursively chowning live data.

Coordinate remote source synchronization with reconciliation using a durable, narrow deployment-maintenance operation. Begin it through the daemon authority (or under exclusive ownership when stopped), mark affected paths, block their new admissions and registry reconciliation, then synchronize, register/verify destinations, prepare and activate, and end maintenance. This permits unaffected integrations to continue. A disconnected/dead deployment leaves affected sources blocked with an operation ID; retry the deployment to recover. Do not auto-resume a potentially partial source tree on a timer. Provide inspection through existing deploy status/diagnostics. If the repository's deploy sequence already has a safe equivalent, reuse it instead of adding parallel mechanisms.

Old active immutable releases must remain intact on failure. On startup recover deployment/identity operations before treating synchronized directories as authoritative observations. Cover both bootstrap with no running daemon and update with a running daemon.

## 10. Safe legacy migration

SQL schema creation and semantic identity bootstrap are separate steps. `database.Migrate` currently applies embedded SQL in independent transactions; filesystem marker initialization cannot be hidden in that mechanism. Add a bootstrap-complete flag, and never start scheduling merely because the SQL version advanced.

Suggested operator surface: `otter identity migrate --dry-run` and `otter identity migrate --apply --plan <file>`. Normal startup may bootstrap an unambiguous workspace automatically under exclusive ownership; collisions or conflicting provenance must stop bootstrap with actionable instructions. The reviewed plan identifies legacy-ID-to-owner-path decisions and inventory expectations. Applying it revalidates the inventory; a stale plan cannot silently proceed.

Migration procedure:

1. Acquire exclusive ownership. Create a consistent SQLite backup using a supported SQLite backup method or a verified checkpoint/closed-database procedure; do not copy only the main file while ignoring WAL contents. Inventory filesystem artifacts and intended marker edits for rollback.
2. Inventory all legacy IDs from state, runs, queues, webhook tokens, releases, and environment ownership, not just currently discoverable manifests. Reserve orphan IDs as retired records.
3. For an unambiguous active source, preserve `id = old_name`; no state/history/release-directory rename is necessary. Write that legacy ID into its marker through the journal.
4. For duplicate names, require an explicit owner path. Give other source directories fresh IDs. There is only one old state namespace: never pretend it can be separated by directory after the fact.
5. Verify releases against the selected owner and recorded provenance. Quarantine/disable mismatched or unverifiable active releases without destroying them or rewriting their ownership. Block queued attempts pinned to those releases for operator resolution; do not execute them under the selected owner automatically.
6. Preserve absent-source data as retired history. Missing old paths and unverifiable provenance require a decision, not inference from a basename.
7. Complete journaled marker/binding edits and initialize execution generation/name snapshots for legacy queued work where ownership is verified.
8. Mark semantic bootstrap complete only after recovery can produce a consistent result. Repeat application is idempotent and never reallocates already journaled IDs.

Do not choose a winner for the workspace's real `counter` collision in code or tests. Build a fixture with the same conflict. Preserve the old count/history intact pending operator selection. No silent merging, deletion, or assignment based on scan order.

Document coordinated CLI/daemon/SDK rollout. Older binaries are not supported against the upgraded registry because they can recreate name-based state ownership; rollback requires stopping the new runtime and restoring its backup plus recorded marker changes. Do not claim that an old binary's migration reader enforces this automatically.

## 11. Repository implementation map

Verified entry points; inspect callers and tests before refactoring:

| Area | Existing files | Required work |
|---|---|---|
| Discovery | `internal/config/discovery.go`, `discovery_test.go` | Separate manifest label from runtime ID; explicit scan errors; duplicate-label support |
| Persistence | `internal/database/*`, `migrations/0001_init.sql`, `0002_python_environment.sql` | New migration; identity tables; generation/name snapshot fields; bootstrap gate |
| Ownership | `internal/daemon/ownership_unix.go`, `ownership_other.go` | Share ownership lock with offline mutators; platform behavior |
| Runtime | `internal/daemon/daemon.go`, `registry.go`, `recovery.go`, `workers.go`, `view.go`, `tokens.go` | Authoritative identity service; scheduling and execution fencing; lifecycle recovery |
| Data | `internal/state/state.go`, `internal/runs/*`, `internal/queue/*` | Guarded state mutations; durable ID/generation propagation; deletion ordering |
| HTTP | `internal/api/backend.go`, `types.go`, `client.go`, `server.go` | Typed resolution; admin lifecycle operations; token generation; labels |
| CLI | `internal/cli/ref.go`, `cli.go`, `init.go`, `discovery.go`, `reload.go`, `release.go`, `prepare.go` | Remove local name-to-ID assumptions; lifecycle commands; migration interface |
| Child | `internal/executor/executor.go` | Explicit request ID/name; environment correctness |
| Releases | `internal/release/release.go`, `stage.go`, `layout.go` | Binding checks; marker exclusions; ownership and provenance |
| Python env | `internal/pyenv/manager.go` and helpers | ID-based preparation, ownership-aware cleanup |
| Deploy | `internal/deploy/target.go`, `build.go`, `render.go`, `remote.go`, `deployer.go`, `state.go` | Path/ID distinction; marker protection; remote coordination |
| SDK | `sdk/python/otter/*`, `sdk/python/tests/*` | Opaque ID and display name; compatibility tests |

Audit every `Manifest.Name`, `.ID`, `integration_id`, `OTTER_INTEGRATION_ID`, and integration-derived filesystem key. Do not perform a blind textual replacement: labels still belong in logs and manifests, while IDs belong in persistence and authority checks. Avoid adding runtime registration IDs back into parsed manifests.

## 12. Implementation phases and exit criteria

### A. Regression fixtures and containment

- Add tests reproducing duplicate-name release contamination and name reuse.
- If shipping an interim patch, hard-error duplicates in the legacy model and require path-based release ownership checks, including `--source` and `--all`.
- Final behavior allows duplicate labels, so replace the interim duplicate-name assertion at cutover rather than leaving contradictory tests.

### B. Identity foundation

- Add schema, ID/marker helpers, shared lock, operation journal, semantic bootstrap gate, and recovery.
- Implement pure observation-to-plan logic with deterministic table-driven tests.
- Exit: no operation adopts state from a copied marker; crashes between database and filesystem changes recover to one chosen ID.

### C. End-to-end identity plumbing

- Wire resolver, runtime registry, submissions, queue/retries, tokens, guarded state writes, executor, SDK, API, CLI views.
- Keep bootstrap gate closed until release/deploy/migration support is ready.
- Exit: an ID differs from its label throughout a real SDK-backed run, and all state belongs to that ID.

### D. Lifecycle and source reconciliation

- Implement register, reset, delete, move, path suppression, retirement, worker cancellation, and cleanup recovery.
- Exit: replacement never inherits either original or copied-source state; deletion stays deleted across reloads/restarts.

### E. Release and deployment

- Remove all bypass writers, add source-binding checks, environment ownership, destination registration, marker filters, and coordinated synchronization.
- Exit: repeated deploy changes code but preserves remote ID/state; concurrent reset prevents stale activation.

### F. Migration, documentation, and integrated validation

- Complete safe legacy bootstrap, conflict-plan interface, orphan inventory, provenance quarantine, and operator messages.
- Update docs and run full checks.
- Exit: fresh and legacy workspaces both work; migration reruns and crash recovery do not lose state/history or mint accidental replacement IDs.

## 13. Acceptance matrix

Use actual temporary directories and SQLite databases. Include restart/subprocess tests, not only mocked map updates.

### Identity and discovery

- Init and hand-authored manifests register fresh; repeat scan preserves IDs.
- Name-only edit preserves ID, state, history, webhook identity, and environment ownership.
- Copy A to B with the original present: distinct IDs; B starts empty; A retains state.
- Copy A to B then remove A before scan: B fresh; A retired.
- Copy A to B and C then remove A: B and C fresh, independent, same result under reversed observation order.
- Remove A and copy B to A, with daemon running/reloading and with daemon stopped: A fresh; neither old A state nor B state inherited; B unchanged.
- Same test when replacement has no marker: fresh ID.
- Restore same marker to same path: continuity, explicitly documented; reset produces freshness.
- Plain mv: fresh identity at destination, old identity retired. Explicit move: same ID/state at destination.
- Explicit move followed by recreation at the old path: moved instance retained, recreated instance fresh.
- Duplicate labels both register; bare-label run/release/prepare errors with candidates; explicit paths/IDs succeed independently.
- Invalid manifest, temporarily missing manifest, unreadable subtree, read-only new source, malformed marker, marker symlink, and source symlink escape exercise distinct outcomes.
- Incomplete scan writes no markers and retires nothing. Known unsafe sources cannot admit live-source runs.
- `validate` leaves database and filesystem unchanged.

### Execution and races

- Executor exports ID != name correctly; SDK addresses ID; extra environment cannot override runtime identity.
- Reset/retirement races with enqueue, claim, retry, token lookup, and state write: stale generation never writes after the transition commits.
- Existing cancellation grace and termination behavior settles runs; logs stop before purge; no late rows reappear.
- Two CLI processes versus daemon ownership cannot become competing registry writers.
- Release staged before reset/delete/move cannot activate afterward under stale binding.
- Historical release rollback after authorized move works for the same ID.

### Lifecycle recovery

- Delete purges exact owned state/history/logs/tokens/releases/environments, retains tombstone, leaves source, suppresses rediscovery, and is idempotent by ID.
- Register after delete clears suppression and creates fresh ID.
- Deleting retired A does not suppress or purge B at A's former path.
- Move rejects existing destination, nested registrations, root escape, and cross-filesystem rename without changing ownership.
- Inject crash/failure before and after journal creation, marker replacement, rename, binding commit, and each purge stage. Restart converges or reports a safe blocked operation; never aliases state.

### Migration and deployment

- Legacy ID=name state/history/tokens and valid releases survive; IDs may remain non-UUID strings.
- Repeated bootstrap is idempotent. Orphan IDs remain reserved. Duplicate collision requires an explicit plan.
- Conflicting release provenance remains preserved but inactive; queued contaminated snapshots cannot execute.
- Stale migration plan is rejected. Backup includes committed WAL data.
- Deploy stages no markers and leaves existing remote markers intact under actual rsync filter behavior.
- Fresh remote gets fresh remote ID; repeated deploy and label rename preserve it.
- Directory basename different from manifest name works; duplicate labels in separate directories work.
- Partial deployment preserves unrelated integrations. Interrupted synchronization remains safely blocked and retryable.
- Marker changes alone do not affect release content digest or deployment revision.

## 14. Checks and documentation

Run focused package tests while implementing, then `go test ./...`, the repository's Python test target (`make test-python`), and `go vet ./...` where supported. Run targeted Go race tests for identity, daemon, API, and state packages if the host toolchain supports the race detector; note the Makefile defaults CGO off, so configure the race invocation appropriately. Use Linux/macOS coverage for supported filesystem/lock semantics where CI is available. Report skipped platform checks honestly.

Update `README.md`, `docs/manifest-reference.md`, `docs/api-reference.md`, `docs/architecture.md`, `docs/operations.md`, `docs/deploy.md`, `docs/managed-python.md`, `docs/security.md`, and SDK documentation as relevant. Include:

- names versus IDs, reference syntax, ambiguity examples;
- marker format, copy/replacement rules, exact-backup limitation;
- explicit move requirement and read-only-source limits;
- reset versus purge, deletion suppression, recovery diagnostics;
- deployment destination authority and marker protection;
- migration conflict resolution, backup and rollback boundaries;
- opaque IDs in API/run output and the new name environment variable;
- accident prevention versus hostile same-user code isolation.

Final completion report should summarize behavior, migration/operator actions, test results, and known supported-platform boundaries. Do not claim the live workspace was migrated or its collision repaired. Leave the actual implementation internally coherent: no old name-based release path, direct CLI registration writer, or state mutation bypass may remain.
