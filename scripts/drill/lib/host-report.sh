#!/bin/sh
# host-report.sh -- measure ONE host and print a key=value report on stdout.
#
# This is the preflight half of the clean-host backup/restore drill
# (scripts/drill/modes/backup-restore-clean-host.sh). It runs on the host it
# describes: the drill pipes it to each host over ssh, and the selfcheck runs
# it directly against a temporary directory tree. Everything it prints is
# measured, not assumed -- "the target is clean" is a claim an operator reads
# out of this report, so a missing tool that would make the claim unverifiable
# is reported as a missing tool rather than as a pass.
#
# Usage:
#   host-report.sh <role> <data_dir> <jobs_dir> <otter_bin> <api_url> \
#                  <service> [api_token] [env_file] [etc_dir]
#
#   role      source | target -- only labels the report; the drill checks it.
#   data_dir  the runtime's data directory (--data)
#   jobs_dir  the runtime's jobs root (--jobs)
#   otter_bin absolute path to the `otter` CLI on this host
#   api_url   loopback API base URL of the daemon on this host
#   service   systemd unit name that serves it
#   api_token optional bearer token; the drill's explicit override
#   env_file  optional path to the environment file holding OTTER_API_TOKEN
#   etc_dir   where to look for environment files when env_file is empty.
#             `/etc/otter` by default, matching the CLI's own lookup; a host
#             that keeps its secrets elsewhere passes its directory.
#
# The token matters more than it looks. `otter deploy` writes the token to
# `/etc/otter/workspaces/<workspace>.env`, and the CLI's own auto-discovery
# globs only `/etc/otter/*.env` -- which does not cross the `workspaces/`
# directory. On a deployed host every CLI call would therefore start with a 401
# and the drill would report it as "the runtime does not know this job". So the
# probe resolves a token file itself, by the strongest evidence available:
#   1. the token the operator passed explicitly;
#   2. the env file the operator named;
#   3. a file in the serving unit's own `EnvironmentFiles=`;
#   4. a single `/etc/otter/*.env` that holds a token (the CLI's own rule);
#   5. a single `/etc/otter/workspaces/*.env` that holds a token;
# and it reports `ambiguous` rather than guessing when several candidates hold
# different tokens, because the wrong workspace's token is a 401 on a
# multi-workspace host, not a working credential.
#
# The report is one `key=value` per line, values never contain a newline. Keys
# are read back with `sed -n 's/^key=//p'`. An empty or unmeasurable value is
# printed as `-`, never omitted: a consumer that looks for a key and finds no
# line must treat that as "the probe did not run", not as "fine".
#
# The token VALUE never appears in the report, only the path it was read from.
#
# Exit status is 0 whenever a report was produced, even a report full of
# problems: the drill decides, not the probe. A non-zero exit means no usable
# report (bad usage).
set -eu

usage() {
	echo "usage: host-report.sh <role> <data_dir> <jobs_dir> <otter_bin> <api_url> <service> [api_token] [env_file] [etc_dir]" >&2
	exit 2
}

[ "$#" -ge 6 ] || usage
ROLE=$1
DATA=$2
JOBS=$3
BIN=$4
API=$5
SERVICE=$6
TOKEN=${7:-}
ENV_FILE=${8:-}
ETC_DIR=${9:-/etc/otter}
[ -n "$ETC_DIR" ] || ETC_DIR=/etc/otter

case "$ROLE" in
source | target) ;;
*)
	echo "host-report.sh: role must be source or target, got '$ROLE'" >&2
	exit 2
	;;
esac

# value sanitizes one measured string into a single line: no newlines, no
# control characters, bounded length. Anything else would let a filename or a
# daemon error message forge or split a report line.
value() {
	printf '%s' "${1:-}" | tr -d '\r' | tr '\n\t' '  ' | cut -c1-300
	[ -n "${1:-}" ] || printf '-'
}

# empty prints the argument, or `-` when it is empty.
empty() {
	if [ -n "${1:-}" ]; then
		printf '%s' "$1"
	else
		printf -- '-'
	fi
}

# have reports whether a command exists.
have() { command -v "$1" >/dev/null 2>&1; }

# env_value reads one KEY=value out of a systemd environment file, the way the
# CLI reads its own token file: verbatim, first match, no shell expansion. The
# VALUE is only ever assigned, never echoed into the report.
env_value() {
	env_file=$1
	env_key=$2
	[ -f "$env_file" ] || return 0
	sed -n "s/^$env_key=//p" "$env_file" | head -n 1
}

