# Phase 0 host run plan — parallelising the host-gated tasks on one Castor host

Status: proposed. Date: 2026-09-30.

This is the *how* for the half of [phase-0-execution-plan.md](phase-0-execution-plan.md)
§3 **W2** that the host makes possible. Task definitions are in
[phase-0-tasks.md](phase-0-tasks.md); current state is in
[phase-0-status.md](phase-0-status.md).

The question it answers: **the Castor host now exists, so how do the tasks that
need it get staffed with agents without five agents corrupting each other's
evidence on one machine?**

The answer is a reframe, not a bigger fan-out.

## 1. What changed, and what did not

**Changed.** The Castor host exists as infrastructure: the `otter-platform` CDK
stack `CastorRuntime` deployed one `t4g.micro` Ubuntu 24.04 arm64 instance with a
dedicated VPC, an Elastic IP, ED25519 key material in SSM, IMDSv2 enforced, and
SSH the only inbound rule. The execution plan's "obtain the Linux VM and SSH
access" ask is met for the *access* half.

**Not changed.** P0-07 is **not closed by deploying an instance**. Its evidence
bar (phase-0-tasks.md:181–182) is an external port scan plus a permission
assertion test, and its deliverable is one script from an empty VM to a running
daemon. The ledger still reads `host-gated / artifact not written`. The instance
is an input to P0-07, not its evidence.

**Also missing.** Nothing on this disk records the deployed host's identity.
`otter-platform/cdk.out/` contains a template but no `*.outputs.json`, so the
`InstanceId`, `PublicIpAddress` and AMI are only in the `cdk deploy` stdout nobody
saved. Evidence must name the host it came from, so capturing the stack outputs is
the first action of the first window — before any drill that will cite them.

## 2. The reframe: parallelise the *window*, not the task

The seven host-gated tasks (P0-03, P0-07, P0-08, P0-09, P0-10, P0-11, P0-12)
cannot run concurrently *on the host*. They share one daemon, one data
directory, one systemd unit, one disk and one pinned release; two of them running
at once produces a transcript that is true of neither. Attempting parallelism
there buys speed and pays in evidence that `R-19` rejects.

So the host is a **serialised lane**, and the parallelism lives on either side of
it:

| Lane | Concurrency | Where | What happens |
| --- | --- | --- | --- |
| **A — author** | 3–4 workers | Git worktrees, no host | Scripts, units, tests, docs. Everything a task needs before it touches the VM. |
| **B — host window** | **1, always** | The Castor VM | Short, self-asserting, recorded runs. |
| **C — verify** | 3–4 verifiers | Recorded transcript + containers | Independent falsification; almost never needs the host again. |

**The goal is to make lane B short and serial, not wide.** Every hour of
authoring and container iteration that a worker does in lane A is an hour the
shared host is not the bottleneck. A task that needs fifteen minutes of host time
and two days of authoring is a two-day-latency task that spends fifteen minutes
serialised — that is the parallelism win, and it comes from the split, not from
running drills concurrently.

## 3. The host's exclusive resources — the conflict model

Every window holds all of these. That is what makes lane B concurrency 1.

| Exclusive resource | Contended by | Failure if shared |
| --- | --- | --- |
| `otterd` process, `MainPID`, unit state | P0-03, P0-04, P0-09, P0-10, P0-11 | A kill/restart is another drill's unexplained outage |
| `/opt/otter` — `otter.db`, releases, prepared envs, `.otter-id` | P0-03, P0-08, P0-11, P0-12 | Backup and mutation interleave; the restore proves nothing |
| `/etc/otter/daemon.env` (root-owned `0600`) | P0-09, P0-11, P0-12 | Last writer wins; a drill runs against another's configuration |
| The unit, binary and pinned release | P0-04, P0-05, P0-07 | One `otter deploy` clobbers another's release mid-drill |
| Root volume free space | P0-11 (disk-fill) | The daemon cannot write — the documented failure mode |
| The notification channel | P0-09, P0-10 | Untagged alerts; nobody can tell which drill fired |
| Port 7337 on the host, and its local tunnel port | every API-touching drill | Local tunnel collisions; a check reads the wrong daemon |
| The EIP and SSH/firewall posture | P0-07 | The scan records a half-applied posture |

