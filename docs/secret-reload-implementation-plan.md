# Implementation plan: refresh integration secrets with `otter reload`

## Objective

Make an edit to the daemon's configured integration credentials file take effect for subsequent runs after a successful `otter reload`, without restarting the daemon or interrupting executing runs. Support newly added credentials, rotated values, and deleted keys in local foreground, local detached, and deployed operation.

Implement this plan in the existing checkout. Preserve unrelated changes: the working tree already contains substantial HTTP capture and inspection work, including edits to the daemon, executor, API, CLI, and documentation. Inspect the current diff before editing; do not reset, overwrite, or commit unrelated work. Do not read or print real workspace secret files; use temporary fixtures with synthetic values.

## Current implementation and relevant files

- `internal/cli/start.go`: `loadEnvValues` parses files, `loadEnvFile` copies values into the process environment while preserving existing environment values, and `projectEnvFiles` only returns files that exist. `startDetached` launches a child with the already-modified environment.
- `internal/cli/daemon.go`: `RunDaemon` constructs configuration and calls `daemon.New`. Trace both `otter serve` and `otterd` through this entry point.
- `internal/secrets/secrets.go`: `Provider.Get` resolves one key; the default `EnvProvider` reads the live process environment. `Resolve` calls `Get` separately for every declared secret.
- `internal/daemon/daemon.go`: `Options.Secrets` allows injected providers. `Reload` serializes with `reloadMu`, discovers integrations, then updates the registry, limits, and schedules. Discovery includes durable identity reconciliation; it is not a database transaction that can simply be rolled back.
- `internal/daemon/workers.go`: declared secrets are resolved immediately before launching a run and passed to the executor as `ExtraEnv`.
- `internal/executor/executor.go`: child environment assembly inherits host variables, with stronger filtering for managed Python, and manifest environment expansion falls back to `os.Getenv`. Audit both paths for stale file-derived values.
- `internal/deploy/render.go`, `deployer.go`, `target.go`, and `config.go`: deployment maps `otter.env` to `/etc/otter/shared.env`. The service currently gets this through systemd `EnvironmentFile`. The generated shared file also contains the daemon API token. The writer uses a restricted directory and a mode-0600 file, currently documented as root owned.
- `internal/api/types.go` and `internal/cli/reload.go`: reload results and CLI output currently only describe integration changes. This checkout accepts `otter reload` with no positional integration argument.

## Required behavior

1. Keep the existing admin-only `POST /v1/reload` and global `otter reload` command. Do not add a watcher, integration-specific reload, new endpoint, external secret store, or automatic release creation.
2. Configure the credentials path at startup and retain its absolute path even if the default file is absent. A file created after startup must become usable on reload. A remote reload reads the remote daemon's configured file; the client must not send local credentials or select an arbitrary file.
3. Local `otter start` defaults to `<project root>/otter.env`. Add an explicit `--secrets-file` option, wired through start/serve/otterd as appropriate, for standalone and deployed use. An explicit path overrides the inferred default. Resolve relative paths once at startup.
4. A default file absent at initial startup is an empty optional source. A missing explicitly configured file is an error. Once a default file has loaded successfully, its disappearance is a reload error that retains the previous snapshot. To clear all file credentials deliberately, use an existing empty file. Permission errors and malformed files always fail reload.
5. Capture actual inherited environment values before applying project files. Preserve explicit inherited overrides, including an explicitly empty override: an empty effective credential is unavailable and must not fall through to an older value. Changes to a separate shell after startup cannot affect a running daemon.
6. File-backed keys must not also survive in a lower-priority environment provider. Deleting a file key removes that source's value. A genuine inherited override remains authoritative by design.
7. Each run resolves all its declared secrets from a single immutable snapshot. Runs that already captured a snapshot may launch or finish with it. Runs resolving secrets after successful reload use the new snapshot; this includes queued work and retries. Executing child processes are not modified or restarted.
8. Reload refreshes integration credentials only. Do not reconfigure listeners, workers, API authentication, notifications, or other daemon settings. Preserve supported startup settings in existing files, but document that their changes require restart. `otter.daemon.env` remains startup-only.
9. Failed file preparation or failed discovery must not publish a new secrets snapshot. Parse credentials before discovery to avoid identity reconciliation on a malformed-file failure. Preserve existing handling of invalid individual manifests: they are reported, not converted into a global reload failure. Do not claim transactional rollback of identity database side effects.
10. Do not expose values, source lines, hashes of values, or credentials in errors, CLI output, logs, API responses, command arguments, or persisted run metadata.

