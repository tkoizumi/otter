# v0.3.0 exit-gate evidence

One file per recorded run, following the phase-0 rules
([../phase-0/README.md](../phase-0/README.md)): what, when, where, which build,
the exact command, the raw output pasted rather than summarised, the exit
status, and a verdict that says what the run does **not** prove.

This directory is the record of the **independent second operator** run for the
v0.3.0 exit gate (`docs/v0.3.0-release-plan.md` §"Exit gate": *a second operator
completes the operating drills without author intervention*). The operator was a
fresh agent session that wrote none of the code, drills, tests or documents
under test, and read only what the repository publishes.

The interpretation is in [2026-10-02-v0.3.0-gate.txt](2026-10-02-v0.3.0-gate.txt);
the captures are the evidence.

## Naming

    YYYY-MM-DD-v0.3.0-<kind>-<name>.txt

* `drill-<name>` — one clean drill run.
* `sabotage-<name>-<mode>` — one `DRILL_SABOTAGE` run, expected to exit non-zero.
* `aggregate-*` — `make drill`, with every drill runnable and with one that is not.
* `step3-*` — the by-hand seeded-failure diagnosis, which no drill covers.
* `docclaims-*`, `backlog-behavior-*`, `contract-*` — the document/behaviour checks.
* `finding-*` — a defect found while checking, with the raw output that shows it.
* `host-only-not-run` — the machine-only items this run could not attempt.

## What does not belong here

* A restatement of what the code does with no command behind it.
* A green run with no mutation behind it — every drill's falsifiability run is
  recorded beside it, or the absence of one is named in the gate record.
* Fixes. Nothing in this directory was produced by editing the repository; the
  working tree is left as the operator found it, apart from this directory.
