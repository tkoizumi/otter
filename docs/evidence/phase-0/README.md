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
| 2026-09-30 | P0-03 HW-7 first run | [2026-09-30-p0-03-hw7-first-run.txt](2026-09-30-p0-03-hw7-first-run.txt) | two real hosts: the gate passed and the run failed at the archive identity check |
| 2026-09-30 | P0-07 HW-8 assertion | [2026-09-30-p0-07-hw8-assertion.txt](2026-09-30-p0-07-hw8-assertion.txt) | the Castor host: external scan plus the permission assertion passing on the real runtime |
| 2026-09-30 | P0-07 host rebuilt | [2026-09-30-p0-07-host-rebuilt.txt](2026-09-30-p0-07-host-rebuilt.txt) | the Castor host rebuilt from the fixed stack: cloud-init done, swap and report present, deploy, and the permission assertion |
| 2026-09-30 | P0-12 (local half) | [2026-09-30-p0-12-secret-scoping.txt](2026-09-30-p0-12-secret-scoping.txt) | macOS 15 arm64, developer checkout; no remote host. The real Castor credentials and the real host run are still owed — see window HW-5 |
