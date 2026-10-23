#!/bin/sh
# Is the tenant's AGENT process alive?
#
# WHY PROCESS LIVENESS IS THE SIGNAL. The container's own lifecycle tracks only
# its main process: entrypoint.sh `exec`s otterd as uid 10001, so Docker reports
# the container running (and stopped) according to otterd alone. The agent is a
# SECOND process, backgrounded as uid 10002 in the same container. If the agent
# dies, otterd and the container keep running, and a check that reads container
# state would call a tenant with no agent healthy. This check therefore looks for
# a process owned by the agent's uid, not at the container's state.
#
# WHY A ZOMBIE IS NOT ALIVE. otterd does not reap a dead agent, so the agent can
# remain in the process table as a zombie (State Z): the pid and uid are still
# there, but the process is dead and there is no code left to run. Counting a
# zombie as alive would make this check pass in exactly the failure it exists to
# catch -- and keep passing forever, because nothing reaps it. A stopped process
# (State T) is excluded for the same reason: it is not making progress. Only R
# (running), S (interruptible sleep) and D (uninterruptible sleep) count.
#
# WHAT IT DOES NOT DETECT. A hung-but-alive agent: one that is blocked, spinning
# or wedged but still has a live process is "alive" to this check. Proving that
# needs an application-level probe the agent does not expose, so a tenant's own
# job failures remain the signal for it. This check answers process liveness,
# no more.
#
# ROLLOUT. The pool's metric publisher (otter-platform/hosting/pool/pool-metric.sh)
# must not require a health status until the image fleet carries this
# HEALTHCHECK. An older image reports no health at all, and requiring health
# first would silence every tenant on that image. Roll the image out first, then
# the publisher. See the Dockerfile for how this is wired.
#
# /proc is the interface: `ps`, `pgrep` and procps are not installed in
# debian:bookworm-slim, and this check must work without adding them. /bin/sh
# and grep are guaranteed. The HEALTHCHECK runs as root (the image has no USER
# directive; entrypoint.sh drops privileges per-process), so it can read every
# /proc/<pid>/status.
#
# OTTER_AGENT_UID is the same variable entrypoint.sh uses, so the check and the
# process it watches cannot disagree about the uid. OTTER_PROC_ROOT exists so the
# test suite can present a fixture procfs; the image never sets it.
set -eu

uid=${OTTER_AGENT_UID:-10002}
proc=${OTTER_PROC_ROOT:-/proc}

log() { printf 'otter-healthcheck: %s\n' "$*" >&2; }

found=0
alive=0
for status in "$proc"/[0-9]*/status; do
	[ -r "$status" ] || continue
	grep -q "^Uid:[[:space:]]*$uid" "$status" 2>/dev/null || continue
	found=1
	# R, S and D are live. Z (zombie) and T (stopped) are deliberately left
	# out of the class rather than excluded afterwards: a zombie must not
	# count as an agent even though its uid is still visible in /proc.
	if grep -q "^State:[[:space:]]*[RSD]" "$status" 2>/dev/null; then
		alive=1
		break
	fi
done

if [ "$alive" = 1 ]; then
	exit 0
fi
if [ "$found" = 1 ]; then
	log "agent uid $uid has no live process (only a zombie or a stopped process)"
else
	log "no process with uid $uid"
fi
exit 1
