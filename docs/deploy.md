# Deploying Otter to a Host

`otter deploy` installs a running Otter runtime on a Linux machine you already
have, over SSH. It is one command, it is idempotent, and it does not use a
cloud provider API.

That last point is the design, not a shortcut. Otter is a single Go binary, a
SQLite file and one OS process per run attempt. There is nothing to schedule,
nothing to autoscale and no shared state to coordinate, so the only remote
capability the deploy needs is SSH plus rsync — which every VPS, EC2 instance,
Lightsail box and bare-metal machine already has. Provisioning the host (or
choosing the provider) stays your business; installing Otter onto it is this
command's business.

## Run it from your project

`otter deploy` deploys a **project**: the nearest directory at or above your
working directory holding `.otter`, `.git` or `go.mod`. That directory is
scanned recursively for `otter.yaml`, exactly as `otter start` scans it, so
whatever the runtime would serve locally is what the deploy ships.

```text
my-project/                      ← the project root: run otter deploy here
├── otter.env                    shared credentials
├── otter.deploy.yaml            committed, secret-free target
└── shopify_integrations/        any grouping you like
    ├── lib/python/              shared code, declared by the manifests
    ├── customer_sync/
    │   └── otter.yaml           python.path: [../lib/python]
    └── product_sync/
        └── otter.yaml           python.path: [../lib/python]
```

There is no required `integrations/` directory and no requirement that the
project be a Go checkout. Integrations are addressed by the manifest `name:` or
by their directory name, so `--integration product_sync` selects one.

Python is not required at all for integrations that use managed mode. An
integration in external mode runs on the host's own interpreter.

## Workspaces: one host, many projects

A host is a container, not a deployment. Each project you deploy gets its own
**workspace** there, and a deploy only ever converges that workspace:

```text
/opt/otter/
├── workspaces/
│   ├── examples-7f3a91c2/            ← one project
│   │   ├── bin/otterd  bin/otter
│   │   ├── tools/uv/uv
│   │   ├── integrations/<name>/
│   │   ├── workspace.json            host-side record
│   │   └── .otter/data/              its own SQLite, releases, interpreters
│   └── client-sync-1b04de77/         ← another project, untouched by the first
└── ...
/etc/otter/workspaces/examples-7f3a91c2.env      its own secrets
/etc/systemd/system/otterd-examples-7f3a91c2.service
```

Deploying project B does not touch project A's tree, secrets, daemon or data.
Each workspace has its own systemd unit, its own loopback port (7337, 7338, …),
its own API token and its own binaries — so upgrading one never changes what
another executes.

**Identity is what makes "the same project" objective.** A workspace is named
from the project directory plus a short id, and the full id lives in the
committed, secret-free `otter.deploy.yaml`:

```yaml
# otter.deploy.yaml — commit this
workspace: 7f3a91c2-9c4e-4b1f-8a55-2d6c1f0e9b73
slug: examples
```

With it committed, a second machine, a fresh clone or a renamed directory all
deploy into the same workspace. Without it the id comes from this checkout's
`.otter/deploy.json`, so a clone deploys into a *new* workspace — the deploy
says which one it created and prints the line to commit. `--workspace <name>`
adopts an existing workspace by name (or creates one under that name).

## Quick start

```sh
# Any host you can ssh to. A $4-6/month VPS is plenty, and Graviton (arm64)
# costs less than x86 for the same work.
ssh root@203.0.113.10 'echo ok'

# From the project root: install or update this project's workspace, shipping
# every integration found beneath it and the shared trees their manifests declare.
otter deploy --host root@203.0.113.10

# What does this host hold? Lists every workspace and its port.
otter deploy --host root@203.0.113.10 --status

# See the plan without touching anything.
otter deploy --host root@203.0.113.10 --dry-run

# Reach the remote API through a tunnel, exactly like a local daemon.
ssh -N -L 7337:127.0.0.1:7337 root@203.0.113.10 &
otter integrations
otter runs --limit 10
```

## Host requirements

A Linux distribution with systemd and these commands: `rsync`, `find`,
`curl`, `install`, `chown`, `ln`, and the account tools (`getent`, `groupadd`,
`useradd`). A non-root login also needs `sudo` and `runuser`.

They are not universally present. **Amazon Linux 2023 ships without `rsync` and
`find`**, so install them first:

```sh
# Amazon Linux / RHEL / Rocky
sudo dnf install -y rsync findutils

# Debian / Ubuntu
sudo apt-get install -y rsync findutils
```

`otter deploy` checks for every required command before it builds or pushes
anything, and names what is missing:

```
otter: the host is missing tools this deploy needs: rsync find
otter: install them and retry, for example:
  Amazon Linux:   dnf install -y rsync findutils sudo
```

A non-root login needs passwordless `sudo`, which the standard cloud images
provide. The deploy verifies it immediately rather than hanging on a password
prompt, and it escalates the remote `rsync` too, so a login like `ec2-user` or
`ubuntu` can write into `/opt/otter`.

## Where the executables come from

A project that is not the runtime's own source tree has nothing to compile, so
`otter deploy` fetches the runtime's published release archive for the platform
SSH reported — `otter_<version>_<os>_<arch>.tar.gz`, the same artifact
Homebrew installs — and verifies it against the release's `checksums.txt`
before unpacking it. Fetched archives are cached per version and platform, so
deploying to several hosts or workspaces downloads once.

The choice is automatic and can be overridden:

Both executables and the vendored `uv` live *inside the workspace*, so two
workspaces on a host can run different versions and preparing one never
invalidates another's environments.

| Situation | What happens |
|---|---|
| the project contains `cmd/otterd` and `cmd/otter` | compiled from that source, as a contributor expects |
| the project does not, and this `otter` is a released build | the matching release is fetched and verified |
| the project does not, and this `otter` is a development build whose checkout is still on disk | compiled from that checkout |
| `--source <dir>` | compile from that checkout (implies `--build`) |
| `--build` | always compile from the project; an error if the project is not a checkout |
| `--binaries <dir>` | use `otterd` and `otter` from that directory (offline/air-gapped) |

The third row is what makes a Python workspace deployable by a development
build. A binary reporting `v0.1.13-dirty` has no published release to fetch, and
the project it is deploying has no Go source — but the binary knows where it was
built from, and `make build` puts it in `<checkout>/bin/otter`, so the checkout
one or two levels up is used and the deploy reports which directory it compiled.
An installed `otter` (Homebrew, `/usr/local/bin`) never matches one, so it keeps
using the published archive.

`--binaries` checks each file's format against the target platform, because
pushing a macOS executable to a Linux host otherwise fails there with a bare
`exec format error` that names neither the file nor the fix. It accepts either
the bare name or the platform-suffixed name `make cross` writes, so
`--binaries ./bin` works directly against a checkout's cross builds and picks
the pair matching the host rather than your own platform. A development build
with no checkout and no `--binaries` cannot be fetched; the command names all
three ways out rather than failing with a 404.

`OTTER_RELEASE_BASE_URL` points the fetch at a mirror, and
`OTTER_RELEASE_SKIP_CHECKSUM=1` disables verification (it warns loudly) for a
mirror that publishes only the archives.


## What a deploy actually does

```
        your machine                                     the host
  ┌──────────────────────┐                      ┌────────────────────────┐
  │ ssh <host> uname     │─────────────────────▶│ detect platform        │
  │ build, or fetch the  │                      │ read workspace records │
  │   release for it     │◀─────────────────────│ allocate a free port   │
  │ stage every          │─────────────────────▶│ /opt/otter/workspaces/ │
  │   discovered         │        rsync         │   <workspace>/         │
  │   integration and    │─────────────────────▶│     bin/otter(d)       │
  │   its declared trees │                      │     integrations/<name>│
  │ workspace record     │─────────────────────▶│     workspace.json     │
  │ env files (stdin!)   │─────────────────────▶│ /etc/otter/workspaces/ │
  │                      │                      │   <workspace>.env      │
  │ systemd unit +       │─────────────────────▶│ /etc/systemd/system/   │
  │   restart script     │                      │   otterd-<workspace>   │
  │ poll /health over    │◀─────────────────────│ daemon restarted       │
  │   ssh                │                      │                        │
  │ write .otter/        │                      │  data/ untouched       │
  └──────────────────────┘                      └────────────────────────┘
```

In order:

1. **Detect** the remote `GOOS`/`GOARCH` with `uname`. A local guess is never
   trusted, because a binary for the wrong architecture fails on the host with
   a bare `exec format error`.

