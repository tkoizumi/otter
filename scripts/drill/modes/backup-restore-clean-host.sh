#!/bin/sh
# Drill: a hot backup of a LIVE runtime on one host restores onto a SECOND,
# initially empty host, and the restored job runs there. This is P0-03's
# clean-host half (CA-21, R-19); the same-host drill in backup-restore.sh
# proves the backup's contents, this one proves the restore at a distance.
#
# It is reached through the existing entry point, never directly in a recorded
# run:
#
#   DRILL_CLEAN_HOST=1 \
#   DRILL_SOURCE_HOST=root@10.0.0.11 DRILL_TARGET_HOST=root@10.0.0.12 \
#   DRILL_SSH_KEY=~/.ssh/castor.pem \
#   DRILL_REMOTE_DIR=/var/lib/otter DRILL_REMOTE_JOBS_DIR=/srv/otter/jobs \
#   DRILL_REMOTE_SERVICE=otter DRILL_JOB=my-job \
#     make drill DRILL=backup-restore
#
# This file is deliberately NOT a drill entry point: scripts/drill.sh runs
# scripts/drill/*.sh, and this one lives under modes/ so that a plain
# `make drill` cannot pick it up without parameters, refuse, and fail the whole
# suite. The recorded invocation is always the one above.
#
# Parameters. The first block is the machine pair and the install shape; the
# script refuses, naming every missing value, rather than guessing any of them:
#
#   required  DRILL_SOURCE_HOST       ssh destination of the LIVE runtime
#             DRILL_TARGET_HOST       ssh destination of the second, clean host
#             DRILL_SSH_KEY           local private key for both
#             DRILL_REMOTE_DIR        data directory, identical on both hosts
#             DRILL_REMOTE_JOBS_DIR   jobs root, identical on both hosts
#             DRILL_REMOTE_SERVICE    systemd unit that serves the runtime
#             DRILL_JOB               the job to execute on the restored host.
#                                     It must be safe to execute a second time
#                                     and must need no secret: the drill runs it
#                                     for real. A job the operator has not
#                                     vouched for is never auto-selected.
#   optional  DRILL_REMOTE_BIN        `/usr/local/bin/otter`
#             DRILL_REMOTE_API_URL    `http://127.0.0.1:7337`. Its port has to be
#                                     the port the unit listens on; the drill
#                                     refuses when the two disagree.
#             DRILL_REMOTE_BACKUP_DIR `/var/tmp/otter-drill-backup`
#             DRILL_REMOTE_SUDO       `1` when the ssh login is not root. Remote
#                                     commands then run as `sudo -n env ...`;
#                                     `env` rather than a bare `VAR=value`
#                                     prefix, which sudo only accepts when the
#                                     sudoers rule grants SETENV.
#             DRILL_REMOTE_ENV_FILE   the environment file holding
#                                     OTTER_API_TOKEN, when discovery picks the
#                                     wrong one (see below)
#             DRILL_REMOTE_API_TOKEN  the token itself; used only when neither
#                                     the unit's files nor a single environment
#                                     file resolve one
#             DRILL_REMOTE_ETC_DIR    where to look for environment files
#                                     (`/etc/otter`)
#             DRILL_RUN_LIMIT         how many recent run ids to check (25)
#             DRILL_KEEP_TARGET       `1` leaves the target daemon and the staged
#                                     archives in place for inspection
#             DRILL_SABOTAGE          drop-releases | drop-environments |
#                                     drop-tools | drop-jobs; a sabotage run is
#                                     expected to go red, and reaching the end
#                                     with one set is itself a failure
#
# The API token is a precondition with its own checks, because the obvious
# setup does not work by itself: `otter deploy` writes the token to
# `/etc/otter/workspaces/<workspace>.env`, and the CLI's own discovery globs
# only `/etc/otter/*.env` -- `*` does not cross `workspaces/`. Left alone, every
# call would 401 and this drill would report it as "the runtime does not know
# this job". So the probe resolves a token file per host (the serving unit's own
# `EnvironmentFiles=` first, then `/etc/otter/*.env`, then a single
# `/etc/otter/workspaces/*.env`), reports `ambiguous` rather than guessing when
# several candidates hold different tokens, and checks that the token actually
# authenticates before the drill backs anything up. `lib/run-otter.sh` then
# reads it inside the remote shell, so the secret never reaches a command line
# or the transcript.
#
# What it does, in order:
#
#   1. probes BOTH hosts (scripts/drill/lib/host-report.sh) and refuses unless
#      they are two different Linux machines of the same shape -- same data
#      directory, same jobs root, same `otter` version, a resolvable API token,
#      and units that name those same paths and the same port -- and the TARGET
#      is genuinely clean: its data directory and jobs root are empty or absent,
#      no daemon answers there, and its service is installed but not active.
#      This is the assertion that makes the run a clean-host restore at all;
#   2. takes a complete backup on the live source (lib/hot-backup.sh): the
#      database via SQLite's online backup while the daemon keeps writing, plus
#      `.releases/`, `environments/`, `tools/`, `python/`, `cache/` and the job
#      source directories that carry `.otter-id`;
#   3. streams that archive through this machine to the target (host-to-host
#      trust is not required) and verifies it against MANIFEST.sha256 there;
#   4. restores it into the same absolute paths, with the source's ownership;
#   5. starts the target's service and asserts, against the restored runtime:
#      job identity (`.otter-id` and the runtime's own answer), run history,
#      durable state, the release snapshot and its `active` link, the prepared
#      environment and its interpreter, and -- the point -- a NEW run of the job
#      that succeeds, binds to the restored release and environment, and is
#      unknown to the source daemon, which is what proves it ran on the second
#      host.
#
# What it explicitly does NOT prove:
#   * that the source is production. It proves a backup of a running runtime
#     restores onto another host; which runtime that is, the operator chose.
#   * that the two hosts are physically different. It asserts their machine ids
#     and ssh host keys differ, which is the strongest claim two ssh logins can
#     support.
#   * that job secrets survive. `/etc/otter/*.env` is deliberately NOT in the
#     backup, so a job that needs a secret will fail on the target until the
#     secrets are restored separately. The drill only runs a job the operator
#     names with DRILL_JOB, and the operator must pick one whose execution on a
#     second host is safe and needs no secret.
#   * that a managed interpreter built on the source runs on the target when the
#     instances differ in architecture or libc. It runs the restored
#     interpreter and reports what happens; a failure there is a real finding,
#     not a drill artifact.
#   * that `otter prepare` can rebuild `python/` or `cache/uv/`. They are copied
#     when present and are reconstructable by design.
#
# Falsifiability. DRILL_SABOTAGE=<mode> omits one member from the backup and the
# run MUST go red at the assertion that member backs:
#
#   drop-releases      no release snapshot: the restored run has no active
#                      release.
#   drop-environments  no prepared environment: the managed run refuses.
#   drop-tools         vendored uv missing: the environment digest resolves
#                      differently and the run refuses.
#   drop-jobs          no job sources, so no `.otter-id` and no job to run.
#
# Run a clean run first and record it; a sabotage run is only evidence when the
# clean run is green and the sabotage is red for the named reason.
#
# Honest notes for the transcript:
#   * every ssh call this script makes is listed by `sh -x` output if you run it
#     with `sh -x`; the transcript is the assertion of what ran;
#   * the transport is `ssh` unless DRILL_TRANSPORT overrides it, and an
#     override is announced loudly because a run with one is NOT host evidence.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
LIB="$root/scripts/drill/lib"

