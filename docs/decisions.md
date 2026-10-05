# Decisions

Status: living record. Started 2026-10-01.

This log records implementation decisions — the ones that change a design or a
runtime contract and would otherwise be re-derived next session. Roadmap and
sequencing decisions are recorded in
[product-roadmap.md](product-roadmap.md#recorded-decisions); the design docs carry
the detail.

Format: date, the decision, why, and what it changes. If a design doc and this log
disagree, this log wins until it is updated.

## 2026-10-01 — Self-hosted runtime API, UTC schedules

**Committed path.** The runtime API is the product surface for a self-hosted,
single-tenant deployment. `CL-22` (dynamic schedules) and `CL-23` (a reachable,
scoped API) are pulled forward of the Phase B gate. The roadmap entry is
[product-roadmap.md](product-roadmap.md#recorded-decisions).

| Decision | Answer | Consequence |
| --- | --- | --- |
| Reachability | **The operator decides how the runtime API is reached.** Otter Cloud is another client of the same API, with no privileged path. | `CL-04`'s outbound channel becomes one deployment choice rather than the only model. Both modes speak one API contract, so nothing needs a second surface. |
| Supported remote default | Private network + scoped token | `otter deploy` stops refusing a non-loopback bind; `assert-host-permissions.sh` must assert configured posture, so `P0-07`'s recorded host evidence is **re-run**, not amended. |
| Track order | Self-hosted first; Cloud as a later wrapper over the same API | `CL-22` and `CL-23` belong to the Phase 2 "Operable runtime" track, where `v0.4.0` already promises the API stops moving. |
| Schedule time zone | **Pin UTC everywhere** | A behaviour change: `scheduler.New` builds `robfig/cron` without `WithLocation`, so existing manifests currently mean the **host's local time**. The migration documents it; the design carries a per-row `timezone` column for the future. |
| Manifest schedules | One `trigger.cron` per job for the first cut; an `id:`-keyed `schedules:` list later | Keeps reconciliation trivial and the first cut behaviour-identical — that is the parity gate. No schema dependency: `schedules.origin_ref` holds a constant now and an id later, with a one-line backfill. |
| Castor job code | Out of scope for now | The `P0-13` interim shim — tick job, database-as-queue, status mirror — is **cancelled** ([plan](archive/p0-13-castor-import-implementation-plan.md)). The durable Castor fixes (lease, single-active-run index, cancel, shared import core) still stand, because they improve the Lambda path too. |

**Why the reachability answer matters.** Loopback was standing in for
authorization, and treating the platform as privileged would have re-created the
same problem in a different place. One API, one credential model, reachability as
a deployment choice.

**Still open**, tracked in
[dynamic-schedules-design.md](dynamic-schedules-design.md#14-open-questions): token
scope granularity, TLS termination approach, `schedule_fires` retention, and
whether schedule-level pause is exposed.

## 2026-10-03 — A `capture` scope relaxes the R1 narrowing, by name

**Committed path.** The runtime issues a third named scope, `capture`, and the
Cloud inspector reads request and response **bodies** through it. Plan:
`otter-platform/cloud/docs/request-bodies-implementation-plan.md`.

| Decision | Answer | Consequence |
| --- | --- | --- |
| How Cloud reads bodies | **A `capture` scope: read + control + payloads**, minted by name | The widening is explicit, visible in `otter token list`, and independently revocable per runtime. It is a superset of `control` because a control plane needs the command surface and the capture read from one credential. |
| Is `capture` implied by `control`? | **No** | A gateway token minted last week keeps its refusal. Widening `control` would have changed the meaning of every credential already issued. |
| Give Cloud the admin token instead | **No** | Guardrail 1 stays: `OTTER_API_TOKEN` lives in the daemon's environment, not the database, and Cloud never holds one. |
| Headers | **Not displayed, ever, in this plan** | The payload routes still return sanitized headers to a capture credential, but Cloud drops them at the client boundary; no header value reaches the browser. Payload privacy is enforced by two independent layers: redaction before storage on the runtime, and stripping in `http-client.ts`. |

**Why this matters.** R1 deliberately narrowed the scoped credential so a
gateway that commands a runtime could not thereby read the client's traffic.
This entry relaxes that narrowing for an explicitly named scope, and only for a
runtime whose operator mints one. The recorded trade is that a compromise of the
control plane now includes the payloads of the runtimes it is trusted by; the
mitigations are that the credential is per-runtime and revocable on the next
request, and that a two-credential split (option D of the plan) remains the
refinement if a client asks for it.

**Still open**: whether the split into separate control and payload credentials
is worth the machinery (option D of the plan), and whether the body pane should
sit behind an explicit reveal for screens on shared displays.

## 2026-10-05 — Missed occurrences: `coalesce` bounds uptime, `catch_up` is bounded, admission is bounded

**Committed path.** The `missed_policy` semantics for `OT-007` are decided for
both halves of the gap — occurrences that fall during **downtime** and
occurrences that fall while a previous run is still queued or running
(**uptime**). The default is unchanged: `skip` with unbounded admission. The
behaviour lands in `v0.5.0` WS2, in the same release as this record
([v0.5.0-release-plan.md](v0.5.0-release-plan.md#ws2--implement-the-policy-and-bound-admission)).

| Decision | Answer | Alternative (override if) | Consequence |
| --- | --- | --- | --- |
| `coalesce` during uptime | **`coalesce` bounds uptime as well as downtime.** At most one occurrence may be pending per schedule: a new tick while a run is queued or running folds into the pending run instead of adding a second. For a downtime gap, one run fires on the next wake-up standing for every missed occurrence. | `coalesce` should stay the design's narrower reading and collapse only the downtime gap. | `coalesce` becomes the per-schedule answer to the backlog that generates the tickets, instead of buying nothing for the case that generates them. The surviving run records the count and window of the occurrences it absorbed. |
| `catch_up` bound | **Bounded per schedule, default `MaxCatchUp = 100`, oldest first.** Occurrences beyond the cap are skipped and the count is reported, not silently dropped. | A single global bound is enough. | A long outage cannot become a thundering herd. The per-schedule maximum is data (`max_catch_up`, migration `0016`); truncation is counted and logged once with the window. |
| Manifest keys | **`trigger.missed_policy` and `trigger.max_catch_up` are optional, reconciled on reload like `cron` and `payload`.** Their absence means `skip` and the daemon default. | You will migrate manifest schedules to API-owned first. | Every real schedule is manifest-owned and refuses API mutation with `409`, so an API-only policy would be unusable for it. The manifest is the only usable home for a manifest job. |
| `max_queue_depth` | **Optional at the job's top level, read from the live manifest, and governing autonomous admission (cron and webhook) only.** A manual `otter run` is always accepted. | The bound belongs to the daemon, not the job. | It is an operational pressure valve like capture policy, so a reload can relieve it without a release. Refusal is `429` + `overloaded` (override if `503` is reused, or a different code is chosen), counted, timestamped and reported. |
| Default | **`skip` and unbounded admission stay the default.** | You want a safe-by-default bound and will announce the behaviour change. | Absence keeps today's meaning, which is what the compatibility policy requires of an additive minor ([compatibility.md](compatibility.md#manifest-schema)). |

**Why this matters.** `OT-007` conflated two gaps that share one name: a
downtime gap that was never replayed, and an uptime backlog that had no bound at
all. Deciding `coalesce` for downtime only would have left the backlog that
generates the tickets untouched; deciding `catch_up` without a per-schedule cap
would have turned one outage into a herd. The two answers compose with
`max_queue_depth`: `coalesce` bounds a schedule's own backlog, the depth bound
bounds a job's backlog however it was triggered, and `catch_up` is the only
policy that deliberately creates work on wake-up, which is why it is bounded
separately. Every "not a run" outcome is visible — a collapse records its count
and window on the surviving run, a cap-skip records how many were skipped, and a
depth refusal is counted, timestamped and reported — and the occurrence ledger
stays the single source of "this occurrence is done".

**Still open**: the WS2 implementation and migration `0016`; the WS3
observability fields (`refused_total`, `coalesced_total`,
`catch_up_skipped_total`); the manifest reference text for the three keys (WS6);
and whether a daemon-wide queue ceiling is ever needed at all (named out of scope
in WS2).
