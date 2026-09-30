#!/bin/sh
# Dead-man's-switch disk-pressure check for the otter runtime host.
#
# Four things grow on this host -- the SQLite database, release snapshots,
# prepared Python environments and the uv cache -- and the v0.3.0 daemon
# reports none of them: /health answers with status, version and uptime (plus
# counts, with a token), so nothing in the runtime can tell an operator that
# the disk is filling. This check therefore runs beside the runtime, on the
# host, and reports by *silence*, exactly like heartbeat.sh: it pings the
# external service only while every storage measure is inside its threshold.
# When a threshold is crossed -- or when a measure cannot be taken at all -- it
# exits non-zero and sends nothing, and the external service's own grace timer
# raises the alert.
#
# That inversion is why a failed delivery is fatal here too: a host whose
# filesystem is full may be unable to send anything, and a check that dies must
# not look healthy. The cost is that a broken *check* -- a bad threshold, a
# data directory that does not exist, no curl -- also fires the alarm. That is
# deliberate: an unmeasurable host is not a verified host.
#
#   OTTER_DISK_URL                 dead-man service to ping (required)
#   OTTER_DATA_DIR                 the runtime's data directory (default below)
#   OTTER_DISK_MAX_DB_MB           otter.db                          (default 512)
#   OTTER_DISK_MAX_RELEASES_MB     .releases snapshots               (default 1024)
#   OTTER_DISK_MAX_ENVIRONMENTS_MB environments/ plus python/        (default 3072)
#   OTTER_DISK_MAX_CACHE_MB        the uv cache under cache/         (default 1024)
#   OTTER_DISK_MIN_FREE_MB         free space on the data filesystem (default 2048)
#   OTTER_DISK_TIMEOUT             seconds to wait for the ping      (default 10)
#
# The defaults are sized for the runtime host as it is: a t4g.micro with a
# 20 GiB root volume, 14 GiB of it free when P0-11 was measured (HW-6a,
# 2026-09-30). Each one is a cap on a store that grows for a different reason,
# so each is justified by that reason rather than by the volume size:
#
#   * 512 MB for otter.db. It holds every retained run, log line and HTTP
#     capture inside SQLite, so what bounds it is the retention window (2160h
#     runs, 720h logs), not the file. 512 MB is ~2.5% of the volume and well
#     above what a small job fleet reaches in 90 days; crossing it means
#     retention stopped pruning, which is worth an alert on its own.
#   * 1024 MB for .releases. Every deploy stages a snapshot of the job's files,
#     so the store grows by roughly the size of the job per deploy until the
#     keep window prunes it. 1 GiB holds a deep rollback window for a job that
#     is not itself a data dump.
#   * 3072 MB for environments/ plus python/. This is the expensive store: a
#     managed CPython is ~150 MB and every prepared environment carries its own
#     dependency tree, while environment GC is "deliberately deferred"
#     (docs/managed-python.md), so it is the store most likely to grow on its
#     own. 3 GiB fits several environments without crowding the volume; a job
#     that needs more than that needs a bigger volume, not a bigger cap.
#   * 1024 MB for cache/. uv's download cache is safe to delete, but uv keeps
#     every version it has ever fetched, so it grows monotonically. 1 GiB is
#     generous for one job's dependency set and still small enough that growth
#     is noticed before the disk is.
#   * 2048 MB free is the floor: 10% of the root volume, enough for the SQLite
#     WAL, a staged release and a fresh interpreter download at the same time.
#     Below it the failure mode is not "slow", it is "the daemon cannot write",
#     so the floor is deliberately generous. The four caps sum to 5.5 GiB, so a
#     host inside all five bounds still has >= 7.5 GiB free of 20 GiB.
#
# Rounding, and why it is asymmetric: "MB" here means MiB (1024 KiB), because
# du -sk and df -Pk both count 1024-byte blocks. Store sizes round *up* and
# free space rounds *down*, so no measure can flatter the host -- a store one
# block over its cap fails, and a store exactly at its cap passes; free space
# exactly at the floor passes, and a shortfall smaller than a whole MB still
# counts against it.
#
# What counts as a measurement failure: a data directory that does not exist
# (nothing can be seen, so no health can be claimed -- the journal names the
# path, and an operator sets OTTER_DATA_DIR); a du or df that fails or prints
# something that is not a number; and a threshold that is not a whole number of
# MB. A *store* directory that does not exist inside an existing data directory
# is not a failure and measures 0 MB: the runtime creates .releases,
# environments/, python/ and cache/ lazily, and a fresh host that has never
# prepared an environment is healthy, not under pressure. A check that fired on
# a fresh host would train an operator to ignore it, which is worse than the
# hole it closes.
#
# The default data directory is the Castor host's workspace as of 2026-09-30
# (unit otterd-otter-examples-e0309b8c.service). The workspace id is part of
# the path, so a host with a different workspace must set OTTER_DATA_DIR in
# /etc/otter/disk-check.env. It is deliberately *not* a neutral parent such as
# /opt/otter: a parent that exists but holds none of the four stores would
# measure zeros and ping forever, which is the one failure this switch exists
# to prevent. A default that does not exist fails loudly on the first run
# instead, and the journal says which path was missing.
#
# Exit status: 0 means every measure was inside its threshold and the ping was
# delivered. Non-zero means a threshold was crossed, a measure failed, or the
# ping itself could not be delivered -- in every one of those cases nothing was
# sent, deliberately. Run it from a systemd timer; see disk-check.service and
# disk-check.timer beside this script.
set -eu

