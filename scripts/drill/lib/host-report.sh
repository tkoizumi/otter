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
#                  <service> [api_token]
#
#   role      source | target -- only labels the report; the drill checks it.
#   data_dir  the runtime's data directory (--data)
#   jobs_dir  the runtime's jobs root (--jobs)
#   otter_bin absolute path to the `otter` CLI on this host
#   api_url   loopback API base URL of the daemon on this host
#   service   systemd unit name that serves it
#   api_token optional bearer token; empty means "let the CLI find it"
#
# The report is one `key=value` per line, values never contain a newline. Keys
# are read back with `sed -n 's/^key=//p'`. An empty or unmeasurable value is
# printed as `-`, never omitted: a consumer that looks for a key and finds no
# line must treat that as "the probe did not run", not as "fine".
#
# Exit status is 0 whenever a report was produced, even a report full of
# problems: the drill decides, not the probe. A non-zero exit means no usable
# report (bad usage).
set -eu

usage() {
	echo "usage: host-report.sh <role> <data_dir> <jobs_dir> <otter_bin> <api_url> <service> [api_token]" >&2
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

# --- the daemon ---------------------------------------------------------------

# run_status asks the daemon on this host to describe itself. A non-zero exit is
# the normal answer on a clean target; the detail keeps "connection refused"
# distinguishable from "wrong URL" in the transcript.
DAEMON=stopped
DAEMON_DETAIL=""
DAEMON_VERSION="-"
status_out=""
status_err=""
if [ -n "$TOKEN" ]; then
	export OTTER_API_TOKEN="$TOKEN"
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
	# The version the DAEMON reports, not the CLI's: the database schema is
	# versioned, and a restore has to land on the build that wrote it.
	DAEMON_VERSION=$(printf '%s' "$status_out" | sed -n 's/^version: *//p' | head -n 1)
	[ -n "$DAEMON_VERSION" ] || DAEMON_VERSION="-"
else
	DAEMON=stopped
	DAEMON_DETAIL=$(first_line "$status_err")
fi

# --- the service --------------------------------------------------------------

SERVICE_STATE=unknown
SERVICE_EXISTS=no
if have systemctl; then
	state=$(systemctl show -p ActiveState --value "$SERVICE" 2>/dev/null || echo unknown)
	load=$(systemctl show -p LoadState --value "$SERVICE" 2>/dev/null || echo unknown)
	SERVICE_STATE=$(empty "$state")
	[ "$load" = "loaded" ] && SERVICE_EXISTS=yes
else
	SERVICE_STATE=no-systemctl
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

OTTER_VERSION=missing
if [ -x "$BIN" ]; then
	OTTER_VERSION=$(value "$("$BIN" --version 2>/dev/null || echo missing)")
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
printf 'otter_bin=%s\n' "$BIN"
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
printf 'api_url=%s\n' "$API"
printf 'service=%s\n' "$SERVICE"
printf 'service_state=%s\n' "$SERVICE_STATE"
printf 'service_exists=%s\n' "$SERVICE_EXISTS"
printf 'sqlite3=%s\n' "$SQLITE3"
printf 'sha256sum=%s\n' "$SHA256SUM"
printf 'tar=%s\n' "$TAR"
printf 'readlink=%s\n' "$READLINK"
printf 'timeout=%s\n' "$TIMEOUT"