# --- the transport ------------------------------------------------------------
# `ssh` and nothing else in a recorded run. The selfcheck points DRILL_TRANSPORT
# at a fake that answers the preflight from local directories; that run says so
# in its first lines, and its transcript cannot be mistaken for this one.
TRANSPORT=${DRILL_TRANSPORT:-ssh}
case "$TRANSPORT" in
/*) [ -x "$TRANSPORT" ] || {
	echo "drill: DRILL_TRANSPORT=$TRANSPORT is not executable" >&2
	exit 2
} ;;
*)
	command -v "$TRANSPORT" >/dev/null 2>&1 || {
		echo "drill: $TRANSPORT is required to reach the hosts and was not found on PATH" >&2
		exit 2
	}
	;;
esac

# Locally this script needs to stream the archive, inspect it, and read the
# backup database for ground truth.
command -v tar >/dev/null 2>&1 || {
	echo "drill: tar is required locally to move and inspect the archive" >&2
	exit 2
}
command -v sqlite3 >/dev/null 2>&1 || {
	echo "drill: sqlite3 is required locally to read ground truth from the backup database" >&2
	exit 2
}
if command -v sha256sum >/dev/null 2>&1; then
	sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256_of() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	echo "drill: sha256sum (or shasum) is required locally to compare the identity marker across the hosts" >&2
	exit 2
fi

# --- parameters ---------------------------------------------------------------
# Refuse rather than fall back. Every value below is used to build a remote
# shell command, so a value that could change the shape of that command is
# rejected outright rather than quoted and hoped for.

missing=""
require() {
	eval "value=\${$1:-}"
	if [ -z "$value" ]; then missing="$missing $1"; fi
}
require DRILL_SOURCE_HOST
require DRILL_TARGET_HOST
require DRILL_SSH_KEY
require DRILL_REMOTE_DIR
require DRILL_REMOTE_JOBS_DIR
require DRILL_REMOTE_SERVICE
require DRILL_JOB
if [ -n "$missing" ]; then
	echo "drill: the clean-host mode needs parameters that are not set:$missing" >&2
	echo "drill: required: DRILL_SOURCE_HOST DRILL_TARGET_HOST DRILL_SSH_KEY" >&2
	echo "drill:           DRILL_REMOTE_DIR DRILL_REMOTE_JOBS_DIR DRILL_REMOTE_SERVICE DRILL_JOB" >&2
	echo "drill: optional: DRILL_REMOTE_BIN (default /usr/local/bin/otter)," >&2
	echo "drill:           DRILL_REMOTE_API_URL (default http://127.0.0.1:7337)," >&2
	echo "drill:           DRILL_REMOTE_BACKUP_DIR (default /var/tmp/otter-drill-backup)," >&2
	echo "drill:           DRILL_REMOTE_SUDO=1 when the login is not root," >&2
	echo "drill:           DRILL_REMOTE_ENV_FILE when the token file must be named," >&2
	echo "drill:           DRILL_REMOTE_API_TOKEN, DRILL_REMOTE_ETC_DIR, DRILL_RUN_LIMIT," >&2
	echo "drill:           DRILL_SABOTAGE, DRILL_KEEP_TARGET=1" >&2
	echo "drill: refusing to fall back to the same-host drill: that would record evidence for the wrong claim" >&2
	exit 2
fi

SOURCE_HOST=$DRILL_SOURCE_HOST
TARGET_HOST=$DRILL_TARGET_HOST
SSH_KEY=$DRILL_SSH_KEY
DATA_DIR=$DRILL_REMOTE_DIR
JOBS_DIR=$DRILL_REMOTE_JOBS_DIR
SERVICE=$DRILL_REMOTE_SERVICE
JOB=$DRILL_JOB
REMOTE_BIN=${DRILL_REMOTE_BIN:-/usr/local/bin/otter}
API_URL=${DRILL_REMOTE_API_URL:-http://127.0.0.1:7337}
BACKUP_ROOT=${DRILL_REMOTE_BACKUP_DIR:-/var/tmp/otter-drill-backup}
API_TOKEN=${DRILL_REMOTE_API_TOKEN:-}
ENV_FILE=${DRILL_REMOTE_ENV_FILE:-}
ETC_DIR=${DRILL_REMOTE_ETC_DIR:-/etc/otter}
SUDO_MODE=${DRILL_REMOTE_SUDO:-0}
RUN_LIMIT=${DRILL_RUN_LIMIT:-25}
SABOTAGE=${DRILL_SABOTAGE:-}
KEEP_TARGET=${DRILL_KEEP_TARGET:-0}

case "$SABOTAGE" in
"" | drop-releases | drop-environments | drop-tools | drop-jobs) ;;
*)
	echo "drill: unknown DRILL_SABOTAGE=$SABOTAGE" >&2
	echo "drill: valid modes: drop-releases, drop-environments, drop-tools, drop-jobs" >&2
	exit 2
	;;
esac
case "$SUDO_MODE" in
0 | 1) ;;
*)
	echo "drill: DRILL_REMOTE_SUDO must be 0 or 1, got $SUDO_MODE" >&2
	exit 2
	;;
esac
case "$RUN_LIMIT" in
'' | *[!0-9]*)
	echo "drill: DRILL_RUN_LIMIT must be a positive integer, got $RUN_LIMIT" >&2
	exit 2
	;;
esac
[ "$RUN_LIMIT" -gt 0 ] || {
	echo "drill: DRILL_RUN_LIMIT must be a positive integer, got $RUN_LIMIT" >&2
	exit 2
}

# reject_chars refuses a parameter that could not survive `ssh host "<cmd>"`
# unchanged. The set is everything the drill legitimately passes: hosts,
# absolute paths, a URL, a unit name and a job reference.
reject_chars() {
	name=$1
	eval "value=\${$name:-}"
	[ -n "$value" ] || return 0
	case "$value" in
	*[!A-Za-z0-9@._:/=+,-]*)
		echo "drill: $name contains characters that cannot be passed to a remote shell: $value" >&2
		exit 2
		;;
	esac
}
for name in DRILL_SOURCE_HOST DRILL_TARGET_HOST DRILL_REMOTE_DIR DRILL_REMOTE_JOBS_DIR \
	DRILL_REMOTE_SERVICE DRILL_JOB DRILL_REMOTE_BIN DRILL_REMOTE_API_URL DRILL_REMOTE_BACKUP_DIR \
	DRILL_REMOTE_API_TOKEN DRILL_REMOTE_ENV_FILE DRILL_REMOTE_ETC_DIR; do
	reject_chars "$name"
done

case "$DATA_DIR" in
/*) ;;
*) echo "drill: DRILL_REMOTE_DIR must be an absolute path, got $DATA_DIR" >&2; exit 2 ;;
esac
case "$ENV_FILE" in
'' | /*) ;;
*) echo "drill: DRILL_REMOTE_ENV_FILE must be an absolute path, got $ENV_FILE" >&2; exit 2 ;;
esac
case "$ETC_DIR" in
/*) ;;
*) echo "drill: DRILL_REMOTE_ETC_DIR must be an absolute path, got $ETC_DIR" >&2; exit 2 ;;
esac
case "$JOBS_DIR" in
/*) ;;
*) echo "drill: DRILL_REMOTE_JOBS_DIR must be an absolute path, got $JOBS_DIR" >&2; exit 2 ;;
esac
case "$BACKUP_ROOT" in
/*) ;;
*) echo "drill: DRILL_REMOTE_BACKUP_DIR must be an absolute path, got $BACKUP_ROOT" >&2; exit 2 ;;
esac
case "$REMOTE_BIN" in
/*) ;;
*) echo "drill: DRILL_REMOTE_BIN must be an absolute path, got $REMOTE_BIN" >&2; exit 2 ;;
esac
case "$JOB" in
/* | */* | ./ | ../) ;;
*[!A-Za-z0-9@._:-]*)
	echo "drill: DRILL_JOB looks neither like a job name nor an absolute path: $JOB" >&2
	exit 2
	;;
