# Phase 0 execution plan — how to run `phase-0-tasks.md`

Status: proposed. Date: 2026-09-29.

This is the *how* for [phase-0-tasks.md](phase-0-tasks.md) (the *what*). It
answers three questions: which tasks an agent can execute in this repository,
which need a real host or a human, and how to organise the sessions that do the
work.

## 1. What the plan is actually shaped by

**F1 — A third of Phase 0 is already scheduled elsewhere, as `v0.3.0` WS1–WS6.**
The runtime-correctness half of the task list is not greenfield:
[v0.3.0-release-plan.md](v0.3.0-release-plan.md) already schedules

- `OT-011` retry/backlog release binding (= **P0-02**) and the
  `runtime-contract.md` promotion (WS6, lines 410–425);
- scripted backup/restore, upgrade, downgrade-refusal, secret-rotation,
  deployment-failure-recovery and fresh-host drills (= **P0-01**, **P0-03**,
  **P0-06** in part), with the shape already fixed: `scripts/drill.sh`
  subcommands behind `make drill`, each asserting its own outcome and exiting
  non-zero (WS6, lines 427–461; `CA-26` at cloud-alpha-readiness.md:517);
- automatic run/log retention (= **P0-11** half) in WS1;
- queue age, depth and storage-pressure surfaces (= **P0-11** alerting) in WS4;
- trust-boundary verification including file modes (= **P0-07**'s permission
  assertion) in WS6.

`open-work.md`'s rule is **one home per task**. So Phase 0's in-repo work should
*be* the `v0.3.0` workstreams, not a parallel track that drifts from them. Phase 0
then adds exactly two things `v0.3.0` does not have: a real host, and a real
customer job. The evidence bar differs accordingly — `v0.3.0` proves the backup
drill against a second empty data directory; **P0-03** demands a clean *host*
("not a synthetic CI workspace", phase-0-tasks.md:120).

**F2 — Only ten of the eighteen tasks involve work in this repository, and only
five are code.**

| Bucket | Tasks | Who |
| --- | --- | --- |
| Code + test in this repo | P0-01, P0-02, P0-04, P0-06, P0-11 (code half) | Agents, in worktrees |
| Script/unit/doc authored here, executed on a host | P0-03 (drill), P0-05 (procedure), P0-07, P0-08, P0-10 | Agents author, human runs |
| Already implemented; human input only | P0-09, P0-12 | Human |
| Customer/Castor input, in another repository | P0-13 | Human |
| Elapsed or observational | P0-14, P0-15, P0-16, P0-17, P0-18 | Calendar + operator |

Everything in the second and third rows also needs the *same* VM to produce its
evidence — that is one dependency and one person, not five separate blockers.

Summing the in-repo work bottom-up gives roughly **16–22 focused engineer-days**
(five to ten of them inside P0-01), against a task list that prices the whole of
Phase 0 at eight to eleven weeks. The difference is the host, the customer and the
calendar. With three or four workers that in-repo half is about two weeks of wall
clock; the rest is not compressible by adding agents.

**P0-13 is the sharpest constraint.** The job does not live in this repository —
CONTRIBUTING.md:6–11 puts vendor clients and real jobs in their own project, and
the runtime ships no example catalog. So P0-13 cannot even be *started* by an
agent in this checkout: it needs someone who knows Castor's import, its
destination, and its business owner. Start that conversation on day one; it, not
the code, is what the 30-day clock waits on.

**F3 — `R-19` makes a worker's "done" worthless on its own.** Every task is
closed by executable or dated recorded evidence, and "a task is not done because
a document says the behavior exists" (phase-0-tasks.md:9–11). A subagent that
writes a test and reports success is exactly the failure mode `R-19` names. Every
task therefore gets an independent verifier that is not its author.

