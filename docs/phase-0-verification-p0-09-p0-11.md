# Verifying P0-09, P0-10 and P0-11

Status: brief for an independent verifier. Date: 2026-09-30.

These three rows are `pending` in [phase-0-status.md](phase-0-status.md)'s Verifier
column while every other closed task in Phase 0 went through a round that attacked
its claims and usually found something. This document is what such a round needs:
the bar each task was measured against, what was claimed, what would make the
claim false, and where the weak joints are.

## Who verifies what

**A verifier is a session that did not write the thing.** It should work in a git
worktree, re-derive assertions from primary sources rather than re-reading the
transcripts, and try to make each claim fail. Three roles are distinct here:

| Role | Who | Why |
| --- | --- | --- |
| Author | the sessions that wrote the checks, the alarms and the drills | cannot sign off on its own work |
| Verifier | an independent session, in a worktree | attacks the claims and the *fixtures they rest on* |
| Owner | you | the human-visible half (a message arriving in a channel you read) and the judgement calls (channel, AWS path, retention values) |

Two facts are already owner-verified and cannot be re-derived from the repo:

- **P0-09**: two failure notifications arrived in the Slack channel (22:54:26 and
  22:55:08 EDT, runs `6a68d036…` and `042c3e06…`).
- **P0-10**: three alarm emails arrived (two liveness cycles and one storage
  cycle) with nobody watching a terminal.

## What "verified" means here

The house bar, from [phase-0-execution-plan.md](phase-0-execution-plan.md) §2: a
green test that would still pass with the behaviour removed is not evidence. The
work is to *reproduce the assertion independently* and then **mutate the
production behaviour to confirm the drill would have gone red**.

The failure mode every previous round found is the same one: **a double, fixture
or stand-in that encodes an assumption the real thing contradicts.** A fixture
asserted that systemd prints one `EnvironmentFiles=` line; real systemd prints one
per file. A fake ssh fabricated version agreement that no real pair could reach.
Both suites stayed green. So the first question about any claim below is *what is
standing in for reality, and does reality actually behave that way?*

## Access and ground rules

| | |
| --- | --- |
| Runtime host | `i-0ba293ef4c5926b04` / `52.202.163.124`, Ubuntu 24.04 arm64, workspace `otter-examples-e0309b8c` |
| SSH key | `.otter-keys/otter-castor.pem` (workspace-local, **outside both repos** — never commit it) |
| API token | `otter_examples/.otter/state.secret.json`, keyed `<ip>/<workspace-id>` (**gitignored**) |
| AWS | `--profile otter --region us-east-1`; account `426714791664` |
| Alarm/topic | `CastorRuntime-liveness`, `CastorRuntime-disk` → `arn:aws:sns:us-east-1:426714791664:CastorRuntime-alarms` |
| Metrics | namespace `Otter`, dimension `Host=castor-runtime`, metrics `Heartbeat`, `DiskCheck`, `DiskFreeMB` |
| On the host | `/usr/local/lib/otter/{otter-metric,heartbeat,disk-check}.sh`, `/etc/otter/{heartbeat,disk-check}.env`, units `heartbeat.{service,timer}` and `disk-check.{service,timer}` |

Rules:

1. **No credential value in this file or in any report.** Paths only.
2. **Lane B is serial.** One window at a time; the host has one daemon, one data
   directory, one unit namespace, one disk. Announce a window and hold it.
3. **This document is committed; the window scripts are not.** The scripts that
   produced the host transcripts (`hw4a-notify-window.sh`,
   `hw4b-liveness-window.sh`, `hw6b-storage-alert-window.sh`) live in
   `.otter-keys/`, outside both repos. That is itself a gap worth reporting: the
   transcripts are reproducible in principle but the *harness* is not in the
   repo. Ask the custodian for the script, or re-derive the procedure from the
   transcript — every command and its output is in there.