**Rule.** Lane B holds all of them for a window's duration. Nothing else reads
the host during a window — *including `nmap`, `/health` and `journalctl`*. A
read taken during a mutation is a half-state, and a half-state recorded in
`docs/evidence/phase-0/` is worse than no record, because it looks like evidence.

## 4. What each host-gated task actually needs — so most of it moves off the host

The host is the *evidence venue*, not the work venue. Classified by what only the
real host can supply:

| Task | What only the host gives | Moves off-host? |
| --- | --- | --- |
| P0-03 backup/restore | A **second clean host** for the restore, and a backup taken from a live runtime | No — but the second host is itself parallel, see HW-7 |
| P0-04 caps re-run | Real systemd cgroup semantics on the generated unit | Iteration already runs in `unit-caps.Dockerfile`; host run is one confirmation |
| P0-07 evidence | The real EIP, the real firewall, real file modes | No — read-only, so the cheapest window |
| P0-08 egress + Python | Real DNS/egress, and the real pinned interpreter building for linux/arm64 | No — this is the critical path to P0-13 |
| P0-09 notification | Real delivery to a channel a human reads | The failing run can be scripted; the channel is external |
| P0-10 liveness | A real `otterd` killed for real, with an external dead-man alert | No |
| P0-11 retention/disk | Real disk, real thresholds, real pruning | The fill must **not** be the root volume |
| P0-12 credentials | The real `0600` file and a real run failing on a missing secret | No — but it is minutes of host time |

Net: **six short windows and one second host.** Everything else is lane A.

## 5. The window queue

Ordered by dependency, and within equal dependency by destructiveness — the
windows that can leave the host degraded run last. Each window is a runnable
procedure, not a description.

### HW-0 — Host identity and clean baseline (read-only)

*Feeds P0-07; defines the "clean host" that P0-03's restore needs.*

- **Capture the stack outputs first** (`InstanceId`, `PublicIpAddress`,
  `PublicDnsName`) and record them in the evidence file. Recover the private key
  once from SSM into `~/.ssh/`, mode `0600`, never into the repo.
- `nmap -Pn -p- <ip>` — assert **only 22/tcp** open.
- On the host: read `/var/log/otter-provision-report.txt`, `uname -m` (arm64),
  `free -m` (swap present), `df -h`, and confirm IMDSv2 is required (an IMDSv1
  request returns 401).
- Assert **no `otterd` unit exists yet** — this is the baseline the restore in
  HW-7 restores onto.
- Evidence: one dated transcript. Restore: nothing to restore.

### HW-1 — Deploy the pin

*Feeds P0-05, P0-07. Prerequisite for every runtime drill.*

- Precondition: `OTTER_WORKERS=1` in `otter.daemon.env` (1 GiB host; the daemon
  otherwise defaults to one worker per vCPU, and this is a 2-vCPU instance).
- `otter deploy --host ubuntu@<ip>`; then `systemctl show otterd-*.service -p
  MainPID,MemoryMax,MemoryCurrent,MemoryPeak` and `/health` over an SSH tunnel.
- Assert: unit active, `/health` 200, `MemoryMax` present and equal to the
  configured cap, `NRestarts 0`.
- **Blocked on a decision, not a dependency:** the ledger pins **v0.2.0** and says
  "re-evaluate after P0-01 and P0-02 land". Both are merged, so the cutover pin
  must be re-recorded *before* this window, or the host is deployed to a version
  Phase 0 has already superseded.

### HW-2 — Egress and managed Python (P0-08)

*The highest-value window: it is the critical path link to P0-13.*

