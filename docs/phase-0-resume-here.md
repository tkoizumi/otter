# Phase 0 — resume here

Date: 2026-09-30; last session entry 2026-10-01. This replaces the running
commentary as the place to pick up from. It says what is true, what is still in
flight, and what to do next.

## The one thing that matters

**Castor's actual import is still unselected (P0-13), and it belongs to someone
outside this session.** Every host window so far rehearses on a stand-in — the
public `otter_examples` project. That is a good stand-in (real managed Python,
real schedules, real checkpoints) but it is not Castor, and no amount of further
host work or verification changes that.

The host half of Phase 0 is now largely proven. What remains is mostly **asking
people for things**. If you read nothing else: the next real step is a message
to Castor's job owner, not a deploy.

## The host, as it stands

| | |
| --- | --- |
| Runtime host | `i-0ba293ef4c5926b04` / `52.202.163.124`, Ubuntu 24.04 arm64, `t4g.micro`, 20 GiB gp3, 2 GiB swap — **rebuilt 2026-09-30** after the teardown below |
| Running | `otter v0.3.0-rc1`, built from `main` @ `cfd595c` (the string is hand-stamped: the repo's newest tag is `v0.2.0`) |
| Workspace | `otter-examples-e0309b8c` — the stand-in, three jobs, **both cron schedules paused**, **no credentials deployed** |
| Unit caps | `MemoryMax=75%`, `MemorySwapMax=0`, `CPUQuota=200%`, `TasksMax=512`, **no `MemoryHigh`** — proven by HW-3 on the previous host; not yet re-run here |
| Provisioning | `cloud-init: done`, no errors; swap and `/var/log/otter-provision-report.txt` present |
| Key | `.otter-keys/otter-castor.pem` — **re-fetched from SSM** for the new key pair `key-096f5d276bd8eea63`; the old key belongs to the destroyed host |
| API token | `otter_examples/.otter/state.secret.json` — a new token per host, keyed by `<ip>/<workspace-id>` |
| Identity record | `.otter-keys/host-identity.txt` (workspace-local, outside both repos) and [2026-09-30-p0-07-host-rebuilt-2.txt](evidence/phase-0/2026-09-30-p0-07-host-rebuilt-2.txt) |
| Monitoring | checks installed and both alarms live: `CastorRuntime-liveness` and `CastorRuntime-disk`, `OK`, actions armed, wall-clock window, one confirmed email subscription. OT-022 still makes the installer's convergence probe report "not yet converged" on every run |
| Drill target | **destroyed** |
| Previous host | **DESTROYED 2026-09-30**, and verified: no instances, no Elastic IPs, no volumes. That teardown is why the private key above had to be re-fetched rather than reused |

A rebuild is `npx cdk deploy --profile otter -c sshCidr=…`, then `otter deploy`
from the `otter_examples` checkout (~10 minutes), then
`sh scripts/install-monitoring.sh --host ubuntu@<ip>`, then the second
`cdk deploy` with `-c alertEmail=…`. The installer has to follow `otter deploy`,
which creates the `<workspace>/.otter/data` the disk check measures; a host
whose data directory does not exist yet must not have the disk check installed,
and the installer refuses it by name because that check would alarm on a healthy
host. Skip the installer and the checks never publish: the alarms exist and go
to `ALARM` about five minutes after `alertEmail` is passed, so the omission is
loud rather than silent — but only once that second deploy has run.

## Last session — 2026-09-30 16:00 → 2026-10-01 00:20

One line: the runtime half merged and was independently verified; the host was
destroyed, rebuilt and made to alert; P0-09/P0-10/P0-11 were verified a second
time, and the two real gaps that round found were fixed the same night.

**Merged** — P0-12, P0-04, P0-03, P0-08 and P0-07, serially. The gate caught a
build break git could not see: P0-07's test called `ReleaseScript` with the
signature P0-08 had changed, in a different file.
[phase-0-guarantees.md](phase-0-guarantees.md) records what P0-01…P0-08 buy and
where each claim stops.

**Host** — the previous host was destroyed and the teardown verified, then
rebuilt from the fixed stack: `i-0ba293ef4c5926b04` / `52.202.163.124`. HW-0
clean, HW-1 converged in **13 s** with CPython 3.13.1 prepared for all three
stand-in jobs.

**Alerts** — the checks were installed, a CloudWatch publisher written and two
alarms made live. HW-4b found the sliding-window latency (**11m10s**) and the
wall-clock fix measured **5m34s**. HW-6b-alert: ALARM in **5m28s**, one SNS
delivery, restored in 60 s. The disk-fill found `OT-020` — output lost at 10 MiB
free while the run still reported `succeeded`.

**Verified independently** — P0-09, P0-10 and P0-11 re-derived on the live host
and under mutation: [independent
record](evidence/phase-0/2026-10-01-p0-09-p0-11-independent-verification.txt).
Two findings. **D1 (material):** retention was **not in force** on the live host
— it ran `run/log retention 0s`, i.e. retain forever, because the configuration
lived on a host destroyed the same day and nothing re-applied it on a rebuild.
**D2:** the monitoring suites stayed green with the publisher unsigned, the disk
metric renamed and `df` measuring the wrong filesystem.

**Fixed the same night** — `OT-026` (`7760c5a`): the windows are committed in
`otter.deploy.yaml` and rendered into the unit's `ExecStart`, so a stale value in
the env file cannot shadow them; the live host now logs `run_retention=2160h
log_retention=720h` and its unit is byte-identical to the renderer's output.
`otter_examples/otter.deploy.yaml` was **untracked** and is now committed
(`8b27693`). `OT-027` (`c23b0a6`): the fixtures were hardened and wired into
`make test` plus a CI `monitoring` job — which immediately caught a real
Linux-only false red in the permission suite, fixed and re-run **48/48** on the
Castor host. `make test-shell` is six suites, 200 cases.

**State at the end** — host up on `v0.3.0-rc1`, retention in force across a
reboot, both alarms `OK` with actions armed, every host mutation restored
byte-for-byte, and both repositories clean.

**Pick up here tomorrow** — P0-13 is still the critical path, and it still needs
a person rather than a deploy. Nothing in the session above changes that.

## Windows done, with evidence

| Window | What it proved |
| --- | --- |
| HW-0 | Clean baseline; caught the stack's cloud-init failure (no swap, no report) |
| HW-1 | `otter deploy` converges the host; **CPython 3.13.1 prepared on arm64**, which answers P0-08's documented worry |
| HW-3 | The caps kill a runaway in **1.417s**, `swap.current 0`, daemon survives — after the first attempt falsified the claim |
| HW-7 | A live runtime backed up **while serving**, restored onto a clean second host, a job run there — **passed twice** |
| HW-8 | External scan finds only 22/tcp; the permission assertion exits 0 on the real host |

## Merged to `main`

- **P0-12** (credentials) — independently verified.
- **P0-04** (resource caps) — independently verified, and its host claim proven by HW-3.
- **P0-03** (backup and restore) — merged on HW-7 passing twice.
- **P0-08** (egress and managed Python) — merged as "merge-safe, record not"; its record is corrected in place.

Six follow-ups are listed in [phase-0-status.md](phase-0-status.md) under "Follow-ups
recorded, not blocking". None blocks anything.

## Merged, all of it

Five branches: **P0-12**, **P0-04**, **P0-03**, **P0-08**, **P0-07**. `main` is pushed.

The P0-07 merge broke the Go build without a merge conflict — its test called
`ReleaseScript` with the old signature that P0-08 had changed, in a different
file. The gate caught it and it is fixed. Worth remembering: the dangerous
conflicts are the ones git cannot see.

## Blocked on people, not work

1. **Which Castor import, and its owner (P0-13).** The critical path. Nothing
   else on this list matters until it is answered.
2. **Castor's credential names and values (HW-5).**
3. **A notification channel** — exists in `otter.daemon.env` (a Slack webhook,
   gitignored) and HW-4a did post to it, on the host destroyed 2026-09-30. The
   2026-10-01 round re-derived the path against a loopback stand-in rather than
   re-posting to the channel. Posting stays a deliberate act, not a side effect
   of a deploy.
4. **A dead-man's-switch endpoint** — **no longer needed.** HW-4b passed twice,
   and the observer is the CloudWatch liveness alarm (wall-clock window), so
   there is no healthchecks.io-style URL to obtain.
5. **A second host for HW-7** — destroyed; recreate from the committed stack in
   about three minutes when needed.

## Next actions, in order

1. **Ask Castor's job owner** which import, with destination, owner and business
   effect. Everything else is waiting on this. Still the critical path.
2. **Then the ordinary backlog**: HW-5 needs Castor's credential values, the
   OT-022 installer probe fix is small and self-contained, and `OT-028` (split
   daemon settings by secrecy) is the design follow-up to `OT-026`.

**Both findings from the 2026-10-01 verification are closed.** D1: retention is
committed in `otter.deploy.yaml` and rendered into the unit, so a rebuild
reproduces it, and the live host logs `run_retention=2160h0m0s
log_retention=720h0m0s`. D2: the monitoring suites refuse an unsigned publish,
assert the signature and the disk payload, assert which filesystem `df` was asked
about, and run in `make test` and in a CI `monitoring` job. Records:
[verification](evidence/phase-0/2026-10-01-p0-09-p0-11-independent-verification.txt),
[retention](evidence/phase-0/2026-10-01-p0-11-retention-config.txt),
[fixtures](evidence/phase-0/2026-10-01-p0-09-p0-11-fixture-hardening.txt).

Do not start another verification round on `p0-08`. Its residual is three
labelled conservative refusals, all with a terminating remedy, and it is not on
Castor's path.

## The lesson worth carrying forward

Three verification rounds on the clean-host drill produced real fixes — the
token path, the version comparison, `EnvironmentFiles` parsing, empty-argument
loss — and **not one of them reached past the preflight gate.** The first real
execution reached it in 19 seconds and found a one-word bug. The same pattern
held on the host: the assertion written for P0-07 found three wrong expectations
the moment it ran against a real machine.

Verification is worth what it costs only when the thing under test can actually
run. When it cannot, the fixtures become the thing being tested, and a green
suite over a wrong assumption is worse than no suite — it buys confidence that
has not been earned.