**F4 — Linux-only evidence is available locally and in CI.** The baseline Go
suite is green on this macOS checkout. CI (`.github/workflows/test.yml`) runs
`gofmt`/`go vet`/`go test ./...`, the SDK suite, `make smoke` and the release
config on `ubuntu-latest` on every branch push. Docker works locally, and the
existing `otter-systemd-test`, `otter-daemonenv-test` and `al2023-ec2` images
show Linux/systemd drilling is already local practice — but no `Dockerfile` or
drill script is committed, so it is currently irreproducible. Committing it is
part of `CA-26`.

## 2. Session topology

**Not one session, and not eighteen either — one conductor plus short-lived,
isolated workers.**

**One session doing all of it fails** for three reasons: the context budget
(eight to eleven weeks of work, ~3,600 lines of contract docs plus 23 internal
packages), the lost parallelism (most tasks are independent), and `R-19` (a
single context that both writes and blesses its own evidence is not verification).

**One session per task with no conductor also fails**: merges, contract
promotions, doc status flips and the evidence ledger are single-writer
decisions. Two agents editing `runtime-contract.md` or the backup section of
`operations.md` concurrently will produce a contract that is true of neither
branch.

### The conductor

A long-running session (create a goal for it) that owns:

- the ledger — the only durable handoff between contexts (subagents share no
  memory, so if it is not on disk it did not happen);
- merges to `main`, always after `make test`, `make lint`, `make smoke`;
- every contract/document promotion (`runtime-contract.md` Appendix A statuses,
  §5.1, `operations.md` backup procedure, `open-work.md` retirement) — workers
  propose, the conductor edits;
- the evidence records and the final P0-18 gap list.

The conductor should not be the one writing the crash harness.

### The workers

One subagent per task, each in **its own git worktree inside the workspace**:

```sh
git worktree add .worktrees/p0-02 -b p0-02-retry-binding main
```

Verified working in this checkout, including from inside a subagent: a probe
worker created `.worktrees/probe` on its own branch, wrote a file inside it and
committed, with no sandbox denial. `.worktrees/` must be added to `.gitignore`;
the leading dot also keeps it out of Go's `./...` traversal.
The DSH subagent tool has **no built-in worktree/isolation option**, so isolation
is a convention enforced in the prompt: *work only in your worktree, commit on
your branch, never touch the main tree, never push.* A worker prompt must be
fully self-contained (the child cannot see this conversation) and must carry:

1. the verbatim task section from `phase-0-tasks.md`;
2. the worktree path and branch;
3. the evidence bar — the exact command that must exit 0, and the rule that a
   green test which would still pass with the behavior removed is not evidence;
4. the repo conventions that apply (`scripts/smoke.sh` as the drill template,
   `make drill` shape, doc-is-part-of-the-change per CONTRIBUTING.md:130–133);
5. an instruction to report a blocker rather than guess.

### The verifiers

A separate subagent per task that did not write the code, whose job is to falsify
the claim:

- re-run the drill from a clean checkout of the branch;
- **mutation-check the key assertion** — break the production behavior, confirm
  the new test fails, restore it;
- re-read the doc sentence against observed behavior.

Report verdict, commands and raw output. The conductor records the verdict in
the ledger. This is the single highest-value agent budget in the whole plan.

### Tool choice

| Need | Tool |
| --- | --- |
| Conductor's multi-week objective, with continuation | goal tools |
| One task, self-contained | `subagent` (background), one worktree each |
| Follow-up on a task that needs this conversation's context | `subagent_fork` |
| Read-only audit sweep across many tasks/files | `workflow` |
| Fixed-shape fan-out (implement → review → verify per item) | `workflow` pipeline |
| Fresh-agent iteration rounds | `ralph`, only if explicitly wanted |

**Concurrency: three to four workers at a time**, on tasks that touch disjoint
areas. Do not run P0-01 and P0-06 concurrently (both reach into daemon recovery),
and never two doc-promotion tasks at once.

## 3. Wave plan

The critical path in the task list is P0-07 → P0-08 → P0-13 → P0-14 → P0-15 →
P0-16 → P0-18, and every link after P0-08 is human-, host- or calendar-bound.
So the code runs in parallel *around* a critical path that is mostly not code.