data=${OTTER_DATA_DIR:-/opt/otter/workspaces/otter-examples-e0309b8c/.otter/data}
url=${OTTER_DISK_URL:?set OTTER_DISK_URL to the dead-man switch to ping}
timeout=${OTTER_DISK_TIMEOUT:-10}

max_db=${OTTER_DISK_MAX_DB_MB:-512}
max_releases=${OTTER_DISK_MAX_RELEASES_MB:-1024}
max_environments=${OTTER_DISK_MAX_ENVIRONMENTS_MB:-3072}
max_cache=${OTTER_DISK_MAX_CACHE_MB:-1024}
min_free=${OTTER_DISK_MIN_FREE_MB:-2048}

# A threshold that is not a whole number of MB is not treated as "no cap" and
# not as a rounded value: an operator who typed 1.5 or 512MB made a mistake,
# and a switch that quietly ignored its own configuration would report health
# it never checked. Fail loudly, and do not ping.
whole_number() {
	case $1 in
	"" | *[!0-9]*) return 1 ;;
	esac
	return 0
}

threshold() { # name, value
	whole_number "$2" || {
		echo "disk-check: $1=$2 is not a whole number of MB; refusing to guess, not pinging, so the switch fires" >&2
		exit 1
	}
}

threshold OTTER_DISK_MAX_DB_MB "$max_db"
threshold OTTER_DISK_MAX_RELEASES_MB "$max_releases"
threshold OTTER_DISK_MAX_ENVIRONMENTS_MB "$max_environments"
threshold OTTER_DISK_MAX_CACHE_MB "$max_cache"
threshold OTTER_DISK_MIN_FREE_MB "$min_free"

# measure_kib prints the size of $1 in KiB, or 0 when the store does not exist
# yet. A du that fails is fatal: a measure that could not be taken proves
# nothing, and silence is the alarm.
measure_kib() {
	if [ ! -e "$1" ]; then
		echo 0
		return 0
	fi
	out=$(du -sk "$1" 2>/dev/null) || {
		echo "disk-check: cannot measure $1: du failed; not pinging, so the switch fires" >&2
		exit 1
	}
	kib=$(printf '%s\n' "$out" | awk 'NR == 1 { print $1 }') || kib=
	whole_number "$kib" || {
		echo "disk-check: cannot read a size for $1 from du: '$out'; not pinging, so the switch fires" >&2
		exit 1
	}
	printf '%s\n' "$kib"
}

data_missing() {
	echo "disk-check: $data is not a directory: cannot measure any of the four stores; not pinging, so the switch fires" >&2
	echo "disk-check:   why it matters: a check that cannot see the stores must never report health; set OTTER_DATA_DIR to the runtime's data directory in /etc/otter/disk-check.env" >&2
	exit 1
}