- Preflight DNS/TLS to PyPI, `python-build-standalone` and Castor's sources.
- Run `otter prepare` for the pinned interpreter **on the host**, before any
  schedule exists; confirm `cache/uv/` is primed.
- Assert exit 0, interpreter resolvable, cache populated.
- This is where the documented trap surfaces: not every patch version is
  downloadable on every platform (managed-python.md:466). If the pinned
  interpreter has no linux/arm64 build, the remedy is documented and is a host
  swap (`t3.micro`), not a code change.

### HW-3 — Unit caps on the real host (P0-04 re-run)

The ledger requires this re-run now that the host exists.

- Start a deliberately runaway job; assert it is OOM-killed at `MemoryMax`,
  `MainPID` is unchanged, `NRestarts 0`, and `/health` still answers.
- Restore: stop the runaway, confirm the queue is clean.
- Note the inherited `CPUQuota=200%` permits a job to use both vCPUs, far above
  this instance's sustained baseline — record whether the drill observed
  throttling.

### HW-4 — Notification (P0-09) and liveness (P0-10)

Same channel, two sub-windows, **tagged payloads** so the evidence is
attributable.

- Precondition: a human-chosen channel that a human reads, `OTTER_NOTIFY_URL` in
  `/etc/otter/daemon.env`, and `heartbeat.{service,timer}` installed.
- HW-4a: submit a run that fails; assert the channel receives the failure
  notification.
- HW-4b: confirm heartbeat pings; `systemctl stop otterd`; assert pings **stop and
  an alert arrives without anyone watching a terminal**; restart and assert pings
  resume.
- The trap: `/health` always returns 200 and is unauthenticated
  (api/server.go:349–353), so the dead-man's switch asserts the *absence* of the
  check, never a positive 200. Evidence is the alert payload and timestamp, not
  the script's exit code.

### HW-5 — Credentials (P0-12)

- Precondition: Castor's real names, scopes and values (human).
- Write values into the root-owned `0600` env file and deploy.
- Assert: mode `0600`; the manifest contains **names only**; a run with a missing
  secret fails before Python starts and is not retried.
- Restore: leave the env file as the deployed state (or remove test values).

### HW-6 — Retention and disk pressure (P0-11)

- Enable all three windows (`--capture-retention`, `--log-retention`,
  `--run-retention`); assert retention demonstrably prunes — row counts before and
  after, not a log line.
- Disk-fill: raise the pressure to a test threshold, assert the alert fires, then
  clean up.
- **Never fill the root volume.** It is 20 GiB with `deleteOnTermination`, and the
  documented failure mode is not "slow", it is "the daemon cannot write". Use a
  dedicated EBS volume mounted for the drill, or a bounded fill with a cleanup
  trap. A window that cannot restore is a failed window.

### HW-7 — Backup and restore onto a clean host (P0-03)

*The one window that needs a **second** host, and is therefore the one host-gated
task that genuinely parallels the lane.*

- The task's bar is explicit: a **real host, not a synthetic CI workspace**, and
  the drill must restore onto a **clean host**. The merged drill proves same-host,
  different-data-directory, which is why the ledger is still `host-gated`.
- Take the backup from the live Castor runtime (HW-1/HW-2 state), restore onto a
  second instance of the same shape deployed from the same stack under a different
  key pair, and destroy it after.
- Assert: run history, durable state, releases, `.otter-id` identity and a
  **runnable** job all survive; the restored runtime serves `/health` and executes
  a run.