esac
[ "$SOURCE_HOST" != "$TARGET_HOST" ] || {
	echo "drill: DRILL_SOURCE_HOST and DRILL_TARGET_HOST are the same ($SOURCE_HOST); this drill needs two hosts" >&2
	exit 2
}
[ -f "$SSH_KEY" ] || {
	echo "drill: DRILL_SSH_KEY=$SSH_KEY is not a file" >&2
	exit 2
}
[ -r "$SSH_KEY" ] || {
	echo "drill: DRILL_SSH_KEY=$SSH_KEY is not readable" >&2
	exit 2
}

SSH_OPTS="-i $SSH_KEY -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15"
# `env` after sudo, not bare `VAR=value` prefixes: sudo only accepts an
# environment assignment in that position when the sudoers rule grants SETENV,
# and a rule that does not would fail as "command not found: OTTER_API_URL=...".
# Running `env` as root needs no such grant.
if [ "$SUDO_MODE" = 1 ]; then SUDO="sudo -n env"; else SUDO=""; fi

# --- shell state --------------------------------------------------------------

# A developer's shell must not redirect this run.
unset OTTER_API_URL OTTER_API_TOKEN OTTER_DATA_DIR OTTER_JOBS_DIR OTTER_LISTEN 2>/dev/null || true
unset OTTER_PROJECT_ROOT OTTER_SERVE_DIR 2>/dev/null || true

tmpbase=${TMPDIR:-/tmp}
while [ "$tmpbase" != "${tmpbase%/}" ]; do tmpbase=${tmpbase%/}; done
[ -n "$tmpbase" ] || tmpbase=/tmp
WORK=$(mktemp -d "$tmpbase/otter-drill-clean-host.XXXXXX")
START_EPOCH=$(date +%s)
RUN_ID=$(date -u '+%Y%m%dT%H%M%SZ')-$$
SRC_STAGE="$BACKUP_ROOT/$RUN_ID"
TGT_STAGE="$BACKUP_ROOT/$RUN_ID"
ARCHIVE="$WORK/backup.tgz"
TOOK=0
BACKUP_ROOT_SET=0
TARGET_STARTED=0
cleaning=0

say() { echo "drill: $*"; }
fail() {
	echo "drill: FAILED: $*" >&2
	exit 1
}

# get reads one key out of a probe report.
get() { sed -n "s/^$1=//p" "$2" 2>/dev/null | head -n 1; }