### W0 — Setup and the day-one human asks (conductor, ~half a day)

1. Commit the three untracked plan documents (`phase-0-tasks.md`,
   `cloud-alpha-readiness.md`, `cloud-alpha-risks.md`). Right now the plan of
   record is not in git.
2. Create `docs/phase-0-status.md` — the ledger. One row per task with: task,
   status, branch/worktree, artifacts, **evidence command**, evidence record,
   verifier, blocker. This is the shared memory.
3. Create `docs/evidence/phase-0/` with a `README.md` stating the convention:
   one dated file per recorded run, containing host, version, exact commands and
   raw output. No evidence directory exists today.
4. Record the baseline: `make test`, `make lint`, `make smoke`, and a Linux probe
   under Docker; note the version (`v0.2.0-3-gf72d8fb` at time of writing).
5. **Do P0-05 first** (S). Pin the runtime version and write the upgrade
   procedure — one runtime at a time, backup first, quiet window, post-upgrade
   smoke. It is cheap, it is a blocker, and its "backup first" rule is a
   precondition for P0-03's procedure.
6. **Human asks, day one** (not delegable, and the real schedule): name the
   Castor import and its owner; obtain the Linux VM and SSH access; choose the
   notification channel; confirm egress to PyPI/`python-build-standalone`.

### W1 — Runtime-correctness blockers, in parallel (agents, in worktrees)

| Task | Worker shape | Evidence | Notes |
| --- | --- | --- | --- |
| P0-01 crash/kill harness (L) | New Linux-only harness + `make drill kill` | Harness output in CI; Appendix A FM-01/FM-02 → **Real** | Largest item; the task itself says budget for fixing what it finds. Run under Docker locally, ubuntu CI authoritatively. |
| P0-02 retry release/env binding (M) | Test, per `OT-011`/WS6 | Test passes; §5.1 non-guarantee → guarantee | Add the prune case (pending backlog keeps its digest). |
| P0-04 unit caps (S) | `UnitFile` emits `MemoryMax`/`CPUQuota`/`TasksMax` + sizing doc | Runaway job killed; `otterd` and host survive | Confirmed absent: no cap string exists anywhere under `internal/`. Needs a systemd container — commit the `Dockerfile` that produced `otter-systemd-test`. |
| P0-11 code half (M) | Enable the three windows; wire thresholds | Alert fires at a test threshold; retention prunes | Flags exist (`internal/config/daemon.go:362–364`); log/run default to 0 = forever (`:36–42`). Automatic run/log retention is WS1; pressure surfaces are WS4. |
| P0-03 drill script (M) | `scripts/drill.sh backup-restore` + correct `operations.md` | Script output on a second empty data directory | Real-host half is W2. |
| P0-06 deploy-failure recovery (M, after P0-03) | Inject mid-deploy failure | Recorded drill output | Sequence after P0-03; shares the deploy/prune code with P0-01's fix surface. |

#### W1 execution notes (seams already verified in the tree)

**P0-01.** No test in the repository has ever run the daemon as a separate OS
process or signalled one; crash coverage today is *seeded*, not killed
(`internal/daemon/daemon_test.go:961,1050` insert `running` rows and construct a
new `Daemon`). The cheapest new seam is the repo's own re-exec pattern
(`internal/executor/proc_linux_test.go:32`): a helper that runs
`daemon.New` + `Run` against a real temp data dir. Reusable fixtures already
exist — `newDaemonWithLogger` (`daemon_test.go:252`), `releaseAll` (`:109`),
`startDaemon` (`:288`), `awaitTerminal` (`:321`), `readRunFromDisk` (`:406`),
`closeUnstarted` (`:1157`). Four gotchas to put in the worker prompt:

- To have >50 runs *genuinely running* at kill time, raise `--workers` and the
  manifest's `concurrency` (defaults: 4 workers, `concurrency: 1`); otherwise the
  fixture only exercises the queued path.
- "No duplicate execution" cannot be read from run status. The job must append
  `ctx.run_id` to a file outside the database, asserted exactly once per accepted
  run.
