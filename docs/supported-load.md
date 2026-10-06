# The supported-load envelope

Status: **measured on a reference-shaped host, 2026-10-05.** One dimension —
CPU saturation — is still open, and says so below.

This document is the home for the `v0.5.0` WS4 evidence: what a reference-shaped
host actually sustains, measured rather than budgeted. The number this runtime
is sized from was previously a budget
([`../../otter-platform/README.md` §Why this size](../../otter-platform/README.md));
the measurement below is what replaces it.

## How it is measured

The workload is committed in [`../../scripts/load-envelope.sh`](../../scripts/load-envelope.sh)
and is deliberately thin, because the question is the *runtime's* envelope and
not a customer's code:

| Dimension | Reference |
| --- | --- |
| Instance | `t4g.micro`: 2 vCPU (burstable), 1 GiB, 20 GiB gp3 |
| OS | Ubuntu 24.04.5 LTS, arm64 (`Linux 7.0.0-1013-aws`) |
| Daemon | `OTTER_WORKERS=1`, cgroup caps at the defaults (`MemoryMax=75%`) |
| Swap | enabled, at or above the 256 MiB floor `operations.md` documents |
| Job | one paginated job, `concurrency: 1`, no local CPU work, ~0.6s per run |
| Storage | `otter.db` plus its `-wal` sidecar; `otter.db` above ~2 GB is the documented alert threshold |

```sh
make build
scripts/load-envelope.sh              # human-readable table
ENVELOPE_JSON=1 scripts/load-envelope.sh   # the same numbers as JSON
```

The script prints the hardware it detected and states whether the host is the
reference shape. **A run on anything else is a development observation and must
not be pasted into the envelope tables below** — that rule is the reason the
script reports the distinction rather than leaving it to whoever runs it.

## What is measured, and why each row matters

| Measurement | The question it answers |
| --- | --- |
| Sustained run rate at `concurrency: 1` | How much work fits through one worker on this host |
| Queue latency against depth | An operator reads a depth and knows the wait |
| Storage growth per run (`otter.db` + WAL) | How long a host lasts before the ~2 GB threshold |
| Peak RSS per concurrent run | How many runtimes could share this host, and at what cap |
| Overload behaviour | What is refused when the bound is reached, and what a drain looks like |

## Envelope — reference host

Measured 2026-10-05 on a dedicated `t4g.micro` provisioned for this purpose
(never the Castor production runtime). Raw JSON:

```json
{
  "reference_host": true,
  "hardware": {"arch": "aarch64", "cores": 2, "mem_mib": 904,
               "os": "Ubuntu 24.04.5 LTS", "kernel": "Linux 7.0.0-1013-aws"},
  "workload": {"runs": 60, "job_seconds": 0.6, "concurrency": 1},
  "sustained": {"elapsed_seconds": 13, "runs_per_second": 4.62, "drained": true},
  "memory": {"peak_rss_kib": 23556},
  "storage": {"db_plus_wal_growth_bytes": 4214136, "bytes_per_run": 70235},
  "latency": {"target_depth": 20, "observed_depth": 19,
              "oldest_waiting_seconds": 0.226968139}
}
```

### Sustained run rate

| Job duration | Runs/second | Runs/hour | Peak RSS (daemon + children) | Source |
| --- | --- | --- | --- | --- |
| ~0.6 s sleep, 3 subprocess steps, `concurrency: 1` | 4.62 | ~16,600 | 23,556 KiB (23 MiB) | measured |

Read this as "the runtime added no measurable throughput ceiling above the
job's own duration on this host", not as a generic capacity figure. The job
spends 3 x 0.2 s asleep across three steps and `concurrency: 1` serializes runs,
so the ceiling here is the job plus process startup, and the measurement says
the runtime did not add a bottleneck on top of it.

**Peak RSS caveat.** The figure is the high-water mark of the daemon and its
process group, sampled every 100 ms while runs were submitted. A Python child
that starts and exits inside a sampling interval can be missed, so 23 MiB is a
lower bound on the peak rather than an exact one. For sizing a shared host the
useful reading is that a *single* runtime under load sat at ~23 MiB on a 1 GiB
host, and the daemon idle is ~3 MiB of that -- so the per-runtime floor is
dominated by whatever the jobs themselves import, not by `otterd`.

This is the row that bounds pooling density: at ~23 MiB per loaded runtime, a
1 GiB host is not memory-bound at two or three tenants; disk and CPU would
constrain it first. That is a measurement about *this* workload, and a customer
importing a large dependency tree will move it.

### Queue latency against depth

| Depth | `queue.oldest_waiting_seconds` | Notes |
| --- | --- | --- |
| 19 | 0.227 | Sampled immediately after submission, before the pool caught up |

At this depth the oldest claimable run had waited under a quarter of a second,
so on this host a depth in the low tens is a scheduling blip rather than a
backlog an operator needs to act on.

### Storage growth

| Retention | `otter.db` + WAL bytes/run | Runs before the ~2 GB threshold |
| --- | --- | --- |
| capture `off` | 70,235 (~69 KiB) | ~30,500 runs (~1.8 hours at 4.62/s) |

**This is the envelope's first real constraint, and it is not memory.** For a
job doing nothing but sleeping and logging, the runtime writes ~69 KiB per run
once the write-ahead log is counted. At the documented ~2 GB alert threshold
that is roughly 30,500 runs, or under two hours at this rate. A high-frequency
schedule would need retention configured before the host survived a working day;
the row above is measured with capture `off`, so `metadata` or `full` capture
will be worse and is still unmeasured.

The `otter.db`-only figure is **zero** over this window: SQLite writes to its
`-wal` sidecar and only folds it back on checkpoint, so any measurement that
reads the database file alone reports no growth at all. That mistake was made
and fixed in the script before this run.

### Overload

| Bound | What is refused | What is counted | Drain behaviour |
| --- | --- | --- | --- |
| `max_queue_depth` unset | nothing: the host is the limit | — | — |
| `max_queue_depth: N` | cron and webhook triggers | `admission_refusals` | — |

The overload row is asserted by the `scripts/drill/overload.sh` drill, so
"overload behaves as documented" is a test result rather than a table entry.

## Development observations

Figures recorded outside the reference shape. They are kept because they are
useful — a regression, a shape difference, a sanity check — and they are
**labelled as observations** for the same reason the envelope rule exists: a
number measured on different hardware is not an envelope.

### 2026-10-05 — this workstation (16 cores, 61 GiB, x86_64, Ubuntu 26.04)

Run with `ENVELOPE_RUNS=8 ENVELOPE_JOB_SECS=0.3 ENVELOPE_DEPTH=4`, on a host
~60× the reference memory and 8× its cores. The script classified it
`reference shape: no`.

| Measurement | Value | Why it is not an envelope |
| --- | --- | --- |
| Sustained run rate | ~2.7/s | Reflects 16 cores, not 2 burstable vCPUs |
| Peak daemon+child RSS | not captured — see below | — |
| `otter.db` + WAL growth | 0 bytes over 8 runs | Too few runs to resolve growth, and the size is dominated by page allocation rather than per-run volume |
| Queue latency | depth never built | The workload drained as fast as it was submitted |

Two defects in the script were found by this run and fixed before the reference
run: it measured `otter.db` alone and so reported zero growth while SQLite was
writing to its `-wal` sidecar, and it drove the workload through `otter run`,
which blocks until a run finishes, so no queue depth ever existed to measure.
Both are recorded here because the reference tables must not inherit either
mistake.
