# Phase 0 — resume here

Date: 2026-09-30. This replaces the running commentary as the place to pick up
from. It says what is true, what is still in flight, and what to do next.

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
| Runtime host | `i-01ed07dce59ba3f5b` / `44.213.227.52`, Ubuntu 24.04 arm64, `t4g.micro`, 20 GiB gp3, 2 GiB swap |
| Running | `otter v0.3.0-rc1`, built from `main` @ `7319363` |
| Workspace | `otter-examples-e0309b8c` — the stand-in, three jobs, **both cron schedules paused**, **no credentials deployed** |
| Unit caps | `MemoryMax=75%`, `MemorySwapMax=0`, `CPUQuota=200%`, `TasksMax=512`, **no `MemoryHigh`** — proven by HW-3 |
| Provisioning | `cloud-init: done`, no errors; swap and `/var/log/otter-provision-report.txt` present |
| Key | `.otter-keys/otter-castor.pem` (workspace-local, outside both git repos) |
| API token | `otter_examples/.otter/state.secret.json` (gitignored there) |
| Drill target | **destroyed.** Recreate with `npx cdk deploy CastorDrillTarget --profile otter -c sshCidr=… -c drillTarget=true` when a second host is next needed |

The host costs ~$11/month whether or not it is used. Stop or destroy it if it
will sit idle for a long time.

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

## Not merged

- `p0-07-provisioning-script` @ `aa12a6e` — both halves of its evidence bar are met on
  the real host (HW-8 passed). Its fixture-falsifiability fix had a verification still
  in flight when this was written. **Merge if VERIFIED**; if not, its already-evidenced
  host half still stands and the branch can be merged with the residual recorded.

## Blocked on people, not work

1. **Which Castor import, and its owner (P0-13).** The critical path. Nothing
   else on this list matters until it is answered.
2. **Castor's credential names and values (HW-5).**
3. **A notification channel** — one already exists in `otter.daemon.env` (a Slack
   webhook, gitignored), so HW-4a is runnable; it just needs a deliberate go-ahead
   to post to a real channel.
4. **A dead-man's-switch endpoint for HW-4b** (`OTTER_HEARTBEAT_URL`, a
   healthchecks.io-style URL). **This does not exist** and cannot be invented —
   Slack cannot alert on absence. HW-4b cannot run without it.
5. **A second host for HW-7** — destroyed; recreate from the committed stack in
   about three minutes when needed.

## Next three actions, in order

1. **Ask Castor's job owner** which import, with destination, owner and business
   effect. Everything else is waiting on this.
2. **Merge the branches whose verdicts are VERIFIED** (`p0-07`, `p0-03`, and
   `p0-08` if its gate passed), then push so CI records the Linux-authoritative run.
3. **Run HW-6's retention half** — enabling the three windows and demonstrating
   pruning is actionable today; the alerting half needs v0.3.0's WS4 surface,
   which is not built.

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
