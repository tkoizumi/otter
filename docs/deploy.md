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

## Quick start

```sh
# Any host you can ssh to. A $4-6/month VPS is plenty, and Graviton (arm64)
# costs less than x86 for the same work.
ssh root@203.0.113.10 'echo ok'

# Install or update the runtime, shipping ./integrations and ./lib/python.
otter deploy --host root@203.0.113.10

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

Python is not required at all for integrations that use managed mode. An
integration in external mode runs on the host's own interpreter.

## What a deploy actually does

```
        your machine                                     the host
  ┌──────────────────────┐                      ┌────────────────────────┐
  │ ssh <host> uname     │─────────────────────▶│ detect platform        │
  │ go build             │                      │                        │
  │   GOOS/GOARCH        │                      │                        │
  │ stage integrations/  │─────────────────────▶│ /opt/otter/            │
  │   and lib/python     │        rsync         │   bin/otter(d)         │
  │                      │─────────────────────▶│   integrations/        │
  │                      │                      │   lib/python/          │
  │ env files (stdin!)   │─────────────────────▶│ /etc/otter/<name>.env  │
  │                      │                      │   (0600, root)         │
  │ systemd unit +       │─────────────────────▶│ /etc/systemd/system/   │
  │   restart script     │                      │   otterd.service       │
  │ poll /health over    │◀─────────────────────│ daemon restarted       │
  │   ssh                │                      │                        │
  │ write .otter/        │                      │  data/ untouched       │
  └──────────────────────┘                      └────────────────────────┘
```

In order:

1. **Detect** the remote `GOOS`/`GOARCH` with `uname`. A local guess is never
   trusted, because building for the wrong architecture fails on the host with
   a bare `exec format error`.
2. **Build** `otterd` and `otter` locally for that platform, with the same
   `-trimpath -ldflags "-s -w -X main.version=…"` the Makefile uses. No cgo, so
   the build works from any machine and the binaries are static.
3. **Stage** a copy of `integrations/` and `lib/python/` in a temporary
   directory. Tests, `__pycache__`, `.env` files and databases are left out.
4. **Push** both trees with `rsync`. The sources converge with `--delete`, so a
   deleted integration or mapping file actually disappears; the binary tree is
   pushed separately so the running daemon's executable is never the target of
   a partial write.
5. **Write credentials** — `/etc/otter/shared.env`, mode `0600`, shared by
   every integration. The contents travel over SSH **stdin**, never in a command
   line: `argv` is visible to every process on the host for the lifetime of the
   call.
6. **Release** every integration on the host: stage an immutable snapshot,
   validate the snapshot's own manifest, prepare the environment when the
   manifest asks for managed Python, and activate it. A run executes the active
   release, so an unreleased integration would deploy and then refuse to run. A
   failure here still leaves the previous release active, which is why this
   happens before the restart.

   The release runs with `--integrations /opt/otter/integrations` while shared
   code lives at `/opt/otter/lib/python`, so the release base is `/opt/otter`
   and the snapshot places the integration at `integrations/<name>` with the
   tree at `lib/python`. That is what lets the shipped manifest keep
   `python.path: [../../lib/python]` verbatim. Each integration is released by
   name, so its failure is reported against its own name in the deploy log.
   Deploying a newer Otter also re-releases every integration, which is required
   after an upgrade: the release digest format is versioned and an older
   snapshot is never reused. Old snapshots stay in the host's data directory
   until retention prunes them, so a rollback across the upgrade still works.
7. **Install and restart** the systemd unit, then poll the health endpoint on
   the host itself. The API stays bound to loopback the whole time. This is the
   first step that changes anything the running daemon depends on, and it is
   deliberately last: everything before it is reversible, and a failure there
   leaves the previous deployment serving.
8. **Record** what happened in `.otter/deploy.json` and `.otter/state.secret.json`.

The same script also takes ownership of `bin/`, `integrations/` and `lib/` for
the service account — rsync pushes as the SSH login, so the files must be
handed over before the daemon restarts. The data directory is never part of
that: it is already owned correctly, and recursively chowning a live SQLite
database on every deploy would be pointless and risky.

If any step fails, the ones after it do not run, and no state file is written:
a failed deploy never claims success.

## Daemon-wide settings

Settings that belong to the daemon rather than to one integration — a failure
notification endpoint, the log level — live in `otter.daemon.env` at the
repository root:

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
| `otter deploy` | uploaded to `/etc/otter/daemon.env`, loaded by the unit |

```
EnvironmentFile=-/etc/otter/daemon.env    ← optional; daemon settings
EnvironmentFile=-/etc/otter/shared.env    ← optional; shared credentials
```

The leading dash on both is deliberate: a checkout without either deploys and
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
:red_circle: *shopify-to-salesforce* failed (attempt 3)
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

Credentials live in `otter.env` at the repository root, shared by every
integration. At deploy it becomes `/etc/otter/shared.env`, owned by root with
mode `0600`, loaded by systemd's `EnvironmentFile=`.

```sh
cp integrations/shopify-to-salesforce/.env.example otter.env
$EDITOR otter.env
otter deploy --host droplet
```

**One file, not one per integration.** The daemon's environment is a single
process environment — every `EnvironmentFile=` is merged into it — and an
integration receives only the keys its own manifest declares. So a
per-integration file isolated nothing: it just turned one rotated credential
into an N-file edit and let those copies drift apart.

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
# integrations/shopify-orders-to-salesforce/otter.yaml
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

1. the token in this checkout's `.otter/state.secret.json`;
2. `--api-token`, if given;
3. the token already installed on the host (so deploying from a second machine,
   or after deleting `.otter/`, does not lock you out);
4. a freshly generated one.

`--rotate-token` forces a new one and prints it. Regenerating on every deploy
would break every client the operator already has, which is why it is opt-in.
`--dry-run` may generate a candidate token and a candidate platform, but it
writes neither: the state file is only touched by a deploy that succeeded.

## Talking to the remote daemon

The deployed API binds `127.0.0.1:7337` — loopback only. There is no open port,
no firewall rule, no reverse proxy and no TLS certificate to provision or
renew. Reach it with a tunnel:

```sh
make deploy-tunnel HOST=droplet          # foreground, Ctrl-C to stop
ssh -N -L 7337:127.0.0.1:7337 droplet &  # or by hand
export OTTER_API_TOKEN=$(python3 -c 'import json;print(json.load(open(".otter/state.secret.json"))["api_token"])')
otter integrations
otter runs --limit 10
otter logs $(otter run shopify-to-salesforce) --follow
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
| `--remote-dir` | install root | `/opt/otter` |
| `--data-dir` | SQLite and extracted SDK | `<remote-dir>/data` |
| `--service` | systemd unit name | `otterd` |
| `--service-user` | service account | `otter` |
| `--listen` | remote API address | `127.0.0.1:7337` |
| `--platform` | `GOOS/GOARCH`, skips detection | detected over SSH |
| `--env-file` | shared credentials file | `otter.env` |
| `--api-token`, `--rotate-token` | token handling | stored token |
| `--dry-run` | print the plan, change nothing | off |
| `--verbose` | stream every remote command | off |
| `--timeout` | overall bound | `10m` |
| `--status` | show what this checkout last deployed | — |
| `--destroy`, `--keep-data`, `--yes` | removal | — |