4. Local suites (no host, seconds each), from the checkout root:
   `sh scripts/drill/liveness.sh`, `sh scripts/drill/disk-pressure.sh`,
   `DRILL_SABOTAGE=ping-always sh scripts/drill/disk-pressure.sh`,
   `sh scripts/test-disk-check.sh`, `sh scripts/test-otter-metric.sh`,
   `sh scripts/test-unit-names.sh`.

## P0-09 — Failure notification (`CA-30`)

**The bar.** *"Configure a human-visible failure channel for every production
client"* (`CA-30`, MUST). Deliverable: a channel a human actually reads. Evidence:
a deliberately failing run posts to that channel.

**The claim.** On the rebuilt host, a run that fails **before Python starts**
(the two Shopify jobs declare four secrets the host does not have) posted to the
real Slack channel: `notification_sent` with `job=…`, `status=failed`,
`attempt=1` — one delivery, so the failure was not retried — and the host was
restored byte-for-byte (env sha256 unchanged, `OTTER_NOTIFY_URL` gone, `/health`
200). [Transcript](evidence/phase-0/2026-09-30-p0-09-host-hw4a.txt).

**How to verify.**

1. **Re-derive the delivery, don't trust the log line.** `notification_sent` is
   logged only after `post` sees a 2xx (`internal/notify/notify.go`); confirm that
   reading yourself, then confirm the *channel* half with the owner.
2. **Re-run it.** Preconditions: both cron schedules paused (so nothing else can
   post), `OTTER_NOTIFY_ON=failed`, `OTTER_NOTIFY_FORMAT=slack`, `OTTER_LOG_LEVEL=debug`.
   Add `OTTER_NOTIFY_URL` to the deployed `/etc/otter/workspaces/<ws>.daemon.env`,
   restart, `otter run <job>`, and read the run's terminal status. Observe one
   `notification_sent` per run, and a message in Slack.
3. **Mutations that must turn it red.**
   - `OTTER_NOTIFY_ON=timed_out` → a failed run must publish **nothing**. If it
     still notifies, the status filter is not load-bearing.
   - Point `OTTER_NOTIFY_URL` at a reserved path (or a stand-in that answers 500)
     → the daemon must log a delivery failure and **not** `notification_sent`.
     This is the difference between "we sent something" and "someone was told".
   - Run a job that succeeds (`drill-job`) → no notification.
4. **What is not covered.** Only the `slack` body format has been exercised on a
   host. `json`, `discord` and `teams` are covered by unit tests only — a
   stand-in receiver with `OTTER_NOTIFY_FORMAT=json` would close that cheaply and
   is a legitimate finding if it fails.
5. **The standing configuration is not in place.** The deployed stand-in env file
   is deliberately back to notify-off, because every stand-in run fails and would
   post every five minutes. So `CA-30`'s "configure a channel" is met *for the
   pattern* and not *for a production client* — Castor has no channel until
   P0-12/P0-13 exist. Judge whether the row says that clearly enough.

## P0-10 — Independent liveness detection (`CA-31`)

**The bar.** *"Do not rely on `otterd` to report its own death"* (`CA-31`, MUST);
a minute-level external check against `/health` that alerts when the check
**stops** — a dead-man's switch, not a positive check. Evidence: kill the daemon
and confirm an alert arrives with nobody watching a terminal.

**The claim.** `heartbeat.sh` publishes `Otter/Heartbeat=1` while `/health`
answers and publishes nothing when it does not; `heartbeat.timer` runs it every
minute; the CloudWatch alarm has `TreatMissingData: breaching` **and a wall-clock
evaluation window**; SNS mails the owner. Two host runs:
[sliding window, kill → ALARM **11m10s**](evidence/phase-0/2026-09-30-p0-10-host-hw4b-liveness.txt),
[wall-clock window, **5m34s**](evidence/phase-0/2026-09-30-p0-10-host-hw4b-liveness-walled.txt).

**How to verify.**

1. **Re-read the alarm from AWS, not from the transcript**:
   `aws cloudwatch describe-alarms --alarm-names CastorRuntime-liveness CastorRuntime-disk`
   → period 60, 5 evaluation periods, `TreatMissingData=breaching`,
   `EvaluationWindow={WallClockWindow:{}}`, one action (the topic), dimension
   `Host=castor-runtime`. Then read
   `otter-platform/lib/castor-runtime-stack.ts` and confirm the stack is what
   produces that — and that removing the window line breaks a test.