## Implementation steps

### 1. Share a safe environment-file parser

Extract a small parser into a package usable by CLI, deployment, and secrets without circular dependencies, such as `internal/envfile`. Replace the duplicate parsing paths with it.

Retain the documented syntax: blank lines, full-line comments, optional `export`, `KEY=value`, and matching surrounding quotes. Preserve literal inline `#`, `$`, and backslashes according to the existing supported format; do not introduce shell execution or interpolation. Validate environment names and reject NUL bytes. Review existing parser tests before changing edge-case behavior. Diagnostics should give path, line number, and a generic reason, never the raw line or value. Add explicit regression coverage for malformed input containing a synthetic credential.

### 2. Introduce an immutable, reloadable credentials source

Implement a provider in `internal/secrets` with fixed inherited overrides and a replaceable file snapshot. Separate preparation (read/parse/build candidate) from publication (a short atomic-pointer or lock-protected swap). Candidate preparation must not mutate the live provider or process environment. Track optional-file history only when a candidate is committed.

Keep the existing `Provider` interface compatible with injected/static/external providers. Add an optional snapshot capability, for example `Snapshot() Provider`, and have `Resolve` acquire that snapshot exactly once before iterating keys. A per-key atomic lookup alone is insufficient: two keys could otherwise come from different generations. Published maps must never be mutated or returned to callers for mutation.

Expose preparation/publication only to the daemon's configured reloadable source; do not assume every injected `Options.Secrets` supports file reload. Existing non-reloadable providers must keep working. Keep the default environment-only mode available when no file is configured.

### 3. Make startup preserve environment provenance

Refactor startup so it keeps three distinct inputs: original inherited environment, startup-only daemon configuration, and reloadable integration credentials. Prefer parsing/merging maps and passing explicit startup/provider options over installing integration credentials into global `os.Environ`. Retain current startup configuration behavior where files provide daemon settings, but freeze those settings after configuration construction.

For detached startup, pass the original inherited environment plus necessary non-secret internal markers to the child, and let the child load files itself. Do not pass the parent's merged file values as apparent shell overrides. Keep the parent's resolved project, data, and listen decisions consistent with the existing startup flow. Never serialize secret values into argv or a new on-disk bootstrap record.

Document and test precedence explicitly. Actual inherited environment overrides the credentials file. `otter.daemon.env` is a startup configuration source, not a stale fallback copy of `otter.env`. Check compatibility for existing environment-only secret use and startup settings; avoid silently broadening this feature into general configuration hot reload.

### 4. Integrate publication into daemon reload and run execution

Under the existing reload mutex:

1. Prepare the candidate credentials snapshot.
2. Discover integrations using the existing flow.
3. Calculate the registry result and retain existing removed-run cancellation behavior.
4. Enter the successful publication phase: publish the candidate secrets before making newly discovered integrations runnable, then update registry, limits, and schedules. No fallible secret-file work belongs after publication.
5. Return only after both updates are visible.

This does not require a new global transaction spanning the registry and secrets. It does require a clear secrets publication point and whole-snapshot resolution for every run. Existing runs may observe the old or new complete generation during the transition, never a mixed pair of credentials.

Ensure the executor receives declared secrets exclusively from the captured result. Audit unmanaged child inheritance and `${...}` expansion so they cannot retrieve deleted or outdated file values from `os.Getenv`. Keep existing scoped `OTTER_*` protections and host/runtime environment behavior. File-backed integration secrets should be declared in `secrets:`; expansion of a declared secret uses the captured `ExtraEnv`. If removing accidental inheritance of undeclared file credentials changes existing behavior, document that migration explicitly and add a regression test rather than leaving a stale fallback.

### 5. Wire deployed operation correctly

Update generated units to pass the shared credentials path explicitly. Stop using systemd's shared-file `EnvironmentFile` entry as the source of integration values: those values would otherwise look like inherited overrides forever. Keep startup-only daemon environment loading as appropriate.

Preserve the API token currently written into the shared file: the daemon's startup configuration must still receive it before authentication is initialized, while reload must not rotate the live API token. Test this transition; simply deleting the unit entry would break authentication.

