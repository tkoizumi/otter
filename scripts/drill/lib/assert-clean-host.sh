#!/bin/sh
# assert-clean-host.sh -- decide whether two host reports describe a legal
# clean-host restore, and say exactly why not when they do not.
#
# This is the assertion that separates the clean-host drill
# (scripts/drill/modes/backup-restore-clean-host.sh) from the same-host drill that was
# already recorded: it refuses to proceed unless the *target* is a genuinely
# empty machine of the same shape as the source. The drill must not be able to
# restore onto a host that already holds a runtime and then report that a
# restore brought a runtime back -- the second host's emptiness is the claim.
#
# It is deliberately a separate program over two report files
# (host-report.sh's output), for three reasons:
#   * it can be run by hand against a host's report to re-check a machine;
#   * it has no ssh, no daemon and no filesystem of its own, so the selfcheck
#     can drive every branch of it without a host; and
#   * every refusal names the measurement that caused it, so a red run is a
#     diagnosis rather than a shrug.
#
# Usage:
#   assert-clean-host.sh <source-report> <target-report> [sudo]
#
#   sudo  1 when the drill's remote commands run under sudo, 0 when the ssh
#         login is root. A non-root login without sudo cannot read the data
#         directory it is supposed to back up, so that combination is refused.
#
# Exit 0 when the pair is legal; 1 with `drill: FAILED: ...` on stderr when it
# is not. Stdout is a sequence of `drill: ... ok: ...` lines, one per check, so
# the transcript shows the whole gate and not just its first failure.
set -eu

usage() {
	echo "usage: assert-clean-host.sh <source-report> <target-report> [sudo]" >&2
	exit 2
}

[ "$#" -ge 2 ] || usage
SOURCE_REPORT=$1
TARGET_REPORT=$2
SUDO=${3:-0}

for f in "$SOURCE_REPORT" "$TARGET_REPORT"; do
	[ -f "$f" ] || {
		echo "assert-clean-host: no such report: $f" >&2
		exit 2
	}
done

# get reads one key out of a report. A missing key prints nothing, and every
# caller below treats empty as a failure: an unanswered question is not a pass.
get() {
	sed -n "s/^$2=//p" "$1" 2>/dev/null | head -n 1
}

fail() {
	echo "drill: FAILED: $*" >&2
	exit 1
}

ok() {
	echo "drill: $*"
}

note() {
	echo "drill: note: $*"
}

# --- the two reports are the two roles ---------------------------------------

s_role=$(get "$SOURCE_REPORT" role)
t_role=$(get "$TARGET_REPORT" role)
[ "$s_role" = source ] || fail "the source report has role=$s_role, want source: ./host-report.sh was handed the wrong file"
[ "$t_role" = target ] || fail "the target report has role=$t_role, want target: ./host-report.sh was handed the wrong file"
for key in machine_id kernel otter_version data_dir jobs_dir data_clean jobs_clean daemon service_exists service_state sqlite3 tar find uid; do
	s_val=$(get "$SOURCE_REPORT" "$key")
	t_val=$(get "$TARGET_REPORT" "$key")
	[ -n "$s_val" ] || fail "the source report has no $key: the probe did not complete"
	[ -n "$t_val" ] || fail "the target report has no $key: the probe did not complete"
done
ok "reports    both hosts answered the probe (source $s_role, target $t_role)"

# --- they are two machines ----------------------------------------------------

s_machine=$(get "$SOURCE_REPORT" machine_id)
t_machine=$(get "$TARGET_REPORT" machine_id)
s_key=$(get "$SOURCE_REPORT" ssh_host_key)
t_key=$(get "$TARGET_REPORT" ssh_host_key)
if [ "$s_machine" = "$t_machine" ] && [ "$s_key" = "$t_key" ]; then
	fail "source and target report the same machine ($s_machine, ssh host key $s_key): this is not a second host, and a restore on the same machine proves nothing about a clean host"
fi
ok "distinct   source $s_machine / target $t_machine (ssh host keys $(printf '%s' "$s_key" | cut -c1-12)… / $(printf '%s' "$t_key" | cut -c1-12)…)"

# --- the two hosts hold the same paths ----------------------------------------
# Checked before the cleanliness gate because the paths are what the restore
# writes to; a mismatch is refused whichever way it points.

s_data=$(get "$SOURCE_REPORT" data_dir)
t_data=$(get "$TARGET_REPORT" data_dir)
s_jobs=$(get "$SOURCE_REPORT" jobs_dir)
t_jobs=$(get "$TARGET_REPORT" jobs_dir)
[ "$s_data" = "$t_data" ] || fail "the data directory differs between the hosts ($s_data vs $t_data): releases and environment markers record absolute paths, so the restore must land at the path it was backed up from"
[ "$s_jobs" = "$t_jobs" ] || fail "the jobs root differs between the hosts ($s_jobs vs $t_jobs): job identity and the source recorded in every release are path-bound, and a source path cannot be repointed"
ok "paths      data $s_data, jobs $s_jobs on both hosts"

# --- THE assertion: the target is clean ---------------------------------------

t_clean=$(get "$TARGET_REPORT" data_clean)
t_reason=$(get "$TARGET_REPORT" data_reason)
[ "$t_clean" = yes ] || fail "the target data directory $t_data is not clean ($t_reason): a restore onto a host that already held a runtime is not a clean-host restore"
ok "clean      target data directory is empty or absent"