cleanup() {
	status=$?
	[ "$cleaning" -eq 1 ] && exit "$status"
	cleaning=1

	if [ "$TARGET_STARTED" -eq 1 ]; then
		if [ "$KEEP_TARGET" = 1 ]; then
			echo "drill: leaving the target daemon running (DRILL_KEEP_TARGET=1)" >&2
		else
			remote_cmd "$TARGET_HOST" "systemctl stop $SERVICE" >/dev/null 2>&1 || true
		fi
	fi

	if [ "$status" -ne 0 ] && [ "$TOOK" -eq 1 ]; then
		echo "drill: failed; last target daemon lines:" >&2
		remote_cmd "$TARGET_HOST" "journalctl -u $SERVICE -n 40 --no-pager" >&2 2>/dev/null || true
	fi

	# Staged archives are 0700 directories under $BACKUP_ROOT on both hosts. A
	# failure leaves them for diagnosis only when the operator asked for that;
	# otherwise a repeated failure would accumulate them in /var/tmp forever.
	if [ "$status" -ne 0 ] && { [ "$TOOK" -eq 1 ] || [ "$BACKUP_ROOT_SET" -eq 1 ]; }; then
		if [ "$KEEP_TARGET" = 1 ]; then
			echo "drill: staged archives left in place (DRILL_KEEP_TARGET=1):" >&2
			echo "drill:   $SOURCE_HOST:$SRC_STAGE" >&2
			echo "drill:   $TARGET_HOST:$TGT_STAGE" >&2
		else
			remote_cmd "$SOURCE_HOST" "rm -rf $SRC_STAGE" >/dev/null 2>&1 || true
			remote_cmd "$TARGET_HOST" "rm -rf $TGT_STAGE" >/dev/null 2>&1 || true
			echo "drill: removed the staged archives on both hosts ($BACKUP_ROOT/$RUN_ID)" >&2
			echo "drill: pass DRILL_KEEP_TARGET=1 to leave them behind for diagnosis" >&2
		fi
	fi
	if [ "$status" -ne 0 ]; then
		echo "drill: workspace was $WORK" >&2
	fi

	echo "drill: elapsed $(( $(date +%s) - START_EPOCH ))s" >&2
	if [ -n "${OTTER_DRILL_KEEP:-}" ] || { [ "$status" -ne 0 ] && [ -z "${CI:-}" ]; }; then
		echo "drill: keeping $WORK" >&2
	else
		rm -rf "$WORK"
	fi
	exit "$status"
}
trap cleanup EXIT INT TERM

# --- remote execution ---------------------------------------------------------
# remote_cmd runs one command line on a host. remote_script pipes a script from
# this checkout to `sh -s -- args` on the host, which is how the non-trivial
# steps cross ssh without a quoting layer between them.

remote_cmd() {
	remote_host=$1
	remote_line=$2
	if [ -n "$SUDO" ]; then
		"$TRANSPORT" $SSH_OPTS "$remote_host" "$SUDO $remote_line"
	else
		"$TRANSPORT" $SSH_OPTS "$remote_host" "$remote_line"
	fi
}

remote_script() {
	remote_host=$1
	remote_file=$2
	shift 2
	if [ -n "$SUDO" ]; then
		"$TRANSPORT" $SSH_OPTS "$remote_host" "$SUDO sh -s -- $*" <"$remote_file"
	else
		"$TRANSPORT" $SSH_OPTS "$remote_host" "sh -s -- $*" <"$remote_file"
	fi
}

# remote_otter asks the daemon on a host through its own CLI.
#
# It runs the CLI through lib/run-otter.sh, which reads OTTER_API_TOKEN out of
# the environment file the daemon itself loads and hands it over in the CLI's
# environment. Two reasons:
#   * `otter deploy` writes the token to /etc/otter/workspaces/<ws>.env, which
#     the CLI's own discovery glob (/etc/otter/*.env) does not cross -- without
#     this every call would 401 and the drill would report it as "the runtime
#     does not know this job";
#   * the token never appears in a command line or in this transcript, and
#     reading it as root under sudo works whether or not the login can read a
#     0600 file.
# DRILL_REMOTE_API_TOKEN is the explicit override and wins over the file.
remote_otter() {
	remote_host=$1
	shift
	if [ -n "$API_TOKEN" ]; then
		# The explicit override travels in the command line, so it is visible in
		# the host's process list for the length of the call. That is the cost of
		# the override; the discovered-file path above does not pay it.
		remote_cmd "$remote_host" "OTTER_API_TOKEN=$API_TOKEN OTTER_API_URL=$API_URL $REMOTE_BIN $*"
		return
	fi
	remote_script "$remote_host" "$LIB/run-otter.sh" "$(token_file_for "$remote_host")" \
		"$API_URL" "$REMOTE_BIN" $*
}

# token_file_for echoes the token file the probe resolved on a host, or `-`.
token_file_for() {
	case "$1" in
	"$SOURCE_HOST") printf '%s' "${SRC_TOKEN_FILE:--}" ;;
	"$TARGET_HOST") printf '%s' "${TGT_TOKEN_FILE:--}" ;;
	*) printf '%s' "-" ;;
	esac
}

# --- 0. announce --------------------------------------------------------------

echo "drill: clean host mode: $SOURCE_HOST -> $TARGET_HOST"
echo "drill: data $DATA_DIR jobs $JOBS_DIR service $SERVICE job $JOB"
echo "drill: api $API_URL bin $REMOTE_BIN sudo=$SUDO_MODE sabotage=${SABOTAGE:-none} run limit $RUN_LIMIT"
if [ "$TRANSPORT" != ssh ]; then
	echo "drill: *** TRANSPORT OVERRIDDEN: DRILL_TRANSPORT=$TRANSPORT ***"
	echo "drill: *** this run answers the preflight from local state; it is NOT host evidence ***"
fi

# --- 1. preflight: two hosts, same shape, clean target ------------------------

