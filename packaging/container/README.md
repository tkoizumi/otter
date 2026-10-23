# Pooled tenant runtime image

WP1's production-oriented image definition: one container per tenant, in which
`tini` is PID 1 and the image's entrypoint runs two processes as two uids —
`otterd` as 10001, the runtime agent as 10002 — over the container's loopback. It
is deliberately separate from the images under
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

The network is not optional and must never be left implicit. Docker's **default
`bridge` is not an acceptable tenant network**: the qualification measured a
tenant on it reaching the **instance metadata service** (`HTTP 401 ... Server:
EC2ws`) and the **host's SSH port**. Create a per-tenant network first, then name
it explicitly on the run.

```sh
# Per tenant. The subnet must not overlap the host VPC or Docker's default
# bridge; 100.64.0.0/10 is the CGNAT carve-out the pool uses. Do not use
# --network=none or --internal here: the agent needs public HTTPS to reach
# Cloud. See "Egress" below for the narrow egress contract.
docker network create --subnet 100.64.10.0/24 tenant-a-net

docker run -d \
  --name tenant-a \
  --runtime=runsc \
  --read-only \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777 \
  --tmpfs /agent:rw,size=64m,mode=0755 \
  --cap-drop=ALL \
  --cap-add=SETUID --cap-add=SETGID --cap-add=CHOWN \
  --security-opt no-new-privileges \
  --security-opt seccomp=default \
  --memory 256m --memory-swap 256m \
  --cpus 1 \
  --pids-limit 64 \
  --network tenant-a-net \
  -v /var/lib/otter/tenants/a:/workspace \
  -v /run/otter/resolv-tenant-a.conf:/etc/resolv.conf:ro \
  -e OTTER_AGENT_BOOTSTRAP=runtime-secret \
  -e OTTER_AGENT_BOOTSTRAP_SECRET="$(cat /run/secrets/tenant-a-bootstrap)" \
  -e OTTER_AGENT_CLOUD_URL=https://app.runotter.dev \
  -e OTTER_AGENT_RUNTIME_ID=rt-tenant-a \
  otter-runtime:local
```

There is no `--user` and no command after the image. `tini` is the `ENTRYPOINT`
and `/usr/local/bin/otter-entrypoint` is the `CMD`; the entrypoint backgrounds
the agent as uid 10002, then `exec`s `otterd` as uid 10001, which is why the
container is granted exactly `SETUID`, `SETGID` and `CHOWN`. A fixed `--user`
would make one of the two drops impossible, and a command after the image would
bypass both the drop and the stripping of the agent-only variables.

`--runtime=runsc` is the gVisor choice under evaluation; omit it to measure the
plain-container baseline the thresholds are compared against. Everything else is
part of the contract, and each flag maps to a requirement:

| Flag | Why |
| --- | --- |
| `--read-only` | The only writable paths are the tenant volume, `/tmp` and the agent's `/agent` tmpfs. A root filesystem that can change is state the backup does not capture |
| `--cap-drop=ALL` | No process holds a capability after the entrypoint drops to uid 10001 (`otterd`) and 10002 (the agent); `tini` is PID 1 and reaps orphaned children |
| `no-new-privileges` | A setuid binary inside a tenant's own preparation must not be able to gain anything |
| `--cap-add=SETUID --cap-add=SETGID --cap-add=CHOWN` | Exactly what the entrypoint needs to drop `otterd` to 10001 and the agent to 10002; the volume is ownership-checked against those fixed uids |
| `--tmpfs /agent:…,mode=0755` | The agent's data and credential live here, owned by 10002, so a tenant process running as 10001 cannot read them |
| `--memory-swap` equal to `--memory` | Disables swap for the container. Without it a tenant exceeding its cap is paged instead of killed, and the OOM threshold becomes a host-level question |
| `--cpus` / `--pids-limit` | Set explicitly. The daemon has no `--memory-max`, `--cpu-quota` or `--tasks-max` flags: **these limits are the sandbox's, not otterd's**, which is why they must be verified in the cgroup rather than trusted from the command line |
| `--network tenant-a-net` | A per-tenant network with **its own subnet**, named explicitly. The daemon binds loopback only, and a shared loopback is not authorization. Omitting this puts the tenant on the host's default bridge, where it reaches instance metadata and the host's services — measured, not hypothesised |
| `-v …/resolv-tenant-a.conf:/etc/resolv.conf:ro` | **Required for egress.** Docker always installs `127.0.0.11` as the container resolver and `--dns` only sets that stub's upstream, so the query never becomes an outbound packet and dies against the loopback denial. Mounting a `resolv.conf` that names the resolver directly makes DNS a normal outbound packet the tenant's own policy governs |
| `-v …:/workspace` | One volume, mounted at the one path the tenant owns. No host socket, device, credential or broad root |

