# Phase 0 evidence

One file per recorded run. This directory is the answer to `R-19`: a Phase 0
blocker is closed by executable or recorded evidence, never by prose.

## What belongs here

A record of a run that actually happened, containing:

- **what** — the task ID and the claim the run supports;
- **when** — the date, with the time and zone;
- **where** — the host and platform. "macOS 15 arm64, developer checkout" and
  "Ubuntu 24.04, the Castor VM" are different evidence for the same script, and
  the difference matters;
- **which build** — `git rev-parse --short HEAD` plus `git describe --tags`;
- **the exact command** that was run, copy-pasteable;
- **the raw output**, including failures. Paste it, do not summarise it;
- **the exit status**;
- **the verdict** — what this does and does not prove.

## Naming

    YYYY-MM-DD-<task>-<slug>.txt

For example `2026-10-04-p0-01-crash-harness-ci.txt`. A raw capture may sit beside
a short `.md` that interprets it; the capture is the evidence, the interpretation
is not.

## What does not belong here

- A restatement of what the code does, with no command behind it.
- A passing unit test with no indication that it would fail if the behavior were
  removed. Every task's evidence names the verifier's mutation check for exactly
  this reason (see [phase-0-execution-plan.md](../phase-0-execution-plan.md) §2).
- Credentials, tokens, or captured payloads from a real destination. Redact the
  command, keep the output.

## Index