say "probing $SOURCE_HOST"
remote_script "$SOURCE_HOST" "$LIB/host-report.sh" source "$DATA_DIR" "$JOBS_DIR" "$REMOTE_BIN" "$API_URL" "$SERVICE" "$API_TOKEN" "$ENV_FILE" "$ETC_DIR" >"$WORK/source.report" ||
	fail "could not probe the source host $SOURCE_HOST"
say "probing $TARGET_HOST"
remote_script "$TARGET_HOST" "$LIB/host-report.sh" target "$DATA_DIR" "$JOBS_DIR" "$REMOTE_BIN" "$API_URL" "$SERVICE" "$API_TOKEN" "$ENV_FILE" "$ETC_DIR" >"$WORK/target.report" ||
	fail "could not probe the target host $TARGET_HOST"

# The token file each host resolved, for remote_otter. `-` means the daemon
# needs none.
SRC_TOKEN_FILE=$(get token_file "$WORK/source.report")
TGT_TOKEN_FILE=$(get token_file "$WORK/target.report")

say "source  $(get hostname "$WORK/source.report") (kernel $(get kernel "$WORK/source.report"), version $(get otter_version "$WORK/source.report"), daemon $(get daemon "$WORK/source.report"))"
say "target  $(get hostname "$WORK/target.report") (kernel $(get kernel "$WORK/target.report"), version $(get otter_version "$WORK/target.report"), daemon $(get daemon "$WORK/target.report"))"
say "target data $(get data_dir "$WORK/target.report") clean=$(get data_clean "$WORK/target.report") reason=$(get data_reason "$WORK/target.report")"
say "target jobs $(get jobs_dir "$WORK/target.report") clean=$(get jobs_clean "$WORK/target.report") reason=$(get jobs_reason "$WORK/target.report")"
say "unit    $(get service "$WORK/source.report") -> $(get unit_data_dir "$WORK/source.report") + $(get unit_jobs_dir "$WORK/source.report") on $(get unit_listen "$WORK/source.report")"
say "token   source $(get token_source "$WORK/source.report") $(get token_file "$WORK/source.report"), target $(get token_source "$WORK/target.report") $(get token_file "$WORK/target.report")"

sh "$LIB/assert-clean-host.sh" "$WORK/source.report" "$WORK/target.report" "$SUDO_MODE" ||
	fail "the two hosts are not a legal source/target pair; see the refusal above"

SRC_MACHINE=$(get machine_id "$WORK/source.report" | cut -c1-12)
TGT_MACHINE=$(get machine_id "$WORK/target.report" | cut -c1-12)
# The runtime answers with the physical path it resolved, which can differ from
# the operator's spelling of the jobs root when a path component is a symlink.
JOBS_REAL=$(get jobs_real "$WORK/source.report")
TGT_JOBS_REAL=$(get jobs_real "$WORK/target.report")
if [ "$TGT_JOBS_REAL" != "$JOBS_REAL" ]; then
	say "note: the jobs root resolves to $JOBS_REAL on the source and $TGT_JOBS_REAL on the target"
fi

# --- 2. ground truth from the live runtime ------------------------------------

# The runtime answers for identity and location; nothing here trusts a
# directory name. One call, so the two answers describe the same moment.
# The failure text is reported as-is: a 401 here means the token could not be
# resolved, which is a different problem from a job that does not exist, and the
# operator should not have to guess which one they have.
if ! INSPECT=$(remote_otter "$SOURCE_HOST" "inspect $JOB" 2>&1); then
	echo "drill: otter inspect failed on $SOURCE_HOST:" >&2
	printf '%s\n' "$INSPECT" | sed 's/^/drill:   /' >&2
	fail "the source runtime did not answer inspect for $JOB (a 401 or 'requires an API token' here is the token, not the job; see DRILL_REMOTE_ENV_FILE and DRILL_REMOTE_API_TOKEN)"
fi
SRC_JOBID=$(printf '%s\n' "$INSPECT" | sed -n 's/^job: *//p' | head -n 1)
[ -n "$SRC_JOBID" ] || fail "otter inspect on the source printed no job id for $JOB"
SRC_JOBDIR=$(printf '%s\n' "$INSPECT" | sed -n 's/^path: *//p' | head -n 1)
[ -n "$SRC_JOBDIR" ] || fail "the source runtime did not report a path for job $JOB"
# The path comes back from the host, not from the operator, so it gets the same
# character check a parameter does before it is used to build a remote command.
case "$SRC_JOBDIR" in
*[!A-Za-z0-9@._:/=+,-]*)
	fail "the job's source path contains characters the drill cannot pass to a remote shell: $SRC_JOBDIR"
	;;