2. **Re-run the kill** and measure the latency from **CloudWatch's own state
   history**, not from the drill's own printed timestamps:
   `describe-alarm-history --history-item-type StateUpdate`.
3. **Mutations that must turn it red.**
   - Delete `EvaluationWindow` from a stack copy and redeploy (or `set-alarm-state`
     is not enough — that bypasses evaluation): the detection latency must go back
     to ~11 minutes. This is the claim's crux; a re-run that measures only the
     fixed config has not tested it.
   - Remove `StorageResolution=60` from a `otter-metric.sh` copy → the local
     suite must go red.
   - Point the check at a dead API port → non-zero exit, no publish
     (`sh scripts/drill/liveness.sh` already asserts this; confirm the assertion
     is not vacuous).
4. **Test the tolerance the design claims.** A *single* missed minute must **not**
   alarm (that is why 5 periods are used). Measure it: run the service, stop the
   check for one period, confirm the alarm stays `OK`. If a single miss alarms,
   the documented false-alarm tolerance is false.
5. **Test the absent-host case.** The switch's premise is that a host which
   cannot send anything is still caught. Stop the timer (not the daemon) and
   confirm `ALARM`; the host sending nothing is the same signal as the host being
   gone.
6. **Weak joints to attack.**
   - **Latency.** The check is minute-level; the *alert* waits ~5.5 minutes by
     design. Is that "alerts when the check stops" for the task's purpose, or
     should the row say the number out loud? (It does, now.)
   - **Substitution.** `CA-31` says *the control plane* tracks the heartbeat.
     Phase 0 has no control plane; CloudWatch stands in. The row should record
     the substitution explicitly rather than leave it to the reader.
   - **Install is manual.** Until `scripts/install-monitoring.sh` lands, a
     rebuilt host has no checks and the alarms fire forever — loud, but only
     because `alertEmail` was passed. Verify the installer actually converges and
     refuses a host whose data directory does not exist yet.

## P0-11 — Disk retention and thresholds (`CA-19`, `CA-33`)

**The bar.** *"Monitor filesystem usage, SQLite size, release artifacts, Python
environment/cache growth, and log/capture retention. Define retention and alert
thresholds before enabling unattended jobs"* (`CA-33`, MUST). Deliverable:
enable the three retention windows (they default to retain-forever) and set alert
thresholds covering all four stores; include a disk-fill drill, because the
failure mode is not "slow", it is "the daemon cannot write". Evidence: the alert
fires at a test threshold, and retention demonstrably prunes.

**The claims.**

- **Retention prunes**: four passes over *copies* of a consistent live snapshot —
  a long-window control that pruned nothing, the run window alone, the log window
  alone, and the capture window alone — each isolating one store.
  [Transcript](evidence/phase-0/2026-09-30-p0-11-host-hw6a.txt).
- **The fill's finding**: at 99% and even 100% full (10 MiB free) the runtime is
  unaffected; a job emitting 64 MiB then lost its output
  (`run_log_write_failed … database or disk is full (13)`, eleven times at error)
  **while the run reported `succeeded`**.
  [Transcript](evidence/phase-0/2026-09-30-p0-11-host-hw6b-disk-fill.txt).
- **The alert fires**: three real breaches (a 1 MB cap on the live 62 MB
  environments store; a 999999 MB floor on the real disk; the default 2048 MB
  floor on a genuinely full 2 GiB EBS volume at 49 MB free) each silenced the
  check, the timer stopped publishing, the alarm went `ALARM` 5m28s later, SNS
  delivered, and a byte-exact restore cleared it.
  [Transcript](evidence/phase-0/2026-09-30-p0-11-host-hw6b-alert.txt).

**How to verify.**

1. **Retention.** Re-derive the count yourself: `sqlite3` the copy before and
   after each pass and read the four stores (`runs`, `run_logs`,
   `http_exchanges`, `run_capture.payloads_expired`). Mutations: run the control
   pass and confirm **nothing** prunes; flip one window and confirm only its own
   store moves.
