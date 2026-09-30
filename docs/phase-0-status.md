# Phase 0 status ledger

Status: living document. Created: 2026-09-29.

The state of every task in [phase-0-tasks.md](phase-0-tasks.md), and the record
of what closed it. The execution plan is
[phase-0-execution-plan.md](phase-0-execution-plan.md); the W2 host run plan is
[phase-0-host-run-plan.md](phase-0-host-run-plan.md).

## How this document is maintained

- **`R-19` is the bar.** A row moves to `done` only with a command that exits 0
  or a dated record in [evidence/phase-0/](evidence/phase-0/). "The code does
  this" is not a status.
- **One writer.** The conductor session owns this file. Workers and verifiers
  report; they do not edit it.
- **A status is only as good as its verifier.** Every `done` row names the
  independent check that would have caught it being wrong — normally a mutation
  of the assertion.
- **Blocker or date, never a feeling.** A stalled row says what it is waiting
  for and who owns it.

Status values: `not started`, `in progress`, `blocked`, `host-gated`,
`done`.

## Ledger

| ID | Task | Status | Work | Evidence → record | Verifier |
| --- | --- | --- | --- | --- | --- |
| P0-01 | Real crash/kill harness | **done** | merged `4e8fe7e`; `internal/daemon/crash_harness*_test.go` | [record](evidence/phase-0/2026-09-29-p0-01-crash-harness.txt) — Linux `ok 33.2s`, harness deterministic over 3 runs, 3 mutations red→green. Verifier removed `Pdeathsig`: 53 child survivals and 10 duplicated chains, so "none executed twice" is load-bearing. Appendix A: FM-01 **Real (Linux)**, FM-02 **Real (atomicity)** | VERIFIED @ `42ca217` |
| P0-02 | Retry release/environment binding | **done** | merged `7b990bc`; `retry_binding_test.go` + `recoveryRetryPolicy` in `recovery.go` | four tests (six cases) prove the retry half, the managed environment and the prune, and the verifier's three mutations — live policy, live release, live environment — each break them. `runtime-contract.md` §5.1 promoted from non-guarantee to guarantee; `OT-011` retired. Findings: the recovery live-vs-bound policy defect was fixed; a transiently unreadable snapshot strands a retry → `OT-012` | VERIFIED @ `05e3187`, corrections applied |
| P0-03 | Full-fidelity backup and restore | **done; merged `22d6263`** | merged `fa233e8`; clean-host mode on branch `p0-03-clean-host-mode` @ `3bb2a16`: a two-host mode plus a preflight gate that refuses a dirty target, a same-machine pair, a path or version mismatch; hostless selfcheck wired into `go test ./...` via `internal/drill`; default mode unchanged. **HW-7 is now runnable.** Merges cleanly with W-B (verified: both features survive, `sh -n` clean). `operations.md` wording proposed but not applied | [record](evidence/phase-0/2026-09-29-p0-03-backup-restore-drill.txt) — drill clean ×3, three sabotage modes red at the run assertion. Verifier wrote 9 additional independent sabotages and confirmed the repointing is a legitimate restore step, not a masked defect | **PARTIALLY VERIFIED @ `3bb2a16`** — the hostless half is real: the selfcheck runs the *shipped* scripts (real sqlite3, real manifest, real probe) against real directories, not stubs, and 4 independent mutations all went red, including deleting the gate call itself, which grew the raw call log by exactly the `otter inspect` step that must never run for a dirty target. Cache invalidation genuinely works (checked for a new file and a deleted file). **Open, fix round dispatched:** R1 is a likely HW-7 blocker — `remote_otter` relies on the CLI's token glob `/etc/otter/*.env` while `otter deploy` writes `/etc/otter/workspaces/<ws>.env`, so a normally-deployed pair 401s at the first call and reports it as "the runtime does not know this job". Also R2 (target unit paths/port never asserted), R3 (ownership account not checked on target), R5 (`STATE_KEY` unvalidated into a remote command), R6 (the shell `or` binds to `head`, masking failures), R7 (staging dirs left on failure). R1–R3 are inspection findings, not reproduced failures **Re-verification: NOT VERIFIED @ `2a32ca7`.** R2–R8 were confirmed sound (R5 and R7 extracted verbatim and executed under dash; R6's old masking reproduced live; no token value leaks into any report or fixture), but the composition cannot run. D1: the gate compares the source daemon's version (from `otter status`) against `otter --version`, which prints `otter vX` while the daemon reports `vX` — so no real pair can ever agree and HW-7 dies at preflight before any backup; the selfcheck fabricates agreement by setting both fields to the same string, so it never catches it. D2: `systemctl show -p EnvironmentFiles` piped to `head -n 1` keeps only the first file, but real systemd prints one line per file — so the token-bearing `.env` is dropped; a single-workspace host survives on the `/etc/otter/workspaces/*.env` fallback, but a multi-workspace host is falsely refused as ambiguous. Both are the same class of defect: fixtures that encode assumptions real systemd contradicts, and the selfcheck certifies them — its R1 case puts both `EnvironmentFiles` on one line (real systemd prints one per line) and its good-pair fixture sets both version fields to one string while the fake ssh fabricates agreement. Fix round landed @ `bbce619`: both sides reduced to the version token before comparison (D1), every `EnvironmentFiles=` line collected instead of the first (D2), the fixtures rebuilt to the real shapes, the probe now falls back to the serving workspace's own CLI when the dispatcher refuses `--version` (so the one-workspace-per-host requirement is gone), and the transcript re-recorded with consistent build references. **Third verification in flight**, framed to audit every fake in the suite against the real thing it stands in for — two rounds running, a hand-written fixture asserted something real systemd does not do, and the suite certified it. **Round 3: NOT VERIFIED @ `bbce619`.** D1 and D2 are genuinely fixed — reproduced against real systemd 255 *and* 252 with a live daemon, including decoy layouts — and the verifier confirmed the `EnvironmentFiles`/`ExecStart` double is now a byte-faithful capture. But two new defects, both fatal, both hidden by the transport double. **D5:** `remote_script` builds the remote command with `$*`, which drops empty arguments — so the documented invocation (no token) delivers 7 arguments instead of 9 and `/etc/otter` lands in the api_token slot, failing the token gate on *every* host; and the clean backup call gets 4 arguments where `hot-backup.sh` requires 5, so **the clean path cannot run at all** while the sabotage path works by accident. A scratch copy with quoted arguments passes the entire gate, which isolates it. **D4:** the CLI fallback is computed *after* the daemon is measured, so on a two-workspace host a live daemon reports `daemon=stopped` and the gate refuses — the claimed removal of the one-workspace-per-host requirement is falsified. **The structural point:** the only assertion about a real probe's `daemon` field demanded `stopped`, so a mutation making the probe report `stopped` unconditionally left the suite green — that is what hid D4. Also noted: because the selfcheck runs on macOS, the Linux branches (GNU `stat -c`, `/etc/machine-id`, `getent`, uid 0) are only ever exercised by a synthesized fixture. Round 4 dispatched (D4, D5, the `daemon=running` assertion, Linux-branch coverage). The flat-vs-nested precedence judgement was tested and **found safe** — no false-pass path |
| P0-04 | Resource caps in the generated unit | **done; merged `7319363`; host claim now proven** | merged `f5dc220`; branch `p0-04-memory-swap-max` @ `f22c9bb` adds `MemorySwapMax` with **default `0`** (a separate validator, because `0` is valid and meaningful here while the size caps refuse it), and reworks the drill (real 512 MiB loop swap in the container, `memory.swap.max` asserted against the unit, survivor detection from `cgroup.procs` — the old `pgrep` was proven dead by planting a forking runaway, all four claim clauses reported together). `MemorySwapMax=0` fixes the thrash, but the worker then measured that **`MemoryHigh=60%` alone still defeats the kill**: over 900s `oom_kill 0`, `memory.current` settling 803→831 MiB between the 787 MiB soft cap and the 983 MiB hard cap, ended only by the job's own 600s timeout. **Decision implemented @ `6036d55`:** `DefaultMemoryHigh` is now `off` (the flag, its validation and the YAML key are untouched — only the default moved), so `MemoryMax` is the binding cap. `deployed-style` passes with `oom_kill 1`, and a new `soft-cap` case keeps the feature covered by asserting its honest consequence — parked below `memory.max`, `oom_kill 0`, ended at its own timeout — rather than by weakening a clause. `make drill` is green again. **Verification in flight**, centred on the one question that matters here: whether any killed-clause assertion was relaxed to make the suite green. **The Castor unit still runs `MemoryHigh=568918016` (60%) and must be redeployed before HW-3 can re-run against the fixed runtime** — after redeploy the unit should show `MemoryMax=75%`, `MemorySwapMax=0`, `CPUQuota=200%`, `TasksMax=512` and no `MemoryHigh` line | [host failure](evidence/phase-0/2026-09-30-p0-04-unit-caps-host-failure.txt) — the Castor re-run that falsified the claim. The branch adds `2026-09-30-p0-04-unit-caps-swap.txt`, `…-swap-mutations.txt` and `2026-09-30-p0-04-memory-high-default.txt`, linked here once verified and merged | container VERIFIED @ `def32b8`; the host claim was falsified |
| P0-05 | Pin the version; no auto-upgrade | **done** | `main` (W0) | [pin recorded](#pinned-runtime-version) + [operations.md](operations.md#upgrades); no code path updates the runtime — 2026-09-29 baseline | 2026-09-29 baseline run |
| P0-06 | Deployment-failure recovery | **done** | merged `30f4fd9`; `scripts/drill/deploy-failure.sh` + a containerised SSH/systemd deploy target | [record](evidence/phase-0/2026-09-29-p0-06-deploy-failure-drill.txt) — the real `otter deploy` converges a systemd container over ssh; clean ×2 (21s/24s). The candidate is staged, then its preparation is refused, so it never activates: `MainPID` unchanged, `/health` answered, and a new run succeeds bound to the previous release **and** environment digests. Verifier: both sabotages red on their matching assertion, the injection confirmed after staging and before activation, and the checkout verified clean after every run. Scope: container not the Castor VM, no binary/unit swap, stub `uv` | VERIFIED @ `bb6f6a9`; `30f4fd9` delta (stub rename, re-recorded transcript) reviewed |
| P0-07 | Provision the Castor host | **done; merged** | Infra deployed (`otter-platform` `CastorRuntime`), and the stack was **fixed and the host rebuilt** after its user-data aborted twice on the same class of mistake — an untolerated socket-activated `sshd` reload, then an `sshd -t` needing `/run/sshd` on a first boot. The rebuilt host reaches `cloud-init: done` with no errors, creates its own swap, and writes the provisioning report. HW-0/HW-1 re-run there. Branch `p0-07-provisioning-script` @ `5fb9752` adds `scripts/provision.sh`, `scripts/assert-host-permissions.sh`, two fixture suites and `provisioning-test`. **First real-host assertion run (21 pass, 5 fail)** found three of the failures are the assertion, not the host: it cannot start against an `otter deploy` workspace (it reads `service_name`/`data_dir`; the deployer writes `unit` and derives the data dir), it demands the credential env files be owned by the service account when root-owned `0600` is both what is implemented and the safer posture, and it rejects `otter.db` at `644` inside a `0700` directory | [baseline](evidence/phase-0/2026-09-30-p0-07-host-baseline.txt), [deploy](evidence/phase-0/2026-09-30-p0-07-hw1-deploy.txt), [rebuilt host + assertion](evidence/phase-0/2026-09-30-p0-07-host-rebuilt.txt) | **PARTIALLY VERIFIED @ `5fb9752`** — the loaded-unit reading, the no-reload finding, the hard swap/report gates and the script boundary all held under attack; but the suites' falsifiability claim was false (deleting the data-directory owner assertion left all 30 cases green, and the `otter.db` mode, `::` wildcard and `swappiness` checks could be neutered), and the tool list is a superset, not a match |
| P0-08 | Egress and managed Python | **done; merged `262be2d`** | Branch `p0-08-egress-managed-python` @ `3f9f300`: `internal/pyenv/egress.go` (prepare-time preflight that fails before any fetch, with platform/catalogue attribution only when the preflight passed), `--index`/`--python-mirror`/`--egress-endpoint`/`--skip-egress-check` threaded through prepare, release and deploy; route excluded from environment identity. **HW-2's core is met**: the deploy prepared CPython 3.13.1 on arm64. **Not merged** | pending — HW-2 preflight line. **Round 3 @ `0286b2d`** (code `f718d63`) fixed both refinements by measurement: `canonicalIndexURL` now folds exactly what uv folds (scheme/host case, redundant default port, empty path to `/`, dot segments on the escaped path) and keeps a non-empty trailing slash and percent escapes significant; the mismatch hint points at the spelling uv records, so following it terminates; and `UV_INDEX`/`UV_EXTRA_INDEX_URL`/`UV_FIND_LINKS` are refused with an otter-level message when the lock records a registry, while a no-registry lock and `UV_NO_INDEX` stay accepted. Seven mutations red. **Fourth verification in flight**, centred on two questions: whether the canonicaliser over-refuses shapes the tests do not cover, and whether refusing an additive variable is wrong when it names a source the lock already records | **PARTIALLY VERIFIED @ `3f9f300`** — all 7 claims reproduced and 8 mutations went red, including an independent execution of `Resolve` on both revisions (identical digest `57486ed5…90bf` with and without the route, so identity really is route-free). **Material defect:** the session pinned uv 0.5.9 refuses `uv sync --locked --default-index <mirror>` whenever the lock records a different registry URL — *"The lockfile needs to be updated"* — so the normal PyPI-locked project passes the new preflight and then fails at sync with no otter-level hint, and the docs advertise `--index` as the mirror-only route. No test can see it (all route tests use a fake uv). Also missing: **no evidence file was committed**. Also: `RecipeVersion` not bumped though the comment says to bump when preparation flags change **Round 4 verification: PARTIALLY VERIFIED @ `0286b2d`** — the verifier built its own folding matrix with real uv 0.5.9. The bare-host fix works, the guards are genuinely narrow (no-registry lock and `UV_NO_INDEX` confirmed benign), the hint terminates for the spellings it names, and every mutation goes red. But: the **additive-variable refusal over-refuses a source the lock already records** (uv `exit 0` where otter refuses — a plain, realistic multi-index setup — and the hint's first remedy then fails, since `--index A` alone cannot resolve a two-index lock; only omitting `--index` works); two **under-refusals reproduce the original defect** (otter folds percent-escape hex case and drops an empty query where uv does not, so `Manager.Prepare` proceeds and uv's bare lockfile error reaches the operator); and `lock_test.go:163-164` **asserts the opposite of measured uv behaviour** — a mutation that stops folding hex case goes red, so the suite actively defends the bug. Sharpest result: a *fix-shaped* mutation that adds `%2E`-dot folding left the whole focused suite green while flipping three measured over-refusals. Round 4 dispatched, and **bounded**: the realistic items must be fixed, but the exotic URL spellings may be handled by making the hint always terminate rather than by chasing uv's canonicalisation. |
| P0-09 | Failure notification | blocked on human | code already complete | pending — needs a real channel | pending |
| P0-10 | Independent liveness detection | in progress — artifact done, **evidence host-gated** | `main`; `scripts/heartbeat.sh`, `heartbeat.service`, `heartbeat.timer`, `scripts/drill/liveness.sh` | [record](evidence/phase-0/2026-09-29-p0-10-liveness-drill.txt) — no runtime → no ping and a non-zero exit; real `otterd` → the ping is logged; runtime killed → pings stop. `DRILL_SABOTAGE=ping-always` goes red on the first assertion, so the drill tests the dead-man property rather than a script's exit code. The task's own evidence — kill the daemon and watch an alert arrive — still needs the external service and the host | pending |
| P0-11 | Disk retention and thresholds | not started | — | pending | pending |
| P0-12 | Scope Castor's credentials | blocked on human (values) | Branch `p0-12-credentials` @ `a162f51` (test and doc only): value-in-list, `secrets:` mapping, a never-started tripwire, 0600 env files; `manifest-reference.md` corrected; [capture](evidence/phase-0/2026-09-30-p0-12-secret-scoping.txt). Finding: an identifier-shaped value is a legal env-var name, so syntactic validation cannot separate name from value | pending — needs names, scopes, values; window HW-5 | **VERIFIED @ `a162f51`** — all five claims reproduced from clean, and 9 mutations went red (6 of the verifier's own). It closed three gaps the author's evidence left: an always-failing resolver passes every negative assertion and is caught only by the positive control; the stdout-empty assertion is non-vacuous; and the restart leg is a genuinely distinct `Daemon` over the same data dir. Two prose overstatements to correct: "every new assertion" (4 of 6) and a parse-time/validate-time blur in `manifest-reference.md`. Residual: the mode assertions check generated script text, never its execution on a host — that is HW-5's job |
| P0-13 | Select and harden the job | blocked on customer | — | pending | pending |
| P0-14 | Shadow run beside the Lambda | not started | after P0-13 | pending | pending |
| P0-15 | Cutover | not started | after all blockers | pending | pending |
| P0-16 | Operate for 30+ days | not started | elapsed | pending | pending |
| P0-17 | Write the runbook | not started | written while operating | pending | pending |
| P0-18 | Produce the ranked gap list | not started | after P0-16 | pending | pending |