There is deliberately **no `OTTER_API_TOKEN`**: `otterd` binds loopback inside the
one container, so there is no bridge-reachable listener to authenticate and no
sibling network namespace to join. The bootstrap secret
(`OTTER_AGENT_BOOTSTRAP_SECRET`) is the agent's Cloud credential; the entrypoint
strips it and the other `OTTER_AGENT_*` variables from `otterd`'s environment, and
because no command follows the image nothing can skip that stripping.

## Verification

Two scripts, because two different things are being claimed:

- `verify-limits.sh` reads back the resource limits the kernel actually enforced
  — `memory.max`, `memory.swap.max`, `cpu.max`, `pids.max`, the read-only root,
  the dropped capabilities and the uid. A launch flag is a request; the cgroup is
  the fact.
- `verify-egress.sh` attempts **connections**, not resolutions, to the addresses
  a tenant must not reach, including a name that resolves to a blocked address
  (DNS rebinding) and the IPv6 equivalents. A policy that holds only against
  name lookup is not a boundary.

## Effective-limit verification

A launch flag is a request, not a fact. `verify-limits.sh` reads back what the
kernel actually enforced — `memory.max`, `memory.swap.max`, `cpu.max`,
`pids.max`, the read-only root, the dropped capabilities and the uid — and fails
if any of them differs from what was asked for. That distinction is WP0's
threshold A1/A4 territory: a limit believed but not applied is worse than one
never set, because the placement decision is made on it.

**`verify-limits.sh` follows the two-uid contract.** It asserts there is no
`Config.User` — a fixed user would make one of the entrypoint's two drops
impossible — and that `SETUID`, `SETGID` and `CHOWN` were granted, alongside the
cgroup limits it reads back as before. The two-uid isolation itself is proved by
`sidecar-isolation.test.sh`, and `launch-contract.test.sh` pins the launch flags.

## Healthcheck

The image declares a `HEALTHCHECK` that runs `/usr/local/bin/otter-healthcheck`.
It exits 0 only when a process with the agent's uid (default `10002`) is in state
`R`, `S` or `D`; a zombie (the state a dead agent is left in, because `otterd`
does not reap it) or a stopped process fails. The container's own state tracks
only `otterd`, so this is the only evidence that the AGENT — the process that
makes the tenant reachable from Cloud — is alive. It proves process liveness, not
that the agent is responsive: a hung-but-alive agent still passes, and a
tenant's own job failures remain that signal.

The pool's `pool-metric.sh` publishes `TenantHeartbeat=1` only when this check
reports `healthy`. Roll the image fleet out before the publisher: an image
without a healthcheck reports `none`, which the publisher deliberately treats as
unhealthy, so the reverse order silences every tenant on an old image.

## Egress

Per decision D3, v1's egress contract is deliberately narrow, and it is carried
as an **`egress_profile_id`** so it can widen later without changing what a job
is:

| Profile | Permits | Denies |
| --- | --- | --- |
| `shared-public` (v1) | Public internet destinations, via shared Otter-managed egress | RFC1918, loopback, link-local, the IPv6 equivalents, and cloud metadata endpoints |

The denial is enforced **where the connection is made**, not by filtering DNS.
Name-based rules are bypassable by DNS rebinding and by connecting to an address
directly, so a policy that only blocks resolution is not a policy. It must be
tested by connecting, which is what `verify-egress.sh` does.

`--network=none` is the strictest form, but it is not usable for a pooled tenant:
the agent in the same container needs Cloud over public HTTPS. The pooled launch
therefore uses a per-tenant bridge, with the egress path below.

A tenant that *does* make outbound calls needs **three** things together, and two
of them are easy to miss:

1. a per-tenant bridge with its **own non-overlapping subnet** — if Docker picks a
   range inside the host VPC's CIDR, the RFC1918 denial blocks the tenant's own
   upstream;
2. **a host-local resolver listening on that tenant's bridge gateway**, which the
   filter chain already permits, so no rule is widened and **no loopback exception
   is needed** — a sibling's loopback API stays denied;
3. that `resolv.conf` mount above, because otherwise Docker's stub intercepts.

Verified: public DNS resolves, HTTPS returns 200, and metadata, RFC1918 and
loopback all remain blocked.

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
