# Operations Guide

Everything needed to run Otter on a real host: installation, systemd, data
layout, backups, retention, upgrades, tuning and troubleshooting.

- [Deployment targets](#deployment-targets)
- [First run on a Linux VM or EC2 instance](#first-run-on-a-linux-vm-or-ec2-instance)
- [systemd unit](#systemd-unit)
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
| Raspberry Pi (arm64/armv7) | Cross-compiled with `make cross`; fine for a handful of jobs. |

Otter has no external service dependencies: no database server, no broker, no
shared filesystem. A host needs a Python 3 interpreter, a writable data
directory, and (for cron triggers) a clock that is roughly correct.

## First run on a Linux VM or EC2 instance

The supported path from an empty Ubuntu 24.04 host to a running daemon is two
scripts and one command:

```bash
# On your workstation, from this checkout:
make build
sh scripts/provision.sh --host ubuntu@203.0.113.10 --identity ~/.ssh/castor
```

`scripts/provision.sh` does the work *before* a runtime exists on the host, then
hands off and checks the result:

1. it asserts the host is reachable, the login has passwordless `sudo`, and the
   architecture is amd64 or arm64 (it detects which, never assumes one);
2. it asserts the host is Ubuntu 24.04, has the tools `otter deploy` requires,
   and meets the RAM, free-disk and swap floors;
3. it asserts `sshd` refuses passwords and root logins, that the host is not
   already provisioned, and that the provisioning report exists;
4. it runs `otter deploy`, which converges the service account, the `0700` data
   and `0600` environment files, the systemd unit and the release;
5. it runs `scripts/assert-host-permissions.sh` **on the host** and fails if the
   permissions, the unit's sandbox, the loopback bind or the listening ports are
   not what the deploy claims.

Nothing in step 4 is reimplemented by the provisioning script: `otter deploy`
remains the only thing that creates the account, the directory modes and the
unit, and re-running it is how a host converges.

`--dry-run` prints every command instead of running it, which is how to review
what a host will get before it gets it.

### Sizing a 1 GiB host

Two settings decide whether a small host survives:

- **`OTTER_WORKERS=1` in `otter.daemon.env`.** The daemon defaults to one worker
  per CPU core, and `otter deploy` caps the whole unit cgroup at
  `MemoryMax=75%`. On the 1 GiB / 2 vCPU Castor instance that default means two
  Python processes plus `otterd` inside roughly 680 MiB, which is how the host
  OOMs. `provision.sh` writes `OTTER_WORKERS=1` when the project has no
  `otter.daemon.env` and injects it before the deploy.
  [`otter.daemon.env.example`](../otter.daemon.env.example) is the annotated
  template for the rest of the daemon settings.
- **The swap floor.** `provision.sh` fails if active swap is below 256 MiB, and
  `assert-host-permissions.sh` fails again after the deploy. This is not
  defensive padding: on the Castor host a provisioning script aborted partway
  and the host came up with **zero** swap while only `cloud-init status` — which
  nothing reads — reported the error.

### Scripts and what each is for

| Script | Runs on | What it is for |
| --- | --- | --- |
| `scripts/provision.sh` | your workstation | Empty Ubuntu 24.04 host → `otter deploy` → asserted host. Drills the plan with `--dry-run`. |
| `scripts/assert-host-permissions.sh` | the host | Asserts CA-09/CA-10 on a deployed host: account, modes, ownership, unit state and sandbox, loopback-or-private API, approved ports, swap, provisioning report. |
| `scripts/test-provision.sh` | your workstation | Falsifiability for `provision.sh`: 27 fixture cases. A ready fixture must pass, and a missing swap, missing report, unreadable `sshd`, half-provisioned host or failed deploy must each fail the named check. |
| `scripts/test-assert-host-permissions.sh` | your workstation | Falsifiability for the assertion script: 48 fixture cases, one per assertion it names. Every check can be disabled by mutating the script and the case that names it goes red. |

The assertion script can also be run by hand against a deployed host:

```bash
sudo sh scripts/assert-host-permissions.sh \
  --workspace-dir /opt/otter/workspaces/castor-24856da9
```

It reads the unit, environment-file basename and listen address out of the
workspace's own `workspace.json` — the record the deploy writes, whose keys are
`unit`, `name` and `listen` — and derives the data directory as
`<workspace>/.otter/data`, so it checks the paths the deploy wrote rather than
paths an operator remembered. On a normally-deployed host the invocation above
is therefore enough; no other flags are needed.

`--approved-ports` is the list of ports the host may be listening on (default
`22`): the script enumerates the host's own listening sockets with `ss -tln` and
fails on anything else, or on a wildcard bind on an unapproved port. It does
**not** scan from outside the host — CA-10's external port scan is a separate
step (`nmap` from another machine), and this check is the on-host half of the
same claim. `--provision-report ''` skips the report check for a host that was
provisioned by other means.

Two ownership rules it asserts are worth stating because they look backwards at
first glance. The data directory is `0700` owned by the service account; the
environment files are `0600` owned by **root**, inside a `0700` root-owned
`/etc/otter/workspaces`. systemd reads `EnvironmentFile=` as root before it
drops to `User=otter`, so the daemon never opens them; making them
service-owned would let any compromised job rewrite its own credentials. The
`otter.db` file mode is reported rather than failed: its `0700` directory
already contains it, and the documented requirement is the directory.

### Doing it by hand

The manual path is below. It is the fallback and the explanation of what the
scripts above do, not the supported first run: `otter deploy` performs the
account, directory, unit and release steps itself, and doing them by hand first
only gives it less to converge and you more to get wrong.

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

Make sure the host has swap before it runs anything. A 1 GiB instance without
swap fails under two concurrent Python processes, and the failure looks like an
unexplained `otter daemon restarted during execution`:

```bash
# On the host, as root: a 2 GiB swapfile, persistent across reboots.
fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
grep -q '^/swapfile' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
free -m   # Swap total must be non-zero
```

If this host was provisioned by a cloud-init image that leaves
`/var/log/otter-provision-report.txt`, check it before deploying. Its absence
means the image's own provisioning stopped partway — which is how the zero-swap
host above happened — and `scripts/provision.sh` refuses to deploy until the
cause is understood. A host provisioned some other way can still be deployed by
hand and asserted with `--provision-report ''` to record that decision.

Create the service account and directories:

```bash
sudo useradd --system --create-home --home-dir /var/lib/otter --shell /usr/sbin/nologin otter
sudo install -d -o otter -g otter -m 0700 /var/lib/otter
sudo install -d -o root  -g otter -m 0750 /etc/otter
sudo install -d -o otter -g otter -m 0750 /srv/otter/jobs
```

Check that the interpreter is visible to the service user (see
[Troubleshooting](#python-not-found)):

```bash
sudo -u otter python3 --version
```

Validate manifests before you start the daemon:

```bash
otter validate /srv/otter/jobs/*/otter.yaml
```

Start the daemon in the foreground once to confirm discovery:

```bash
sudo -u otter otterd \
  --jobs /srv/otter/jobs \
  --data /var/lib/otter \
  --log-format pretty
```

You should see one `job_registered` line per valid job and an
`http_listening` line for `127.0.0.1:7337`. Ctrl-C stops it gracefully.

## systemd unit

`/etc/otter/otter.env` (secrets live here, never in the unit file or a manifest):

```bash
# /etc/otter/otter.env — mode 0600, owner otter
OTTER_JOBS_DIR=/srv/otter/jobs
OTTER_DATA_DIR=/var/lib/otter
OTTER_LISTEN=127.0.0.1:7337
OTTER_LOG_FORMAT=json
OTTER_LOG_LEVEL=info
OTTER_WORKERS=4
OTTER_SHUTDOWN_GRACE=30s

# Optional: extract the embedded Python SDK somewhere other than
# <data dir>/sdk/python. The directory must exist and be writable by otter.
#OTTER_SDK_PATH=/opt/otter/sdk/python

# Fail-closed by default: startup refuses if crash recovery cannot complete.
# Uncomment only to bring the daemon up anyway while diagnosing that.
#OTTER_ALLOW_INCOMPLETE_RECOVERY=true

# Job secrets, referenced by name from otter.yaml `secrets:`.
SHOPIFY_TOKEN=shpat_xxxxxxxxxxxxxxxxxxxx
ERP_TOKEN=erp_xxxxxxxxxxxxxxxxxxxx
```

```bash
sudo install -o otter -g otter -m 0600 /dev/null /etc/otter/otter.env   # then edit it
```

`/etc/systemd/system/otter.service`:

```ini
[Unit]
Description=Otter job runtime
Documentation=https://github.com/tkoizumi/otter/blob/main/docs/operations.md
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
> `ReadWritePaths`. Add every directory a job writes to (for example
> its own scratch space) to `ReadWritePaths`, or the job will fail with
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
  the database, but anything a job does outside SQLite (a half-written
  file) is the job's problem. Checkpoint from state rather than from
  side effects.

## Data directory layout

```
/var/lib/otter/
├── otter.db          # the only durable state: jobs, runs, logs, queue, tokens
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
  must own it and it should be mode `0700` — it holds job state and the
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
otter jobs --schedule
OTTER_API_TOKEN=xyz otter status   # an explicit value always wins
```

The loopback restriction matters: a tunnel forwards a *remote* daemon to
`127.0.0.1` on a machine that may have its own `/etc/otter`, and quietly
presenting the wrong token would be harder to diagnose than a 401.

Reading the file is not a privilege escalation -- it is mode 0600, so only the
daemon's owner can read it either way.

## Is it running on a schedule?

A scheduled job fires without anyone watching, so the question "is this
actually running?" comes up immediately after the first start. Three places
answer it.

**The schedule view** lists every cron job with its next run time and
what happened last time:

```sh
otter jobs --schedule
# or: otter jobs --schedule
```

```
JOB                  CRON             NEXT RUN              IN         LAST SUCCESS
--------------------------------------------------------------------------------------------
shopify-to-salesforce */5 * * * *     2026-09-13 17:05:00   2m14s      2026-09-13 17:00:04 (4m56s ago)
```

A job that has never succeeded reports `never succeeded`, which distinguishes
"not yet due" from "silently not firing". The column is the daemon's own last
success, computed once for the whole listing, so it costs no run listing per
job.

**The daemon log** records every tick as it happens. `otter start` runs the
daemon in the foreground, so cron lines appear in that terminal:

```
INFO cron_fired job=shopify-to-salesforce cron=*/5 * * * *
```

To keep that output, start the daemon with a log file instead of `otter start`:

```sh
set -a; . ./otter.env; set +a
./bin/otterd --jobs ./jobs --data ./tmp --log-format pretty \
  2>&1 | tee /tmp/otter.log
```

then `tail -f /tmp/otter.log` from another terminal.

**The run history** is the ground truth:

```sh
otter runs shopify-to-salesforce --limit 10
```

## Pausing one job

Sometimes the schedule is the problem: a vendor is down, a credential expired,
or a job is making a mess and needs to stop firing *now* without being
torn down. Pausing is that control.

```sh
cd jobs/shopify-to-salesforce
otter pause
# paused: shopify-to-salesforce
# cron and webhook will not fire; otter run still runs it on demand
```

With no argument the job in the working directory is used, so `otter
pause` and `otter pause .` are the same command, exactly like `otter run`. A
name, a path or `id:<id>` works from anywhere.

What a pause stops, and what it leaves alone:

| Stops | Left alone |
| --- | --- |
| The cron trigger (unregistered, so the schedule view stops offering a next run) | The identity, the state, the run history and the logs |
| Webhook triggers (`503`, not `404`) | The webhook token, the releases, the managed environment |
| Nothing else | In-flight runs, already-queued runs, and a retry chain already admitted |

An explicit `otter run` is **not** stopped. An operator asking for a run is not
what the pause was about, and you often want to test the fix before resuming.

```sh
otter resume                     # re-arm; missed windows are not replayed
otter jobs --schedule    # NEXT RUN shows "paused" for a paused one
otter inspect                    # paused: since 2026-09-13 17:02:11
```

Resuming takes the cron expression from the live manifest and computes the next
fire time from now. It does not replay the runs that were due while paused, and
it does not need a reload or a restart.

**Work already in flight.** A pause stops new admission; it deliberately does
not reach into a run that is already executing, and it does not clear the queue.
Both are ended deliberately:

```sh
otter runs shopify-to-salesforce --status queued
otter cancel 0f9c1e2a-...        # never retried, whatever the manifest says
```

**Pause belongs to the identity, not the address.** `otter move` carries the
pause to the new directory, because the identity moved with it. `otter reset`
mints a fresh identity, and it starts enabled. Purging with `otter delete`
removes the pause with everything else. The pause is durable: it survives
`otter reload`, a daemon restart and a deploy, so a job paused at 3am
is still paused after the morning's `otter deploy`.

**In scripts.** Pause and resume are idempotent: pausing an already-paused
job exits `0` and reports `changed: false`, so a deploy step can call it
unconditionally.

```sh
otter pause shopify-to-salesforce --json   # {"paused":true,"changed":false,...}
```

There is deliberately **no** `enabled:` flag under `trigger:` in `otter.yaml`.
A manifest is code: it is released, snapshotted and immutable for a run. An
emergency pause is an operator decision about the runtime, not a code change,
and a declarative flag would let the next `otter reload` silently undo it.
Recurring blackout windows would be a scheduling feature, not this one.

## Managed Python

Every job executes an immutable release snapshot rather than its source
tree, so a release is required before its first run and an edit is not live
until it is released again (`otter release`, or automatically during
`otter deploy`). Separately, jobs that set `python.mode: managed` run on
an interpreter and a dependency set that Otter prepared, not on the host's
Python. Both are separate from execution and never part of a run. See
[managed-python.md](managed-python.md).

A release places the job and every shared tree it declares relative to
one base — the closest common ancestor of the discovery root, the
job and each captured tree — so `python.path` keeps resolving verbatim
whatever shape the workspace has. A declared tree that is missing, an absolute
`python.path`, and a symlink that resolves outside the captured trees are all
refused at release time rather than shipped as a snapshot that depends on the
live tree. Activation re-validates the snapshot and, for a managed job,
that its environment is ready, so `--activate` cannot roll back onto a broken or
unprepared release.

Releases accumulate under `<data dir>/.releases`. `otter release --keep N`
prunes inactive releases beyond `N`, and `otter deploy` runs the same prune with
a default of `--keep 3`, so a deploy converges the release directory as well as
the code. Check `otter release --list <job>` (or `otter release --list --all`
for every job and every gap) if the data directory grows. Retention is per job
and `--keep` applies to `--all` as well, so `otter release --all --keep 3`
prunes a whole workspace in one command.

Every prune keeps more than the window:

- the **active release** is never removed;
- the **newest inactive release** is never removed, so a rollback always has a
  target;
- any release a `queued`, `running` or `retrying` run is bound to is never
  removed, because that attempt executes that snapshot; and
- a prune **refuses to run at all** when the run registry cannot be read, rather
  than act on a pin set that only looks empty.

A deploy never writes or deletes run history, sync watermarks or the extracted
Python SDK. It reads the run registry only to find the releases a pending run is
bound to, and it removes only release snapshots, and only the ones outside that
window. `otter release` keeps every release unless you pass `--keep`; a deploy
prunes by default because convergence is the point of a deploy.

## Backups

A backup is only as good as its restore, and `otter.db` on its own does not
restore a runtime. Run history references releases that live on disk beside the
database, and job identity is a marker inside the job's **source** directory
([Job identity](security.md#job-identity)). A database-only backup restores
history whose code is gone: the runs list, and nothing can execute.

So there are two halves, and both are required — the database first, then
[everything else a restore needs](#what-else-a-restore-needs).

Stop the writer, or checkpoint first. The simplest correct database backup is a
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
automatically if the source changes mid-copy. Restoring the database file is
equally simple — but this is the *database half only*, and everything in
[What else a restore needs](#what-else-a-restore-needs) is required with it:

```bash
sudo systemctl stop otter
sudo install -o otter -g otter -m 0600 \
  "/var/backups/otter/otter-2024-06-01.db" /var/lib/otter/otter.db
sudo rm -f /var/lib/otter/otter.db-wal /var/lib/otter/otter.db-shm
sudo systemctl start otter
```

Delete stale `-wal`/`-shm` files when restoring by hand; a WAL from a different
database generation is not valid for the restored file.

### What else a restore needs

`<data>` is the data directory — `--data`, `/var/lib/otter` on a deployed host.
Everything below is checked against a real data directory, not inferred.

| What | Where | Why it is required |
| --- | --- | --- |
| Run history, queue, durable state, tokens | `<data>/otter.db` | the database half, above |
| Release snapshots | `<data>/.releases/<job>/<digest>/` plus the `<job>/active` symlink | an attempt is bound to a digest and re-reads its manifest from that snapshot. Without it the run fails with `release ... is no longer available` |
| Prepared Python environments | `<data>/environments/<digest>/` | a managed-Python run resolves the digest recorded at submission. A missing environment fails the run before Python starts. The directory carries an `otter-ready.json` that must match the recorded identity |
| Managed interpreters, uv cache, vendored uv | `<data>/python/`, `<data>/cache/uv/`, `<data>/tools/uv` | reconstructable with `otter prepare`; copying avoids re-fetching the interpreter and wheels on a host with no egress |
| Job source directories, including `.otter-id` | the jobs root | identity is minted into a marker inside the source directory. Restoring without it produces a *new instance* with empty state. Each release also records its source path in `otter-release.json` |
| Secrets | `/etc/otter/workspaces/<ws>.env` | a missing secret fails the run before Python starts, and is not retried. Keep this one encrypted and out of the archive holding everything else |
| The runtime binary and its version | the pinned release | the schema is versioned. Restore the version you backed up, not the newest one |

Do **not** back up, and do not restore:

- `<data>/sdk/` — the embedded Python SDK is re-extracted from the binary at
  startup;
- `<data>/otter.lock`, `<data>/serve.pid` and the workspace's `.otter/serve/` —
  transient process state;
- `otter.db-wal` / `otter.db-shm` — already folded into the database by a
  checkpoint or `.backup`. Copying a WAL alongside a database from a different
  generation is how a restore gets silently corrupted.

Two writers need more quiescing than a database checkpoint provides: a deploy
stages and **prunes** `.releases/`, and `otter prepare` writes `environments/`.
Releases and environments are immutable once published, so copying them is safe
while runs execute — but a prune during the copy can delete a snapshot the
database still references. Take the backup outside a deploy, or pin what history
needs.

### Restore onto a clean host

1. Install the **pinned** runtime ([Upgrades](#upgrades)).
2. Stop the daemon if it is running.
3. Restore the data directory: `otter.db`, `.releases/`, `environments/`,
   `tools/uv`, and `python/` + `cache/uv/` if you copied them. Owned by the
   service account, data directory mode `0700`; the environment file is mode `0600` and owned by
   `root`, like everything in `/etc/otter/workspaces`.
4. Restore the job source directories **at the same absolute paths**. Identity,
   and the `source` recorded in each release, are path-bound: a restore into a
   different path is a different instance with different state.
5. **If the restored data directory is not at the path it was backed up from,
   repoint the absolute paths it records.** Two are stored as absolutes and are
   otherwise resolved against the *old* directory:
   - `.releases/active/<job>` is a symlink to
     `<old-data>/.releases/<job>/<digest>`; recreate it pointing into the
     restored directory;
   - `environments/<digest>/otter-ready.json` records `interpreter` as an
     absolute path; rewrite it to
     `<new-data>/environments/<digest>/bin/python`.

   Without both, the daemon reports either `no active release` or `managed
   Python interpreter is missing`. A restore to the same absolute path needs
   neither step — which is the simpler restore, and the reason this is easy to
   miss. `scripts/drill/backup-restore.sh` performs both, and demonstrates why:
   omit either and the restored job cannot run.
6. Remove `otter.db-wal` and `otter.db-shm` if any came along.
7. Start the daemon and verify in this order: `otter status` (serving),
   `otter jobs` (manifests resolve from the snapshots), `otter runs --all
   --limit 10` (history reads back), then **execute one job for real**.
8. Step 7 is the test. A restore that starts and lists history but cannot run a
   job has not restored a runtime.

A complete backup is a script, not a memory:

```sh
#!/bin/sh
# Quiesce deploys first: they are the only writer to .releases/.
set -eu
data=/var/lib/otter
jobs=${JOBS_ROOT:?set JOBS_ROOT to the jobs root before running this}
dest=/var/backups/otter/$(date +%F)
install -d -o root -g root -m 0700 "$dest"
# The database: a hot, atomic copy while the daemon is writing.
sqlite3 "$data/otter.db" ".backup '$dest/otter.db'"
# Everything the database references. Not every directory exists on every
# host: tools/, python/ and cache/ are managed-Python state.
for d in .releases environments tools python cache; do
  if [ -e "$data/$d" ]; then
    cp -a "$data/$d" "$dest/$d"
  fi
done
# The job sources, because identity is a .otter-id marker inside each one and
# every release records its absolute source path.
cp -a "$jobs" "$dest/jobs"
```

Keep the secrets file out of that archive and store it encrypted. Retain as many
generations as you are willing to lose, and keep each database **together with
its releases** — a database whose releases were rotated away is exactly the
failure this section exists to prevent.

**Status of this procedure.** The inventory above is corrected against the real
on-disk layout, and it is exercised at two levels.
`scripts/drill/backup-restore.sh` takes a hot backup while a run is in flight,
restores onto a *different, empty* data directory served by a fresh daemon, and
asserts the job is runnable there. Its clean-host mode goes further: it backs up
a **live** runtime, transfers the archive to a genuinely clean **second host**,
restores it, starts the daemon, and runs a job from the restored release and
environment — then checks from outside Otter that the source runtime does not
know that run's id, which is what proves the run happened on the second machine.
That drill passed twice on two real hosts:

> [2026-09-30-p0-03-hw7-clean-host.txt](evidence/phase-0/2026-09-30-p0-03-hw7-clean-host.txt)

**What this procedure does not provide.** It is manual, and it is not off-host.
Nothing schedules it, and as written the archive lands in `/var/backups/otter`
on the machine it is protecting — which covers corruption and mistaken deletion,
but not the loss of the host. A schedule, off-host transport and a retention
policy are all still yours to add, and the secrets file must be stored encrypted
somewhere else again. The drill states two further limits: the credentials are
excluded from the archive by design, so a restore supplies no secrets, and the
prepared interpreter travelled **in** the archive rather than being rebuilt, so
`otter prepare` reconstructing `python/` or `cache/uv/` from scratch is not
proven.

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
job logged, kept separate so a field named `run_id` or `level` inside
your own payload cannot collide with the envelope. Every line parses on its own
— a multi-line traceback is escaped into a single record — so `jq` works
directly with no prefix-stripping and nothing is lost to terminal wrapping.

Two consequences worth knowing:

- **All streams go to stdout in the JSON form**, including captured job
  stderr. Splitting them across two file descriptors would silently drop half
  the output from a pipe; which stream a line came from is the `stream` field
  instead. The human form keeps them separated, so a 2>/dev/null still hides
  job stderr.
- **Values are bounded in the human form.** A large nested record is truncated
  with `…` rather than wrapping the line across the terminal several times. The
  JSON form always has the complete value.

To pick a run to look at, `otter runs <job> --limit 5` lists recent ones,
and `otter runs <job> --limit 1 --json | jq -r '.[0].id'` gives the
newest run id. Add `--all` to search the whole workspace instead of one
job.

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
- If you must write to a file, pipe stdout through `logrotate` (for example via
  `ExecStart=/bin/sh -c 'exec /usr/local/bin/otterd 2>&1 | rotatelogs ...'`) or a
  supervisor that rotates. Do not point the daemon at a log file: it does not
  manage one.

Typical daemon log volume is small — a handful of lines per run plus HTTP access
records. The bursty, unbounded part is the second stream.

**Run logs** are child stdout/stderr captured into SQLite (`run_logs`). A chatty
job can add hundreds of thousands of rows. The daemon can bound them
automatically; the `sqlite3` examples below remain for a stopped daemon or a
one-off cleanup.

### Automatic retention

Run history and run output have separate windows, and both are opt-in: `0` —
the default — retains forever, so nothing is deleted until you ask for it.

```bash
otterd --log-retention 336h --run-retention 2160h   # 14 days of output, 90 days of runs
otterd --log-retention 0 --run-retention 0          # the default: keep everything
```

- **`--log-retention D`** deletes the captured output of runs older than `D` and
  keeps the run rows, so `otter runs` history survives a shorter output window.
- **`--run-retention D`** deletes the run rows as well, together with their
  output and any captured HTTP payloads.

What is never deleted, whatever the windows say:

- a run in `queued`, `running` or `retrying`: it is live work, its output may
  still be appended, and a queued backlog keeps the release it is bound to;
- any attempt in a retry chain while another attempt in that chain is live — a
  chain goes as a unit or not at all;
- a run inside either window. A chain is dated by its newest attempt, so a
  recent retry keeps the whole chain even when its root is old.

The sweep runs hourly in bounded batches and needs no downtime. A sweep that
removes something logs `logs_retained` or `runs_retained` with the cutoff it
used and the counts removed, so an operator watching the daemon log can see the
window is live rather than guess.

`--capture-retention` (see "Reading a run's HTTP requests") is a third,
independent window: it expires HTTP payloads but keeps the per-run summary.
`--run-retention` removes the summary with the run.

### Manual pruning

Use the statements below when the daemon is stopped, or for a one-off cleanup.
The timestamp compared here is `runs.created_at`, the submission time.

Delete captured output for runs older than 14 days. This removes every
`run_logs` row for those runs — both the child's own output (`origin = 'child'`)
and the runtime's annotations (`origin = 'daemon'`). Add `AND l.origin =
'child'` to the inner query to keep the annotations, so `otter trace` still
shows a run's lifecycle after its output is gone.

```sql
DELETE FROM run_logs
 WHERE id IN (
   SELECT l.id
     FROM run_logs l
     JOIN runs r ON r.id = l.run_id
    WHERE r.created_at < datetime('now', '-14 days')
 );
```

Keep only the newest 2000 lines per run, for all runs:

```sql
DELETE FROM run_logs
 WHERE id NOT IN (
   SELECT id FROM (
     SELECT id, ROW_NUMBER() OVER (PARTITION BY run_id ORDER BY id DESC) AS rn
       FROM run_logs
   ) WHERE rn <= 2000
 );
```

Optionally drop old run history too. Logs are pruned above, so this orphans
nothing:

```sql
DELETE FROM runs
 WHERE created_at < datetime('now', '-90 days')
   AND status IN ('succeeded', 'failed', 'cancelled', 'timed_out');
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
30 4 * * 0 root sqlite3 /var/lib/otter/otter.db "DELETE FROM run_logs WHERE id IN (SELECT l.id FROM run_logs l JOIN runs r ON r.id = l.run_id WHERE r.created_at < datetime('now','-14 days'));" && sqlite3 /var/lib/otter/otter.db 'PRAGMA wal_checkpoint(TRUNCATE); VACUUM;'
```

## Reading a run's HTTP requests

Otter records every run's outgoing HTTP by default — headers and sanitized JSON
bodies included — so a failure can be explained after it happened rather than
after someone has added logging. A job opts down in its manifest, and a
deployment can lower the default. The recording is read back with two commands:

```sh
otter requests <run-id>                  # list summaries; --limit and --after-id page it
otter request <request-id>               # one exchange; the owning run is resolved from storage
otter request <run-id> <request-id>      # one exchange, when the id needs disambiguating
```

`otter request` takes the request id on its own, so the run id is normally
unnecessary. The two-argument form remains the escape hatch for an id that more
than one run recorded: that case is reported as a conflict naming the runs rather
than resolved arbitrarily.

Both commands follow the `otter logs` convention: a terminal gets readable prose,
a pipe gets JSON, and `--json` and `--pretty` override that. `otter requests`
always prints the capture state first, because an empty list is ambiguous on its
own — `complete` with zero requests means capture observed nothing, `off` means
this run was not recorded, and `unavailable` means the run predates capture or
was never configured for it. It also prints the coverage the run had: `urllib`
always, plus `requests` and `httpx` when the run's interpreter had them
installed. An empty list under coverage that omits the client a job
uses means that client was not instrumented, not that nothing was sent. The list
never loads bodies, so it is safe to run against a run with many exchanges.

Capture is bounded by design: 256 KiB per body, 10 MiB and 1,000 request records
per run. If a run hits those limits, capture is dropped rather than the run being
failed, and the dropped counts appear in the capture summary.

**Choosing what to record.** The per-run `--capture` flag wins, then the
job's own `capture:` field, then the daemon default:

```bash
otterd --capture-default metadata     # deployment-wide summaries only
otterd --capture-default off          # record nothing at all
```

A job that handles regulated or personal data should say so itself, so
the decision travels with the code and survives a change to the deployment
default:

```yaml
# otter.yaml
capture: off        # or metadata; capture: full keeps payloads under a lower default
```

`otter inspect <job>` prints the policy a new run would use, spelled out.
The job's declaration is read from the live manifest, so lowering it
takes effect on `otter reload` without a new release.

Redaction can be extended, never weakened, from the daemon environment:

```bash
OTTER_CAPTURE_REDACT_HEADERS="X-Tenant-Key,X-Trace-Id"
OTTER_CAPTURE_REDACT_QUERY="session,access_key"
OTTER_CAPTURE_REDACT_FIELDS="patient_id,ssn"
```

**Capture retention.** Captured payloads expire after seven days by default.
Configure it with the daemon's `--capture-retention` flag:

```bash
otterd --capture-retention 168h     # the default: seven days
otterd --capture-retention 0        # disable automatic expiry
```

Expiry removes payloads but keeps a small per-run summary, so an expired
recording is still distinguishable from a run that observed nothing. The sweep
runs hourly in bounded batches and does not require downtime. Like `run_logs`,
capture data lives in `otter.db`; `--capture-retention 0` means the database
keeps growing with payload data until you prune it yourself.

## Reloading jobs

The daemon reads the jobs directory when it starts. Adding or editing a
manifest is picked up with `otter reload`, which re-reads the directory against
the running daemon:

```bash
otter reload
added        shopify-to-netsuite
changed      shopify-to-erp
no changes   2 job(s)
```

Nothing is stopped. The process, the API listener, the worker pool and every
executing run are left alone, and a cron trigger whose expression did not change
keeps its next fire time. Only what the daemon knows about is replaced.

That makes the ordinary "deploy a job" loop restart-free:

```bash
# Drop the new job into the jobs root, then:
otter reload                    # the daemon can now see it
otter release shopify-to-netsuite   # a run executes the active release
otter run shopify-to-netsuite
```

Reload also covers edits to scheduling-relevant fields — `cron`, `concurrency`
and `trigger.webhook` — and fixes a manifest that previously failed to validate.
`reload` reports `invalid` for a manifest that is present but broken, which is
the difference between "not there" and "there but broken": only the second is
fixed by editing the file.

Two things reload does not do:

- **It does not release anything.** A newly visible job has no active
  release, so `otter run` answers `409` until `otter release` stages one.
- **It does not cancel work for a job that still exists.** Removing a
  job from the directory does end its *queued* runs, because they can
  never execute; runs already executing are left to finish. The reload reports
  how many were cancelled.

Restarting is still the right answer for changing the runtime itself — a new
binary, a new embedded SDK, or daemon-level configuration. See
[Upgrades](#upgrades).

## Upgrades

Otter is a single binary, so an upgrade is "replace the file and restart". Schema
migrations are applied automatically on startup, in order, inside a transaction.

```bash
# 1. Back up first. Always — and make it the complete backup, not just the
#    database: release snapshots and job source identity live on disk too.
#    See [Backups](#backups) for what a restore actually needs.
sudo sqlite3 /var/lib/otter/otter.db ".backup '/var/backups/otter/pre-upgrade.db'"

# 2. Stop the daemon. This waits for running jobs up to --shutdown-grace.
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
{"level":"info","event":"migration_applied","version":3,"name":"0003_integration_identity.sql","timestamp":"2024-06-01T03:00:00Z"}
{"level":"info","event":"recovery_marked_failed","run_id":"run_01HZY...","error":"otter daemon restarted during execution","timestamp":"2024-06-01T03:00:01Z"}
```

`name` is the migration's filename, which is also its description: the schema
stores no separate blurb, so the log line and `schema_migrations` agree by
construction. One `migration_applied` is emitted at `info` per migration that
start applied, in the order they were applied. A restart with nothing pending
logs none of them, so "no migration lines" means the schema did not move, not
that nobody looked.

A migration that fails aborts startup, and is reported first, at `error`, with
the same two fields before the process exits:

```json
{"level":"error","event":"migration_failed","version":3,"name":"0003_integration_identity.sql","error":"database: apply migration 0003_integration_identity.sql: ...","timestamp":"2024-06-01T03:00:00Z"}
```

A failed migration rolls back on its own; earlier migrations stay applied, so a
repaired migration can be shipped and the next start continues from where this
one stopped.

After the restart:

```bash
otter status
otter jobs
otter runs --all --limit 10
```

Behavior worth expecting:

- **In-flight runs do not survive a restart** in any version. Crash recovery
  marks them `failed` with `otter daemon restarted during execution` and
  re-enqueues a retry when the policy allows. Design jobs to be
  idempotent, or stop the daemon when nothing long-running is in progress.
- **Downgrades are not supported.** The old binary does not understand a newer
  schema. Restore the pre-upgrade backup if you must roll back.
- **`sdk/python/` is refreshed** from the new binary at startup, so a newer SDK
  takes effect without any action. Jobs written against the old SDK keep
  working unless a changelog says otherwise.
- **Manifests are re-validated** at startup. An upgrade that adds stricter
  validation can turn a previously valid job invalid; the daemon logs
  `job_invalid` and keeps running.
- **Every job must be released once after upgrading.** The release
  digest format is versioned, and the new implementation deliberately does not
  reuse a snapshot laid out by an older one, even for identical inputs. A
  deploy prunes inactive snapshots beyond `--keep` (default 3), but the previous
  release is the newest inactive one, so a rollback to a pre-upgrade release
  still works. A deployment does this automatically; a local workspace needs
  `otter release --all` (or one `otter release` per job) before runs resume
  executing current code.
- **Startup can refuse to run.** If crash recovery or queue reconciliation
  cannot complete, the daemon exits instead of serving with stranded work. See
  [Daemon refuses to start after a crash](#daemon-refuses-to-start-after-a-crash).

### Upgrade discipline

One runtime at a time, and never without a way back.

1. **Announce a window.** Schema migrations run on startup; a job mid-flight is
   interrupted, and crash recovery will mark it failed and retry it when the
   policy allows.
2. **Take the complete backup** ([Backups](#backups)) and verify it restores
   before you need it.
3. **Upgrade one runtime.** Fleets and paired hosts are upgraded in sequence, so
   a failure stops at one host instead of all of them.
4. **Choose the quiet window deliberately.** Nothing schedules work for you —
   pause jobs or wait for a gap ([Pausing jobs](#pausing-jobs)).
5. **Smoke after the restart**, in this order: `otter status` (the daemon is
   serving and the schema moved), `otter jobs` (manifests still validate),
   `otter runs --all --limit 10` (history is readable), then one real run.
6. **Record what happened**, including a rollback. A rollback is a restore, not a
   downgrade — see below.

### There is no unattended upgrade path

A runtime never replaces itself, and nothing in Otter decides to move a host to a
newer version:

- a deploy names its version explicitly. Fetching an archive requires a version
  matching `^\d+\.\d+\.\d+$`, and a development build is refused with a hint to
  build from source instead (`internal/deploy/fetch.go`);
- the runtime has no self-update code, and does not resolve "latest";
- the generated systemd unit restarts the same binary (`Restart=always`) and
  carries no timer, no download and no upgrade step.

An upgrade is therefore always a decision someone made, which is what makes a
pinned version meaningful.

## Capacity and concurrency tuning

Two knobs, applied together:

| Knob | Scope | Default | Effect |
| --- | --- | --- | --- |
| `--workers N` (`OTTER_WORKERS`) | Whole daemon | CPU cores, capped at 8 | Maximum simultaneous child processes across all jobs. |
| `concurrency: N` (manifest) | One job | `1` | Maximum simultaneous runs of that job. Excess triggers queue. |

A run starts only when **both** limits allow it. With `--workers 2` and eight
jobs at the default `concurrency: 1`, at most two jobs run at any
instant and the rest wait in the durable queue.

Sizing guidance:

- **Workers ≈ CPU cores** is the right starting point, which is why it is the
  default. Jobs are usually I/O-bound (HTTP to SaaS APIs), so you can
  safely go higher — `2 × cores` is common — but each worker is a Python
  process with real memory cost.
- **Watch RSS, not CPU.** A worker holding a large pandas DataFrame can use
  hundreds of MB. Budget `workers × peak_python_RSS` against available RAM, and
  leave room for the page cache that makes SQLite fast.
- **Raise `concurrency` only for stateless jobs.** A job that
  increments a shared counter through `ctx.state` is safe at `concurrency: 1`
  and racy above it. A counter that reads and writes one key must stay at
  `concurrency: 1`; a checkpoint-based sync needs care to run concurrently
  because two attempts can claim the same work.
- **Long runs + cron schedules queue up.** A 20-minute job on a
  `*/5` schedule with `concurrency: 1` produces a growing backlog of queued runs
  instead of overlapping execution. Fix the schedule or shorten the run; raising
  `concurrency` may hammer the downstream API.
- **Backoff is not a worker.** A run in `retrying` is not holding a worker; it is
  parked in the queue with `available_at` set. Retries do not consume capacity
  while they wait.

Check the current picture:

```bash
otter status                                    # queue age and depth, run counts, freshness, storage
otter runs --all --status running               # what is executing right now
otter runs --all --status queued --limit 100    # what is waiting
curl -s http://127.0.0.1:7337/health | python3 -m json.tool
```

If `queue_depth` grows without bound and `runs.running` sits at the worker limit,
add workers (if RAM allows) or reduce run duration. If `runs.running` is below
the worker limit while work is queued, the per-job `concurrency` is the
constraint.

## Health checking

```bash
# Liveness: cheap, no auth needed on loopback.
curl -fsS http://127.0.0.1:7337/health

# The CLI does the same thing with a nicer exit code.
otter status >/dev/null && echo healthy || echo unhealthy

# Are runs succeeding?
otter runs --all --status failed --limit 5
```

```json
{
  "status": "ok",
  "version": "v0.2.0",
  "uptime_seconds": 81234.5,
  "jobs": {"total": 7, "valid": 6, "invalid": 1},
  "queue_depth": 2,
  "runs": {"queued": 2, "running": 2, "succeeded": 1043, "failed": 17, "retrying": 1, "cancelled": 0, "timed_out": 3},
  "queue": {
    "oldest_waiting_at": "2026-10-01T04:02:11Z",
    "oldest_waiting_seconds": 3725.4,
    "by_job": {"shopify-to-salesforce": 2},
    "retrying": 1,
    "next_retry_at": "2026-10-01T05:14:00Z"
  },
  "freshness": [
    {"job_id": "9f1c...", "name": "shopify-to-salesforce", "last_success_at": "2026-10-01T05:00:04Z", "age_seconds": 325.1},
    {"job_id": "41ab...", "name": "nightly-report"}
  ],
  "storage": {"db_bytes": 812345678, "disk_free_bytes": 12884901888, "disk_total_bytes": 21474836480}
}
```

The counters answer "how many?". The three blocks after them answer the rest:

- **`queue`** — `oldest_waiting_seconds` is the age of the oldest run that is
  *claimable now* (`available_at <= now`). A run parked for retry backoff is
  excluded from that age and reported separately, because it is waiting on a
  clock rather than on capacity: `retrying` is the number of such runs and
  `next_retry_at` is when the soonest of them becomes claimable. `by_job` is the
  queue depth per job, so "which job is backing up?" has an answer.
- **`freshness`** — one entry per job, from the newest succeeded run's
  `finished_at`. `age_seconds` turns "the daemon is up" into "the schedule is
  working". A job that has never succeeded has neither field; that absence is
  the signal, not a bug in the response.
- **`storage`** — `db_bytes` is `PRAGMA page_count * page_size` on the live
  database (so it excludes the WAL file), and the disk figures are the data
  directory's filesystem, read daemon-side. That is deliberate: a remote
  operator can now watch the database grow and the disk fill through the API
  instead of needing a shell on the host.

An unauthenticated caller — and any caller presenting a wrong token once
`--api-token` is set — still receives only `status`, `version` and
`uptime_seconds`. These blocks are never disclosed to the network.

Useful alerting rules:

| Signal | Rule | Why |
| --- | --- | --- |
| Daemon up | `curl -fsS /health` fails twice in a row | The process or listener is gone; systemd should be restarting it. |
| Invalid manifests | `jobs.invalid > 0` | Someone shipped a broken `otter.yaml`; it is logged and skipped. |
| Queue age | `queue.oldest_waiting_seconds` over ~15 minutes | Work has been claimable and unclaimed for a quarter of an hour: capacity, not a slow job. |
| Per-job backlog | `queue.by_job["<job>"]` rising | One job is monopolising the queue; its `concurrency` or its run duration needs attention. |
| Backlog | `queue_depth` rising for more than an hour | Workers or per-job `concurrency` cannot keep up. |
| Retry storm | `queue.retrying` climbing, or `queue.next_retry_at` perpetually in the future | A failing upstream is being retried faster than it recovers. |
| Freshness | `freshness[].age_seconds` older than the job's own cron period, or `last_success_at` absent | The schedule stopped succeeding. No counter shows this: a job failing since yesterday still has a healthy `queue_depth`. |
| Failures | `runs.failed` and `runs.timed_out` increasing | An upstream changed, or timeouts need tuning. |
| Database growth | `storage.db_bytes` over ~2 GB | `run_logs` needs retention (see above). This used to be a host-level check on `otter.db`; it is now readable remotely. |
| Disk pressure | `storage.disk_free_bytes` below your floor | The daemon cannot write. Pair it with the host-level `scripts/disk-check.sh`, which watches the same filesystem. |

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
{"level":"warn","event":"run_finished","job":"shopify-to-erp","run_id":"run_...","status":"failed","error":"job shopify-to-erp requires secrets that are not available: SHOPIFY_TOKEN"}
```

The client sees `exit code` unset and this error on the run:

```console
$ otter run-status run_01HZY7Q1W2E3R4T5Y6U7I8O9P0
...
status:        failed
error:         job shopify-to-erp requires secrets that are not available: SHOPIFY_TOKEN
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
job's `.env` file. A `.env` next to `main.py` is invisible to Otter
unless the job loads it itself.

### Job invalid

**Symptom.** A directory does not appear in `otter jobs`, or
`otter run` returns `400 invalid_manifest`.

```bash
otter jobs --all          # includes invalid and retired jobs
otter validate /srv/otter/jobs/shopify-to-erp
otter inspect shopify-to-erp      # shows "valid": false and the error
```

`otter jobs` hides an invalid job because only a running runtime has read its
manifest. With the runtime stopped the listing comes from the identity
registry, which records ownership rather than manifest validity — status is
deliberately independent of it — so the job is listed there until a daemon can
say otherwise.

Typical causes:

| Error | Fix |
| --- | --- |
| `name must match ^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$` | Lowercase it; remove leading/trailing separators. |
| `ambiguous job` | Two jobs declare the same label. Use `id:<id>` or a path; see [identity.md](identity.md). |
| `entrypoint "main.py" does not exist` | Fix the path or the filename; it is relative to the job directory. |
| `entrypoint escapes the job directory` | Remove `../` or an absolute path. |
| `unknown field "timeouts"` | Fix the typo; unknown fields are rejected. |
| `invalid cron expression` | Use exactly 5 fields: `minute hour dom month dow`. |
| `version must be 1` | Add `version: 1`. |

An invalid job is logged (`"event":"job_invalid"`) and reported
by the API, and it never crashes the daemon. After fixing the manifest, run
`otter reload` to re-read it — see [Reloading jobs](#reloading-jobs).

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

or jobs fail with `Permission denied` when writing files.

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
  `ReadWritePaths=/var/lib/otter` (and any directory a job writes to)
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

Make jobs idempotent, or checkpoint with `ctx.state` so a repeated
attempt resumes instead of starting over — persist a cursor in `ctx.state` and
read it back at the start of the next run.

### Daemon refuses to start after a crash

**Symptom.** The daemon exits at startup with `startup recovery did not
complete` or `startup queue reconciliation did not complete`, and the API never
listens.

This is fail-closed by design: the daemon could not repair interrupted or
orphaned runs, and starting anyway would leave them stranded with no queue row
and no terminal state. The log line carries the underlying database error; fix
that first. A full disk, an unreadable data directory, and a locked or corrupt
database are the common causes:

```bash
journalctl -u otter -n 50
df -h /var/lib/otter
sqlite3 /var/lib/otter/otter.db 'PRAGMA integrity_check;'
```

Only when you must bring the daemon up to inspect or repair a database it cannot
read, start it once with `--allow-incomplete-recovery`
(`OTTER_ALLOW_INCOMPLETE_RECOVERY=true`). It logs `recovery_failed` or
`queue_reconcile_failed` at `error` level and serves; the affected runs stay
non-terminal until a later restart recovers them successfully.

### Runs pile up in the queue

**Symptom.** `otter status` shows a `queue_depth` that only grows.

1. Is anything running?
   ```bash
   otter runs --all --status running
   otter runs --all --status queued --limit 20
   ```
2. If `running` equals `--workers`, the daemon is saturated: raise workers (RAM
   permitting) or make runs faster.
3. If `running` is 0 or low, a per-job `concurrency` limit is blocking,
   or runs are parked in `retrying` backoff. Check the manifests and the retry
   policy.
4. If nothing runs at all, look for missing secrets or an invalid manifest
   (both fail fast, before Python starts) and for interpreter problems. The
   daemon log has one line per transition.

### Everything is fine but a run "does nothing"

**Symptom.** Runs succeed but no work happens.

Check that the job is not silently short-circuiting on state: state
persists across runs by design, so a `cursor` or `last_processed_*` key left
behind by an earlier experiment keeps the job from redoing work.

```bash
otter state get my-job cursor
otter state set my-job cursor 'null'
```