- Rejected alternatives: stop-and-recreate the one instance (destroys the live
  source, so it is not a clean-host restore), and a container (rejected by the
  task's own wording).

### HW-8 — Final posture re-scan (closes P0-07's evidence)

`otter deploy` is what installs the unit, so P0-07's posture assertions are only
final **after** the host has converged through HW-1…HW-5. Re-run the port scan and
the hardening/permission assertions here, and record this as P0-07's evidence —
not the HW-0 baseline. This ordering is the whole reason HW-0 is not the last
word on P0-07.

## 6. Lane B rules

1. **Concurrency 1, no exceptions** — including read-only commands (§3).
2. **One deploy at a time.** `otter deploy` is the only writer of the unit, the
   binary and the env file; a second concurrent deploy is undefined.
3. **Every window ends green:** `systemctl is-active otterd`, `/health` 200, and
   `df` above threshold. Record the restore step's output, not just the assertion.
4. **Tag every alert.** Two drills must never share an untagged channel window.
5. **Destroy most last:** caps → notify/liveness → retention/disk-fill →
   backup/restore → final scan.
6. **The key never enters the repo.** Host identity (instance id, EIP, AMI) goes
   into the evidence record; the private key stays in `~/.ssh` or SSM.
7. **Keep the daemon at one worker.** A drill that raises `--workers` to force
   concurrency on this 1 GiB host must restore it in the same window.

## 7. The host custodian

Lane B has one owner: the conductor session. **Workers never touch the host.**
They author in worktrees and submit the exact command list their window needs;
the custodian runs it, records the transcript, and reports back.

Mechanics worth building once:

- a `scripts/drill-host.sh <name>` wrapper that enforces
  precondition → run → assert → transcript → restore, and **refuses to run without
  `OTTER_HOST` and the window lock** — the host analogue of `scripts/drill.sh`,
  which already established the shape (header naming date/host/checkout, assert
  your own outcome, exit non-zero with the failing output, never skip quietly);
- a lock, local `flock` plus a marker on the host, so a second custodian fails
  fast instead of interleaving;
- a fixed local-tunnel port per window, so parallel lanes never collide on the
  operator's machine;
- SSM Session Manager as the fallback path when the operator's IP churns — it
  needs no inbound rule, so the `sshCidr` redeploy is not on the critical path.

## 8. Lane A — the author fan-out

Four to six streams. This is where the agents go.

| Stream | Task | In-repo artifact | Feeds |
| --- | --- | --- | --- |
| **W-A** | P0-07 | Provisioning script (empty VM → running daemon) + the permission assertion test | HW-0, HW-8 |
| **W-B** | P0-08 | `--index`/mirror passthrough (the manager hard-sets `UV_NO_CONFIG=1`) + prepare-time egress preflight | HW-2 |
| **W-C** | P0-11 | Retention enablement and thresholds — **gated, see below** | HW-6 |
| **W-D** | P0-12 | The "names only, missing secret fails cleanly" assertion test | HW-5 |
| **W-E** | P0-03 | A clean-host mode for the existing backup/restore drill | HW-7 |
| — | P0-09, P0-10 | **Nothing to author** — both are merged | HW-4 |

### Three findings that change who gets dispatched

**1. The unit already has its caps and sandbox.** `internal/deploy/render.go`
`UnitFile` emits `MemoryMax`/`MemoryHigh`/`CPUQuota`/`TasksMax`, `OOMPolicy=continue`,
and `NoNewPrivileges`/`ProtectSystem=strict`/`ProtectHome`/`PrivateTmp`/`ReadWritePaths`
(P0-04, merged `f5dc220`). P0-07's "optional but cheap" hardening item is therefore
**already done in code**, and P0-04's host run is a *verification* window, not a
code change. Nobody needs to reopen `render.go` for either task — which removes
the one file two of these streams would otherwise have fought over.

**2. P0-11's code half cannot be started as a Phase 0 task.** Automatic run/log
retention is `v0.3.0` **WS1** and the storage-pressure surface is **WS4**, and
neither is implemented: `runs.LogStore.DeleteOlderThan` is called only from its own
test, there is no run-deletion path at all, and `/health` returns counts only. So
W-C is either "start WS1" (the correct home — one home per task) or it waits.
**Do not assign a worker to "wire the alert thresholds" against a surface that
does not exist**; that is the busywork the execution plan warns about, and it
would produce a green test over nothing.

**3. HW-0 is not P0-07's evidence; HW-8 is.** §5.

### Worker prompt shape

Unchanged from execution-plan §2: verbatim task section, worktree path and branch,
the exact command that must exit 0, the rule that a green test which would still
pass with the behavior removed is not evidence, the repo conventions
(`scripts/drill.sh` as the template, doc-is-part-of-the-change), and an
instruction to report a blocker rather than guess. Add one line for this plan:
**never run anything against `OTTER_HOST`; submit the window's commands to the
custodian instead.**

## 9. Lane C — verification stays off the host

A host claim is verified by re-reading the transcript, re-deriving the assertion,
and **mutating the production behavior in a container** to confirm the drill would
have gone red. That work needs no host. Only P0-03's restore and P0-10's alert
inherently need real infrastructure, so the verifier's command list is folded into
the same window and confirmed from the raw transcript afterwards. **Never spend a
second host window just to satisfy author ≠ verifier** — that trades the scarce
resource for the cheap one.

## 10. Ledger and evidence changes

- Add a **Host windows** table to `phase-0-status.md`: window, task, precondition,
  transcript file, restored (yes/no), verifier. A window with no `restored` value
  is an open window.
- **P0-07** moves `host-gated` → `in progress`: infrastructure exists, artifact and
  evidence pending. Record `InstanceId`, EIP and AMI in its evidence record.
- Evidence naming: `<date>-<task>-host-<window>.txt`, following the existing
  convention (host, version, exact commands, raw output).
- The real critical path is still the **human asks**: the notify channel (HW-4),
  Castor's credentials (HW-5), the second host (HW-7), and above all the P0-13 job
  owner. Open them before the first window, not after.

## 11. Start today

**Lane A, in parallel (4 workers):** W-A, W-B, W-D, W-E. W-C only if WS1 is opened
deliberately.

**Lane B, serially:** HW-0 (read-only; needs only the key) then HW-1 (deploy the
pin). W-A's provisioning *script* is a reproducer of a sequence the operator runs
by hand in HW-0/HW-1 — so the first windows do not wait on lane A.

**Blocked on a human:** HW-4 (channel), HW-5 (credentials), HW-7 (second host),
and the pin decision in HW-1.

## 12. Open decisions and risks

| Item | Why it matters | Options |
| --- | --- | --- |
| **The pin for HW-1** | The ledger pins v0.2.0 and defers to "after P0-01/P0-02 land"; both are merged | Re-record the cutover pin before deploying, or knowingly ship the host a superseded version |
| **A second host for HW-7** | P0-03's clean-host bar cannot be met on one instance | A matching `t4g.micro` from the same stack under a different key pair, destroyed after (~$11/mo for days) |
| **Disk-fill on the root volume** | Can render the daemon unable to write — the exact failure it tests | A dedicated EBS volume for the drill, or a bounded fill with a cleanup trap |
| **The non-Castor host in `.otter/deploy.json`** | Gitignored local state records a 2026-09-16 deploy to a **different** host (amd64, root user). It is not the Castor host | Confirm whether it is still live before its state is mistaken for Castor evidence; if live, it is an unpinned runtime, which is what P0-05 forbids |
| **Host identity is committed nowhere** | Evidence must name its host; the CDK outputs were never saved | Capture the stack outputs in HW-0 |
| **1 GiB, two vCPUs** | Every window must hold `OTTER_WORKERS=1` and `concurrency: 1` | A drill that forces concurrency must restore the worker count in the same window |

## Related

- [phase-0-execution-plan.md](phase-0-execution-plan.md) — §3 W2, the plan this elaborates.
- [phase-0-tasks.md](phase-0-tasks.md) — the evidence bars each window must meet.
- [phase-0-status.md](phase-0-status.md) — the ledger the window table extends.
- [evidence/phase-0/README.md](../evidence/phase-0/README.md) — the recording convention.
- [otter-platform/README.md](../../../otter-platform/README.md) — the stack that created
  the host, its sizing rationale and the first-run sequence.