## Host windows (lane B, concurrency 1)

The Castor host exists as infrastructure, so the host-gated tasks run as a queue of
short windows rather than as parallel drills. The queue, its ordering and its rules
are in [phase-0-host-run-plan.md](phase-0-host-run-plan.md) §5–6. **One window at a
time**; nothing else touches the host during a window, including read-only commands.
A window with no transcript is an open window.

| Window | Task | Status | Transcript |
| --- | --- | --- | --- |
| HW-0 host identity + clean baseline | P0-07 | **done** 2026-09-30 | [baseline](evidence/phase-0/2026-09-30-p0-07-host-baseline.txt) |
| HW-1 deploy the pin | P0-05, P0-07 | **done** 2026-09-30 — `v0.3.0-rc1` @ `dd8bb28` | [deploy](evidence/phase-0/2026-09-30-p0-07-hw1-deploy.txt) |
| HW-2 egress + managed Python | P0-08 | core assertion met: CPython 3.13.1 prepared for both jobs on arm64. W-B's explicit preflight still pending | [deploy](evidence/phase-0/2026-09-30-p0-07-hw1-deploy.txt) |
| HW-3 unit caps on the real host | P0-04 | **PASSED 2026-09-30** on the same host, against the fixed unit: `oom_kill 1`, `memory.peak` exactly `memory.max`, `swap.current 0` (the cgroup never swapped), the run killed in **1.417s**, `MainPID` unchanged, `NRestarts 0`, no survivor, `/health` 200, host running. The earlier run of this window had falsified the claim | [failure](evidence/phase-0/2026-09-30-p0-04-unit-caps-host-failure.txt) → [fixed](evidence/phase-0/2026-09-30-p0-04-hw3-unit-caps-fixed.txt) |
| HW-4a notification / HW-4b liveness | P0-09, P0-10 | channel exists (Slack webhook in `otter.daemon.env`); not yet run | — |
| HW-5 credentials | P0-12 | blocked on Castor's values | — |
| HW-6 retention + disk pressure | P0-11 | blocked on WS1/WS4 | — |
| HW-7 backup → restore onto a clean host | P0-03 | **PASSED 2026-09-30, twice.** A live runtime backed up while serving, restored onto a clean second host, a job run there. The drill target was **destroyed** afterwards; recreate it from the committed stack in ~3 minutes when a second host is next needed | [pass](evidence/phase-0/2026-09-30-p0-03-hw7-clean-host.txt) |
| HW-8 final posture re-scan | P0-07 | **done** 2026-09-30 — the external full-range scan found only 22/tcp, and `assert-host-permissions.sh` exited 0 on the real host, reading the sandbox directives back from the loaded unit, the real `workspace.json`, the root-owned `0600` credential files in a `0700` directory, swap and the provisioning report | [assertion](evidence/phase-0/2026-09-30-p0-07-hw8-assertion.txt) |