Make the shared file readable by the configured `RunAsUser` without making it public. Prefer a root-owned directory traversable by the service group and a root-owned, service-group-readable file (for example directory 0750 and file 0640), using the configured group. Verify installation ordering creates the account/group before ownership is applied. Check every install/update path, not just initial deployment.

Write deployed secrets through a restricted temporary file in the same directory and atomically rename after setting ownership/mode. A concurrent reload must never accept a partially written file. Update generation comments and deployment tests. An already-running old binary/unit needs a one-time upgrade/restart to gain this capability; do not promise that this code can retroactively change it. Do not redesign the broader deployment restart workflow for this feature.

### 6. Report secret refreshes and document the contract

Extend `ReloadResult` compatibly with a small nested credentials summary: whether a file source was refreshed and counts of effective keys added, changed, and removed. Omit values and key names. Compare effective snapshots, so a file edit masked by an inherited override is not reported as an effective rotation. No-file/custom-provider mode should not claim that files were refreshed.

Update CLI rendering so a secrets-only reload is visibly successful even when the integration set is unchanged. Distinguish a successful no-change file reread from no configured file. Preserve existing integration summaries, error exits, admin authorization, and concurrent-reload conflict handling. Update API serialization/client tests and CLI help.

Update README, operations, deployment, security, and API documentation as relevant. Explain paths, precedence, default-file creation, missing-file policy, deletion, effects on running/queued work, restart-required settings, and the requirement for an active release. Include the developer workflow: edit `otter.env`, run `otter reload`, then run the integration. Add/release a new integration using existing commands when needed.

## Acceptance tests

Use temporary fixtures and deterministic synchronization; never depend on real workspace secrets or sleep-based race timing.

- Start without optional `otter.env`, create it, reload, and successfully run an integration requiring the new secret.
- Rotate a credential; subsequent execution sees the new value without changing daemon PID or API availability.
- Remove a required key; subsequent execution fails before Python starts and does not recover the startup value. Empty the entire existing file and verify all file keys are removed.
- Genuine inherited overrides remain authoritative, including blank overrides. Detached and foreground startup behave identically.
- Missing explicit file fails startup; disappearing previously loaded default file, permission failures, and malformed files fail reload and preserve old credentials. Initial optional absence remains valid.
- Malformed-file reload does not update the integration registry; fatal discovery failure does not publish prepared credentials. Existing invalid-manifest reporting still works.
- An executing process retains its old value; a queued run and retry resolving after reload use the new one.
- Concurrent resolution during repeated swaps only sees complete old/new pairs of credentials. Run the affected packages with the race detector.
- Existing static/custom-provider daemon tests still pass without requiring file configuration.
- Managed and unmanaged child paths, plus manifest environment expansion, cannot access stale/deleted file credentials. Unrelated host variables and scoped run authentication still behave correctly.
- A secrets-only reload has useful human output and compatible JSON; a no-change reload reports successful refresh; errors do not expose synthetic secret values anywhere.
- Generated service configuration passes the correct path without injecting shared secrets through systemd. Startup API authentication still works, file ownership permits service reads, and the deployment writer uses atomic replacement. Cover non-default service users/groups.
- Existing reload behavior remains intact: no interruption of executing runs, unchanged cron next-fire times, newly discovered integrations still require releases, and concurrent reload remains a conflict.

## Validation and completion

Run focused tests while developing, then `go test ./...`, `go vet ./...`, and `CGO_ENABLED=1 go test -race ./internal/secrets ./internal/daemon ./internal/cli ./internal/executor` when the host supports the race detector. Include the extracted parser package in focused/race checks as appropriate. Run existing Python tests if executor/SDK behavior is touched. Format only edited Go files. Record pre-existing failures separately from regressions.

Add a local end-to-end test using a temporary workspace and a built binary for foreground/detached secret refresh; verify PID continuity and real child behavior. If Linux/systemd is unavailable, validate rendering, permissions scripts, and path wiring in tests, and explicitly report that live systemd verification was not performed.

The final handoff should list the resulting behavior, changed files, test results, compatibility/migration notes, and any remaining limitations. Do not claim completion with only a provider unit test: startup provenance, detached execution, deployment permissions, deletion, and per-run snapshot consistency are essential parts of this change.