# canonical echoes the absolute path a directory will have once it exists.
# readlink -f resolves a missing final component on coreutils; the cd fallback
# covers a host without it, and is only usable for a directory that exists.
canonical() {
	if have readlink && out=$(readlink -f -- "$1" 2>/dev/null) && [ -n "$out" ]; then
		printf '%s' "$out"
		return 0
	fi
	if [ -d "$1" ] && out=$(CDPATH= cd -- "$1" && pwd -P); then
		printf '%s' "$out"
		return 0
	fi
	printf '%s' "$1"
}

# owner_of / mode_of fall back from GNU stat to BSD stat so the selfcheck can
# run this probe on a developer machine as well as on the Linux host it is
# written for.
owner_of() {
	out=$(stat -c '%U:%G' "$1" 2>/dev/null || stat -f '%Su:%Sg' "$1" 2>/dev/null) || out=""
	empty "$out"
}

mode_of() {
	out=$(stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1" 2>/dev/null) || out=""
	empty "$out"
}

# first_line keeps a daemon's error message readable in the report.
first_line() {
	printf '%s' "${1:-}" | head -n 1 | cut -c1-200
}

# --- identity of the machine -------------------------------------------------

MACHINE_ID=""
MACHINE_SOURCE="none"
for candidate in /etc/machine-id /var/lib/dbus/machine-id; do
	if [ -r "$candidate" ] && [ -s "$candidate" ]; then
		MACHINE_ID=$(head -n 1 "$candidate" | tr -d '[:space:]')
		MACHINE_SOURCE=$candidate
		break
	fi
done
if [ -z "$MACHINE_ID" ]; then
	MACHINE_ID=$(hostname 2>/dev/null || echo unknown)
	MACHINE_SOURCE="hostname"
fi

# The ssh host key is a second, independent machine fingerprint. If an AMI
# ships with an empty /etc/machine-id, two instances would otherwise look like
# one host and the "second host" claim would rest on nothing.
SSH_HOST_KEY="-"
for pub in /etc/ssh/ssh_host_ed25519_key.pub /etc/ssh/ssh_host_rsa_key.pub; do
	if [ -r "$pub" ]; then
		if have sha256sum; then
			SSH_HOST_KEY=$(sha256sum "$pub" | cut -c1-64)
		elif have shasum; then
			SSH_HOST_KEY=$(shasum -a 256 "$pub" | cut -c1-64)
		else
			SSH_HOST_KEY="unhashable"
		fi
		break
	fi
done

# --- the paths ---------------------------------------------------------------

DATA_REAL=$(canonical "$DATA")
JOBS_REAL=$(canonical "$JOBS")

if [ ! -e "$DATA" ]; then
	DATA_EXISTS=no
	DATA_KIND=missing
elif [ -d "$DATA" ]; then
	DATA_EXISTS=yes
	DATA_KIND=dir
else
	DATA_EXISTS=yes
	DATA_KIND=file
fi

DATA_ENTRIES=0
DATA_CLEAN=no
DATA_REASON="does not exist"
if [ "$DATA_KIND" = dir ]; then
	DATA_ENTRIES=$(ls -A "$DATA" 2>/dev/null | wc -l | tr -d ' ')
	if [ "$DATA_ENTRIES" = "0" ]; then
		DATA_CLEAN=yes
		DATA_REASON=""
	else
		DATA_REASON="not empty ($DATA_ENTRIES entries: $(ls -A "$DATA" 2>/dev/null | head -n 5 | tr '\n' ' '))"
	fi
elif [ "$DATA_KIND" = file ]; then
	DATA_REASON="not a directory"
else
	# A missing directory is the empty state: the daemon creates it on first
	# start and the restore creates it deliberately.
	DATA_CLEAN=yes
	DATA_REASON=""
fi

if [ ! -e "$JOBS" ]; then
	JOBS_EXISTS=no
	JOBS_KIND=missing
elif [ -d "$JOBS" ]; then
	JOBS_EXISTS=yes
	JOBS_KIND=dir
else
	JOBS_EXISTS=yes
	JOBS_KIND=file
fi

JOBS_ENTRIES=0
JOBS_CLEAN=no
JOBS_REASON="does not exist"
FIND_OK=yes
have find || FIND_OK=no
if [ "$JOBS_KIND" = dir ]; then
	JOBS_ENTRIES=$(ls -A "$JOBS" 2>/dev/null | wc -l | tr -d ' ')
	if [ "$FIND_OK" = no ]; then
		# No find means "the jobs root is empty" cannot be proven from a
		# listing alone (a hidden nested tree would be missed), and a claim
		# nobody can check is the failure mode this whole mechanism exists to
		# prevent.
		JOBS_CLEAN=no
		JOBS_REASON="find is not installed, so the jobs root cannot be proven clean"
	elif [ "$JOBS_ENTRIES" = "0" ]; then
		JOBS_CLEAN=yes
		JOBS_REASON=""
	else
		JOBS_MANIFESTS=$(find "$JOBS" -name otter.yaml -type f 2>/dev/null | wc -l | tr -d ' ')
		JOBS_MARKERS=$(find "$JOBS" -name .otter-id -type f 2>/dev/null | wc -l | tr -d ' ')
		JOBS_REASON="not empty ($JOBS_ENTRIES entries: $(ls -A "$JOBS" 2>/dev/null | head -n 5 | tr '\n' ' ')); $JOBS_MANIFESTS otter.yaml, $JOBS_MARKERS .otter-id"
	fi
elif [ "$JOBS_KIND" = file ]; then
	JOBS_REASON="not a directory"
else
	JOBS_CLEAN=yes
	JOBS_REASON=""
fi

DB_PRESENT=no
[ -f "$DATA/otter.db" ] && DB_PRESENT=yes

DATA_OWNER=$(owner_of "$DATA")
DATA_MODE=$(mode_of "$DATA")

# --- the service, and where it gets its paths and its token -------------------
#
# The unit is the authority on where the daemon will come up and what it will
# serve. `otter deploy` renders
#   ExecStart=<ws>/bin/otterd --jobs <jobs> --data <data> --listen <listen> ...
#   EnvironmentFile=-/etc/otter/workspaces/<ws>.daemon.env
#   EnvironmentFile=-/etc/otter/workspaces/<ws>.env
# so a deployed unit names its paths as arguments; a hand-written one may name
# them in its environment files instead. Both are read, arguments first, and
# the effective values are reported so the drill can refuse a unit that would
# serve somewhere else instead of timing out or failing later with a misleading
# message.

SERVICE_STATE=unknown
SERVICE_EXISTS=no
SERVICE_BIN=""
UNIT_DATA_DIR="-"
UNIT_JOBS_DIR="-"
UNIT_LISTEN="-"
UNIT_ENV_FILES=""
if have systemctl; then
	# One call, parsed by key: `show -p A -p B` prints `A=...` lines, which is
	# stable across the systemd versions a target may run.
	show=$(systemctl show -p ActiveState -p LoadState -p ExecStart -p EnvironmentFiles "$SERVICE" 2>/dev/null || true)
	state=$(printf '%s\n' "$show" | sed -n 's/^ActiveState=//p' | head -n 1)
	load=$(printf '%s\n' "$show" | sed -n 's/^LoadState=//p' | head -n 1)
	SERVICE_STATE=$(empty "$state")
	[ "$load" = "loaded" ] && SERVICE_EXISTS=yes

	exec_line=$(printf '%s\n' "$show" | sed -n 's/^ExecStart=//p' | head -n 1)
	# The unit's own environment files, in the order systemd loads them.
	#
	# `systemctl show -p EnvironmentFiles` prints ONE LINE PER FILE, even though
	# the manual page describes a single space-separated list:
	#
	#   EnvironmentFiles=/etc/otter/workspaces/app-1a2b3c4d.daemon.env (ignore_errors=yes)
	#   EnvironmentFiles=/etc/otter/workspaces/app-1a2b3c4d.env (ignore_errors=yes)
	#
	# (captured on systemd 255 and 252; `otter deploy` renders the
	# daemon-settings file first and the credentials file second). Keeping only
	# the first line therefore drops the file that holds the token, and the
	# drill falls back to a glob that can pick another workspace's token. So
	# every line is collected, in order, and the `(ignore_errors=...)` suffix
	# and any unit-file leading dash are stripped.
	UNIT_ENV_FILES=$(printf '%s\n' "$show" | sed -n 's/^EnvironmentFiles=//p' |
		tr ' ' '\n' | sed -n 's/^-*\(\/.*\)$/\1/p' | tr '\n' ' ' || true)
	[ -n "$UNIT_ENV_FILES" ] || UNIT_ENV_FILES="-"

	# ExecStart's argv, whether systemd prints `{ path=... ; argv[]=... }` or a
	# bare command line. `--flag value` pairs are read out of it either way.
	if [ -n "$exec_line" ]; then
		if [ -z "$SERVICE_BIN" ]; then
			SERVICE_BIN=$(printf '%s' "$exec_line" | sed -n 's/.*path=\([^ ;]*\).*/\1/p' | head -n 1)
		fi
		# `--data X` and `--data=X` are both legal spellings of the same flag.
		arg_data=$(printf '%s' "$exec_line" | sed -n 's/.*--data[= ]\([^ ;]*\).*/\1/p' | head -n 1)
		arg_jobs=$(printf '%s' "$exec_line" | sed -n 's/.*--jobs[= ]\([^ ;]*\).*/\1/p' | head -n 1)
		arg_listen=$(printf '%s' "$exec_line" | sed -n 's/.*--listen[= ]\([^ ;]*\).*/\1/p' | head -n 1)
		[ -n "$arg_data" ] && UNIT_DATA_DIR=$arg_data
		[ -n "$arg_jobs" ] && UNIT_JOBS_DIR=$arg_jobs
		[ -n "$arg_listen" ] && UNIT_LISTEN=$arg_listen
	fi
	# A unit that names none of them may still set them in an EnvironmentFile.
	for f in $UNIT_ENV_FILES; do
		[ -r "$f" ] || continue
		[ "$UNIT_DATA_DIR" != "-" ] || UNIT_DATA_DIR=$(env_value "$f" OTTER_DATA_DIR)
		[ "$UNIT_JOBS_DIR" != "-" ] || UNIT_JOBS_DIR=$(env_value "$f" OTTER_JOBS_DIR)
		[ "$UNIT_LISTEN" != "-" ] || UNIT_LISTEN=$(env_value "$f" OTTER_LISTEN)
	done
	[ -n "$UNIT_DATA_DIR" ] || UNIT_DATA_DIR="-"
	[ -n "$UNIT_JOBS_DIR" ] || UNIT_JOBS_DIR="-"
	[ -n "$UNIT_LISTEN" ] || UNIT_LISTEN="-"
	[ -n "$SERVICE_BIN" ] || SERVICE_BIN="-"
else
	SERVICE_STATE=no-systemctl
	SERVICE_BIN="-"
fi

# --- the API token ------------------------------------------------------------
#
# See the header: the CLI's own glob does not cross `workspaces/`, so the probe
# resolves a token file by the strongest evidence it has and reports the path,
# never the value.

TOKEN_FILE="-"
TOKEN_SOURCE=none
TOKEN_CANDIDATES=0
TOKEN_REASON=""
candidates=""

# unit_env_candidates lists the serving unit's environment files that actually
# hold a token. This is the strongest automatic evidence: it is the file the
# daemon itself loads.
if [ "$UNIT_ENV_FILES" != "-" ]; then
	for f in $UNIT_ENV_FILES; do
		[ -n "$(env_value "$f" OTTER_API_TOKEN)" ] || continue
		candidates="$candidates $f"
	done
fi
if [ -n "$candidates" ]; then
	TOKEN_SOURCE=unit-env
fi

# No unit evidence: fall back to the CLI's own rule, then to the per-workspace
# directory deploy uses.
if [ -z "$candidates" ]; then
	for f in "$ETC_DIR"/*.env; do
		[ -f "$f" ] || continue
		[ -n "$(env_value "$f" OTTER_API_TOKEN)" ] || continue
		candidates="$candidates $f"
	done
	[ -n "$candidates" ] && TOKEN_SOURCE=etc-otter
fi
if [ -z "$candidates" ]; then
	for f in "$ETC_DIR"/workspaces/*.env; do
		[ -f "$f" ] || continue
		[ -n "$(env_value "$f" OTTER_API_TOKEN)" ] || continue
		candidates="$candidates $f"
	done
	[ -n "$candidates" ] && TOKEN_SOURCE=workspaces
fi

# The operator's explicit choices outrank all discovery.
if [ -n "$ENV_FILE" ]; then
	TOKEN_SOURCE=given-file
	if [ -n "$(env_value "$ENV_FILE" OTTER_API_TOKEN)" ]; then
		candidates=" $ENV_FILE"
	else
		TOKEN_REASON="the environment file named by the drill holds no OTTER_API_TOKEN"
		candidates=""
	fi
fi
if [ -n "$TOKEN" ]; then
	TOKEN_SOURCE=explicit
	candidates=""
fi

TOKEN_CANDIDATES=$(printf '%s' "$candidates" | tr ' ' '\n' | grep -c . || true)
if [ "$TOKEN_SOURCE" != "explicit" ] && [ "$TOKEN_CANDIDATES" -gt 1 ]; then
	# More than one file holds a token and no unit scoped the choice: guessing
	# the first sorted match is how a multi-workspace host answers 401.
	TOKEN_SOURCE=ambiguous
	TOKEN_REASON="several files hold a token:$(printf '%s' "$candidates")"
fi
if [ "$TOKEN_SOURCE" != "explicit" ] && [ "$TOKEN_SOURCE" != "ambiguous" ] && [ -n "$candidates" ]; then
	TOKEN_FILE=$(printf '%s' "$candidates" | tr ' ' '\n' | grep . | head -n 1)
fi

# --- the daemon ---------------------------------------------------------------
#
# A non-zero exit is the normal answer on a clean target; the detail keeps
# "connection refused" distinguishable from "wrong URL" in the transcript.
# `otter status` exits 0 even when the daemon wants a token it did not get, so
# authentication is judged by whether the response carried run counts: that is
# the field the daemon only discloses to an authenticated caller.

DAEMON=stopped
DAEMON_DETAIL=""
DAEMON_VERSION="-"
AUTH=unknown
AUTH_REQUIRED=unknown
status_out=""
status_err=""
if [ -n "$TOKEN" ]; then
	export OTTER_API_TOKEN="$TOKEN"
elif [ -n "$TOKEN_FILE" ] && [ "$TOKEN_FILE" != "-" ]; then
	OTTER_API_TOKEN=$(env_value "$TOKEN_FILE" OTTER_API_TOKEN)
	export OTTER_API_TOKEN
fi
ERRFILE=$(mktemp 2>/dev/null || echo /tmp/otter-probe-status.$$)
if have timeout; then
	status_out=$(OTTER_API_URL="$API" timeout 15 "$BIN" status 2>"$ERRFILE") || true
else
	status_out=$(OTTER_API_URL="$API" "$BIN" status 2>"$ERRFILE") || true
fi
status_err=$(cat "$ERRFILE" 2>/dev/null || true)
rm -f "$ERRFILE" 2>/dev/null || true
if [ -n "$status_out" ] && printf '%s' "$status_out" | grep -q '^status: *ok'; then
	DAEMON=running
	DAEMON_DETAIL=""
	DAEMON_VERSION=$(printf '%s' "$status_out" | sed -n 's/^version: *//p' | head -n 1)
	[ -n "$DAEMON_VERSION" ] || DAEMON_VERSION="-"
	if printf '%s' "$status_out" | grep -q '^jobs: *[0-9]'; then
		AUTH=ok
		AUTH_REQUIRED=no
	else
		# No counters: either no token was sent and the daemon requires one, or
		# a token was sent and it was refused.
		if [ -n "$TOKEN" ] || { [ -n "$TOKEN_FILE" ] && [ "$TOKEN_FILE" != "-" ]; }; then
			AUTH=unauthorized
		else
			AUTH=token-missing
		fi
		AUTH_REQUIRED=yes
	fi
else
	DAEMON=stopped
	DAEMON_DETAIL=$(first_line "$status_err")
fi

# --- tooling ------------------------------------------------------------------

SQLITE3=no
have sqlite3 && SQLITE3=yes
SHA256SUM=no
have sha256sum && SHA256SUM=yes
TAR=no
have tar && TAR=yes
READLINK=no
have readlink && READLINK=yes
TIMEOUT=no
have timeout && TIMEOUT=yes
GETENT=no
have getent && GETENT=yes

# The CLI the operator names may be deploy's host dispatcher
# (`/usr/local/bin/otter`), which refuses `--version` and `status` outside a
# workspace when the host holds more than one workspace. The serving unit knows
# which CLI belongs to the runtime: its ExecStart names <workspace>/bin/otterd,
# and the workspace's CLI is that file's sibling. Fall back to it -- and report
# both paths, so a transcript shows the drill switched.
OTTER_BIN_EFFECTIVE=$BIN
OTTER_VERSION=missing
if [ -x "$BIN" ]; then
	OTTER_VERSION=$(value "$("$BIN" --version 2>/dev/null || echo missing)")
fi
if [ "$OTTER_VERSION" = missing ]; then
	# `case`, not `[ ... = */otterd ]`: an unquoted pattern inside `[ ]` is
	# pathname-expanded first, so a checkout holding bin/otterd would turn the
	# test into "too many arguments".
	case "${SERVICE_BIN:--}" in
	*/otterd)
		sibling=$(dirname "$SERVICE_BIN")/otter
		if [ -x "$sibling" ]; then
			candidate=$(value "$("$sibling" --version 2>/dev/null || echo missing)")
			if [ "$candidate" != missing ]; then
				OTTER_BIN_EFFECTIVE=$sibling
				OTTER_VERSION=$candidate
			fi
		fi
		;;
	esac