**Deployed workspace (stand-in, not Castor):** `otter-examples-b379f933` on
`ubuntu@44.198.213.184`, unit `otterd-otter-examples-b379f933.service`, running
`v0.3.0-rc1`. The project is the public `otter_examples` repo (Shopify →
Salesforce). It is a **stand-in** for P0-13's job, chosen because it is a real
managed-Python project with real schedules. **Both jobs are paused** (`otter
pause`), the pause is persisted (it survives a unit restart), and no credentials
are deployed, so nothing can reach a vendor API. Tear it down before Castor's
real project is deployed.

- **P0-07's `--listen` fallback is unpinned, and it guards a security assertion.** An exhaustive sweep
  of all 33 `fail` statements in `assert-host-permissions.sh` found 32 go red when neutered and one —
  `could not determine the daemon's --listen address`, around line 483 — does not. With it removed and
  the address empty, the `[ -n "$api_addr" ]` guards skip **both** the loopback assertion and the
  API-listening check, so a security assertion disappears while all 48 cases stay green. The record and
  the `ExecStart` derivation mask each other, so neither is pinned, and `--workspace-dir` alone is the
  only path the fixtures exercise.
- **P0-07's "one case per assertion, every check can be disabled and its case goes red" is overstated.**
  48 cases include positive and clean cases, and 11 of `provision.sh`'s 27 `die` paths are unpinned
  (probe and transport failures). The security-relevant dies are all pinned.