esac
case "$SRC_JOBDIR" in
"$JOBS_REAL"/*) ;;
*) fail "job $JOB lives at $SRC_JOBDIR, which is not under the jobs root $JOBS_REAL the drill backs up" ;;
esac
JOB_REL=${SRC_JOBDIR#"$JOBS_REAL"/}
if ! SRC_MARKER=$(remote_cmd "$SOURCE_HOST" "cat $SRC_JOBDIR/.otter-id"); then
	fail "could not read $SRC_JOBDIR/.otter-id on the source host"
fi
[ "$SRC_MARKER" = "$SRC_JOBID" ] ||
	fail "the live runtime reports job $SRC_JOBID but $SRC_JOBDIR/.otter-id says '$SRC_MARKER'"
say "job         $SRC_JOBID at $SRC_JOBDIR (.otter-id agrees)"

# --- 3. the hot backup on the source ------------------------------------------

say "backing up   $SOURCE_HOST:$DATA_DIR (daemon still serving)"
remote_cmd "$SOURCE_HOST" "install -d -m 0700 $BACKUP_ROOT" ||
	fail "could not create $BACKUP_ROOT on the source host"
BACKUP_ROOT_SET=1
remote_script "$SOURCE_HOST" "$LIB/hot-backup.sh" "$DATA_DIR" "$JOBS_DIR" "$SRC_STAGE" "$SABOTAGE" "$SRC_MACHINE" ||
	fail "the backup on the source host failed"

# --- 4. move the archive through this machine ---------------------------------

# Nothing about the two hosts has to trust each other: the archive is read from
# the source and written to the target by this process.
say "transferring $SOURCE_HOST:$SRC_STAGE -> $TARGET_HOST:$TGT_STAGE (through $(hostname))"
remote_cmd "$SOURCE_HOST" "tar -C $SRC_STAGE -czf - ." >"$ARCHIVE" ||
	fail "could not read the backup archive from the source host"
BYTES=$(wc -c <"$ARCHIVE" | tr -d ' ')
[ "$BYTES" -gt 0 ] || fail "the backup archive is empty"
tar tzf "$ARCHIVE" >"$WORK/archive.list" 2>/dev/null ||
	fail "the archive read from the source host is not a tar archive"
grep -q 'otter\.db$' "$WORK/archive.list" || fail "the archive holds no otter.db"
grep -q 'RECORD$' "$WORK/archive.list" || fail "the archive holds no RECORD"
grep -q 'MANIFEST\.sha256$' "$WORK/archive.list" || fail "the archive holds no MANIFEST.sha256"
say "archive     $BYTES bytes, $(wc -l <"$WORK/archive.list" | tr -d ' ') entries"

TOOK=1
remote_cmd "$TARGET_HOST" "install -d -m 0700 $TGT_STAGE" ||
	fail "could not create $TGT_STAGE on the target host"
remote_cmd "$TARGET_HOST" "tar -C $TGT_STAGE -xzf -" <"$ARCHIVE" ||
	fail "could not write the backup archive onto the target host"

# The archive is also this drill's ground truth: what the backup holds is what
# the restore must reproduce, and the live source keeps moving after the copy.
mkdir -p "$WORK/backup"
tar -xzf "$ARCHIVE" -C "$WORK/backup" || fail "could not expand the archive locally"
[ -f "$WORK/backup/$JOB_REL/.otter-id" ] ||
	fail "the backup holds no $JOB_REL/.otter-id: the job's identity did not make it into the archive"
EXPECT_MARKER=$(sha256_of "$WORK/backup/$JOB_REL/.otter-id")
EXPECT_MARKER_SRC=$(remote_cmd "$SOURCE_HOST" "sha256sum $SRC_JOBDIR/.otter-id" | cut -d' ' -f1)
[ "$EXPECT_MARKER" = "$EXPECT_MARKER_SRC" ] ||
	fail "the marker in the archive does not match the marker on the source host"

REL_LINK="$WORK/backup/data/.releases/active/$SRC_JOBID"
[ -L "$REL_LINK" ] || fail "the backup holds no active release link for $SRC_JOBID"
REL_TARGET=$(readlink "$REL_LINK")
REL_DIGEST=$(basename "$REL_TARGET")
META="$WORK/backup/data/.releases/$SRC_JOBID/$REL_DIGEST/otter-release.json"
[ -f "$META" ] || fail "the backup holds no release metadata at $META"
ENV_DIGEST=$(sed -n 's/^[[:space:]]*"environment": "\([^"]*\)".*/\1/p' "$META")
REL_SOURCE=$(sed -n 's/^[[:space:]]*"source": "\([^"]*\)".*/\1/p' "$META")
[ -n "$ENV_DIGEST" ] || fail "could not read the environment digest from $META"
[ -n "$REL_SOURCE" ] || fail "could not read the recorded source from $META"
[ "$REL_SOURCE" = "$SRC_JOBDIR" ] ||
	fail "the release records source $REL_SOURCE, not the live job directory $SRC_JOBDIR"
say "release     $REL_DIGEST, environment $ENV_DIGEST, source $REL_SOURCE"

DB="$WORK/backup/otter.db"
EXPECT_STATE=$(sqlite3 "$DB" "select key || '=' || value from job_state where job_id='$SRC_JOBID' order by key;") ||
	fail "could not read durable state from the backup database"
STATE_KEY=$(sqlite3 "$DB" "select key from job_state where job_id='$SRC_JOBID' order by key limit 1;") ||
	fail "could not read a state key from the backup database"
# The key is read out of the database, not typed by the operator, and it is
# about to be interpolated into a remote command like every other value that
# crosses ssh. The row-for-row comparison below is the real state assertion;
# this one only proves the DAEMON serves the restored state, so a key the drill
# cannot pass safely is dropped rather than forced through.
case "$STATE_KEY" in
'' ) ;;
*[!A-Za-z0-9._:-]*)
	say "note: state key '$STATE_KEY' cannot be passed to a remote shell; the row-for-row database comparison still checks the state"
	STATE_KEY=""
	;;
esac
EXPECT_RUNS=$(sqlite3 "$DB" "select id from runs where job_id='$SRC_JOBID' order by created_at desc limit $RUN_LIMIT;") ||
	fail "could not read run history from the backup database"
TOTAL_RUNS=$(sqlite3 "$DB" "select count(*) from runs where job_id='$SRC_JOBID';") ||
	fail "could not count run history in the backup database"
EXPECT_RUN_COUNT=$(printf '%s\n' "$EXPECT_RUNS" | grep -c . || true)
say "snapshot    $TOTAL_RUNS runs in total, $EXPECT_RUN_COUNT checked; ${EXPECT_STATE:+state $STATE_KEY present}"

# --- 5. restore on the target -------------------------------------------------

# The restored trees must be owned by the account the source's are, or the
# daemon cannot read the database it just restored.
OWNER=$(get data_owner "$WORK/source.report")
OWNER_USER=${OWNER%%:*}
OWNER_GROUP=${OWNER#*:}
if [ -z "$OWNER_USER" ] || [ -z "$OWNER_GROUP" ] || [ "$OWNER" = "$OWNER_USER" ]; then
	fail "could not read the source data directory's owner from the probe; refusing to restore with unknown ownership"
fi
case "$OWNER_USER$OWNER_GROUP" in
*[!A-Za-z0-9._-]*)
	fail "the source data directory's owner is not a user:group pair the drill can pass on: $OWNER"
	;;
esac

