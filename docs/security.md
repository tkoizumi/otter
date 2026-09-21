# Security Model

This document states plainly what Otter does and does not protect against, and
what an operator must do to run it safely.

> **The trust model, in one sentence: an Otter integration executes with the
> operating-system permissions of the Otter worker process, and Otter does not
> sandbox integrations in the MVP.**

Anyone who can create or edit a directory containing an `otter.yaml` under the
integrations root can run arbitrary Python as the `otterd` user. Anyone who can
reach the control-plane API can start a run, which is the same thing
(unauthenticated **only** on loopback). Treat the integrations root and the API
token as code-execution privileges, not as configuration.

- [What Otter assumes](#what-otter-assumes)
- [The trust model in detail](#the-trust-model-in-detail)
- [Recommended deployment: dedicated unprivileged user](#recommended-deployment-dedicated-unprivileged-user)
- [When you need real isolation](#when-you-need-real-isolation)
- [Network exposure and API authentication](#network-exposure-and-api-authentication)
- [Webhook authentication](#webhook-authentication)
- [Secrets](#secrets)
- [Filesystem permissions](#filesystem-permissions)
- [Remote access: TLS and reverse proxies](#remote-access-tls-and-reverse-proxies)
- [Logging and secret leakage](#logging-and-secret-leakage)
- [Hardening checklist](#hardening-checklist)

## What Otter assumes

- **The integrations root is trusted code.** Every `otter.yaml` in it is
  executed. If several teams share a host, they must not share an integrations
  root unless they already trust each other completely.
- **The data directory is private.** `otter.db` contains integration state,
  captured stdout/stderr from integrations, and webhook tokens. Anything an
  integration prints is in there.
- **The daemon's environment holds credentials.** The `secrets:` mechanism reads
  values from the daemon's environment and copies them into child processes.
- **The host and its Python interpreter are trusted.** Otter runs whatever
  `python.executable` names.

## The trust model in detail

Otter's isolation boundary is the child process. That buys crash isolation,
timeout enforcement and clean cancellation — not security isolation.

| Property | Provided? | Detail |
| --- | --- | --- |
| Integration crash cannot kill the daemon | Yes | Separate child processes; a segfault or `os._exit()` is just a non-zero exit. |
| Hard timeout | Yes | SIGTERM to the process group, then SIGKILL after ~5s. |
| Filesystem isolation between integrations | **No** | All children run as the same OS user, with that user's access. |
| Network isolation | **No** | Integrations can reach anything the host can reach. |
| Memory limits, CPU quotas | **No** | Use cgroups/systemd (`MemoryMax=`, `CPUQuota=`) or containers. |
| Syscall filtering | **No** | Seccomp is not applied. |
| Protection from a malicious integration | **No** | It is the same user; it can read state, tokens and the database. |

Concretely: integration A can read integration B's state through the database,
read the webhook tokens, read the run logs of any integration, and fetch the
daemon's environment (including other integrations' secrets) from `/proc`. This
is acceptable for the common case of one operator, one host, code they wrote.
It is not acceptable for running untrusted third-party code, and the docs say so
rather than implying otherwise.

## Recommended deployment: dedicated unprivileged user

Never run `otterd` as root, and never as an interactive login account.

```bash
sudo useradd --system --create-home --home-dir /var/lib/otter \
  --shell /usr/sbin/nologin otter
sudo install -d -o otter -g otter -m 0700 /var/lib/otter
sudo install -d -o root  -g otter -m 0750 /srv/otter/integrations
sudo install -d -o root  -g otter -m 0750 /etc/otter
sudo install -o otter -g otter -m 0600 /dev/null /etc/otter/otter.env
```

Then run the daemon under systemd as `User=otter` with the hardening directives
from [operations.md](operations.md#systemd-unit): `NoNewPrivileges=true`,
`ProtectSystem=strict`, `ProtectHome=true`, `PrivateTmp=true`, and an explicit
`ReadWritePaths=/var/lib/otter`.

```ini
[Service]
User=otter
Group=otter
ReadWritePaths=/var/lib/otter
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
NoNewPrivileges=true
RestrictSUIDSGID=true
```

Add resource limits so one runaway integration cannot take the host down:

```ini
MemoryMax=2G
CPUQuota=200%
TasksMax=256
```

Because integrations inherit the service's cgroup, these limits apply to the
child Python processes too. Note that a memory cap makes OOM kills more likely,
which shows up as runs failing with `otter daemon restarted during execution`
after the daemon is restarted — size the limit for the sum of your workers.

## When you need real isolation

Choose the boundary based on who wrote the code:

| Scenario | Recommendation |
| --- | --- |
| You wrote all integrations, one trusted team | One host, one `otter` user, systemd hardening. |
| Several teams, mutually trusted, shared host | One daemon **per team**: separate integrations roots, separate data directories, separate service users, separate ports. |
| Untrusted or third-party code | **Do not run it in the same container or as the same user.** Use one container (or one micro-VM/VM) per trust boundary, each with its own `otterd`, data directory and credentials. |
| Strong multi-tenant isolation | One VM or one container per tenant. Otter has no tenant model, so the OS is the tenant boundary. |

The pattern that works with Otter's design is **one daemon per trust boundary**,
not one daemon with many tenants. Each daemon is cheap: one process, one SQLite
file, one Python child per run.

## Network exposure and API authentication

The default `--listen 127.0.0.1:7337` is the safe configuration: only processes
on the host can reach the API, so no token is required.

| Binding | Token required | Meaning |
| --- | --- | --- |
| Loopback (`127.0.0.1`, `::1`) | No | Local-only control plane. |
| Any non-loopback address | **Yes** | `OTTER_API_TOKEN` must be set or the daemon refuses to start. |

```bash
# Safe default
otterd --integrations /srv/otter/integrations --data /var/lib/otter

# Public-ish binding: token is mandatory and enforced
export OTTER_API_TOKEN="$(openssl rand -hex 32)"
otterd --listen 0.0.0.0:7337 --integrations /srv/otter/integrations --data /var/lib/otter

# Without the token, the daemon refuses to start:
# FATA refusing to start: --listen 0.0.0.0:7337 is not a loopback address and
#      OTTER_API_TOKEN is not set; set OTTER_API_TOKEN or bind to 127.0.0.1
```

The token is a **bearer** credential: it must be compared in constant time and
must never be put in a URL, a shell history, or a log line. Pass it to the CLI
through the environment rather than the command line where possible:

```bash
export OTTER_API_TOKEN=...           # systemd EnvironmentFile, or a secret store
otter --api http://127.0.0.1:7337 status
# Prefer: otter --token "$OTTER_API_TOKEN" ...   over hard-coding it in scripts
```

`--api-token` on the command line is visible to other users through `ps`. Prefer
the `OTTER_API_TOKEN` environment variable, which systemd sets from a mode-`0600`
`EnvironmentFile`.

What the daemon token can do is important: it is full control plane, including
`POST /v1/integrations/{id}/runs`, which executes code. There is no read-only
token and no per-integration API authorization in the MVP. Scope tokens by
running separate daemons per trust boundary (see above).

Two narrower credentials exist for the two cases that are not operators:

- **Run state tokens** (`OTTER_STATE_TOKEN`) are minted per run and passed to the
  child process. They authorize state read/write and log writes for that run's
  integration only, and cannot start or cancel runs. This is why an integration
  does not need the daemon token.
- **Webhook tokens** authorize exactly one thing: enqueuing a run for one
  integration. A webhook token cannot read state, read logs or drive anything
  else.

If the API is reachable from anywhere but the host itself, terminate TLS in
front of it (see below).

## Webhook authentication

Webhooks come from external systems, so they cannot use the daemon token. Each
integration with `trigger.webhook.enabled: true` gets a token generated on first
start and persisted in SQLite:

```bash
curl -s -H "Authorization: Bearer $OTTER_API_TOKEN" \
  http://127.0.0.1:7337/v1/integrations/order-events \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["webhook_token"])'
```

The hook caller presents it as `X-Otter-Token: <token>` (preferred) or
`?token=<token>` (for callers that cannot set headers):

```bash
curl -s -X POST -H "X-Otter-Token: $WEBHOOK_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"order_id": 4242}' https://otter.internal.example.com/v1/hooks/order-events
```

Rules and caveats:

- Use the header form. A `?token=` value lands in access logs, browser history
  and proxy logs.
- A wrong or missing token is `401`; hooks for integrations without the webhook
  trigger are `404`, so disabled hooks look nonexistent.
- **Rotate a webhook token** by deleting its row and reloading:
  `DELETE FROM webhook_tokens WHERE integration_id='order-events';` then
  `otter reload`. The reload generates a fresh token without restarting the
  daemon; callers must be updated.
- A webhook token only enqueues runs, but those runs execute code. Keep it out of
  client-side JavaScript and public repositories.
- Hooks are unauthenticated-by-bearer but token-guarded; they are still a public
  write path. Rate-limit them at the reverse proxy if they face the internet, and
  remember `concurrency` makes a flood queue rather than fail — queued runs are
  persistent work, not dropped work.

## Secrets

Today, secrets come from the **daemon's environment**:

```yaml
# otter.yaml: names only, never values
secrets:
  - SHOPIFY_TOKEN
  - ERP_TOKEN
```

```bash
# /etc/otter/otter.env: mode 0600, owner otter
SHOPIFY_TOKEN=shpat_xxx
ERP_TOKEN=erp_xxx
```

- Never put secret values in `otter.yaml` or in an integration's source. The
  manifest is code-reviewed and committed; the environment file is not.
- Never put secrets in `ExecStart=` on the command line in a unit file; use
  `EnvironmentFile=`.
- A missing secret fails the run **before Python starts**, and it is not
  retried. The error names the variable, never the value.
- The `env:` block is for non-sensitive configuration. It is expanded from the
  daemon environment at run time; a `${VAR}` there has the same reach as a
  secret and should be treated as configuration, not credentials.
- The daemon does not redact arbitrary integration output. An integration that
  prints its own credentials puts them in `run_logs` — which is why
  [run-log retention](operations.md#log-rotation-and-run-log-retention) and
  database permissions matter.

**Planned: a `SecretProvider` interface.** The runtime is designed so secret
resolution sits behind a `SecretProvider` interface, with the environment as the
built-in implementation. The intended integrations are AWS Secrets Manager,
HashiCorp Vault, 1Password and Castor Cloud, configured per daemon rather than
per manifest, so manifests keep listing names only and never learn about a
backend. Until that ships, the practical options are:

1. Populate `EnvironmentFile` from your secret manager at boot (for example an
   `ExecStartPre=` hook, or systemd's `LoadCredential=`/`SetCredential=`).
2. Run one daemon per secret scope so a compromise reaches fewer credentials.
3. Give an integration a scoped credential file only it can read, and let the
   integration load it itself.

Option 1 is the recommended pattern today: the daemon contract stays
"credentials are in the environment", and how they got there is an operator
choice.

## Filesystem permissions

```bash
sudo chown -R otter:otter /var/lib/otter
sudo chmod 0700 /var/lib/otter
sudo chmod 0600 /etc/otter/otter.env
sudo chown -R root:otter /srv/otter/integrations
sudo find /srv/otter/integrations -type d -exec chmod 0750 {} +
sudo find /srv/otter/integrations -type f -exec chmod 0640 {} +
```

- **Data directory `0700`, owned by the service user.** It holds state, logs and
  webhook tokens. Anyone who can read `otter.db` can read every integration's
  state and captured output.
- **`otter.env` `0600`.** It holds credentials.
- **Integrations root read-only for the service user.** Otter never writes there.
  Making it `root:otter 0750` means the `otter` user cannot drop a new
  integration into place, which turns "write to the integrations directory" into
  a privilege you must have root for.
- **`sdk/python/`** under the data directory is rewritten at startup and inherits
  the data directory's permissions. It contains no secrets, but it is executable
  code on `PYTHONPATH`, so it must not be writable by other users. If you set
  `--sdk-path`, apply the same ownership and mode.
- If `/tmp` is shared, integrations using `PrivateTmp=true` (recommended) get
  their own namespace and cannot read each other's temporary files.
- Backups of `otter.db` contain secrets-adjacent data (state, logs). Store them
  with the same care as the original, encrypted at rest.

## Remote access: TLS and reverse proxies

Do not expose `otterd` directly to an untrusted network. There is no TLS in the
daemon by design — it is meant to sit behind a proxy or a private network.

Recommended shapes:

```bash
# 1. Local only (default). Use over SSH port-forwarding if you need remote CLI.
ssh -N -L 7337:127.0.0.1:7337 ops@host
otter --api http://127.0.0.1:7337 status
```

```caddy
# 2. Caddy in front, automatic TLS. Keep otterd on 127.0.0.1:7337.
otter.internal.example.com {
    reverse_proxy 127.0.0.1:7337
    # Hooks may stay public; the control plane should not be.
    @control path /v1/integrations* /v1/runs*
    respond @control "forbidden" 403
    @hooks path /v1/hooks/*
    reverse_proxy @hooks 127.0.0.1:7337
}
```

```nginx
# 3. nginx in front, private network binding, token still required.
server {
    listen 443 ssl;
    server_name otter.internal.example.com;

    ssl_certificate     /etc/ssl/certs/otter.crt;
    ssl_certificate_key /etc/ssl/private/otter.key;

    location / {
        proxy_pass http://127.0.0.1:7337;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Additional guidance:

- Keep the API on a private subnet or a VPN (WireGuard/Tailscale) rather than the
  public internet.
- Require `OTTER_API_TOKEN` even behind a proxy — defense in depth if the proxy
  is ever misconfigured.
- If only webhooks need to be public, expose `POST /v1/hooks/*` and nothing else,
  as in the Caddy example. `GET /health` is safe to expose if your monitor needs
  it.
- Rate-limit and size-limit hook traffic at the proxy.
- Forward `X-Forwarded-For` if you want meaningful access logs; the daemon logs
  the peer address it sees.

## Logging and secret leakage

Daemon logs are structured JSON on stdout and are designed not to contain secret
values:

```json
{"level":"info","event":"run_started","integration":"shopify-to-erp","run_id":"run_01HZY3","timestamp":"2024-06-01T12:00:03Z"}
```

- Secret **names** may appear in errors (`integration shopify-to-erp requires
  secrets that are not available: SHOPIFY_TOKEN`); secret **values** do not, and
  must not be logged by any code path.
- Manifest `env:` values are logged in `debug` only, and are treated as
  non-sensitive. Do not put credentials in `env:`.
- The API never returns secret values, and returns `env` exactly as written in
  the manifest (with `${VAR}` unexpanded).
- Webhook tokens are returned by `GET /v1/integrations/{id}` (deliberately, so
  operators can retrieve them) but are never written to log lines.
- **Integrations can leak their own secrets**: anything printed to stdout/stderr
  is captured into `run_logs`, and that table lives in the database. Avoid
  logging tokens, and prune run logs on a schedule. If a secret has been printed
  and captured, rotate it and delete the offending rows:
  ```sql
  DELETE FROM run_logs WHERE message LIKE '%shpat_%';
  ```
- Daemon logs on stdout go to journald or the supervisor's log store. Ensure
  those are access-controlled too; they contain integration names, run ids, peer
  addresses and error strings.

## Hardening checklist

- [ ] `otterd` runs as a dedicated, unprivileged system user (`otter`), never
      root, with a nologin shell.
- [ ] systemd hardening enabled: `NoNewPrivileges`, `ProtectSystem=strict`,
      `ProtectHome`, `PrivateTmp`, explicit `ReadWritePaths`.
- [ ] Resource limits set (`MemoryMax`, `CPUQuota`, `TasksMax`) so one
      integration cannot exhaust the host.
- [ ] `--listen` is `127.0.0.1:7337` unless remote access is genuinely required.
- [ ] `OTTER_API_TOKEN` is a long random value (`openssl rand -hex 32`), supplied
      through a mode-`0600` `EnvironmentFile`, never on the command line.
- [ ] `/var/lib/otter` is `0700`, owned by the service user.
- [ ] `/etc/otter/otter.env` is `0600`, owned by the service user.
- [ ] The integrations root is writable only by administrators, and its contents
      are reviewed like any other code that runs in production.
- [ ] Secrets live in the daemon environment sourced from a secret manager;
      manifests contain names only.
- [ ] Webhook tokens are delivered via `X-Otter-Token`, not `?token=`, and rotate
      on a schedule or after any exposure.
- [ ] Remote access is via VPN or a TLS-terminating reverse proxy that exposes
      only `/v1/hooks/*` (and optionally `/health`) if hooks must be public.
- [ ] `run_logs` retention is configured; the database file size is monitored.
- [ ] Backups of `otter.db` are access-controlled and encrypted at rest.
- [ ] One daemon per trust boundary; untrusted integrations run in separate
      containers or VMs.

## Integration identity

State, run history, webhook tokens, releases and prepared environments belong to
an integration's durable identity, which the runtime mints and records in a
`.otter-id` marker inside the source directory. The marker is a claim, not a
credential: write access to an integration directory already lets its holder
change the code the runtime executes, so a forged marker is not an escalation.

What identity does protect against is mistakes: copying a directory creates a
new instance with empty state, and deleting and recreating one does not inherit
the previous instance's data. It is not isolation from a hostile process running
as the same user. See [identity.md](identity.md).