- **P0-07 also leaves `env-absent-strict` unpinned** — making absent env files fatal keeps the suite
  green, yet `otter deploy` deliberately writes no shared `.env` when nothing shared is configured and
  the unit uses `EnvironmentFile=-`. The tolerance is load-bearing.
- **The assert harness is machine-conditioned**: `FIX_SS_ABSENT` removes only the fixture shim, so a
  test machine with a real `ss` on `PATH` gives a false red. Worth knowing before believing a green run.
- **`FIX_OWNER`, `FIX_GROUP`, `FIX_NAME` and `FIX_WORKSPACE_DIR` are dead knobs** that change nothing.

## Follow-ups recorded, not blocking

- **P0-03's contract test has a spelling hole.** Its raw-read guard matches only the literal
  `$WORK/backup/`, so `"$WORK"/backup/...` would bypass it and the selfcheck would still pass. The
  shipped fix is sound — all four archive reads route through `backup_path()` — so this is a hole in
  a *detector*. Harden it structurally: reject any `$WORK`/`backup` occurrence outside `backup_path`.
- **P0-08 lost two negative tests** in a round-4 restructure and they were not replaced: `UV_NO_INDEX`
  is not a conflicting source, and a no-registry lock with an additive variable set. The merge-gate
  verifier proved both with mutations that are green now and were red at `f718d63`. The shipped
  behaviour is correct; the guards are missing.
