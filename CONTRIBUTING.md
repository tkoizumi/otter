# Contributing to Otter

This repository is the **runtime**: the daemon, the CLI, the embedded Python SDK
and the contracts between them. It is not an integration project, and it ships
no example catalog.

That distinction decides most reviews. Code that the runtime *is* (execution,
scheduling, state, identity, releases, deployment) and code an integration
*imports* (the SDK) belong here. Code that *is a deployment* — vendor clients,
mappings, real integrations — belongs in its own project, which installs Otter
and uses it.

## Prerequisites

- **Go** — the version in [`go.mod`](go.mod); CI reads it from there too.
- **Python 3.13** — what CI uses for the SDK suite and the smoke workflow. The
  daemon itself needs a `python3` on the machine it runs integration code on.
- `git`, and `make` for the documented loop.

Nothing else. There are no SaaS credentials, no local services and no network
access in the test suite.

## The development loop

```bash
make build     # ./bin/otterd and ./bin/otter
make test      # Go suite plus the embedded Python SDK
make lint      # gofmt (fails on offenders), go vet, golangci-lint if installed
make smoke     # `otter init` end to end in a temporary workspace
make cross     # cross-compile for Linux and macOS
```

`make help` lists every target. The four that matter:

- **`make test`** runs the Go suite, which starts real daemons and real Python
  child processes, and the SDK suite, which drives the SDK against a fake
  daemon over HTTP.
- **`make lint`** fails on unformatted Go. `gofmt -l` on its own exits 0 while
  listing offenders, so both this target and CI wrap it deliberately.
- **`make smoke`** is the end-to-end contract: it builds this checkout's binary,
  creates a workspace **outside** the repository, and drives `otter init` →
  `validate` → `release` → `start` → `run` → `state` → `stop`. Because the
  canonical sample is `otter init`'s output, the scaffold users get is the
  scaffold this repository tests — which is why no example directories ship.
- **`make cross`** proves the four release targets compile.

Run a single package while iterating:

```bash
go test ./internal/daemon/ -run TestReload -v
go test ./internal/identity/...
```

The race detector is worth using on the concurrency-heavy packages:

```bash
CGO_ENABLED=1 go test -race ./internal/daemon/ ./internal/api/ ./internal/identity/
```

## Where things live

[README.md](README.md#repository-layout) has the tree; [docs/architecture.md](docs/architecture.md)
has the subsystem diagram, the schema and the run lifecycle. The short version:

| Changing | Look at |
| --- | --- |
| Manifest fields, validation, discovery | `internal/config/` |
| Run lifecycle, workers, retries, recovery | `internal/daemon/`, `internal/runs/`, `internal/queue/` |
| Durable identity, markers, reconciliation | `internal/identity/` |
| Releases, environment preparation | `internal/release/`, `internal/pyenv/` |
| HTTP API and the CLI's client | `internal/api/` |
| Commands, including the `otter init` scaffold | `internal/cli/` |
| Remote install and systemd | `internal/deploy/` |
| The Python SDK | `sdk/python/` |
| Schema | `migrations/` |

## Tests and fixtures

- **Prefer temporary directories.** A test that builds the workspace it needs
  under `t.TempDir()` is easier to read than one that depends on a checked-in
  tree, and it cannot rot when the tree moves.
- **Use `testdata/` only when a checked-in fixture is genuinely clearer** — for
  a parser corpus, say. Keep it small, deterministic and credential-free.
- **Fake the network locally.** `httptest` in Go, `http.server` in Python. No
  test may reach a real vendor API.
- **Cover contracts, not applications.** Tests here prove that shared-tree
  layouts resolve, that manifests validate, that a run persists state, and that
  deployment places trees correctly. Business mapping belongs to the project
  that owns it.

## Debugging in a real workspace

The smoke workflow is the automated version of this; doing it by hand is
sometimes easier:

```bash
tmp=$(mktemp -d) && cd "$tmp"
"$OLDPWD/bin/otter" init my-project
cd my-project
"$OLDPWD/bin/otter" release my-project
"$OLDPWD/bin/otter" start --detach
"$OLDPWD/bin/otter" run my-project
"$OLDPWD/bin/otter" status
"$OLDPWD/bin/otter" stop
```

Two things to keep in mind:

- **Use an absolute path to this checkout's binary.** `otter` on your `PATH`
  may be an installed release, which is a different program from the one you
  just built.
- **Keep the workspace outside the repository.** `otter init` writes `.otter/`
  and an integration directory; neither belongs in this tree. Both are
  gitignored as a safety net, not as an invitation.

## Documentation

`docs/` is a product contract, not a scratch pad: `manifest-reference.md`,
`api-reference.md`, `managed-python.md`, `identity.md`, `security.md`,
`operations.md` and `deploy.md` describe what an *installed* Otter does. If a
change alters behaviour, the document is part of the change.

Integration-authoring tutorials do not live here. Keep this repository's prose
about the runtime and point at an installed workflow.

## Packaging and releases

`.goreleaser.yaml` builds two static binaries per platform and publishes them as
`otter_<version>_<os>_<arch>.tar.gz`; `.github/workflows/release.yml` runs the
test job, the smoke workflow, then GoReleaser on a tag. `.github/workflows/test.yml`
is the pull-request gate. Packaging carries the two binaries and the embedded
SDK, and nothing else — no vendor code, no local artifacts, no runtime state.

## What not to send

- Vendor clients, mappings, credentials or a real integration.
- An example directory. If you want the runtime to demonstrate something, extend
  the `otter init` scaffold and `scripts/smoke.sh` — that is how it stays true.
- Generated files, `bin/`, `.otter/`, `run.log`, editor state.