| Date | Task | Record | Host |
| --- | --- | --- | --- |
| 2026-09-29 | baseline, P0-05 | [2026-09-29-baseline.txt](2026-09-29-baseline.txt) | macOS 15 arm64, developer checkout |
| 2026-09-29 | tooling for P0-01/P0-04 | [2026-09-29-linux-docker-probe.txt](2026-09-29-linux-docker-probe.txt) | Linux container (`golang:1.24`) on macOS |
| 2026-09-29 | P0-01 | [2026-09-29-p0-01-crash-harness.txt](2026-09-29-p0-01-crash-harness.txt) | macOS 15 arm64 and a `golang:1.24` Linux container |
| 2026-09-29 | P0-03 | [2026-09-29-p0-03-backup-restore-drill.txt](2026-09-29-p0-03-backup-restore-drill.txt) | macOS 15 arm64 and a Linux container with `sqlite3` |
| 2026-09-29 | P0-04 | [2026-09-29-p0-04-unit-caps-drill.txt](2026-09-29-p0-04-unit-caps-drill.txt) | macOS 15 arm64; systemd 255 in an Ubuntu 24.04 container |
| 2026-09-29 | P0-04 falsification | [2026-09-29-p0-04-unit-caps-mutation.txt](2026-09-29-p0-04-unit-caps-mutation.txt) | same |
| 2026-09-29 | P0-06 | [2026-09-29-p0-06-deploy-failure-drill.txt](2026-09-29-p0-06-deploy-failure-drill.txt) | macOS 15 arm64; Ubuntu 24.04 with systemd and sshd as the deploy target |
| 2026-09-29 | aggregate suite | [2026-09-29-aggregate-drills.txt](2026-09-29-aggregate-drills.txt) | macOS 15 arm64; `make drill` runs all four drills on `main` |
| 2026-09-29 | P0-10 | [2026-09-29-p0-10-liveness-drill.txt](2026-09-29-p0-10-liveness-drill.txt) | macOS 15 arm64; real `otterd` on loopback plus a local stand-in for the dead-man service |
| 2026-09-30 | P0-07 host identity | [2026-09-30-p0-07-host-identity.txt](2026-09-30-p0-07-host-identity.txt) | operator checkout; read-only AWS/CloudFormation calls against the otter account (no host access) |
| 2026-09-30 | P0-07 HW-0 baseline | [2026-09-30-p0-07-host-baseline.txt](2026-09-30-p0-07-host-baseline.txt) | the Castor VM (`i-00dcd4c34a0dcb0ef`), read-only, plus a local full-range TCP scan |
| 2026-09-30 | P0-07 HW-1 blocked | [2026-09-30-p0-07-hw1-deploy-blocked.txt](2026-09-30-p0-07-hw1-deploy-blocked.txt) | operator checkout; the documented first-run deploy command deploys nothing |
| 2026-09-30 | P0-07 HW-1 deploy | [2026-09-30-p0-07-hw1-deploy.txt](2026-09-30-p0-07-hw1-deploy.txt) | the Castor VM: v0.3.0-rc1 deployed, caps live, CPython 3.13.1 prepared on arm64 |
| 2026-09-30 | P0-04 host re-run (failed) | [2026-09-30-p0-04-unit-caps-host-failure.txt](2026-09-30-p0-04-unit-caps-host-failure.txt) | the Castor VM: a runaway job is throttled and swapped, not OOM-killed; otterd starved |
| 2026-09-30 | P0-11 HW-6b disk-fill | [2026-09-30-p0-11-host-hw6b-disk-fill.txt](2026-09-30-p0-11-host-hw6b-disk-fill.txt) | the Castor host: 100% full is harmless until a write exceeds what is left, and then a run reports succeeded while its output is lost |
| 2026-09-30 | P0-11 HW-6a **passed** | [2026-09-30-p0-11-host-hw6a.txt](2026-09-30-p0-11-host-hw6a.txt) | the Castor host: every retention window pruned its own store, the control pruned nothing, and the live runtime was left configured for 30-day runs / 30-day logs |
| 2026-09-30 | P0-04 HW-3 **passed** | [2026-09-30-p0-04-hw3-unit-caps-fixed.txt](2026-09-30-p0-04-hw3-unit-caps-fixed.txt) | the Castor host: the runaway killed in 1.417s, the cgroup never swapped, otterd survived |
| 2026-09-30 | P0-03 HW-7 **passed** | [2026-09-30-p0-03-hw7-clean-host.txt](2026-09-30-p0-03-hw7-clean-host.txt) | two real hosts: a live runtime restored onto a clean second host, and a job run there |
| 2026-09-30 | P0-03 HW-7 first run | [2026-09-30-p0-03-hw7-first-run.txt](2026-09-30-p0-03-hw7-first-run.txt) | two real hosts: the gate passed and the run failed at the archive identity check |
| 2026-09-30 | P0-07 HW-8 assertion | [2026-09-30-p0-07-hw8-assertion.txt](2026-09-30-p0-07-hw8-assertion.txt) | the Castor host: external scan plus the permission assertion passing on the real runtime |
| 2026-09-30 | P0-07 host rebuilt | [2026-09-30-p0-07-host-rebuilt.txt](2026-09-30-p0-07-host-rebuilt.txt) | the Castor host rebuilt from the fixed stack: cloud-init done, swap and report present, deploy, and the permission assertion |
| 2026-09-30 | P0-12 (local half) | [2026-09-30-p0-12-secret-scoping.txt](2026-09-30-p0-12-secret-scoping.txt) | macOS 15 arm64, developer checkout; no remote host. The real Castor credentials and the real host run are still owed — see window HW-5 |
| 2026-09-30 | P0-04 swap bound — **partly superseded** by the MemoryHigh-default record below | [2026-09-30-p0-04-unit-caps-swap.txt](2026-09-30-p0-04-unit-caps-swap.txt) | macOS 15 arm64; Ubuntu 24.04 systemd container with a real swap device. Its deployed-style case was **red** under the then-current MemoryHigh=60%; the measurement is what changed the default |
| 2026-09-30 | P0-04 swap-bound falsification | [2026-09-30-p0-04-unit-caps-swap-mutations.txt](2026-09-30-p0-04-unit-caps-swap-mutations.txt) | same |
| 2026-09-30 | P0-04 MemoryHigh default | [2026-09-30-p0-04-memory-high-default.txt](2026-09-30-p0-04-memory-high-default.txt) | macOS 15 arm64; Ubuntu 24.04 systemd container with a real swap device. All four cases pass; the deployed-style case renders the default unit, and the soft-cap case measures why the default is `off` |
| 2026-09-30 | P0-03 (hostless half, R1–R8, D1–D5 and the HW-7 archive-layout fix) | [2026-09-30-p0-03-clean-host-mode-selfcheck.txt](2026-09-30-p0-03-clean-host-mode-selfcheck.txt) | macOS 15 arm64; the clean-host mode's selfcheck (real probe against real directories and a real otterd; ssh and systemctl doubles whose output shapes are captured from the real ones), the same-host drill clean and sabotaged, and every defect the verification rounds found — D1, D2, D4, D5 and mutation M5 — reintroduced to show the selfcheck catches them (includes the archive-layout contract case that the first real HW-7 run showed was missing; the selfcheck reaches CI through `go test ./...`, not `make drill`) |
| 2026-09-30 | P0-08 | [2026-09-30-p0-08-egress-managed-python.txt](2026-09-30-p0-08-egress-managed-python.txt) | macOS 15 arm64, developer checkout; uv 0.5.9 against a loopback PEP 503 index (no remote host) |
| 2026-09-30 | P0-07 (rebuilt host) | [2026-09-30-p0-07-host-rebuilt-2.txt](2026-09-30-p0-07-host-rebuilt-2.txt) | macOS 15 arm64 → a rebuilt `t4g.micro` (`i-0ba293ef4c5926b04`, `52.202.163.124`): the clean baseline (arm64, swap, IMDSv2 enforced, no `otterd` unit, only SSH listening) and `otter deploy` converging it to `v0.3.0-rc1` with CPython 3.13.1 prepared for all three jobs. The previous host was destroyed the same day, so the private key is new and was re-fetched from SSM |
| 2026-09-30 | P0-10 (HW-4b, sliding window) | [2026-09-30-p0-10-host-hw4b-liveness.txt](2026-09-30-p0-10-host-hw4b-liveness.txt) | real `t4g.micro`, real `otterd` stopped for real: the check went silent, the CloudWatch alarm fired and SNS delivered one notification, then the restart cleared it. **Latency 11m10s**, not the ~5 minutes the settings imply — CloudWatch's default sliding evaluation window keeps re-reading the last real datapoints and ignores the missing-data treatment. This run is the measurement that found it |
| 2026-09-30 | P0-10 (HW-4b, wall-clock window) | [2026-09-30-p0-10-host-hw4b-liveness-walled.txt](2026-09-30-p0-10-host-hw4b-liveness-walled.txt) | same drill after both alarms were switched to a **wall-clock** evaluation window: **5m34s** from kill to ALARM (CloudWatch's own transition timestamp), ALARM→OK 1m59s after the restart, SNS notification delivered in the cycle. The fix is what makes `Period 60s x 5` mean five minutes |
| 2026-09-30 | P0-11 (HW-6b, storage alert) | [2026-09-30-p0-11-host-hw6b-alert.txt](2026-09-30-p0-11-host-hw6b-alert.txt) | real host plus a dedicated 2 GiB EBS volume (created, filled to 49MB free, deleted): three real breaches — a 1MB cap on the live 62MB environments store, a 999999MB floor on the real disk, and the default 2048MB floor on the full volume — each silenced the check, the timer stopped publishing, the CloudWatch alarm fired **5m28s** later, SNS delivered a notification, and a byte-exact restore cleared it in 60s. The root volume was never filled |
| 2026-09-30 | P0-09, P0-10, P0-11 (**author-run** verification round) | [2026-09-30-p0-09-p0-11-author-verification.txt](2026-09-30-p0-09-p0-11-author-verification.txt) | NOT independent verification, and it cannot fill the Verifier column — the verifier wrote the publisher and the window scripts. It re-derived claims from live CloudWatch and the live host and mutated production behaviour: the P0-10 crux reproduced under identical conditions (sliding 11m57s vs wall-clock 6m39s, no host), P0-11's alert path re-derived (ALARM ~6 min after silence, cleared in 1 min, actions re-armed), P0-09's contract pinned by three mutations. One defect (OT-022: the installer's convergence probe can never report a converged host, and its suite stubs the probe wholesale) and three gaps (OT-023 no notify drill, OT-024 the metric contract unasserted, OT-025 the window harness not in the repo) |
