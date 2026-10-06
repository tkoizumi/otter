# Pooled tenant runtime image

WP1's production-oriented image definition: one container per tenant, running
`otterd` directly. It is deliberately separate from the images under
`scripts/drill/`, which run privileged with a writable root because they test
systemd behaviour rather than isolating a tenant.

**Nothing in this directory has been validated yet.** It is the definition and
the launch contract; the qualification that would turn it into an adopt/reject
decision for gVisor is WP1's validation list, and it needs a host with Docker
and `runsc`. Until that runs, no claim here is evidence.

## Build

Both inputs are pinned, because "latest" is not reproducible:

```sh
make build                      # produces bin/otterd and bin/otter
docker build \
  --build-arg OTTER_VERSION="$(git describe --tags --always)" \
  -f packaging/container/Dockerfile \
  -t otter-runtime:local .
```

The binary is **copied** from `bin/` rather than downloaded in the image, so a
build cannot silently change because a URL did.

Note the context: it is the repository root, and `-f` names the Dockerfile. The
`COPY` instructions reference `bin/otterd` and `bin/otter`, which exist only
relative to the root. Running `docker build … packaging/container` sends an
~800-byte context instead and fails with `checksum … /bin/otter: not found` for
a file that plainly exists — the misleading error that cost three attempts while
`.dockerignore` was blamed.

## Launch

```sh
docker run -d \
  --name tenant-a \
  --runtime=runsc \
  --read-only \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777 \
  --cap-drop=ALL \
  --security-opt no-new-privileges \
  --security-opt seccomp=default \
  --user 10001:10001 \
  --memory 256m --memory-swap 256m \
  --cpus 1 \
  --pids-limit 64 \
  --network tenant-a-net \
  -v /var/lib/otter/tenants/a:/workspace \
  -e OTTER_API_TOKEN="$(cat /run/secrets/tenant-a-token)" \
  otter-runtime:local
```

`--runtime=runsc` is the gVisor choice under evaluation; omit it to measure the
plain-container baseline the thresholds are compared against. Everything else is
part of the contract, and each flag maps to a requirement:

| Flag | Why |
| --- | --- |
| `--read-only` | The only writable paths are the tenant volume and `/tmp`. A root filesystem that can change is state the backup does not capture |
| `--cap-drop=ALL` | The runtime needs no capability; `tini` reaps as the user it runs as |
| `no-new-privileges` | A setuid binary inside a tenant's own preparation must not be able to gain anything |
| `--user 10001:10001` | Matches the image's fixed uid, so a host-mounted volume can be ownership-checked against a known value |
| `--memory-swap` equal to `--memory` | Disables swap for the container. Without it a tenant exceeding its cap is paged instead of killed, and the OOM threshold becomes a host-level question |
| `--cpus` / `--pids-limit` | Set explicitly. The daemon has no `--memory-max`, `--cpu-quota` or `--tasks-max` flags: **these limits are the sandbox's, not otterd's**, which is why they must be verified in the cgroup rather than trusted from the command line |
| `--network tenant-a-net` | A per-tenant network. The daemon binds loopback only, and a shared loopback is not authorization |
| `-v …:/workspace` | One volume, mounted at the one path the tenant owns. No host socket, device, credential or broad root |

`OTTER_API_TOKEN` is required rather than decorative: the plan's launch contract
says a token is mandatory because loopback alone on a shared host authorises
everyone on that host.

## Effective-limit verification

A launch flag is a request, not a fact. `verify-limits.sh` reads back what the
kernel actually enforced — `memory.max`, `memory.swap.max`, `cpu.max`,
`pids.max`, the read-only root, the dropped capabilities and the uid — and fails
if any of them differs from what was asked for. That distinction is WP0's
threshold A1/A4 territory: a limit believed but not applied is worse than one
never set, because the placement decision is made on it.

## Preparation sandbox

Managed Python preparation builds dependency trees, which means running
third-party build code. It therefore runs in its own container with:

- the same restrictions as above, plus
- a **staging** volume, never the active `/workspace`, so untrusted build code
  cannot read or modify the running tenant's database or release snapshots, and
- build-only credentials, not production secrets.

Promotion from staging into `/workspace` validates file types and paths, so a
tenant's artifact never becomes something the host executes.

## What WP1 must still establish

The definition above is a hypothesis. WP1 validates it against the seven
thresholds in `hosting/docs/qualification.md` and the plan's list: a real run,
network calls, native imports, preparation, cancellation, timeout, daemon
restart, OOM recovery, SQLite durability, and restoration on another host —
confirming in each case that no child escapes termination or resource
accounting. The adopt/reject recommendation for gVisor follows from that
evidence, not from this file.