# The account has to exist on the target: restore-host.sh chowns the restored
# trees to it, and a chown to a name that is not there leaves a daemon that
# cannot read its own database.
if ! remote_cmd "$TARGET_HOST" "getent passwd $OWNER_USER >/dev/null && getent group $OWNER_GROUP >/dev/null"; then
	fail "the target has no account $OWNER: the restore would chown the data directory and jobs root to an owner that does not exist there"
fi
say "owner       $OWNER exists on $TARGET_HOST"

say "restoring    $TARGET_HOST:$DATA_DIR from the archive"
remote_script "$TARGET_HOST" "$LIB/restore-host.sh" "$TGT_STAGE" "$DATA_DIR" "$JOBS_DIR" "$OWNER" ||
	fail "the restore on the target host failed"

say "starting     systemctl start $SERVICE on $TARGET_HOST"
remote_cmd "$TARGET_HOST" "systemctl start $SERVICE" ||
	fail "could not start $SERVICE on the target host"
TARGET_STARTED=1
i=0
until remote_otter "$TARGET_HOST" "status" >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -lt 300 ] || fail "the restored daemon did not answer at $API_URL on the target host"
	sleep 0.5
done
say "restored     the target daemon is serving $DATA_DIR"

# The build that came up on the target must be the build that wrote the
# database: the schema is versioned, and a restore onto a newer daemon is a
# migration, not a restore.
TGT_STATUS=$(remote_otter "$TARGET_HOST" "status" || true)
TGT_DAEMON_VERSION=$(printf '%s\n' "$TGT_STATUS" | sed -n 's/^version: *//p' | head -n 1)
[ -n "$TGT_DAEMON_VERSION" ] || fail "the restored daemon answered but reported no version"
case "$TGT_DAEMON_VERSION" in
*[!A-Za-z0-9@._:/=+,-]*)
	fail "the restored daemon reported an unusable version string: $TGT_DAEMON_VERSION"
	;;
esac
SRC_DAEMON_VERSION=$(get daemon_version "$WORK/source.report")
if [ -n "$SRC_DAEMON_VERSION" ] && [ "$SRC_DAEMON_VERSION" != "-" ]; then
	[ "$TGT_DAEMON_VERSION" = "$SRC_DAEMON_VERSION" ] ||
		fail "the restored daemon runs $TGT_DAEMON_VERSION but the source daemon runs $SRC_DAEMON_VERSION"
	say "version     restored daemon runs the same build as the source ($TGT_DAEMON_VERSION)"
fi

# --- 6. the restored runtime is a runtime -------------------------------------

# 6a. identity: the marker on the target is byte-identical to the archived one,
# and the runtime reads the same job id out of it.
TGT_MARKER=$(remote_cmd "$TARGET_HOST" "sha256sum $JOBS_DIR/$JOB_REL/.otter-id" | cut -d' ' -f1) ||
	fail "could not hash $JOBS_DIR/$JOB_REL/.otter-id on the target host"
[ -n "$TGT_MARKER" ] || fail "the target has no $JOBS_DIR/$JOB_REL/.otter-id"
[ "$TGT_MARKER" = "$EXPECT_MARKER" ] ||
	fail "the restored .otter-id marker differs from the one in the backup: the job's identity did not survive"
TGT_JOBID=$(remote_otter "$TARGET_HOST" "inspect $JOB" 2>&1) || {
	echo "drill: otter inspect failed on $TARGET_HOST:" >&2
	printf '%s\n' "$TGT_JOBID" | sed 's/^/drill:   /' >&2
	fail "the restored runtime did not answer inspect for $JOB: the daemon is up, so this is the runtime's own error, not a missing job"
}
TGT_JOBID=$(printf '%s\n' "$TGT_JOBID" | sed -n 's/^job: *//p' | head -n 1)
[ -n "$TGT_JOBID" ] || fail "otter inspect on the target printed no job id for $JOB"
[ "$TGT_JOBID" = "$SRC_JOBID" ] ||
	fail "job identity changed across the restore: $SRC_JOBID -> ${TGT_JOBID:-missing}"
say "identity    survived ($SRC_JOBID, marker sha256 $EXPECT_MARKER)"

# 6b. run history reads back.
remote_otter "$TARGET_HOST" "runs --all --limit 500" >"$WORK/target-runs.out" 2>&1 ||
	fail "otter runs failed against the restored runtime"
missing_runs=""
for run in $EXPECT_RUNS; do
	grep -q "$run" "$WORK/target-runs.out" || missing_runs="$missing_runs $run"
done
[ -z "$missing_runs" ] ||
	fail "the restored history is missing run(s):$missing_runs"
say "history     $EXPECT_RUN_COUNT of $TOTAL_RUNS run ids present (the most recent $RUN_LIMIT)"

# 6c. durable state survived, row for row.
remote_cmd "$TARGET_HOST" "sqlite3 $DATA_DIR/otter.db \"select key || '=' || value from job_state where job_id='$SRC_JOBID' order by key;\"" >"$WORK/target-state.out" ||
	fail "could not read durable state from the restored database"
if [ "$EXPECT_STATE" != "$(cat "$WORK/target-state.out")" ]; then
	echo "drill: backup state:" >&2
	printf '%s\n' "$EXPECT_STATE" >&2
	echo "drill: restored state:" >&2
	cat "$WORK/target-state.out" >&2
	fail "durable state did not survive the restore"
fi
if [ -n "$STATE_KEY" ]; then
	GOT_STATE=$(remote_otter "$TARGET_HOST" "state get $JOB $STATE_KEY") ||
		fail "otter state get $JOB $STATE_KEY failed against the restored runtime"
	say "state       $STATE_KEY survived (the restored daemon serves it)"
else
	GOT_STATE=""
	say "state       the backup holds no state rows for $JOB; nothing to compare"
fi

# 6d. the release snapshot and its activation link are on disk and resolve.
[ "$(remote_cmd "$TARGET_HOST" "test -f $DATA_DIR/.releases/$SRC_JOBID/$REL_DIGEST/otter-release.json && echo yes" || true)" = yes ] ||
	fail "the restored release snapshot is missing"
