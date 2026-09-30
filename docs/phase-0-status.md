# Phase 0 status ledger

Status: living document. Created: 2026-09-29.

The state of every task in [phase-0-tasks.md](phase-0-tasks.md), and the record
of what closed it. The execution plan is
[phase-0-execution-plan.md](phase-0-execution-plan.md).

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
| P0-02 | Retry release/environment binding | in progress — verification | `.worktrees/p0-02` @ `05e3187`, new test file + recovery fix | five tests pass; recovery retry-policy defect found and fixed (a looser live policy could extend a bound run's budget); mutations M1–M5, M4b recorded; record + §5.1 promotion → pending verifier verdict | in flight |
| P0-03 | Full-fidelity backup and restore | in progress — **host-gated** | merged `fa233e8`; `scripts/drill/backup-restore.sh`; [operations.md](operations.md#backups) | [record](evidence/phase-0/2026-09-29-p0-03-backup-restore-drill.txt) — drill clean ×3, three sabotage modes red at the run assertion. Verifier wrote 9 additional independent sabotages and confirmed the repointing is a legitimate restore step, not a masked defect. **The task's evidence bar is a real host, so this is not closed**: the drill proves same-host, different-data-directory | VERIFIED @ `91b8cd3` (drill and doc) |
| P0-04 | Resource caps in the generated unit | **done** | merged `f5dc220`; `UnitOptions` + `scripts/drill/unit-caps.sh` | [drill](evidence/phase-0/2026-09-29-p0-04-unit-caps-drill.txt), [mutation](evidence/phase-0/2026-09-29-p0-04-unit-caps-mutation.txt) — runaway OOM-killed at the cap, `MainPID` survived, `NRestarts 0`, host answered. Verifier removed `OOMPolicy`: drill fails with `MainPID after: 0`, so it detects daemon death. Zero-cap validation defect found and fixed. Re-run on the Castor VM once P0-07 exists | VERIFIED @ `def32b8` |
| P0-05 | Pin the version; no auto-upgrade | **done** | `main` (W0) | [pin recorded](#pinned-runtime-version) + [operations.md](operations.md#upgrades); no code path updates the runtime — 2026-09-29 baseline | 2026-09-29 baseline run |
| P0-06 | Deployment-failure recovery | not started | after P0-03 | pending | pending |
| P0-07 | Provision the Castor host | host-gated | artifact not written | pending | pending |
| P0-08 | Egress and managed Python | host-gated | artifact not written | pending | pending |
| P0-09 | Failure notification | blocked on human | code already complete | pending — needs a real channel | pending |
| P0-10 | Independent liveness detection | host-gated | artifact not written | pending | pending |
| P0-11 | Disk retention and thresholds | not started | — | pending | pending |
| P0-12 | Scope Castor's credentials | blocked on human | code already complete | pending — needs names, scopes, values | pending |
| P0-13 | Select and harden the job | blocked on customer | — | pending | pending |
| P0-14 | Shadow run beside the Lambda | not started | after P0-13 | pending | pending |
| P0-15 | Cutover | not started | after all blockers | pending | pending |
| P0-16 | Operate for 30+ days | not started | elapsed | pending | pending |
| P0-17 | Write the runbook | not started | written while operating | pending | pending |
| P0-18 | Produce the ranked gap list | not started | after P0-16 | pending | pending |

## Shared prerequisites

Not Phase 0 tasks themselves, but the machinery several tasks' evidence depends
on. Each was verified rather than assumed.

| Prerequisite | Status | Where |
| --- | --- | --- |
| Evidence convention — one dated record per run, raw output, named host | done 2026-09-29 | [evidence/phase-0/README.md](evidence/phase-0/README.md) |
| Drill harness — `scripts/drill.sh`, `make drill`, `scripts/drill/<name>.sh` | done 2026-09-29; dispatcher verified for the empty, failing, aggregate and unknown-name cases | [scripts/drill.sh](../scripts/drill.sh) |
| Linux container route for Linux-only evidence | done 2026-09-29; the `Pdeathsig` test builds and passes in a container | [2026-09-29-linux-docker-probe.txt](evidence/phase-0/2026-09-29-linux-docker-probe.txt) |
| Worktree isolation for workers (`.worktrees/`, one branch per task) | done 2026-09-29; verified from inside a subagent | gitignored, see [phase-0-execution-plan.md](phase-0-execution-plan.md) §2 |

## Pinned runtime version

**P0-05 deliverable.** The Castor host runs a named release, and the pin moves
only by a written decision recorded here.

| | |
| --- | --- |
| Pinned version | **v0.2.0** — the newest published release with release notes ([docs/releases/v0.2.0.md](releases/v0.2.0.md)) |
| Recorded | 2026-09-29 |
| Confirmed by | *host owner — pending* |
| Re-evaluate after | P0-01 and P0-02 land, since both change runtime behavior and are scheduled in the `v0.3.0` line |

`v0.2.0` is recorded as the current pin because it is the only version with
published artifacts, not because it is the version Phase 0 should cut over on.
The cutover pin is the release that carries P0-01's and P0-02's fixes, and it
should be re-recorded here when those close.

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