- **P0-08's refusal comment is unreproducible.** It says an unrecorded index carrying a needed package
  exits 2 while an empty one exits 0. Measured: with the lock's index configured uv accepts both;
  without it, uv exits 2 because it falls back to pypi.org. Only the prose is wrong.
- **`DefaultMemorySwapMax` is pinned by the Go tests, not by the drill.** Every unit-caps case passes
  `-memory-swap-max` explicitly, so the drill would not catch a regression of that default. Closing it
  means giving the case the same `default` sentinel `MemoryHigh` got.
- **`docs/phase-0-execution-plan.md:323` still cites `managed-python.md:482–485` and
  `manager.go:392–405`**, both moved by P0-08's edits. The five citations W-B listed earlier were
  re-pointed; this one was missed.

## Shared prerequisites

Not Phase 0 tasks themselves, but the machinery several tasks' evidence depends
on. Each was verified rather than assumed.

| Prerequisite | Status | Where |
| --- | --- | --- |
| Evidence convention — one dated record per run, raw output, named host | done 2026-09-29 | [evidence/phase-0/README.md](evidence/phase-0/README.md) |
| Drill harness — `scripts/drill.sh`, `make drill`, `scripts/drill/<name>.sh` | done 2026-09-29; dispatcher verified for the empty, failing, aggregate and unknown-name cases | [scripts/drill.sh](../scripts/drill.sh) |
| Linux container route for Linux-only evidence | done 2026-09-29; the `Pdeathsig` test builds and passes in a container | [2026-09-29-linux-docker-probe.txt](evidence/phase-0/2026-09-29-linux-docker-probe.txt) |
| Worktree isolation for workers (`.worktrees/`, one branch per task) | done 2026-09-29; verified from inside a subagent; in use since 2026-09-30 — four live worktrees (`p0-03`, `p0-07`, `p0-08`, `p0-12`) | gitignored, see [phase-0-execution-plan.md](phase-0-execution-plan.md) §2 |