2. **Thresholds.** Read `scripts/disk-check.sh` and confirm all five measures
   exist and that the rounding is asymmetric (sizes ceil, free space floors).
   Run the local suites, then mutate: flip `-lt` to `-le` on the free floor and
   confirm `test-disk-check.sh` goes red; make the check publish on a breach and
   confirm the drill goes red.
3. **The alert.** Cheapest re-derivation: stop the `disk-check.timer` (no
   publishes) and confirm `ALARM` — the same signal as a breach, without
   re-creating a volume. For the full drill, re-create a small volume, mount it,
   and **verify the free-space floor is what fires**, not the store caps.
4. **Weak joints to attack.**
   - **Capture retention cannot be configured.** There is no environment
     variable and no `otter.deploy.yaml` key, so "enable all three windows" is
     satisfied *in force* (capture is on at its 168h default) but not in
     configurability. Is that the task's bar, or a gap?
   - **The threshold values are ours, not Castor's.** 512 / 1024 / 3072 / 1024 MB
     with a 2048 MB free floor. The fill drill showed the *free-space* floor is
     the interesting one — 10 MiB free looked fine until a job wrote more than
     that. Ask whether a 2 GiB floor is high enough given that failure mode, and
     whether a per-store cap is the right instrument at all.
   - **The alerting is out of band.** `CA-33` is met by a host script, not by a
     daemon surface; `/health` still reports nothing about storage. Say so
     plainly in the row rather than letting "monitors storage" imply the daemon
     knows.
   - **`CA-19` (SHOULD, prime the uv cache)** is evidenced only indirectly: the
     deploy vendors uv and prepares the interpreter. Confirm the cache is
     actually primed on the host rather than assumed.

## Cross-cutting claims

- **Two repos, one mechanism.** The check lives in `otter`, the alarm and topic in
  `otter-platform`. Neither repo's suite exercises the other; the only place they
  meet is the metric contract (namespace, metric name, dimension, `Host` value).
  A mismatch is silent — the alarm would sit in `ALARM` forever and nobody would
  connect it to a typo. A verifier should check both sides against the contract
  by hand, and consider whether anything should assert it.
- **The metric contract is verified against the real API exactly once**, by hand:
  a raw `PutMetricData` from the host returned HTTP 200 and the datapoints are
  readable with `get-metric-statistics`. The *drills* use a stand-in receiver that
  accepts anything, so the drills alone never proved the signature. Re-derive it
  on the host (`OTTER_METRIC_ENDPOINT` unset) and read the datapoints back.
- **The evidence transcripts are the primary artifacts.** Compared to P0-03's
  round, these claims rest on fewer doubles — real CloudWatch, real systemd, real
  disk — but the fixtures inside `disk-pressure.sh` and `test-otter-metric.sh`
  stand in for CloudWatch and IMDS. Attack the doubles.

## What a verdict must contain

A `VERIFIED` or `NOT VERIFIED` line per task, and for each: the commands run, the
mutations and where each went red, any claim reproduced independently, every
defect with a reproduction, and the residual that stays unverified (with the
reason). The precedent is the P0-03 entry in
[phase-0-status.md](phase-0-status.md): numbered findings, each either reproduced
or explicitly marked as an inspection finding.

If a claim cannot be verified without a host window, say so and name the window —
do not downgrade the claim to prose.

## Related

- [phase-0-tasks.md](phase-0-tasks.md) — the task definitions and their bars.
- [phase-0-status.md](phase-0-status.md) — the ledger these rows live in.
- [phase-0-host-run-plan.md](phase-0-host-run-plan.md) — lane B, the serialised
  host lane and its rules.
- [evidence/phase-0/README.md](evidence/phase-0/README.md) — the recording
  convention.
- [cloud-alpha-readiness.md](cloud-alpha-readiness.md) — `CA-19`, `CA-30`,
  `CA-31`, `CA-33` and the cutover blocker table.
