#!/bin/sh
# One container per tenant: two processes, two uids, one network namespace.
#
# WHY ONE CONTAINER. The agent must reach otterd over loopback. gVisor does not
# honour `--network=container:<other>` -- a sidecar in its own container gets a
# namespace with no usable route to a runsc sibling, so it cannot command the
# runtime at all, and giving it its own bridge address only moves the problem
# (otterd's listener is not reachable across a gVisor bridge either). Two
# processes in ONE container share the network namespace by construction, so the
# runtime stays loopback-only, needs no API token, and exposes nothing on any
# bridge.
#
# THE ISOLATION INVARIANT, and the reason this starts as root: otterd runs as
# uid 10001 and owns the tenant volume (mode 0700); the agent runs as uid 10002
# and must not read or write that volume, nor regain uid 10001 or root. Root is
# needed only to drop to two different uids, and the container is given exactly
# SETUID, SETGID and CHOWN for it. After the drop no process holds a capability,
# and the container runs with --security-opt no-new-privileges, so there is no
# setuid path back up. packaging/container/tests/sidecar-isolation.test.sh proves
# all three: no read, no write, no escalation.
set -eu

tenant_uid=${OTTER_TENANT_UID:-10001}
agent_uid=${OTTER_AGENT_UID:-10002}
jobs=${OTTER_JOBS:-/workspace}
data=${OTTER_DATA:-/workspace/.otter/data}
listen=${OTTER_LISTEN:-127.0.0.1:7337}
agent_data=${OTTER_AGENT_DATA:-/agent/.otter/data}
cloud=${OTTER_AGENT_CLOUD_URL:-https://app.runotter.dev}
runtime_url=${OTTER_AGENT_RUNTIME_URL:-http://127.0.0.1:7337}
runtime_id=${OTTER_AGENT_RUNTIME_ID:-}

log() { printf 'otter-entrypoint: %s\n' "$*" >&2; }

[ -n "$runtime_id" ] || { log "OTTER_AGENT_RUNTIME_ID is required"; exit 1; }
[ -n "${OTTER_AGENT_BOOTSTRAP_SECRET:-}" ] || log "warning: no OTTER_AGENT_BOOTSTRAP_SECRET; the agent cannot bootstrap"

# The agent's data lives on its own tmpfs, never in the tenant volume. It is
# chowned here rather than through a tmpfs mount option, so a change of mount
# flags cannot silently leave it root-owned (and therefore unusable) or
# world-readable (and therefore a leak of the agent's credential).
agent_root=$(dirname "$agent_data")
if ! mkdir -p "$agent_root" 2>/dev/null; then
  log "$agent_root is not writable; mount a tmpfs there (the root filesystem is read-only)"
  exit 1
fi
chown "$agent_uid:$agent_uid" "$agent_root"
chmod 0700 "$agent_root"

# Variables the TENANT process must never see, because they carry the agent's
# Cloud credential. The supervisor keeps them; otterd is started without them.
agent_only="OTTER_AGENT_BOOTSTRAP OTTER_AGENT_BOOTSTRAP_SECRET OTTER_AGENT_CLOUD_URL OTTER_AGENT_RUNTIME_ID OTTER_AGENT_DATA"

# 1. The agent, as uid 10002. It inherits the agent-only variables from this
#    process. Started first so it is already bootstrapping while otterd comes up;
#    it retries, and the runtime is loopback-only either way.
setpriv --reuid "$agent_uid" --regid "$agent_uid" --clear-groups -- \
  /usr/local/bin/otter agent \
    --cloud "$cloud" \
    --runtime-id "$runtime_id" \
    --runtime-url "$runtime_url" \
    --data "$agent_data" &
agent_pid=$!
log "agent uid $agent_uid pid $agent_pid -> $cloud as $runtime_id"

# 2. otterd, as uid 10001, in the foreground. `exec` makes it the container's
#    main process, so tini forwards SIGTERM directly to it rather than to a shell
#    that might not pass it on.
strip=""
for var in $agent_only; do strip="$strip -u $var"; done
# shellcheck disable=SC2086
exec env $strip setpriv --reuid "$tenant_uid" --regid "$tenant_uid" --clear-groups -- \
  /usr/local/bin/otterd \
    --jobs "$jobs" \
    --data "$data" \
    --listen "$listen"