- `Pdeathsig` is Linux-only (`proc_linux.go:29` vs `proc_darwin.go`), so on macOS
  the orphaned Python child legitimately survives and the no-duplicate assertion
  legitimately fails. Run the harness everywhere; gate the child-death assertion
  behind a Linux build tag and record Darwin as excluded, rather than shipping a
  test that is red on the developer's machine.
- There is no injectable clock and no kill barrier (`workers.go:192,311`;
  `recovery.go:103`); the "wait until running" predicate must read database or
  queue state, not sleep, or it will flake. The FM-02 window (kill between finish
  and retry) is inherently racy — label it best-effort rather than pretending.

**P0-02.** Every code path already exists; this is fixture work. Stage release A,
capture the active digest, edit the job, stage release B (pattern at
`daemon_test.go:2214–2253`), fail attempt 1 with `backoff: none`, and assert the
retry's row still carries A's digest by reading the database (`readRunFromDisk`),
because `runs.ReleaseSourceDir` is a real column but `json:"-"`. For managed
Python, build the environment offline exactly as
`internal/pyenv/manager_test.go:153` does — a hand-written `otter-ready.json` plus
a stub `bin/python`; no `uv`, no network. Prune safety already exists in
`internal/cli/release.go:676` (`pinnedReleases`); the missing coverage is that a
pending backlog keeps its digest through a prune.

**One new probe, not in the task list.** Crash recovery calls `planRetry` with the
*live* registry manifest as the retry policy (`internal/daemon/recovery.go:88–107`
and `:140–153`), while the fields it copies come from the *bound* release. A
release whose retry policy changed between attempts can therefore apply the newer
policy to an older run. Nothing asserts this either way. Give it a named test in
P0-02 and record the answer in the contract, whichever way it falls.

**P0-03.** No backup or restore code exists anywhere under `internal/` or `cmd/` —
the documented procedure is shell prose in `operations.md`. So this task is a new
`scripts/drill.sh` subcommand plus a doc correction, and its evidence is produced
on the host. Cheap to write, slow to evidence.

**P0-04.** Confirmed absent: no `MemoryMax`, `CPUQuota`, `TasksMax` or
`MemoryHigh` string exists anywhere under `internal/`. The runtime has no notion
of a unit cap today, so the change is renderer plus docs plus a systemd-container
drill — and the previous ad-hoc `otter-systemd-test` image should become a
committed `Dockerfile` so the drill is reproducible.