## Deploying one integration

`otter deploy` ships every integration under `integrations/`. To ship just one
and leave the rest of the host alone:

```sh
otter deploy --host droplet --integration shopify-to-salesforce
```

The other integration directories on the host are protected from the
converging sync, so they keep running exactly as they were, and their secrets
files are not rewritten.

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

```sh
otter deploy --host droplet --destroy --keep-data   # keep the data directory
otter deploy --host droplet --destroy               # also delete it
```

Without `--keep-data` the command names the data directory, explains what it
holds, and asks you to type the host name before doing anything. Deleting it
discards every integration's watermark, which means the next deploy rescans the
source system from scratch — expensive at Shopify and rude at Salesforce. The
`make` targets mirror this: `make deploy-destroy` keeps data, `make deploy-purge`
does not.

## Day-to-day operations

```sh
make deploy HOST=droplet            # build locally, then converge the host
make deploy-plan HOST=droplet       # what would change, touching nothing
make deploy-status                  # what this checkout last deployed
make deploy-tunnel HOST=droplet     # forward the API to localhost
make deploy-remote-runs HOST=droplet
```

On the host itself:

```sh
systemctl status otterd
journalctl -u otterd -f            # daemon logs
ls -l /etc/otter/                  # one secrets file per integration
/opt/otter/bin/otter status        # the CLI is installed on the host too
```

## Backups

Worth doing on day one, because the data directory is the only state Otter has.
A copy of a stopped database is valid — the daemon checkpoints the WAL on
shutdown — so the simplest safe backup is a service stop, a copy, and a start:

```sh
systemctl stop otterd
cp /opt/otter/data/otter.db /var/backups/otter-$(date +%F).db
systemctl start otterd
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