[ -d "$data" ] || data_missing

db_kib=$(measure_kib "$data/otter.db")
releases_kib=$(measure_kib "$data/.releases")
# The environments store is two directories: the prepared environments and the
# managed interpreter they were built from. They are summed in KiB and rounded
# once, because they are one store for the purpose of the cap.
environments_kib=$(measure_kib "$data/environments")
python_kib=$(measure_kib "$data/python")
environments_kib=$((environments_kib + python_kib))
cache_kib=$(measure_kib "$data/cache")

# POSIX fixes the fields of df -P: filesystem, 1024-blocks, used, available,
# capacity, mount point. Field 4 is therefore Available -- checked against
# `df -Pk /` on this machine, where field 3 is the ~12 GiB used and field 4 the
# ~270 GiB free. The last line carrying four fields is the filesystem itself;
# a device name long enough to wrap would only add lines before it.
df_out=$(df -Pk "$data" 2>/dev/null) || {
	echo "disk-check: cannot read free space for $data: df failed; not pinging, so the switch fires" >&2
	exit 1
}
free_kib=$(printf '%s\n' "$df_out" | awk 'NR > 1 && NF >= 4 { v = $4 } END { print v }') || free_kib=
whole_number "$free_kib" || {
	echo "disk-check: cannot read free space for $data from df: '$df_out'; not pinging, so the switch fires" >&2
	exit 1
}

# Sizes round up, so the reported MB is the smallest MB the store is known to
# fit in; a store over its cap is over by at least one whole block.
db_mb=$(((db_kib + 1023) / 1024))
releases_mb=$(((releases_kib + 1023) / 1024))
environments_mb=$(((environments_kib + 1023) / 1024))
cache_mb=$(((cache_kib + 1023) / 1024))
# Free space rounds down, so the reported MB is the smallest free MB the host
# is known to have.
free_mb=$((free_kib / 1024))

pressed=0

over() { # store, measured MB, cap MB, why it matters
	if [ "$2" -gt "$3" ]; then
		echo "disk-check: BREACH $1 is ${2}MB, over the ${3}MB cap" >&2
		echo "disk-check:   why it matters: $4" >&2
		pressed=1
	fi
}

over "otter.db" "$db_mb" "$max_db" \
	"it holds every retained run, log line and capture; once the filesystem fills, SQLite cannot write its WAL and runs start failing."
over ".releases" "$releases_mb" "$max_releases" \
	"it is the rollback path; a deploy that fills the disk mid-stage leaves a half-written release and no clean snapshot to return to."
over "environments + python" "$environments_mb" "$max_environments" \
	"prepared environments and the managed interpreter are re-creatable but expensive, and environment GC is deferred, so this store grows on its own."
over "cache" "$cache_mb" "$max_cache" \
	"the uv download cache grows monotonically and is not needed to run anything; unbounded, it takes the headroom the other three stores need."

# Free space is compared with >=: free space exactly at the floor passes, and
# anything below it fails, however small the shortfall.
if [ "$free_mb" -lt "$min_free" ]; then
	echo "disk-check: BREACH free space is ${free_mb}MB, below the ${min_free}MB floor" >&2
	echo "disk-check:   why it matters: with no free space the daemon cannot write the database, stage a release or fetch an interpreter, and it may not even be able to write the log that would explain it." >&2
	pressed=1
fi

if [ "$pressed" -ne 0 ]; then
	echo "disk-check: not pinging: the switch must stay silent so the external service's grace timer raises the alert" >&2
	exit 1
fi

echo "disk-check: ok db=${db_mb}/${max_db}MB releases=${releases_mb}/${max_releases}MB environments=${environments_mb}/${max_environments}MB cache=${cache_mb}/${max_cache}MB free=${free_mb}/${min_free}MB: pinging"

# Every measure is inside its bound: say so. A delivery failure is a real
# failure rather than something to swallow -- a switch that is not told this
# host is alive will fire, which is the correct alarm for a ping that could not
# be delivered.
curl -fsS --max-time "$timeout" "$url" >/dev/null