## Pinned runtime version

**P0-05 deliverable.** The Castor host runs a named release, and the pin moves
only by a written decision recorded here.

| | |
| --- | --- |
| Pinned version | **v0.3.0-rc1** — a from-source build of `main` @ `dd8bb28`, deployed to the host 2026-09-30 |
| Recorded | 2026-09-30 |
| Confirmed by | conductor, under the host owner's instruction to deploy the pinned build |
| Re-evaluate after | the real `v0.3.0` tag exists and its exit gate has passed with a named second operator |

`v0.3.0-rc1` is deliberately **not** a published release: `release.yml` publishes
only on a `v*.*.*` tag, and nothing has been tagged or pushed. It is a label on a
build of `main`, which carries P0-01's and P0-02's fixes, and the local `otter`
CLI was built with `make build VERSION=v0.3.0-rc1` so the host's `/health` reports
the same string the ledger records. `v0.2.0` is therefore superseded as the pin:
it was recorded only because it was the newest version with published artifacts,
not because Phase 0 should validate it.

The pin cannot be mistaken for a fetchable release: `v0.3.0-rc1` fails
`IsReleasedVersion`, so `otter deploy` will never try to download it — it compiles
from the named checkout instead.

**There is no unattended upgrade path.** Verified in the code, not assumed:

- the runtime never looks up "latest" — a deploy version must match
  `^\d+\.\d+\.\d+$` (`internal/deploy/fetch.go:40`) or the deploy refuses with a
  hint to build from source (`:99–106`);
- there is no self-updater: no `self-update`/`auto-update` symbol exists anywhere
  under `internal/` or `cmd/`;
- the generated systemd unit has `Restart=always` and no timer, no `OnCalendar`
  and no download step (`internal/deploy/render.go:14–63`);
- the only thing that replaces a binary is an explicit `otter deploy` or a manual
  `install`.

Upgrade discipline, which the host pin depends on, is now written in
[operations.md](operations.md#upgrades).

## Decisions and access needed from a human

These are the real critical path. None of them is agent work.

| Need | Blocks | Owner | Status |
| --- | --- | --- | --- |
| Castor import selected, with destination, owner and business effect | P0-13, P0-14, P0-15 | Castor job owner | open |
| A Linux VM with SSH/root, plus confirmation the host is not already provisioned | P0-07, P0-08, P0-10, P0-11, P0-12 | host owner | open |
| A notification channel a human reads | P0-09 | host owner | open |
| Confirmation of egress to PyPI, `python-build-standalone` and Castor's sources | P0-08 | host owner | open |
| Castor credential names, scopes and values | P0-12 | Castor job owner | open |
| Confirmation of the cutover pin recorded above | P0-15 | host owner | open |

**Check before writing a provisioning script:** untracked local state
(`.otter/deploy.json`) records a prior real deploy to a remote host
(`/opt/otter`, `127.0.0.1:7337`, dated 2026-09-16) and `.otter/data/.releases/`
holds staged `shopify-to-salesforce` releases. Nothing links them to Castor, and
none of it is committed. Confirm whether the Castor VM already exists before
P0-07 starts from an empty VM.

## Evidence index

See [evidence/phase-0/README.md](evidence/phase-0/README.md) for the convention
and the list of records.