t_jobs_clean=$(get "$TARGET_REPORT" jobs_clean)
t_jobs_reason=$(get "$TARGET_REPORT" jobs_reason)
[ "$t_jobs_clean" = yes ] || fail "the target jobs root $t_jobs is not clean ($t_jobs_reason): restore job sources into an empty jobs root"
ok "clean      target jobs root is empty or absent"

t_daemon=$(get "$TARGET_REPORT" daemon)
t_detail=$(get "$TARGET_REPORT" daemon_detail)
[ "$t_daemon" = stopped ] || fail "a daemon already answers on the target at $(get "$TARGET_REPORT" api_url): stop it, and re-provision the instance clean"
ok "target     no daemon is serving on the target (${t_detail:-(no detail)})"

t_svc_state=$(get "$TARGET_REPORT" service_state)
t_svc_exists=$(get "$TARGET_REPORT" service_exists)
[ "$t_svc_exists" = yes ] || fail "the target has no systemd unit named $(get "$TARGET_REPORT" service) (service_state=$t_svc_state): the restored runtime needs a service to start, so provision the instance with the same unit as the source and leave it stopped"
[ "$t_svc_state" != active ] || fail "the target's $(get "$TARGET_REPORT" service) service is active: stop and disable it before the drill, or the machine is not clean"
ok "target     service $(get "$TARGET_REPORT" service) is installed and $t_svc_state"

# --- the source is live, and the two are the same shape -----------------------

s_daemon=$(get "$SOURCE_REPORT" daemon)
s_detail=$(get "$SOURCE_REPORT" daemon_detail)
[ "$s_daemon" = running ] || fail "no daemon answers on the source at $(get "$SOURCE_REPORT" api_url) (${s_detail:-no detail}): the point of the drill is a backup taken from a live runtime"
ok "live       the source runtime is serving"

s_kernel=$(get "$SOURCE_REPORT" kernel)
t_kernel=$(get "$TARGET_REPORT" kernel)
[ "$s_kernel" = Linux ] || fail "the source host reports kernel=$s_kernel; the runtime's supported target is Linux"
[ "$t_kernel" = Linux ] || fail "the target host reports kernel=$t_kernel; restore onto a Linux host of the same shape as the source"
ok "kernel     both hosts are Linux"

s_version=$(get "$SOURCE_REPORT" otter_version)
t_version=$(get "$TARGET_REPORT" otter_version)
[ "$s_version" != missing ] || fail "no runnable otter binary at $(get "$SOURCE_REPORT" otter_bin) on the source host"
[ "$t_version" != missing ] || fail "no runnable otter binary at $(get "$TARGET_REPORT" otter_bin) on the target host"
[ "$s_version" = "$t_version" ] || fail "the hosts run different versions ($s_version vs $t_version): restore the version that was backed up, because the schema is versioned"
ok "version    $s_version on both hosts"

# The CLI on the source must be the build its daemon is, or the operator is
# driving one build, backing up a second and restoring onto a third.
s_daemon_version=$(get "$SOURCE_REPORT" daemon_version)
if [ -n "$s_daemon_version" ] && [ "$s_daemon_version" != "-" ]; then
	[ "$s_daemon_version" = "$s_version" ] ||
		fail "the source daemon reports $s_daemon_version but its otter CLI reports $s_version: the database and the tooling around it are different builds"
	ok "version    the source daemon and its CLI agree ($s_daemon_version)"
fi

# --- the tools each side needs ------------------------------------------------

for side in source target; do
	if [ "$side" = source ]; then report=$SOURCE_REPORT; else report=$TARGET_REPORT; fi
	for tool in sqlite3 tar find sha256sum readlink; do
		have=$(get "$report" "$tool")
		[ "$have" = yes ] || fail "$side host has no $tool: the $side half of the restore cannot be performed or checked"
	done
done
ok "tools      sqlite3, tar, find, sha256sum, readlink on both hosts"

s_uid=$(get "$SOURCE_REPORT" uid)
t_uid=$(get "$TARGET_REPORT" uid)
if [ "$SUDO" != 1 ]; then
	[ "$s_uid" = 0 ] || fail "the source login is uid=$s_uid and DRILL_REMOTE_SUDO is not 1: the data directory is mode 0700 and owned by the service account, so a non-root login cannot read it"
	[ "$t_uid" = 0 ] || fail "the target login is uid=$t_uid and DRILL_REMOTE_SUDO is not 1: the restore must write the data directory and own it as the service account"
fi
ok "privilege  uid=$s_uid on the source, uid=$t_uid on the target (sudo=$SUDO)"

# --- ownership, when the target path already exists ---------------------------

t_exists=$(get "$TARGET_REPORT" data_exists)
if [ "$t_exists" = yes ]; then
	s_owner=$(get "$SOURCE_REPORT" data_owner)
	t_owner=$(get "$TARGET_REPORT" data_owner)
	if [ "$s_owner" != "$t_owner" ]; then
		note "the target data directory exists and is owned by $t_owner, the source's by $s_owner; the restore chowns it to the source's owner"
	fi
fi

echo "drill: ok: the target is a clean, empty host of the same shape; a restore onto it is a clean-host restore"
