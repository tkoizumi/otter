# Compatibility and deprecation policy

Status: published for `v0.4.0`. Date: 2026-10-03. This is the written half of
the `v0.4.0` interface freeze (`R4` of
[v0.4.0-and-cloud-phase-b-plan.md](v0.4.0-and-cloud-phase-b-plan.md)). It says
which interfaces are frozen, what counts as a change to each, and how a change
is announced and retired.

## What is frozen

Four interfaces cross the runtime's boundary and carry a compatibility promise.
Everything inside the boundary does not.

| Interface | Where it is specified | Version carrier |
| --- | --- | --- |
| **Manifest schema** | [manifest-reference.md](manifest-reference.md) | `version:` in `otter.yaml` (`1`) |
| **Python SDK** | [managed-python.md](managed-python.md), `sdk/python` | `sdk.Version` (semver) |
| **CLI JSON** | this document, `--json` output | `schema_version` in every object document |
| **HTTP API** | [api-reference.md](api-reference.md) | `schema_version`, `GET /v1/version` |

The runtime contract itself — the promises and non-promises in
[runtime-contract.md](runtime-contract.md) — is versioned separately as
`contract_version`. A change to a promise or a non-promise increments it,
including a narrowing.

A client learns all of these from one request:

```bash
otter --api http://127.0.0.1:7337 --json version
curl -s http://127.0.0.1:7337/v1/version
```

```json
{
  "schema_version": 1,
  "contract_version": 1,
  "product_version": "0.4.0",
  "manifest_schema": 1,
  "sdk_version": "0.1.0",
  "supported_platforms": ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
}
```

`GET /v1/version` and `GET /health` are public, so a client can establish what
shape and schema it is talking to before it holds a credential.

## What counts as a change

### Product version (semver)

Otter releases `otter` and `otterd` together from one archive. The binary pair
is the unit: do not mix versions. Within a **major**:

- **Patch** (`0.4.1`): bug fixes only. No interface change, no contract change.
- **Minor** (`0.5.0`): additive change to a frozen interface; a documented
  contract amendment; a new optional manifest key; a new endpoint. Removing or
  changing the meaning of anything requires a major.

### Manifest schema

- **Additive within a major.** A new optional key is a minor release. A key that
  changes a job's *behaviour* when present is still additive as long as its
  absence means "the previous behaviour".
- **Strict parsing is part of the contract.** An unknown key is a validation
  error, not a silent ignore, so a manifest written against a newer schema fails
  loudly on an older runtime rather than running with different meaning.
- **Breaking** (major only): removing a key, changing a default, changing a
  key's type, or requiring a key that was optional. Bumping `version:` is the
  signal; `config.SupportedVersion` is the only value the runtime accepts.

### Python SDK

- The SDK is versioned independently and embedded in the daemon; the daemon
  reports the embedded version. A minor SDK release may add helpers. Removing a
  helper, changing a signature, or changing `ctx` semantics is a major SDK
  release.
- The SDK is delivered *to the child process*, not installed by the user, so a
  job always runs the SDK from the daemon that released it.

### CLI JSON

- The JSON a command prints with `--json` is an interface. Human-readable output
  is not: it may change in any release.
- **Additive within a major**: a new field, a new document, a new command. A
  consumer must ignore fields it does not know.
- **Breaking** (major only): removing a field, renaming one, changing a type, or
  changing what a value means. Object documents carry `schema_version`; its
  value changes exactly when a breaking change lands. Commands that print a
  list (`otter jobs --json`, `otter runs --json`) print the payload array, as
  the HTTP list endpoints do; `otter --json version` is where the version is
  read.

### HTTP API

- Same rule as CLI JSON. In addition:
  - A new endpoint, a new optional request field, or a new response field is a
    minor release.
  - A removed endpoint, a changed status code, a changed error `code`, or a
    changed response shape is a major release.
  - Error responses use one envelope (`{"schema_version", "error":{"code",
    "message"}}`); a new `code` is additive, a renamed one is breaking.
- Routers are unversioned in the URL (`/v1/...`); the version is negotiated
  through `schema_version` rather than a path segment, so an additive change
  does not require a second route tree.
- **Object documents carry `schema_version`; list endpoints return the payload
  array itself** (`GET /v1/jobs`, `GET /v1/runs`). A consumer that needs the
  version reads one object document — `GET /v1/version` or `GET /health` — once,
  rather than paying a version field on every element. Wrapping the list
  responses is a breaking change and is therefore deferred to a major release
  if it is ever wanted; until then the array shape is the frozen shape.

## Deprecation

A deprecated interface keeps working for at least **one minor release**, and is
announced in all three of:

1. this document, in the table below;
2. the `CHANGELOG.md` entry for the release that deprecates it;
3. the runtime itself — a log line at startup or first use, or an HTTP
   `Deprecation` header where one is cheap.

Removal happens only in a major release. Nothing in this table is removed in
`v0.4.x`.

| Deprecated in | Interface | Replacement | Removal |
| --- | --- | --- | --- |
| `v0.4.0` | `PUT`/`DELETE /v1/jobs/{id}/schedule` | `POST /v1/jobs/{id}/schedules` and the `/v1/schedules/{id}` family | next major |
| `v0.4.0` | `otter schedule set` / `otter schedule clear` | `otter schedule add` / `update` / `remove` | next major |
| `v0.4.0` | `trigger.cron` interpreted in the host's local time | `trigger.cron` is UTC; create a schedule with `--timezone` for a local-time meaning | already changed — see below |

The `trigger.cron` change is not a deprecation but a **documented breaking
change** that could not be deferred: the old meaning was "whatever the host's
clock was configured to", which is not a contract. It shipped in `v0.4.0`, a
minor release, because no configured deployment existed outside the Phase 0
hosts at the time; the change and its exact meaning are in
[runtime-contract.md §3.1.1](runtime-contract.md#311-one-run-per-occurrence-and-which-clock-decides)
and [manifest-reference.md](manifest-reference.md#cron).

## Platforms

The supported OS/architecture contract is
[runtime-contract.md §6](runtime-contract.md#6-supported-platforms):
`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`. Other Unix targets
may compile but carry **no** guarantee. A contract test pins the code's list to
that document, so the two cannot drift.

## What is not frozen

- Internal Go packages and their APIs.
- Log line wording, log formats and event field names, except where a monitoring
  contract names them (`monitoring-contract.json` is the exception and is
  itself versioned by test).
- Human-readable CLI output.
- Database schema internals; the *file* is opaque except for backup/restore, and
  a database migrated by a newer binary is refused rather than downgraded
  ([runtime-contract.md](runtime-contract.md#7-honest-limits)).
- The shape of capture payloads, which follow the captured protocol, not Otter.

## Freeze point

The four interfaces above are frozen at `v0.4.0` and are expected to remain
stable to `v1.0.0`. `contract_version` is **1** and `schema_version` is **1**.
Before `v1.0.0`, a minor release may still amend the runtime contract; it may
not silently change a frozen shape. From `v1.0.0`, contract version increments
follow the runtime-contract rules and a breaking interface change is a major
product release.