2. **Resolve the workspace**: read the host's records and either adopt the one
   this project owns or take the next free loopback port for a new one. Nothing
   is written yet, so `--dry-run` reports the workspace it would create.
3. **Obtain** `otterd` and `otter` for that platform: compiled from the
   project's own source when it is a Go checkout, otherwise fetched from the
   matching published release and verified against its checksums. Either way the
   binaries are static — no cgo — because the SQLite driver is pure Go — and
   they live inside the workspace, so another workspace's runtime is untouched.
4. **Stage** a copy of every discovered integration and every shared tree its
   manifest declares, in a temporary directory. Tests, `__pycache__`, `.env`
   files and databases are left out.
5. **Push** the staged tree with `rsync`. The sources converge with `--delete`,
   so a deleted integration or mapping file actually disappears — and the sweep
   is scoped to this workspace, so it can never reach another one. The binary
   tree is pushed separately so the running daemon's executable is never the
   target of a partial write.
6. **Record** the workspace on the host (`workspace.json`), then write
   credentials — `/etc/otter/workspaces/<workspace>.env`, mode `0600`. The
   contents travel over SSH **stdin**, never in a command line: `argv` is
   visible to every process on the host for the lifetime of the call.
7. **Release** every integration in the workspace: stage an immutable snapshot,
   validate the snapshot's own manifest, prepare the environment when the
   manifest asks for managed Python, and activate it. A run executes the active
   release, so an unreleased integration would deploy and then refuse to run. A
   failure here still leaves the previous release active, which is why this
   happens before the restart.

   Every integration lands at `<workspace>/integrations/<name>`, and each shared
   tree lands at the relative depth its manifest declares from there. A manifest
   saying `python.path: [../lib/python]` therefore puts shared code at
   `<workspace>/integrations/lib/python`, while one saying
   `[../../lib/python]` — the layout of the runtime repository itself — puts it
   beside the workspace's `integrations/`. In both cases the shipped manifest is unmodified
   and its relative path resolves verbatim, because placement preserves the
   geometry the declaration depends on rather than assuming one repository's
   shape. Each integration is released by name, so its failure is reported
   against its own name in the deploy log. Deploying a newer Otter also
   re-releases every integration, which is required after an upgrade: the
   release digest format is versioned and an older snapshot is never reused. Old
   snapshots stay in the host's data directory until retention prunes them, so a
   rollback across the upgrade still works.
8. **Install and restart** the workspace's systemd unit, then poll the health endpoint on
   the host itself. The API stays bound to loopback the whole time. This is the
   first step that changes anything the running daemon depends on, and it is
   deliberately last: everything before it is reversible, and a failure there
   leaves the previous deployment serving.
9. **Remember** what happened in `.otter/deploy.json` and
   `.otter/state.secret.json`, keyed by host, so a later deploy of this project
   knows which workspace it owns there.

The same script also takes ownership of `bin/`, `integrations/` and the staged
shared trees for the service account — rsync pushes as the SSH login, so the
files must be handed over before the daemon restarts. The data directory is
never part of that: it is already owned correctly, and recursively chowning a
live SQLite database on every deploy would be pointless and risky.

If any step fails, the ones after it do not run, and no state file is written:
a failed deploy never claims success.

## Daemon-wide settings

Settings that belong to the daemon rather than to one integration — a failure
notification endpoint, the log level — live in `otter.daemon.env` at the
project root:

```sh
# otter.daemon.env   (gitignored: a notification URL carries its own token)
OTTER_NOTIFY_URL=https://hooks.slack.com/services/T.../B.../xxxx
OTTER_NOTIFY_ON=failed,timed_out
```

The same file configures both places, so local and deployed behaviour cannot
drift:

| | How it is read |
|---|---|
| `otter start` | loaded before the daemon starts |
| `otter deploy` | uploaded to `/etc/otter/workspaces/<workspace>.daemon.env`, loaded by the unit |

```
EnvironmentFile=-/etc/otter/workspaces/<workspace>.daemon.env   ← daemon settings
EnvironmentFile=-/etc/otter/workspaces/<workspace>.env          ← credentials
```