RESOLVED=$(remote_cmd "$TARGET_HOST" "readlink -f $DATA_DIR/.releases/active/$SRC_JOBID" 2>/dev/null || true)
case "$RESOLVED" in
"$DATA_DIR"/*) ;;
*) fail "the restored active release link resolves outside the restored data directory: ${RESOLVED:-missing}" ;;
esac
[ "$(remote_cmd "$TARGET_HOST" "test -d $RESOLVED && echo yes" || true)" = yes ] ||
	fail "the restored active release link is dangling: $RESOLVED"
say "release     snapshot present and active -> $RESOLVED"

# 6e. the prepared environment exists and its interpreter runs ON THIS HOST. A
# managed interpreter is host-specific; this is where an incompatible second
# instance shows up.
ENV_MARKER="$DATA_DIR/environments/$ENV_DIGEST/otter-ready.json"
[ "$(remote_cmd "$TARGET_HOST" "test -f $ENV_MARKER && echo yes" || true)" = yes ] ||
	fail "the restored environment marker is missing: $ENV_MARKER"
INTERPRETER=$(remote_cmd "$TARGET_HOST" "sed -n 's/.*\"interpreter\": \"\\([^\"]*\\)\".*/\\1/p' $ENV_MARKER" | head -n 1)
[ -n "$INTERPRETER" ] || fail "the restored environment marker records no interpreter"
case "$INTERPRETER" in
*[!A-Za-z0-9@._:/=+,-]*)
	fail "the restored environment records an interpreter path the drill cannot pass to a remote shell: $INTERPRETER"
	;;
esac
PY_VERSION=$(remote_cmd "$TARGET_HOST" "$INTERPRETER --version" 2>&1) ||
	fail "the restored interpreter does not run on the target host: $INTERPRETER"
say "environment restored and its interpreter runs: $INTERPRETER ($PY_VERSION)"

# 6f. THE POINT: the restored job executes again, on the second host.
say "executing   a new run of $JOB on the restored host"
if ! NEW=$(remote_otter "$TARGET_HOST" "run --no-wait $JOB" 2>"$WORK/run-refused.err"); then
	echo "drill: the restored runtime refused to submit a run:" >&2
	sed 's/^/drill:   /' "$WORK/run-refused.err" >&2
	fail "the restored job cannot run"
fi
[ -n "$NEW" ] || fail "the restored runtime returned no run id"
i=0
NEW_STATUS=""
while [ "$i" -lt 1200 ]; do
	NEW_STATUS=$(remote_otter "$TARGET_HOST" "run-status $NEW" 2>/dev/null | sed -n 's/^status: *//p' | head -n 1 || true)
	case "$NEW_STATUS" in
	succeeded | failed | timed_out | cancelled) break ;;
	esac
	i=$((i + 1))
	sleep 0.5
done
case "$NEW_STATUS" in
succeeded) ;;
*) fail "the restored run $NEW settled as ${NEW_STATUS:-unknown}" ;;
esac

NEW_STATUS_OUT=$(remote_otter "$TARGET_HOST" "run-status $NEW")
printf '%s\n' "$NEW_STATUS_OUT" | grep -q "^release: *$REL_DIGEST$" ||
	fail "the restored run did not bind to the restored release $REL_DIGEST"
printf '%s\n' "$NEW_STATUS_OUT" | grep -q "^environment: *$ENV_DIGEST$" ||
	fail "the restored run did not bind to the restored environment $ENV_DIGEST"
NEW_DIR=$(remote_cmd "$TARGET_HOST" "sqlite3 $DATA_DIR/otter.db \"select release_source_dir from runs where id='$NEW';\"" || true)
case "$NEW_DIR" in
"$DATA_DIR"/*) ;;
*) fail "the restored run executed from outside the restored data directory: ${NEW_DIR:-empty}" ;;
esac
say "executed    run $NEW succeeded from $NEW_DIR"

# 6g. it ran on the TARGET: the source daemon has never heard of that run id.
# "Not found" specifically -- an unreachable source would also exit non-zero, and
# that is not the same claim.
if SRC_LOOKUP=$(remote_otter "$SOURCE_HOST" "run-status $NEW" 2>&1); then
	fail "the source host already knows run $NEW: the run did not execute on the target host"
fi
case "$SRC_LOOKUP" in
*"not found"*) ;;
*) fail "could not prove run $NEW is unknown to the source host: ${SRC_LOOKUP:-no output}" ;;
esac
say "cross-host  the source runtime does not know run $NEW; it ran on $TARGET_HOST"

# 6h. the source runtime was not disturbed by any of this.
remote_otter "$SOURCE_HOST" "status" >/dev/null 2>&1 ||
	fail "the source runtime stopped answering during the drill; a backup must not disturb the live runtime"
say "source      still serving $DATA_DIR on $SOURCE_HOST"

# --- 7. tear down the staging, keep the evidence ------------------------------

if [ "$KEEP_TARGET" = 1 ]; then
	say "keeping the target daemon and the staged archives (DRILL_KEEP_TARGET=1)"
else
	remote_cmd "$TARGET_HOST" "systemctl stop $SERVICE" >/dev/null 2>&1 || true
	TARGET_STARTED=0
	remote_cmd "$SOURCE_HOST" "rm -rf $SRC_STAGE" >/dev/null 2>&1 || true
	remote_cmd "$TARGET_HOST" "rm -rf $TGT_STAGE" >/dev/null 2>&1 || true
	say "cleanup     stopped $SERVICE on the target and removed the staged archives on both hosts"
fi

say "not proven  secrets (/etc/otter/*.env excluded), interpreters across architectures, and that 'otter prepare' can rebuild python/ or cache/uv/"
if [ "$SABOTAGE" != "" ]; then
	fail "DRILL_SABOTAGE=$SABOTAGE omitted a backup member and the restored job still ran: this drill is not falsifiable in this mode, so the clean run above is not evidence"
fi
say "ok: a live runtime was backed up, restored onto a clean second host, and the job runs there"
