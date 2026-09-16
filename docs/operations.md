# Operations Guide

Everything needed to run Otter on a real host: installation, systemd, data
layout, backups, retention, upgrades, tuning and troubleshooting.

- [Deployment targets](#deployment-targets)
- [First run on a Linux VM or EC2 instance](#first-run-on-a-linux-vm-or-ec2-instance)
- [systemd unit](#systemd-unit)
- [Docker](#docker)
- [Raspberry Pi](#raspberry-pi)
- [Data directory layout](#data-directory-layout)
- [Backups](#backups)
- [Log rotation and run-log retention](#log-rotation-and-run-log-retention)
- [Upgrades](#upgrades)
- [Capacity and concurrency tuning](#capacity-and-concurrency-tuning)
- [Health checking](#health-checking)
- [Troubleshooting](#troubleshooting)

## Deployment targets

| Target | Notes |
| --- | --- |
| Linux VM / EC2 | The primary target. One static binary, one directory, systemd. |
| Docker / Podman | Mount the data directory as a volume and the integrations directory read-only. |
| Raspberry Pi (arm64/armv7) | Cross-compiled with `make cross`; fine for a handful of integrations. |

Otter has no external service dependencies: no database server, no broker, no
shared filesystem. A host needs a Python 3 interpreter, a writable data
directory, and (for cron triggers) a clock that is roughly correct.

## First run on a Linux VM or EC2 instance

Cross-compile locally and copy the binaries, or build on the host:

```bash
# On your workstation:
make cross
scp bin/otterd-linux-amd64 ubuntu@host:/tmp/otterd
scp bin/otter-linux-amd64  ubuntu@host:/tmp/otter

# On the host:
sudo install -m 0755 /tmp/otterd /usr/local/bin/otterd
sudo install -m 0755 /tmp/otter  /usr/local/bin/otter
otterd --version
```

Create the service account and directories:

```bash
sudo useradd --system --create-home --home-dir /var/lib/otter --shell /usr/sbin/nologin otter
sudo install -d -o otter -g otter -m 0700 /var/lib/otter
sudo install -d -o root  -g otter -m 0750 /etc/otter
sudo install -d -o otter -g otter -m 0750 /srv/otter/integrations
```

Check that the interpreter is visible to the service user (see
[Troubleshooting](#python-not-found)):

```bash
sudo -u otter python3 --version
```

Validate manifests before you start the daemon:

```bash
otter validate /srv/otter/integrations/*/otter.yaml
```

Start the daemon in the foreground once to confirm discovery:

```bash
sudo -u otter otterd \
  --integrations /srv/otter/integrations \
  --data /var/lib/otter \
  --log-format pretty
```

You should see one `integration_registered` line per valid integration and an
`http_listening` line for `127.0.0.1:7337`. Ctrl-C stops it gracefully.

## systemd unit

`/etc/otter/otter.env` (secrets live here, never in the unit file or a manifest):

```bash
# /etc/otter/otter.env — mode 0600, owner otter
OTTER_INTEGRATIONS_DIR=/srv/otter/integrations
OTTER_DATA_DIR=/var/lib/otter
OTTER_LISTEN=127.0.0.1:7337
OTTER_LOG_FORMAT=json
OTTER_LOG_LEVEL=info
OTTER_WORKERS=4
OTTER_SHUTDOWN_GRACE=30s

# Optional: extract the embedded Python SDK somewhere other than
# <data dir>/sdk/python. The directory must exist and be writable by otter.
#OTTER_SDK_PATH=/opt/otter/sdk/python

# Integration secrets, referenced by name from otter.yaml `secrets:`.
SHOPIFY_TOKEN=shpat_xxxxxxxxxxxxxxxxxxxx
ERP_TOKEN=erp_xxxxxxxxxxxxxxxxxxxx
```

```bash
sudo install -o otter -g otter -m 0600 /dev/null /etc/otter/otter.env   # then edit it
```

`/etc/systemd/system/otter.service`:

```ini
[Unit]
Description=Otter integration runtime
Documentation=https://github.com/otter-run/otter/blob/main/docs/operations.md
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=otter
Group=otter
WorkingDirectory=/var/lib/otter
EnvironmentFile=/etc/otter/otter.env
ExecStart=/usr/local/bin/otterd
Restart=always
RestartSec=5s
KillSignal=SIGTERM
TimeoutStopSec=120s

# Graceful shutdown needs SIGTERM delivered to otterd itself; otterd then
# signals each child process group.
KillMode=mixed

# A crash loop must not spin forever with no logs.
StartLimitIntervalSec=300
StartLimitBurst=10

# The data directory must be writable; everything else can be read-only.
ReadWritePaths=/var/lib/otter
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
NoNewPrivileges=true
RestrictSUIDSGID=true
ProtectKernelTunables=true
ProtectControlGroups=true

[Install]
WantedBy=multi-user.target
```

> `ProtectSystem=strict` makes the whole filesystem read-only except
> `ReadWritePaths`. Add every directory an integration writes to (for example
> its own scratch space) to `ReadWritePaths`, or the integration will fail with
> `Permission denied`.

Enable and start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now otter
systemctl status otter
journalctl -u otter -f
otter status
```

Note that `Restart=always` plus crash recovery is exactly the intended
combination: a killed daemon restarts, marks orphaned `running` runs as `failed`
with `otter daemon restarted during execution`, and re-enqueues them if the
manifest's retry policy allows.

## Docker

The image is a slim Python base plus the two binaries. Build it locally — the
binaries are **not** built inside the image:

```bash
make build          # produces bin/otterd and bin/otter
make docker         # docker build -t otter:$(VERSION) .
```

Run it:

```bash
export OTTER_API_TOKEN=$(openssl rand -hex 32)

docker run -d --name otter \
  -p 127.0.0.1:7337:7337 \
  -e OTTER_API_TOKEN="$OTTER_API_TOKEN" \
  -e OTTER_LISTEN=0.0.0.0:7337 \
  -e SHOPIFY_TOKEN="$SHOPIFY_TOKEN" \
  -v /srv/otter/integrations:/var/lib/otter/integrations:ro \
  -v otter-data:/var/lib/otter \
  --restart unless-stopped \
  otter:dev --integrations /var/lib/otter/integrations
```

- `-p 127.0.0.1:7337:7337` keeps the API on the host loopback even though the
  daemon binds all interfaces inside the container.
- `OTTER_API_TOKEN` is mandatory because the container binds `0.0.0.0`; the
  daemon refuses to start without it.
- The integrations directory is mounted read-only. Otter does not write to it,
  so read-only is strictly better.
- `docker stop` sends `SIGTERM`, so the graceful shutdown path (including
  `--shutdown-grace`) runs. Keep `--stop-timeout` (default 10s in Docker)
  larger than the grace period, for example `docker stop -t 60 otter`.
- The image's `HEALTHCHECK` polls `/health` with `python3`, which the base image
  provides. `docker ps` shows `healthy`/`unhealthy`.

Persisting data in a named volume means `docker compose down` does not lose
state. Back the volume up the same way as a bare-metal data directory
(see [Backups](#backups)).

## Raspberry Pi

```bash
make cross
scp bin/otterd-linux-arm64 pi@raspberrypi:/tmp/otterd
scp bin/otter-linux-arm64  pi@raspberrypi:/tmp/otter
```

Use the same systemd setup as a VM, but tune for the hardware:

- `OTTER_WORKERS=1` or `2` and `concurrency: 1` in manifests. A Pi has little
  RAM; each Python process can easily use 30–100 MB.
- Use a real SSD or a high-endurance SD card. SQLite in WAL mode writes
  frequently, and consumer SD cards wear out. Set the data directory on the
  SSD, and consider mounting `/var/lib/otter` with `noatime`.
- `timeout` values matter more than on a server: a run that is merely slow on a
  Pi will be killed and retried if the timeout is tuned for server hardware.
- Power loss is common on a Pi. WAL mode plus a journaling filesystem protects
  the database, but anything an integration does outside SQLite (a half-written
  file) is the integration's problem. Checkpoint from state rather than from
  side effects.

## Data directory layout

```
/var/lib/otter/
├── otter.db          # the only durable state: integrations, runs, logs, queue, tokens
├── otter.db-wal      # write-ahead log (transient, checkpointed on clean shutdown)
├── otter.db-shm      # shared-memory index (transient)
└── sdk/python/       # Python SDK extracted from the binary at startup
    └── otter/
        ├── __init__.py
        └── ...
```

- The entire runtime state is `otter.db` plus its WAL sidecars. Back up the
  directory, not individual files.
- `sdk/python/` is derived data: it is rewritten from the binary on every
  startup, so it never needs backing up. `--sdk-path` overrides where it is
  extracted, and that directory must also be writable.
- The daemon creates the data directory if it does not exist. The service user
  must own it and it should be mode `0700` — it holds integration state and the
  webhook tokens.
- Nothing else writes into the data directory. No log files, no PID files, no
  sockets.

## The API token on the host

`otter deploy` writes the API token into `/etc/otter/shared.env`, and systemd
loads it for the daemon. An operator's shell is a different process, so on the
host every command would otherwise start with a 401.

The CLI therefore reads the token from that file itself, but **only when the API
is on loopback**. On the host:

```sh
otter status          # just works
otter integrations --schedule
OTTER_API_TOKEN=xyz otter status   # an explicit value always wins
```

The loopback restriction matters: a tunnel forwards a *remote* daemon to
`127.0.0.1` on a machine that may have its own `/etc/otter`, and quietly
presenting the wrong token would be harder to diagnose than a 401.

Reading the file is not a privilege escalation -- it is mode 0600, so only the
daemon's owner can read it either way.

## Is it running on a schedule?

A scheduled integration fires without anyone watching, so the question "is this
actually running?" comes up immediately after the first start. Three places
answer it.

**The schedule view** lists every cron integration with its next run time and
what happened last time:

```sh
make sync-schedule
# or: otter integrations --schedule
```

```
INTEGRATION          CRON             NEXT RUN              IN         LAST RUN
--------------------------------------------------------------------------------------------
shopify-to-salesforce */5 * * * *     2026-09-13 17:05:00   2m14s      succeeded at 17:00:04
```

An integration that has never run reports `no runs yet`, which distinguishes
"not yet due" from "silently not firing".

**The daemon log** records every tick as it happens. `make sync-up` runs the
daemon in the foreground, so cron lines appear in that terminal:

```
INFO cron_fired integration=shopify-to-salesforce cron=*/5 * * * *
```

To keep that output, start the daemon with a log file instead of `make sync-up`:

```sh
set -a; . ./otter.env; set +a
./bin/otterd --integrations ./integrations --data ./tmp --log-format pretty \
  2>&1 | tee /tmp/otter.log
```

then `tail -f /tmp/otter.log` from another terminal.

**The run history** is the ground truth:

```sh
otter runs --integration shopify-to-salesforce --limit 10
```

## Managed Python

Integrations that set `python.mode: managed` run on an interpreter and
dependency set that Otter prepared, not on the host's Python, and execute an
immutable release snapshot rather than their source tree. Both steps are
separate from execution (`otter release`, or automatically during
`otter deploy`), never part of a run. See [managed-python.md](managed-python.md).

Releases accumulate under `<data dir>/.releases`. Nothing removes them unless
you pass `otter release --keep N`, so check `otter release --list <integration>`
if the data directory grows.

## Backups

Stop the writer, or checkpoint first. The simplest correct backup is a
checkpoint plus a file copy:

```bash
# Preferred when you can stop the daemon for a moment:
sudo systemctl stop otter
sudo install -d -o root -g root -m 0700 /var/backups/otter
sudo tar czf "/var/backups/otter/otter-$(date +%F).tgz" -C /var/lib/otter otter.db
sudo systemctl start otter
```

If you cannot stop the daemon, force a WAL checkpoint so the main database file
is self-contained, then copy it:

```bash
# Requires the sqlite3 CLI; run without stopping the daemon.
sudo sqlite3 /var/lib/otter/otter.db 'PRAGMA wal_checkpoint(TRUNCATE);'
sudo cp /var/lib/otter/otter.db "/var/backups/otter/otter-$(date +%F).db"
```

A checkpoint makes the `-wal` file empty at that instant, but writers continue
immediately afterwards. For a guaranteed-consistent hot backup, use SQLite's
online backup API, which copies a live database atomically:

```bash
sudo sqlite3 /var/lib/otter/otter.db ".backup '/var/backups/otter/otter-$(date +%F).db'"
```

`.backup` is transactional and safe while `otterd` is writing: it restarts
automatically if the source changes mid-copy. Restoring is equally simple:

```bash
sudo systemctl stop otter
sudo install -o otter -g otter -m 0600 \
  "/var/backups/otter/otter-2024-06-01.db" /var/lib/otter/otter.db
sudo rm -f /var/lib/otter/otter.db-wal /var/lib/otter/otter.db-shm
sudo systemctl start otter
```

Delete stale `-wal`/`-shm` files when restoring by hand; a WAL from a different
database generation is not valid for the restored file.

Because integrations are code, back up the integrations root too — it is usually
in git, which is the right answer. Back up `/etc/otter/otter.env` only if you
keep those secrets somewhere safe and encrypted (it is not a good idea to put
plaintext credentials in an ordinary backup).

Suggested cron:

```cron
15 3 * * * root sqlite3 /var/lib/otter/otter.db ".backup '/var/backups/otter/otter-$(date +\%F).db'" && find /var/backups/otter -name 'otter-*.db' -mtime +30 -delete
```

## Reading a run's logs

`otter logs <run-id>` changes shape depending on where its output goes, so the
common cases need no flag and no temp file:

```sh
otter logs 42e84cd5-bd9                 # a terminal: readable prose
otter logs 42e84cd5-bd9 | jq            # a pipe: JSONL, one object per line
otter logs 42e84cd5-bd9 | jq -r .text   # just the human text
otter logs --pretty 42e84cd5-bd9 | less # a pipe, but you want to read it
```

Each JSONL record is:

```json
{"run_id":"42e84cd5-bd9","timestamp":"2026-09-15T04:01:33Z","stream":"otter",
 "text":"sync finished","fields":{"fetched":17,"written":17,"failed":0}}
```

`text` is the human prefix and `fields` is the structured payload the
integration logged, kept separate so a field named `run_id` or `level` inside
your own payload cannot collide with the envelope. Every line parses on its own
— a multi-line traceback is escaped into a single record — so `jq` works
directly with no prefix-stripping and nothing is lost to terminal wrapping.

Two consequences worth knowing:

- **All streams go to stdout in the JSON form**, including captured integration
  stderr. Splitting them across two file descriptors would silently drop half
  the output from a pipe; which stream a line came from is the `stream` field
  instead. The human form keeps them separated, so a 2>/dev/null still hides
  integration stderr.
- **Values are bounded in the human form.** A large nested record is truncated
  with `…` rather than wrapping the line across the terminal several times. The
  JSON form always has the complete value.

To pick a run to look at, `otter runs --limit 5` lists recent ones, and
`otter runs --limit 1 --json | jq -r '.[0].id'` gives the newest run id.

## Log rotation and run-log retention

There are two separate log streams, with different lifecycles.

**Daemon logs** go to stdout as JSON, one object per line. They are deliberately
not written to disk by Otter, so rotation is the supervisor's job:

- systemd: journald handles rotation.
  ```bash
  sudo journalctl -u otter --since '1 hour ago'
  # Cap journald usage if this host is small:
  #   /etc/systemd/journald.conf -> SystemMaxUse=500M
  ```
- Docker: use the `json-file` driver with limits.
  ```bash
  docker run --log-opt max-size=10m --log-opt max-file=5 ...
  ```
- If you must write to a file, pipe stdout through `logrotate` (for example via
  `ExecStart=/bin/sh -c 'exec /usr/local/bin/otterd 2>&1 | rotatelogs ...'`) or a
  supervisor that rotates. Do not point the daemon at a log file: it does not
  manage one.

Typical daemon log volume is small — a handful of lines per run plus HTTP access
records. The bursty, unbounded part is the second stream.

**Run logs** are child stdout/stderr captured into SQLite (`run_logs`). A chatty
integration can add hundreds of thousands of rows. Prune them on a schedule with
the `sqlite3` CLI, and reclaim space afterwards:

```bash
# Delete captured output older than 14 days, including runtime annotations.
sudo sqlite3 /var/lib/otter/otter.db <<'SQL'
DELETE FROM run_logs
 WHERE id IN (
   SELECT l.id
     FROM run_logs l
     JOIN runs r ON r.id = l.run_id
    WHERE r.queued_at < datetime('now', '-14 days')
 );
SQL
```

```bash
# Keep only the newest 2000 lines per run, for all runs.
sudo sqlite3 /var/lib/otter/otter.db <<'SQL'
DELETE FROM run_logs
 WHERE id NOT IN (
   SELECT id FROM (
     SELECT id, ROW_NUMBER() OVER (PARTITION BY run_id ORDER BY id DESC) AS rn
       FROM run_logs
   ) WHERE rn <= 2000
 );
SQL
```

```bash
# Optionally drop old run history too (this also orphans nothing: logs are
# already pruned above).
sudo sqlite3 /var/lib/otter/otter.db \
  "DELETE FROM runs WHERE queued_at < datetime('now','-90 days') AND status IN ('succeeded','failed','cancelled','timed_out');"
```

SQLite does not return freed pages to the filesystem automatically. Reclaim
space when the file has grown large:

```bash
sudo sqlite3 /var/lib/otter/otter.db 'PRAGMA wal_checkpoint(TRUNCATE); VACUUM;'
```

`VACUUM` needs free disk space roughly equal to the database size and blocks
writers while it runs, so run it during a quiet window (or use
`PRAGMA incremental_vacuum` with `auto_vacuum` enabled, which trades file size
for some write overhead). As a rule of thumb, alert when `otter.db` exceeds
about 2 GB; the state table itself is tiny, so growth is almost always
`run_logs`.

Example weekly maintenance:

```cron
30 4 * * 0 root sqlite3 /var/lib/otter/otter.db "DELETE FROM run_logs WHERE id IN (SELECT l.id FROM run_logs l JOIN runs r ON r.id = l.run_id WHERE r.queued_at < datetime('now','-14 days'));" && sqlite3 /var/lib/otter/otter.db 'PRAGMA wal_checkpoint(TRUNCATE); VACUUM;'
```

## Upgrades

Otter is a single binary, so an upgrade is "replace the file and restart". Schema
migrations are applied automatically on startup, in order, inside a transaction.

```bash
# 1. Back up first. Always.
sudo sqlite3 /var/lib/otter/otter.db ".backup '/var/backups/otter/pre-upgrade.db'"

# 2. Stop the daemon. This waits for running integrations up to --shutdown-grace.
sudo systemctl stop otter

# 3. Replace the binaries atomically (install writes a new inode).
sudo install -m 0755 bin/otterd-linux-amd64 /usr/local/bin/otterd
sudo install -m 0755 bin/otter-linux-amd64  /usr/local/bin/otter
otterd --version

# 4. Start and watch the migration output.
sudo systemctl start otter
journalctl -u otter -n 50 -f
```

Expected on a version bump with schema changes:

```json
{"level":"info","event":"migration_applied","version":3,"description":"add root_run_id to runs","timestamp":"2024-06-01T03:00:00Z"}
{"level":"info","event":"recovery_marked_failed","run_id":"run_01HZY...","error":"otter daemon restarted during execution","timestamp":"2024-06-01T03:00:01Z"}
```

After the restart:

```bash
otter status
otter integrations
otter runs --limit 10
```

Behavior worth expecting:

- **In-flight runs do not survive a restart** in any version. Crash recovery
  marks them `failed` with `otter daemon restarted during execution` and
  re-enqueues a retry when the policy allows. Design integrations to be
  idempotent, or stop the daemon when nothing long-running is in progress.
- **Downgrades are not supported.** The old binary does not understand a newer
  schema. Restore the pre-upgrade backup if you must roll back.
- **`sdk/python/` is refreshed** from the new binary at startup, so a newer SDK
  takes effect without any action. Integrations written against the old SDK keep
  working unless a changelog says otherwise.
- **Manifests are re-validated** at startup. An upgrade that adds stricter
  validation can turn a previously valid integration invalid; the daemon logs
  `integration_invalid` and keeps running.

## Capacity and concurrency tuning

Two knobs, applied together:

| Knob | Scope | Default | Effect |
| --- | --- | --- | --- |
| `--workers N` (`OTTER_WORKERS`) | Whole daemon | CPU cores, capped at 8 | Maximum simultaneous child processes across all integrations. |
| `concurrency: N` (manifest) | One integration | `1` | Maximum simultaneous runs of that integration. Excess triggers queue. |

A run starts only when **both** limits allow it. With `--workers 2` and eight
integrations at the default `concurrency: 1`, at most two integrations run at any
instant and the rest wait in the durable queue.

Sizing guidance:

- **Workers ≈ CPU cores** is the right starting point, which is why it is the
  default. Integrations are usually I/O-bound (HTTP to SaaS APIs), so you can
  safely go higher — `2 × cores` is common — but each worker is a Python
  process with real memory cost.
- **Watch RSS, not CPU.** A worker holding a large pandas DataFrame can use
  hundreds of MB. Budget `workers × peak_python_RSS` against available RAM, and
  leave room for the page cache that makes SQLite fast.
- **Raise `concurrency` only for stateless integrations.** An integration that
  increments a shared counter through `ctx.state` is safe at `concurrency: 1`
  and racy above it. `examples/counter` is deliberately `concurrency: 1`;
  `examples/customer-sync` is checkpoint-based and would need care to run
  concurrently.
- **Long runs + cron schedules queue up.** A 20-minute integration on a
  `*/5` schedule with `concurrency: 1` produces a growing backlog of queued runs
  instead of overlapping execution. Fix the schedule or shorten the run; raising
  `concurrency` may hammer the downstream API.
- **Backoff is not a worker.** A run in `retrying` is not holding a worker; it is
  parked in the queue with `available_at` set. Retries do not consume capacity
  while they wait.

Check the current picture:

```bash
otter status                                    # queue depth + per-status run counts
otter runs --status running                     # what is executing right now
otter runs --status queued --limit 100          # what is waiting
curl -s http://127.0.0.1:7337/health | python3 -m json.tool
```

If `queue_depth` grows without bound and `runs.running` sits at the worker limit,
add workers (if RAM allows) or reduce run duration. If `runs.running` is below
the worker limit while work is queued, the per-integration `concurrency` is the
constraint.

## Health checking

```bash
# Liveness: cheap, no auth needed on loopback.
curl -fsS http://127.0.0.1:7337/health

# The CLI does the same thing with a nicer exit code.
otter status >/dev/null && echo healthy || echo unhealthy

# Are runs succeeding?
otter runs --status failed --limit 5
```

```json
{"status":"ok","version":"0.4.1","uptime_seconds":81234.5,"integrations":{"total":7,"valid":6,"invalid":1},"queue_depth":2,"runs":{"queued":2,"running":2,"succeeded":1043,"failed":17,"retrying":1,"cancelled":0,"timed_out":3}}
```

Useful alerting rules:

| Signal | Rule | Why |
| --- | --- | --- |
| Daemon up | `curl -fsS /health` fails twice in a row | The process or listener is gone; systemd should be restarting it. |
| Invalid manifests | `integrations.invalid > 0` | Someone shipped a broken `otter.yaml`; it is logged and skipped. |
| Backlog | `queue_depth` rising for more than an hour | Workers or per-integration `concurrency` cannot keep up. |
| Failures | `runs.failed` and `runs.timed_out` increasing | An upstream changed, or timeouts need tuning. |
| Database growth | `otter.db` over ~2 GB | `run_logs` needs retention (see above). |

Outside the host, run the same check through your monitoring agent:

```bash
# Example: systemd timer, every minute
#   ExecStart=/usr/bin/curl -fsS http://127.0.0.1:7337/health
```

## Troubleshooting

### Python not found

**Symptom.** Runs fail immediately with `exec: "python3": executable file not
found`, exit code 127, or `python3: not found`.

1. Confirm the daemon's user can see the interpreter — this is a *different*
   `PATH` from your login shell:
   ```bash
   sudo -u otter python3 --version
   sudo -u otter sh -c 'echo $PATH'
   ```
2. Fix it in one of three ways:
   - set an absolute interpreter in the manifest:
     ```yaml
     python:
       executable: /usr/bin/python3
     ```
   - create a virtualenv and point at it:
     ```yaml
     python:
       executable: /opt/otter/venv/bin/python3
     ```
   - put the interpreter on the service user's `PATH` (for example
     `Environment=PATH=/usr/local/bin:/usr/bin:/bin` in the unit).

The error appears in the run's `otter`-stream logs: `otter logs <run-id>`.

### Missing secret

**Symptom.** The run fails *before Python starts*:

```
{"level":"warn","event":"run_finished","integration":"shopify-to-erp","run_id":"run_...","status":"failed","error":"integration shopify-to-erp requires secrets that are not available: SHOPIFY_TOKEN"}
```

The client sees `exit code` unset and this error on the run:

```console
$ otter run-status run_01HZY7Q1W2E3R4T5Y6U7I8O9P0
...
status:        failed
error:         integration shopify-to-erp requires secrets that are not available: SHOPIFY_TOKEN
```

This is a configuration failure and is **not retried**. `run_logs` contains an
`otter` entry naming the secret, and the exit code is unset because no process
was started.

1. Check the name in the manifest's `secrets:` list matches the daemon's
   environment exactly (case-sensitive, no `OTTER_` prefix).
2. Check the daemon actually has it:
   ```bash
   sudo systemctl show otter -p Environment
   sudo grep -c SHOPIFY_TOKEN /etc/otter/otter.env     # do not cat it
   ```
3. After editing `otter.env`, `sudo systemctl restart otter`. Environment files
   are read only at start.
4. Re-run: `otter run shopify-to-erp`.

Remember secrets are read from the **daemon's** environment, not the
integration's `.env` file. A `.env` next to `main.py` is invisible to Otter
unless the integration loads it itself.

### Integration invalid

**Symptom.** A directory does not appear in `otter integrations`, or
`otter run` returns `400 invalid_manifest`.

```bash
otter integrations --all          # includes invalid ones
otter validate /srv/otter/integrations/shopify-to-erp
otter inspect shopify-to-erp      # shows "valid": false and the error
```

Typical causes:

| Error | Fix |
| --- | --- |
| `name must match ^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$` | Lowercase it; remove leading/trailing separators. |
| `duplicate integration name` | Two directories declare the same `name`. Rename one. |
| `entrypoint "main.py" does not exist` | Fix the path or the filename; it is relative to the integration directory. |
| `entrypoint escapes the integration directory` | Remove `../` or an absolute path. |
| `unknown field "timeouts"` | Fix the typo; unknown fields are rejected. |
| `invalid cron expression` | Use exactly 5 fields: `minute hour dom month dow`. |
| `version must be 1` | Add `version: 1`. |

An invalid integration is logged (`"event":"integration_invalid"`) and reported
by the API, and it never crashes the daemon. After fixing the manifest, restart
the daemon to re-read it.

### Port already in use

**Symptom.** Startup aborts:

```
FATA failed to start HTTP server: listen tcp 127.0.0.1:7337: bind: address already in use
```

```bash
# Who has it?
sudo ss -ltnp | grep 7337
# or: sudo lsof -iTCP:7337 -sTCP:LISTEN

# Another otterd from a previous run?
systemctl status otter
pgrep -a otterd
```

Options: stop the other process (`sudo systemctl stop otter`), or move this
daemon:

```bash
otterd --listen 127.0.0.1:7338 ...
# and point the CLI at it:
otter --api http://127.0.0.1:7338 status
```

Note this is a runtime failure, not a crash loop: systemd's `Restart=always`
will keep retrying until the port is free, so check `systemctl status otter`
for `start request repeated too quickly` and `journalctl -u otter` for the
reason.

### Permission denied on the data directory

**Symptom.**

```
FATA failed to open database: unable to open database file: /var/lib/otter/otter.db (permission denied)
```

or integrations fail with `Permission denied` when writing files.

```bash
ls -ld /var/lib/otter
sudo -u otter test -w /var/lib/otter && echo writable || echo 'not writable'
```

Fixes:

```bash
sudo chown -R otter:otter /var/lib/otter
sudo chmod 0700 /var/lib/otter
```

Watch for two specific traps:

- **SELinux/AppArmor.** On RHEL-family hosts, label the directory:
  `sudo semanage fcontext -a -t var_lib_t '/var/lib/otter(/.*)?' && sudo restorecon -R /var/lib/otter`.
- **systemd hardening.** A unit with `ProtectSystem=strict` without a matching
  `ReadWritePaths=/var/lib/otter` (and any directory an integration writes to)
  produces permission errors that do not appear when you run `otterd` by hand as
  the same user.

### Daemon restarted during execution

**Symptom.** Runs show:

```json
{"event":"recovery_marked_failed","error":"otter daemon restarted during execution"}
```

This is expected after any restart, reboot, OOM kill or deploy. The daemon
cannot reattach to child processes it no longer owns, so the attempt is
recorded as failed and a retry is enqueued if the manifest permits. If this
error appears often, find and fix the underlying restart cause:

```bash
journalctl -u otter --since '24 hours ago' | grep -E 'recovery_marked_failed|shut down during execution'
dmesg -T | grep -i 'killed process'      # OOM kills
systemctl show otter -p NRestarts
```

Make integrations idempotent, or checkpoint with `ctx.state` so a repeated
attempt resumes instead of starting over — that is exactly the pattern in
`examples/customer-sync`.

### Runs pile up in the queue

**Symptom.** `otter status` shows a `queue_depth` that only grows.

1. Is anything running?
   ```bash
   otter runs --status running
   otter runs --status queued --limit 20
   ```
2. If `running` equals `--workers`, the daemon is saturated: raise workers (RAM
   permitting) or make runs faster.
3. If `running` is 0 or low, a per-integration `concurrency` limit is blocking,
   or runs are parked in `retrying` backoff. Check the manifests and the retry
   policy.
4. If nothing runs at all, look for missing secrets or an invalid manifest
   (both fail fast, before Python starts) and for interpreter problems. The
   daemon log has one line per transition.

### Everything is fine but a run "does nothing"

**Symptom.** Runs succeed but no work happens.

Check that the integration is not silently short-circuiting on state: state
persists across runs by design, so a `cursor` or `last_processed_*` key left
behind by an earlier experiment keeps the integration from redoing work.

```bash
otter state get my-integration cursor
otter state set my-integration cursor 'null'
```