The leading dash on both is deliberate: a project without either deploys and
starts normally. Order matters — systemd applies a later `EnvironmentFile` over
an earlier one, so daemon settings load first and credentials second.

### Notification formats

Chat services reject a body that is not shaped for them, so the format is
explicit. Otter's own JSON is the default and carries every field:

| `OTTER_NOTIFY_FORMAT` | Body | Works with |
|---|---|---|
| `json` (default) | every field, machine-readable | your own endpoint, healthchecks.io, an n8n/Zapier bridge |
| `slack` | `{"text": "..."}` | a Slack incoming webhook |
| `discord` | `{"content": "..."}` | a Discord webhook |
| `teams` | MessageCard | a Teams incoming webhook |

A Slack message looks like this:

```
:red_circle: *customer_sync* failed (attempt 3)
sync finished {"failed":12,"written":88}
> process exited with code 1: RuntimeError: destination rejected the batch
_run 42e84cd5-bd9 · 1.84s · release 3c850cfa6c9c_
```

The second line is the integration's own final log line, which is usually the
actionable part: *12 failed, 88 written* rather than just "exit code 1".

**A Teams caveat.** Microsoft has been moving new webhook URLs to Power Automate
Workflows, whose body is an Adaptive Card wrapper rather than a MessageCard.
`teams` emits the MessageCard, which is what an "Incoming Webhook" connector
expects. If you created your webhook through Workflows, use `json` and a bridge
instead -- Otter does not guess at the newer shape.

**Why formats are in the runtime and not a template.** A named format is
testable against the contract the service publishes: the tests assert Slack's
body has `text`, Discord's has `content`, and Teams' is a `MessageCard`. A
template supplied in `.env` could not be checked at all, and an alerting path
that fails silently fails exactly when it is needed.

### Why not otter.yaml

`otter.yaml` is committed, and `otter inspect` prints its `env:` block. A
notification URL embeds a credential in its path, so it cannot live there. It is
also per-integration, while notification is a property of the daemon.

### Why not the shared credentials file

That file is documented as credentials, so a daemon setting placed there reads
as though it were a secret. `SSL_CERT_FILE` ended up in a per-integration
credentials file by accident, which is exactly this failure mode.

## Secrets

Credentials live in `otter.env` at the project root, shared by every
integration **in this workspace**. At deploy it becomes
`/etc/otter/workspaces/<workspace>.env`, owned by root with mode `0600`, loaded
by that workspace's unit.

Per workspace, not per integration: the daemon's environment is a single process
environment and an integration receives only the keys its manifest declares, so
per-integration files isolated nothing. But it is per *workspace*, because each
workspace is a separate daemon — which is what lets two projects on one host use
the same key name (`SHOPIFY_CLIENT_ID`) with different values.

```sh
cp my-integration/.env.example otter.env
$EDITOR otter.env
otter deploy --host droplet
```

**One file per workspace, not one per integration.** The daemon's environment is
a single process environment — every `EnvironmentFile=` is merged into it — and
an integration receives only the keys its own manifest declares. So a
per-integration file isolated nothing: it just turned one rotated credential
into an N-file edit and let those copies drift apart. Separate workspaces are
separate daemons, so their credentials never meet.

What remains per-integration is the *declaration*: `secrets:` in `otter.yaml`
lists what that integration needs, which is what lets the daemon refuse to start
it when a credential is absent. Storage is shared; requirements are not.

The file format is the boring subset systemd itself supports — `KEY=value`,
one per line, optional quotes, `#` comments. Anything more exotic is rejected
rather than guessed at, because a misread credential is worse than a loud
failure. To point at a different shared file:

```sh
otter deploy --host droplet --env-file ~/.otter/shopify-prod.env
```

`otter deploy` warns about any variable listed in a manifest's `secrets:` that
it could not find, naming every integration that needs it, but it still deploys:
the daemon reports missing secrets far more clearly than the deploy command can,
and refusing to deploy would make it impossible to ship a fix for exactly that
problem.

### An integration that needs a different value

Two integrations talking to two stores share key *names*
(`SHOPIFY_CLIENT_ID`) but not values. Give each credential a distinct name in
`otter.env` and bind it in the manifest, which expands `${VAR}` from the shared
file:

```yaml
# shopify_integrations/customer_sync/otter.yaml
env:
  SHOPIFY_CLIENT_ID: ${ORDERS_STORE_CLIENT_ID}
  SHOPIFY_CLIENT_SECRET: ${ORDERS_STORE_CLIENT_SECRET}
```

The difference is then part of the artifact — committed, visible in
`otter inspect`, and carried in the release — rather than a file whose contents
you have to go and find. `otter inspect` shows the template, not the expanded
value, so the credential itself stays out of the output.

## The API token

The first deploy generates a 256-bit token, installs it, and prints it once:

```
API token (store it now; it is also in .otter/state.secret.json):
  export OTTER_API_TOKEN=…
```

Later deploys reuse it, in this order:

1. the token stored for this host and workspace in `.otter/state.secret.json`;
2. `--api-token`, if given;
3. the token already installed in this workspace's environment file (so
   deploying from a second machine, or after deleting `.otter/`, does not lock
   you out);
4. a freshly generated one.

A token belongs to one workspace's daemon, so `.otter/state.secret.json` keeps
one per host and workspace. Two projects on one host get two tokens and two
ports: a client for one has no access to the other.

`--rotate-token` forces a new one and prints it. Regenerating on every deploy
would break every client the operator already has, which is why it is opt-in.
`--dry-run` may generate a candidate token and a candidate platform, but it
writes neither: the state file is only touched by a deploy that succeeded.

## Talking to the remote daemon

The deployed API binds `127.0.0.1:7337` — loopback only. There is no open port,
no firewall rule, no reverse proxy and no TLS certificate to provision or
renew. Reach it with a tunnel:

```sh
ssh -N -L 7337:127.0.0.1:7337 droplet    # foreground, Ctrl-C to stop
export OTTER_API_TOKEN=$(python3 -c 'import json;print(json.load(open(".otter/state.secret.json"))["api_token"])')
otter integrations
otter runs --limit 10
otter logs $(otter run customer_sync) --follow
```

`--listen` can move the daemon off loopback if you genuinely need it, but a
wildcard address (`:7337`, `0.0.0.0:7337`) is refused outright: a static bearer
token on a public port that can execute arbitrary Python is not a deployment
this tool will set up for you.

## Configuration

Everything can come from flags. Anything you would rather not retype can go in
the committed, secret-free `otter.deploy.yaml`, or simply be remembered from
the last deploy — a bare `otter deploy` after the first one goes to the same
host.

```yaml
# otter.deploy.yaml -- committed, no secrets
host: droplet              # or root@203.0.113.10, or an ~/.ssh/config alias
remote_dir: /opt/otter
service_user: otter
listen: 127.0.0.1:7337
```

Precedence, lowest to highest: built-in defaults, `otter.deploy.yaml`, the
previous successful deploy, then the command line.

| Flag | Meaning | Default |
| --- | --- | --- |
| `--host` | SSH destination | required |
| `--user`, `--port`, `--identity` | SSH login, port, key | from `--host`, `22` |
| `--workspace` | workspace to deploy into, by name | this project's own |
| `--remote-dir` | install root holding every workspace | `/opt/otter` |
| `--data-dir` | SQLite and extracted SDK | `<workspace>/.otter/data` |
| `--service` | systemd unit name | `otterd-<workspace>` |
| `--service-user` | service account | `otter` |
| `--listen` | remote API address | first free port from `127.0.0.1:7337` |
| `--platform` | `GOOS/GOARCH`, skips detection | detected over SSH |
| `--build` | compile from the project's Go source, never fetch | off |
| `--source` | Otter checkout to compile from (implies `--build`) | the project |
| `--binaries` | directory holding `otterd` and `otter` for the target | — |
| `--env-file` | shared credentials file | `otter.env` |
| `--api-token`, `--rotate-token` | token handling | stored token |
| `--dry-run` | print the plan, change nothing | off |
| `--verbose` | stream every remote command | off |
| `--timeout` | overall bound | `10m` |
| `--status` | show this project's deploys, and with `--host` what that host holds | — |
| `--destroy`, `--keep-data`, `--yes` | removal | — |

## Deploying one integration

`otter deploy` ships every integration it discovers under the project. To ship
just one and leave the rest of the host alone:

```sh
otter deploy --host droplet --integration customer_sync
```