fi

# --- the report ---------------------------------------------------------------

printf 'role=%s\n' "$ROLE"
printf 'hostname=%s\n' "$(value "$(hostname 2>/dev/null || echo unknown)")"
printf 'machine_id=%s\n' "$MACHINE_ID"
printf 'machine_id_source=%s\n' "$MACHINE_SOURCE"
printf 'ssh_host_key=%s\n' "$SSH_HOST_KEY"
printf 'kernel=%s\n' "$(value "$(uname -s 2>/dev/null || echo unknown)")"
printf 'release=%s\n' "$(value "$(uname -r 2>/dev/null || echo unknown)")"
printf 'arch=%s\n' "$(value "$(uname -m 2>/dev/null || echo unknown)")"
printf 'uid=%s\n' "$(id -u 2>/dev/null || echo unknown)"
printf 'otter_bin=%s\n' "$OTTER_BIN_EFFECTIVE"
printf 'otter_bin_given=%s\n' "$BIN"
printf 'otter_version=%s\n' "$OTTER_VERSION"
printf 'data_dir=%s\n' "$DATA"
printf 'data_real=%s\n' "$DATA_REAL"
printf 'data_exists=%s\n' "$DATA_EXISTS"
printf 'data_kind=%s\n' "$DATA_KIND"
printf 'data_entries=%s\n' "$DATA_ENTRIES"
printf 'data_clean=%s\n' "$DATA_CLEAN"
printf 'data_reason=%s\n' "$(empty "$DATA_REASON")"
printf 'data_owner=%s\n' "$DATA_OWNER"
printf 'data_mode=%s\n' "$DATA_MODE"
printf 'db_present=%s\n' "$DB_PRESENT"
printf 'jobs_dir=%s\n' "$JOBS"
printf 'jobs_real=%s\n' "$JOBS_REAL"
printf 'jobs_exists=%s\n' "$JOBS_EXISTS"
printf 'jobs_kind=%s\n' "$JOBS_KIND"
printf 'jobs_entries=%s\n' "$JOBS_ENTRIES"
printf 'jobs_manifests=%s\n' "${JOBS_MANIFESTS:-0}"
printf 'jobs_markers=%s\n' "${JOBS_MARKERS:-0}"
printf 'jobs_clean=%s\n' "$JOBS_CLEAN"
printf 'jobs_reason=%s\n' "$(empty "$JOBS_REASON")"
printf 'find=%s\n' "$FIND_OK"
printf 'daemon=%s\n' "$DAEMON"
printf 'daemon_version=%s\n' "$(empty "$DAEMON_VERSION")"
printf 'daemon_detail=%s\n' "$(empty "$DAEMON_DETAIL")"
printf 'auth=%s\n' "$AUTH"
printf 'auth_required=%s\n' "$AUTH_REQUIRED"
printf 'api_url=%s\n' "$API"
printf 'token_source=%s\n' "$TOKEN_SOURCE"
printf 'token_file=%s\n' "$(empty "$TOKEN_FILE")"
printf 'token_candidates=%s\n' "$TOKEN_CANDIDATES"
printf 'token_reason=%s\n' "$(empty "$TOKEN_REASON")"
printf 'etc_dir=%s\n' "$ETC_DIR"
printf 'service=%s\n' "$SERVICE"
printf 'service_state=%s\n' "$SERVICE_STATE"
printf 'service_exists=%s\n' "$SERVICE_EXISTS"
printf 'service_bin=%s\n' "$(empty "$SERVICE_BIN")"
printf 'unit_data_dir=%s\n' "$UNIT_DATA_DIR"
printf 'unit_jobs_dir=%s\n' "$UNIT_JOBS_DIR"
printf 'unit_listen=%s\n' "$UNIT_LISTEN"
printf 'unit_env_files=%s\n' "$UNIT_ENV_FILES"
printf 'sqlite3=%s\n' "$SQLITE3"
printf 'sha256sum=%s\n' "$SHA256SUM"
printf 'tar=%s\n' "$TAR"
printf 'readlink=%s\n' "$READLINK"
printf 'timeout=%s\n' "$TIMEOUT"
printf 'getent=%s\n' "$GETENT"