**P0-06.** The code intends this guarantee in two places —
`internal/cli/release.go:28` ("a bad candidate can never take down a working
runtime") and `internal/deploy/render.go:331` ("failure here must leave the
previous release active") — but nothing injects a mid-deploy failure. Treat it as
"claim exists, evidence does not", which is exactly the state `R-19` forbids.

**P0-11.** The three flags exist (`internal/config/daemon.go:362–364`);
`LogRetention` and `RunRetention` default to `0` = retain forever (`:36–42`)
while capture defaults to 7 days (`:27–30`). Automatic run/log retention is
`v0.3.0` WS1; the alerting half needs the storage-pressure surface from WS4. The
in-repo part is small; the drill (fill the disk, watch the alert fire) is host
work.

### W2 — Host artifacts authored here, executed on the VM

P0-07 (provisioning script; permissions asserted, not documented; no public
ingress; restricted SSH; optionally CA-07 hardening), P0-08 (`otter prepare`
before any schedule), P0-09 (notify channel), P0-10 (external dead-man's switch —
`/health` always answers 200 by design at `internal/api/server.go:349–353`, so the
check must assert *content* and alert on its *absence*), P0-12 (secrets in the
`0600` env file, names only in the manifest), and P0-11's real thresholds plus the
disk-fill drill.

Each of these is: agent authors the script/unit/config in-repo, human runs it on
the VM, output goes to `docs/evidence/phase-0/`.

#### W2 execution notes (what is already built, and what is not)

More of W2 is already implemented than the task list implies. Two tasks have
essentially **no in-repo work left**, and saying so up front prevents an agent
from spending days on busywork:

- **P0-09 is done in code.** `OTTER_NOTIFY_URL`/`_ON`/`_FORMAT` exist
  (`internal/config/daemon.go:316–324`), four formats are implemented
  (`internal/notify/format.go:24–38`), failures-only wiring is in place
  (`daemon.go:190–203`), it reaches the unit through `EnvironmentFile`
  (`internal/deploy/render.go:51`), and it is tested. The only missing input is a
  channel a human actually reads — a human decision plus egress.
- **P0-12 is done in code.** Manifests declare names only
  (`internal/config/manifest.go:68,247,560–573`), resolution is environment-only
  and all-or-nothing (`internal/secrets/secrets.go:32–46,109–139`), a missing
  secret fails before Python and is not retried (`:95–105`), and deploy writes a
  root-owned `0600` env file (`internal/deploy/deployer.go:629–676`). What is
  missing is Castor's actual names, scopes and values — customer input.

**P0-07.** `otter deploy` already converges a host: service account with a
`nologin` shell (`render.go:137`), `0700` data and env directories (`:144–145`),
wildcard-bind refusal (`target.go:415–423`, tested), host-tool preflight
(`remote.go:174`), post-deploy `/health` poll (`deployer.go:678–713`). Two gaps:
there is no provisioning script (only manual prose at `operations.md:29–171`), and
the generated unit has **no hardening directives** — no `NoNewPrivileges`,
`ProtectSystem`, `ProtectHome`, `PrivateTmp` or `ReadWritePaths`, which exist only
as documentation (`security.md:76–99`). Adding them to `UnitFile` plus render
tests is about half a day and makes the optional CA-07 item nearly free.

**P0-08.** `otter prepare` exists (`internal/cli/prepare.go:23–95`) and deploy runs
prepare automatically. Two real constraints: there is no offline bundle
(`managed-python.md:482–485`), and the manager hard-sets `UV_NO_CONFIG=1`
(`internal/pyenv/manager.go:392–405`), which blocks the uv-config route to an
internal mirror. A `--index`/mirror passthrough plus a prepare-time egress
preflight is a small, agent-sized change; the evidence (real DNS and a real
`otter prepare` for the pinned interpreter) is host work.

**P0-10.** No dead-man material exists — no script, no timer or watchdog unit, no
`OnFailure`/`WatchdogSec` in `UnitFile`; only prose (`operations.md:849–881`,
`deploy.md:311`). Note the trap: `/health` **always returns 200 and is
unauthenticated**, disclosing only `status`, `version` and `uptime_seconds`
(`internal/api/server.go:349–391`); queue depth appears only on the authenticated
response. A positive check against `/health` therefore proves nothing about work
being done, and the dead-man's switch must alert on the *absence* of the check.

**New lead, worth confirming before W2 starts.** Untracked local state in
`.otter/deploy.json` records a real prior deploy to a remote host (workspace
`/opt/otter`, loopback `127.0.0.1:7337`, dated 2026-09-16), and
`.otter/data/.releases/` contains staged `shopify-to-salesforce` and
`shopify-product-to-salesforce-product` releases. Nothing links that host or those
jobs to Castor. It is gitignored local state, not a committed artifact, so the
provisioning script is still missing — but someone should check whether the Castor
VM and job already exist in that form before a script is written from scratch.

#### Agent-day versus host-day

The task list's sizes (S/M/L) count total effort, which hides that most of W2 is
not engineering. Reconciled per task, as focused engineer-days:

| Task | List size | Agent, in-repo | Host / human |
| --- | --- | --- | --- |
| P0-07 provision + hardening | M | 0.5–1 d | 1–2 d (VM, SSH, port scan) |
| P0-08 egress + managed Python | S | 0.5–1 d (mirror passthrough, prepare-time preflight) | real DNS/egress, real `otter prepare` |
| P0-09 notification | S | ~0 (already implemented) | channel choice + failing-run drill |
| P0-10 liveness | M | 0.5–1 d (script, timer unit, unit test) | kill-daemon drill, real alert channel |
| P0-12 credentials | S | ~0 (already implemented; one assertion test) | scopes, values, file placement |
| P0-13 harden the job | L | 2–4 d *after selection* | selection, owner, destination, business effect |

That is roughly **4–7 agent-days** of the in-repo half of W2, against a task list
that reads as M+S+S+M+S+L. The gap is the point: W2's duration is set by host and
customer access, and staffing agents against it beyond that is wasted.

### W3 — The Castor job and cutover (human-bound)

P0-13 in the job's own project — scheduled import, watermark/checkpoint-shaped,
upsert at the destination, verifiable from outside Otter; then P0-14 shadow run
beside the Lambda with a recorded diff of every difference explained; then P0-15
cutover. An agent can review the job's idempotency design against `CA-40`–`CA-46`
and write the assertion, but not choose the import or own the business effect.

### W4 — Observation (elapsed)

P0-16 (30+ days), P0-17 (runbook written *while* operating), P0-18 (ranked gap
list — the most important Phase 0 output). Schedule the four-to-six-week read, not
a six-month soak.

## 4. What parallel agents do and do not buy

They compress the in-repo half — the six tasks in W1 that would otherwise
serialise behind one person. They do not touch the wall clock after that: the VM,
the Castor decision, the shadow window and the 30-day soak are not
parallelisable, and P0-01 plus P0-13 dominate the estimate in both directions
(R-23: replace the guesses once those two are understood). Realistic shape:
**weeks 1–4 = W0–W2 with 3–4 concurrent workers; week 4–6 = P0-13/P0-14; then the
calendar.** Phase 0's end date is set by when Castor's job owner says yes, not by
agent throughput.

## 5. Risks specific to running this with agents

| Risk | Countermeasure |
| --- | --- |
| Green-test theatre (R-19's exact failure mode) | Independent verifier per task + mutation check + "the test must fail before the fix" |
| Two workers editing the same contract doc | Conductor is the single writer for `runtime-contract.md`, `operations.md`, `open-work.md`, `v0.3.0-release-plan.md` |
| Parallel edits colliding in one working tree | One worktree + branch per task; merge serially; keep `main` green |
| Worktree/branch sprawl | Cap at 3–4 live; delete the worktree on merge |
| Workers writing outside the workspace (e.g. `/etc/systemd`) | Host scripts are authored and validated in Docker, never executed against the developer Mac |
| Platform-only tests silently skipped on macOS | Linux-only rows run in ubuntu CI and Docker; the ledger records the platform each evidence run came from |
| Losing the thread across compacted contexts | Ledger + evidence files are the handoff; conductor is their only writer |
| Fixing what P0-01 finds blowing the estimate | Treat P0-01 as investigation-first: timebox the harness, then re-estimate before scheduling its dependents |

## 6. First 48 hours

1. Commit the three plan docs; add `.worktrees/` to `.gitignore`.
2. Create the ledger and the evidence convention.
3. Do P0-05 (pin + upgrade procedure) — small, unblocking, sets the discipline.
4. Open the four human asks (VM, Castor owner, notify channel, egress).
5. Start P0-02 and P0-04 as the first two workers in worktrees; stand up the
   Linux Docker/drill scaffold (`scripts/drill.sh` + committed `Dockerfile`)
   before P0-01 depends on it.
6. Then start P0-01 — the biggest unknown, and the one most likely to change the
   plan.

## Related

- [phase-0-tasks.md](phase-0-tasks.md) — the task list this executes.
- [v0.3.0-release-plan.md](v0.3.0-release-plan.md) — WS1–WS6, which already carry
  most of the in-repo work.
- [cloud-alpha-readiness.md](cloud-alpha-readiness.md) §17 — Phase 0's gate.
- [cloud-alpha-risks.md](cloud-alpha-risks.md) — `R-19`, `R-23`.
- [operations.md](operations.md) — the procedures the drills must execute.