The name may be the directory or the manifest `name:`. The other integration
directories on the host are protected from the converging sync, so they keep
running exactly as they were, and their secrets files are not rewritten. So are
the shared trees this deploy does not carry: otherwise shipping one integration
would delete a library the others import.

This is the deploy to use when several integrations share a host and you only
changed one.

## Upgrades

A deploy is the upgrade. There is no migration step, no drain window and no
version pinning:

```sh
git pull
otter deploy --host droplet
```

The binary is replaced, systemd restarts the unit, and the daemon's own crash
recovery re-queues whatever was in flight. The data directory is never read,
written or deleted by a deploy, so run history, sync watermarks and the
extracted Python SDK survive. Rollback is the same command against the previous
commit.

Two consequences worth knowing:

- A run that was killed by the restart is recorded as `failed` and retried, not
  `cancelled`. `cancelled` is reserved for an operator cancelling a run, so a
  deploy cannot silently drop work.
- If the new binary cannot start, `otter deploy` fails and prints the last 25
  journal lines from the unit instead of leaving you with a silent dead
  service.

## Removing a deployment

`--destroy` removes **one workspace**: its unit, its two environment files, its
directory and (unless `--keep-data`) its data. Every other workspace on the host
keeps running.

```sh
otter deploy --host droplet --destroy --keep-data   # keep this workspace's data
otter deploy --host droplet --destroy               # also delete it
otter deploy --host droplet --destroy --workspace analytics   # a different one
```

Without `--keep-data` the command names the data directory, explains what it
holds, and asks you to type the host name before doing anything. Deleting it
discards every integration's watermark, which means the next deploy rescans the
source system from scratch — expensive at Shopify and rude at Salesforce.
`otter deploy --destroy --keep-data` keeps the data; `otter deploy --destroy`
removes it.

## Day-to-day operations

```sh
otter deploy --host droplet              # converge this workspace
otter deploy --host droplet --dry-run    # what would change, touching nothing
otter deploy --status                    # this project's deploys, per host
otter deploy --host droplet --status     # every workspace that host holds

ssh -N -L 7338:127.0.0.1:7338 droplet    # forward one workspace's API
otter --api http://127.0.0.1:7338 runs --limit 10
```

On the host itself:

```sh
systemctl status otterd-<workspace>
journalctl -u otterd-<workspace> -f
ls -l /etc/otter/workspaces/       # one secrets file per workspace
/opt/otter/workspaces/<workspace>/bin/otter --data /opt/otter/workspaces/<workspace>/.otter/data status
```

## Backups

Worth doing on day one, because the data directory is the only state Otter has.
A copy of a stopped database is valid — the daemon checkpoints the WAL on
shutdown — so the simplest safe backup is a service stop, a copy, and a start:

```sh
systemctl stop otterd-<workspace>
cp /opt/otter/workspaces/<workspace>/.otter/data/otter.db /var/backups/otter-$(date +%F).db
systemctl start otterd-<workspace>
```

Run it from cron nightly and ship the file off the host. It is a handful of
megabytes; object storage costs cents.

## The rule a deploy must not break

> Otter should remain a small runtime, not evolve into an integration platform
> inside the daemon.

`otter deploy` pushes a binary and starts a service. It does not provision
machines, manage DNS, terminate TLS, mount volumes, join clusters or talk to a
cloud API. If a future provider backend is added, it should end by producing the
same thing: a host you can ssh to, with systemd, running `otterd`.

## Integration identity on the host

The destination registers its own integration identities. Local and remote ids
are independent and need not match, and nothing about a local `.otter-id` travels
with a deploy: the file is excluded from the staged tree, and excluded and
protected in the rsync step so `--delete` cannot remove the marker the host
already has. Each integration is released by its destination path, so a
directory whose name differs from its manifest label is still resolved
correctly.

Each workspace has its own registry, so two projects on one host may both have
an integration named `counter` without colliding -- they are different
directories, different identities and different run histories.

`otter deploy` records the destination identity of every deployed integration in
`.otter/deploy.json` (keyed by host) and prints them from `otter deploy --status`:

```
destination identities:
  counter              c1f0d3a4-6e2b-4b0e-9d21-7a5f8c2e1b90
```

The deployed tree must be writable by the runtime user, because registering a
new integration writes its `.otter-id` marker. See [identity.md](identity.md).
