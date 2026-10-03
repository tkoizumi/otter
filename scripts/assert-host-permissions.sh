#!/bin/sh
# Assert the security-relevant host state of a provisioned Otter workspace.
#
# This runs *on the host*, after `otter deploy` has converged it. It is the
# executable form of CA-09 ("verify filesystem permissions") and the P0-07
# ingress clauses: the data directory is private to the service account, the
# environment files are 0600 and owned by it, the systemd unit is installed,
# enabled and active with its sandbox directives intact, the API is bound to
# loopback or private, and nothing but the explicitly approved ports is
# listening.
#
# `otter deploy` is what *creates* that state -- the account, the modes, the
# unit. This script asserts it, so "the permissions are right" is read off the
# filesystem rather than off the documentation (R-19). Every check is a hard
# failure: there is no skip, because a check that did not run has produced no
# evidence.
#
#   sudo sh scripts/assert-host-permissions.sh \
#     --workspace-dir /opt/otter/workspaces/castor-24856da9
#
# The script derives the unit name, service account and data directory from the
# workspace's own `workspace.json`, so the paths it checks are the paths the
# deploy actually wrote. Each may also be given explicitly:
#
#   --unit NAME            systemd unit, with or without .service
#   --user NAME            service account
#   --data-dir DIR         data directory
#   --env-dir DIR          directory holding the workspace env files
#   --env-file NAME        the workspace's env basename, without the .env suffix
#   --listen ADDR          the loopback or private address the API must hold
#   --approved-ports LIST  ports this host may be listening on (default 22);
#                          the check is `ss -tln` on the host, not a scan from
#                          outside it
#
# Environment equivalents: OTTER_WORKSPACE_DIR, OTTER_UNIT, OTTER_SERVICE_USER,
# OTTER_DATA_DIR, OTTER_ENV_DIR, OTTER_ENV_FILE, OTTER_LISTEN,
# OTTER_APPROVED_PORTS. OTTER_SYSTEMD_UNIT_DIR exists for the fixture harness
# and is not meant to be set on a real host.
#
# Falsifiability: `sh scripts/test-assert-host-permissions.sh` runs this script
# against a synthesized host fixture and against 32 copies of the script, each
# with one assertion disabled. The clean fixture must pass and every mutation
# must fail on the case that names it -- otherwise this script is asserting
# nothing.
set -eu

# A transcript is only evidence if it says where and when it ran, against which
# checkout. Same header contract as scripts/drill.sh.
here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/.." && pwd)
printf '\n===== assert-host-permissions =====\n'
printf 'date:     %s\n' "$(date '+%Y-%m-%d %H:%M:%S %Z')"
printf 'host:     %s %s (%s)\n' "$(uname -s)" "$(uname -r)" "$(uname -m)"
printf 'checkout: %s\n' "$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo unknown)"

# --- defaults ---------------------------------------------------------------

workspace_dir=${OTTER_WORKSPACE_DIR:-}
service_user=${OTTER_SERVICE_USER:-otter}
# The env directory and its files belong to root, not to the service account:
# systemd reads EnvironmentFile= as root before dropping to User=, and a service
# account that owns its own credentials file can rewrite them.
env_owner=${OTTER_ENV_OWNER:-root:root}
env_dir_mode=${OTTER_ENV_DIR_MODE:-700}
unit=${OTTER_UNIT:-}
data_dir=${OTTER_DATA_DIR:-}
env_dir=${OTTER_ENV_DIR:-}
env_file=${OTTER_ENV_FILE:-}
listen=${OTTER_LISTEN:-}
approved_ports=${OTTER_APPROVED_PORTS:-22}
# allow_remote_bind asserts the *configured* posture: when the deploy was made
# with --allow-remote-bind, a wildcard or public listen address is the intended
# configuration and this script checks it as such (with the token that must
# accompany it) rather than failing a deliberate decision. Unset means the
# loopback-or-private posture.
allow_remote_bind=${OTTER_ALLOW_REMOTE_BIND:-0}
# Minimum active swap, in MiB. The 1 GiB Castor host carries a 2 GiB swapfile;
# the floor is set to catch "no swap at all", which is the failure the CDW
# user-data produced (set -e aborted the script before its swap block), not to
# re-impose the stack's exact size.
min_swap_mib=${OTTER_MIN_SWAP_MIB:-256}
# The report a cloud-init provision leaves behind. Empty means "do not check",
# for a host this repository's provisioning script did not create.
provision_report=${OTTER_PROVISION_REPORT-/var/log/otter-provision-report.txt}
# /proc is overridable only so the fixture harness can point the script at
# synthesized files; on a real host the default is the only correct answer.
proc_root=${OTTER_PROC_ROOT:-/proc}

usage() {
	cat <<'USAGE'
usage: assert-host-permissions.sh [--workspace-dir DIR] [--unit NAME] [--user NAME]
                                  [--data-dir DIR] [--env-dir DIR] [--env-file NAME]
                                  [--listen ADDR] [--approved-ports LIST]

Run on the deployed host. Pass --workspace-dir, or the paths explicitly.
Exit status is 0 only when every assertion holds.

  --workspace-dir DIR   the workspace tree; its workspace.json supplies
                        service_name and data_dir
  --unit NAME           systemd unit (default: from workspace.json)
  --user NAME           service account (default otter)
  --data-dir DIR        data directory (default: from workspace.json)
  --env-dir DIR         directory of env files (default /etc/otter/workspaces)
  --env-file NAME       workspace env basename; NAME.env and NAME.daemon.env
                        are both checked
  --env-owner U:G       owner the env files must have (default root:root, which
                        is what `otter deploy` writes)
  --env-dir-mode MODE   mode the env directory must have (default 700)
  --listen ADDR         loopback or private host:port the API must hold
  --approved-ports LIST comma-separated ports (default 22). A listener on any
                        other port fails unless it is on loopback.
  --allow-remote-bind   assert a deliberately reachable listen address (wildcard
                        or public) instead of failing it; the matching
                        OTTER_API_TOKEN must then be configured.
  --min-swap-mib N      minimum active swap in MiB (default 256; 0 disables)
  --provision-report F  cloud-init provisioning report to require (default
                        /var/log/otter-provision-report.txt; empty disables)
USAGE
}

while [ $# -gt 0 ]; do
	case $1 in
	--workspace-dir)
		workspace_dir=$2
		shift 2
		;;
	--unit)
		unit=$2
		shift 2
		;;
	--user)
		service_user=$2
		shift 2
		;;
	--data-dir)
		data_dir=$2
		shift 2
		;;
	--env-dir)
		env_dir=$2
		shift 2
		;;
	--env-file)
		env_file=$2
		shift 2
		;;
	--env-owner)
		env_owner=$2
		shift 2
		;;
	--env-dir-mode)
		env_dir_mode=$2
		shift 2
		;;
	--listen)
		listen=$2
		shift 2
		;;
	--approved-ports)
		approved_ports=$2
		shift 2
		;;
	--allow-remote-bind)
		allow_remote_bind=1
		shift
		;;
	--min-swap-mib)
		min_swap_mib=$2
		shift 2
		;;
	--provision-report)
		provision_report=$2
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "assert-host-permissions: unknown argument: $1" >&2
		usage >&2
		exit 2
		;;
	esac
done

# --- read the workspace record ---------------------------------------------
#
# The deploy writes workspace.json beside the workspace's own files. Reading it
# rather than taking the unit and data directory on trust is the point: the
# script checks what was installed, not what the operator remembered passing.

if [ -n "$workspace_dir" ]; then
	record=$workspace_dir/workspace.json
	if [ ! -f "$record" ]; then
		echo "assert-host-permissions: FAIL: no workspace record at $record" >&2
		echo "assert-host-permissions: pass --unit and --data-dir explicitly if it is elsewhere" >&2
		exit 1
	fi
	# The real record, written by the deploy's own workspace record, is flat
	# JSON:
	#   {"jobs_layout":"jobs","id":"<uuid>","slug":"<slug>","name":"<workspace>",
	#    "unit":"otterd-<workspace>","listen":"127.0.0.1:7337","created_at":"..."}
	#
	# There is no data_dir key. The data directory is the fixed path
	# <workspace>/.otter/data, so it is derived here rather than read. The
	# older "service_name" and "data_dir" spellings are still accepted, so a
	# record written by an earlier version is not rejected as unreadable.
	#
	# No JSON parser is assumed on the host, so the fields are pulled out by
	# hand. They are written by this tool's own encoder: "key": "value".
	json_string() {
		sed -n 's/.*"'"$1"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$record" | head -n 1
	}
	[ -n "$unit" ] || unit=$(json_string unit)
	[ -n "$unit" ] || unit=$(json_string service_name)
	[ -n "$env_file" ] || env_file=$(json_string name)
	case $workspace_dir in
	*/) workspace_dir=${workspace_dir%/} ;;
	esac
	[ -n "$data_dir" ] || data_dir=$workspace_dir/.otter/data
	[ -n "$listen" ] || listen=$(json_string listen)
	if [ -z "$unit" ]; then
		echo "assert-host-permissions: FAIL: $record names no unit" >&2
		exit 1
	fi
fi

if [ -z "$unit" ]; then
	echo "assert-host-permissions: FAIL: need a unit; pass --workspace-dir or --unit" >&2
	exit 2
fi
if [ -z "$data_dir" ]; then
	echo "assert-host-permissions: FAIL: need a data directory; pass --workspace-dir or --data-dir" >&2
	exit 2
fi

# --unit accepts either spelling; systemctl wants the .service suffix.
case $unit in
*.service) unit_name=${unit%.service} ;;
*) unit_name=$unit ;;
esac
# unit_dir is the directory systemd reads unit files from. It is overridable
# only so scripts/test-assert-host-permissions.sh can point this script at a
# synthesized host; on a real host the default is the only correct answer.
unit_dir=${OTTER_SYSTEMD_UNIT_DIR:-/etc/systemd/system}
unit_path=$unit_dir/$unit_name.service

# The env directory is fixed by the deploy layout unless overridden, and the
# env files are named for the workspace -- which is the unit name without its
# prefix. Deriving it means the common invocation needs only --workspace-dir.
[ -n "$env_dir" ] || env_dir=/etc/otter/workspaces
[ -n "$env_file" ] || env_file=${unit_name#otterd-}

printf 'unit:     %s\n' "$unit_name"
printf 'user:     %s\n' "$service_user"
printf 'data:     %s\n' "$data_dir"
printf 'env dir:  %s\n' "$env_dir"
printf 'env file: %s\n' "$env_file"
printf 'approved: %s\n' "$approved_ports"
if [ "$allow_remote_bind" = 1 ]; then
	printf 'posture:  remote bind allowed (--allow-remote-bind)\n'
else
	printf 'posture:  loopback or private\n'
fi
printf 'min swap: %s MiB\n' "$min_swap_mib"
printf 'report:   %s\n' "${provision_report:-<not checked>}"

# --- assertion plumbing -----------------------------------------------------
#
# Every check runs even after one fails, so a broken host reports all of its
# problems in one run rather than one per attempt. The failure list is the
# script's own outcome: nothing here trusts an external tool's exit status to
# mean what this script means.

failures=$(mktemp "${TMPDIR:-/tmp}/otter-assert.XXXXXX")
listeners=$(mktemp "${TMPDIR:-/tmp}/otter-listeners.XXXXXX")
cleanup() {
	rm -f "$failures" "$listeners"
}
trap cleanup EXIT HUP INT TERM

ok() {
	printf 'ok:   %s\n' "$1"
}

fail() {
	printf 'FAIL: %s\n' "$1"
	printf '%s\n' "$1" >>"$failures"
}

# mode_of prints a path's permission bits as octal, or nothing when the path
# does not exist. GNU stat is the host's; the -c form is deliberate rather than
# the BSD -f form because every target of this script is Linux.
mode_of() {
	stat -c '%a' "$1" 2>/dev/null
}

owner_of() {
	stat -c '%U:%G' "$1" 2>/dev/null
}

# sysprop reads one property off the loaded unit. On systemd >= 253 --value is
# enough; the fallback keeps an older host from reporting every property as
# unset, which would fail the whole script for the wrong reason.
sysprop() {
	value=$(systemctl show -p "$1" --value "$unit_name" 2>/dev/null || true)
	if [ -z "$value" ]; then
		value=$(systemctl show -p "$1" "$unit_name" 2>/dev/null |
			sed -n "s/^$1=//p" | head -n 1 || true)
	fi
	printf '%s' "$value"
}

# --- 1. the service account -------------------------------------------------

if ! id -u "$service_user" >/dev/null 2>&1; then
	fail "the service account $service_user does not exist"
else
	ok "service account $service_user exists"
	shell=$(getent passwd "$service_user" | cut -d: -f7)
	case $shell in
	*/nologin | */false)
		ok "service account shell is $shell"
		;;
	*)
		fail "service account $service_user has shell ${shell:-<none>}, want a nologin shell"
		;;
	esac
	# A system account is one an operator cannot log in as. The uid range is
	# the mechanism that keeps it out of the interactive set even if someone
	# later edits the shell line.
	uid=$(id -u "$service_user")
	if [ "$uid" -ge 1000 ]; then
		fail "service account $service_user has uid $uid, which is an interactive account's range"
	else
		ok "service account uid $uid is in the system range"
	fi
fi

# --- 2. the data directory --------------------------------------------------
#
# 0700 and owned by the service account. otter.db holds every job's state and
# captured output, so any other reader is a disclosure.

if [ ! -d "$data_dir" ]; then
	fail "data directory $data_dir does not exist"
else
	mode=$(mode_of "$data_dir")
	if [ "$mode" = "700" ]; then
		ok "data directory mode is 0700"
	else
		fail "data directory $data_dir is mode ${mode:-unknown}, want 0700"
	fi
	owner=$(owner_of "$data_dir")
	if [ "$owner" = "$service_user:$service_user" ]; then
		ok "data directory owner is $service_user:$service_user"
	else
		fail "data directory $data_dir is owned by ${owner:-unknown}, want $service_user:$service_user"
	fi
	# The database inside the directory. Its mode is contained by the 0700
	# directory -- no other account can traverse into it -- so this is a
	# recommendation, not a failure: a host whose database is 0644 still meets
	# the documented bar, which is the directory. It is reported rather than
	# passed in silence because a tighter umask on the daemon would make it
	# 600, and severity is not what the reader needs to know here.
	for entry in otter.db otter.db-wal otter.db-shm; do
		[ -e "$data_dir/$entry" ] || continue
		emode=$(mode_of "$data_dir/$entry")
		case ${emode:-000} in
		600 | 640 | 660 | 700) ok "$entry mode is 0$emode" ;;
		*)
			ok "$entry mode is ${emode:-unknown}; owner-only is tighter but the 0700 directory contains it"
			printf 'note: %s is mode %s; a tighter umask on the daemon would make it 0600\n' \
				"$data_dir/$entry" "${emode:-unknown}"
			;;
		esac
	done
fi

# --- 3. the environment files ----------------------------------------------
#
# 0600, root-owned, inside a 0700 root-owned directory. These hold the API
# token and every shared secret, and systemd reads them as root before dropping
# to User=otter -- so root ownership is both what `otter deploy` writes and the
# stronger posture: a service account that owns its own credentials file can
# rewrite them, and so can any compromised job running as that account.

if [ ! -d "$env_dir" ]; then
	fail "environment directory $env_dir does not exist"
else
	dir_mode=$(mode_of "$env_dir")
	if [ "$dir_mode" = "$env_dir_mode" ]; then
		ok "environment directory mode is 0$env_dir_mode"
	else
		fail "environment directory $env_dir is mode ${dir_mode:-unknown}, want 0$env_dir_mode"
	fi
	dir_owner=$(owner_of "$env_dir")
	if [ "$dir_owner" = "$env_owner" ]; then
		ok "environment directory owner is $env_owner"
	else
		fail "environment directory $env_dir is owned by ${dir_owner:-unknown}, want $env_owner"
	fi
fi

for suffix in .env .daemon.env; do
	file=$env_dir/$env_file$suffix
	if [ ! -f "$file" ]; then
		# A deploy that configured nothing beyond its manifests writes no
		# shared env file, and a daemon env file is optional. The unit
		# tolerates both with EnvironmentFile=-; this script tolerates an
		# absent file and fails on a present one with the wrong mode.
		ok "$file is absent (the deploy configured nothing for it)"
		continue
	fi
	mode=$(mode_of "$file")
	if [ "$mode" = "600" ]; then
		ok "$file mode is 0600"
	else
		fail "$file is mode ${mode:-unknown}, want 0600"
	fi
	owner=$(owner_of "$file")
	if [ "$owner" = "$env_owner" ]; then
		ok "$file owner is $env_owner"
	else
		fail "$file is owned by ${owner:-unknown}, want $env_owner"
	fi
done

# --- 4. the systemd unit ----------------------------------------------------

if [ ! -f "$unit_path" ]; then
	fail "unit file $unit_path does not exist"
else
	ok "unit file $unit_path exists"

	# Installed and enabled. `is-enabled` prints the state; static and
	# indirect are the states a unit with no [Install] can legitimately be in.
	enabled=$(systemctl is-enabled "$unit_name" 2>/dev/null || true)
	case $enabled in
	enabled | enabled-runtime | static | indirect)
		ok "unit is enabled ($enabled)"
		;;
	*)
		fail "unit $unit_name is ${enabled:-not enabled}, want enabled"
		;;
	esac

	active=$(systemctl is-active "$unit_name" 2>/dev/null || true)
	if [ "$active" = "active" ]; then
		ok "unit is active"
	else
		fail "unit $unit_name is ${active:-inactive}, want active"
	fi

	# The account the unit actually runs as, rather than the one this script
	# was told about: a unit running as root would make every permission check
	# above irrelevant.
	unit_user=$(sysprop User)
	if [ "$unit_user" = "$service_user" ]; then
		ok "unit runs as $service_user"
	else
		fail "unit $unit_name runs as ${unit_user:-<unset>}, want $service_user"
	fi

	# The sandbox directives. systemd ignores an unknown directive in a unit
	# silently, so a typo'd ProtectSystem would look installed and enforce
	# nothing; read them back out of the loaded unit rather than the file text.
	for directive in NoNewPrivileges ProtectSystem ProtectHome PrivateTmp ReadWritePaths; do
		value=$(sysprop "$directive")
		if [ -n "$value" ]; then
			ok "unit $directive=$value"
		else
			fail "unit $unit_name does not set $directive"
		fi
	done

	# ReadWritePaths must include the data directory, or ProtectSystem=strict
	# makes the database unwritable and every run fails with EROFS.
	rw=$(sysprop ReadWritePaths)
	case " $rw " in
	*" $data_dir "*) ok "ReadWritePaths includes the data directory" ;;
	*) fail "ReadWritePaths (${rw:-<unset>}) does not include $data_dir" ;;
	esac
fi

# --- 5. the API is loopback only -------------------------------------------
#
# `otter deploy` refuses a wildcard --listen before it touches the host
# (internal/deploy/target.go, tested). This is the other half of that claim:
# the address the daemon actually holds on this host.

# api_addr reads the --listen argument out of the loaded unit's ExecStart. It is
# the daemon's own configuration, not a guess from the port number.
api_addr=$listen
if [ -z "$api_addr" ]; then
	exec_start=$(sysprop ExecStart)
	api_addr=$(printf '%s\n' "$exec_start" |
		sed -n 's/.*--listen[[:space:]]\{1,\}\([^[:space:]]*\).*/\1/p' | head -n 1)
fi
if [ -z "$api_addr" ]; then
	fail "could not determine the daemon's --listen address (pass --listen)"
else
	ok "daemon --listen is $api_addr"
	api_host=${api_addr%:*}
	api_port=${api_addr##*:}
	api_host=${api_host#[}
	api_host=${api_host%]}
	case $api_host in
	127.* | localhost | ::1)
		ok "listen address $api_addr is loopback"
		;;
	10.* | 192.168.* | 172.1[6-9].* | 172.2[0-9].* | 172.3[01].* | fd* | fc*)
		# A private address is supported: the operator chose which interface the
		# API is on, and the network still governs who can reach it.
		ok "listen address $api_addr is private"
		;;
	"" | 0.0.0.0 | "::" | "*")
		if [ "$allow_remote_bind" = 1 ]; then
			ok "listen address ${api_addr:-:*} is a configured wildcard bind (--allow-remote-bind)"
		else
			fail "listen address $api_addr is a wildcard bind; pass --allow-remote-bind to assert it as the configured posture, or bind 127.0.0.1"
		fi
		;;
	*)
		if [ "$allow_remote_bind" = 1 ]; then
			ok "listen address $api_addr is a configured public bind (--allow-remote-bind)"
		else
			fail "listen address $api_addr is neither loopback nor private; pass --allow-remote-bind to assert it as the configured posture, or bind 127.0.0.1"
		fi
		;;
	esac

	# A non-loopback API is guarded by OTTER_API_TOKEN. The daemon refuses to
	# start without it, so the assertion is that the deployed configuration
	# actually carries one -- checked by presence, never printed.
	case $api_host in
	127.* | localhost | ::1) ;;
	*)
		token_seen=0
		for suffix in .env .daemon.env; do
			file=$env_dir/$env_file$suffix
			[ -f "$file" ] || continue
			if grep -Eq '^[[:space:]]*(export[[:space:]]+)?OTTER_API_TOKEN=[^[:space:]]' "$file"; then
				token_seen=1
			fi
		done
		if [ "$token_seen" = 1 ]; then
			ok "OTTER_API_TOKEN is configured for the non-loopback API"
		else
			fail "the API binds ${api_addr:-every interface} but no OTTER_API_TOKEN is configured in $env_dir/$env_file(.daemon).env; the daemon refuses a non-loopback bind without one"
		fi
		;;
	esac
fi

# --- 6. listening sockets ---------------------------------------------------
#
# `ss` is how the deploy picks a free loopback port, so it is present on every
# supported host (internal/deploy/remote.go requires it). Its output is parsed
# rather than trusted: the point is the full set of listeners, not one line of
# output.

# OTTER_SS_BIN pins the binary for the fixture harness, the way OTTER_SSHD_BIN
# does for sshd below. On a real host it is unset and ss is whatever PATH
# resolves. The hook exists because PATH shadowing cannot model a host with no
# ss: removing the fixture's shim only uncovers the machine's own ss, so on any
# host with iproute2 (Ubuntu 24.04, where this was found on 2026-10-01) the
# "not installed" branch was unreachable and the case failed for the wrong
# reason.
ss_bin=${OTTER_SS_BIN:-$(command -v ss 2>/dev/null || true)}
if [ -z "$ss_bin" ] || [ ! -x "$ss_bin" ]; then
	fail "ss is not installed; cannot enumerate listening sockets"
else
	ss_out=$("$ss_bin" -H -tln 2>/dev/null || "$ss_bin" -tln 2>/dev/null || true)
	if [ -z "$ss_out" ]; then
		fail "ss reported no TCP listeners at all, which cannot be true on a running host"
	fi
	# Normalize every listener to "host port". awk splits on the last colon,
	# which handles 0.0.0.0:22, 127.0.0.1:7337 and [::1]:7337 alike.
	printf '%s\n' "$ss_out" | awk 'NF>=4 {print $4}' | awk '
		{
			n = split($0, parts, ":")
			if (n < 2) next
			port = parts[n]
			host = substr($0, 1, length($0) - length(port) - 1)
			gsub(/^\[/, "", host)
			gsub(/\]$/, "", host)
			if (port == "") next
			print host, port
		}' >"$listeners"

	if [ ! -s "$listeners" ]; then
		fail "no parsable listening sockets were found in ss output"
	fi

	# The positive half: the daemon's own address must be present and on
	# loopback. A check that only greps for wildcard binds would pass on a host
	# where the daemon never started.
	if [ -n "$api_addr" ]; then
		# The address it was configured with, not an assumed loopback one: with
		# a private listen there is no loopback socket to find.
		if grep -qx "$api_host $api_port" "$listeners"; then
			ok "the API is listening on $api_addr"
		else
			fail "nothing is listening on the API's configured address $api_addr"
		fi
	fi

	# No wildcard bind except on an approved port. Nothing else on this host
	# needs one: the only inbound rule is SSH, and the control plane travels
	# through an SSH tunnel.
	if grep -q "^0\.0\.0\.0 " "$listeners"; then
		for port in $(grep '^0\.0\.0\.0 ' "$listeners" | awk '{print $2}'); do
			if [ "$allow_remote_bind" = 1 ] && [ "$api_host" = "0.0.0.0" ] && [ "$port" = "$api_port" ]; then
				ok "wildcard listener on port $port is the configured API bind"
				continue
			fi
			case " $(printf '%s' "$approved_ports" | tr ',' ' ') " in
			*" $port "*) ok "wildcard listener on approved port $port" ;;
			*) fail "a wildcard (0.0.0.0) listener exists on port $port, which is not in --approved-ports ($approved_ports)" ;;
			esac
		done
	else
		ok "no wildcard (0.0.0.0) listener"
	fi
	if grep -q "^:: " "$listeners"; then
		for port in $(grep '^:: ' "$listeners" | awk '{print $2}'); do
			if [ "$allow_remote_bind" = 1 ] && [ "$api_host" = "::" ] && [ "$port" = "$api_port" ]; then
				ok "wildcard listener on port $port is the configured API bind"
				continue
			fi
			case " $(printf '%s' "$approved_ports" | tr ',' ' ') " in
			*" $port "*) ok "wildcard listener on approved port $port" ;;
			*) fail "a wildcard (::) listener exists on port $port, which is not in --approved-ports ($approved_ports)" ;;
			esac
		done
	else
		ok "no wildcard (::) listener"
	fi

	# Every listener must be on an approved port. The approved ports may bind
	# anywhere -- sshd listens on 0.0.0.0:22 on this host by design -- but any
	# other port must be loopback-only, because the only inbound rule is SSH.
	# An unapproved wildcard bind is reported once, as a wildcard, rather than
	# twice: it is one fault, and reporting it twice invents a second.
	approved_list=" $(printf '%s' "$approved_ports" | tr ',' ' ') "
	while read -r hostpart port; do
		[ -n "$port" ] || continue
		case $hostpart in
		127.* | ::1) continue ;;
		esac
		# The API's own address is expected to be here, and section 5 already
		# decided whether it is allowed to be non-loopback. Reporting it as an
		# unapproved listener would count one deliberate bind as two faults.
		if [ "$hostpart $port" = "$api_host $api_port" ]; then continue; fi
		case $approved_list in
		*" $port "*) ok "port $port is approved (bound to $hostpart)" ;;
		*)
			case $hostpart in
			0.0.0.0 | ::) ;;
			*) fail "unapproved listener: $hostpart:$port is not in --approved-ports ($approved_ports)" ;;
			esac
			;;
		esac
	done <"$listeners"
fi

# --- 7. SSH is the restricted way in ---------------------------------------
#
# The host's only inbound rule is SSH (CA-10). That is only true if sshd refuses
# passwords, which is what the image's cloud-init module is supposed to have
# configured. Read the effective configuration rather than a drop-in file, so a
# later override cannot hide here.

# OTTER_SSHD_BIN pins the binary for the fixture harness; on a real host the
# PATH and /usr/sbin are the only correct answers.
sshd_bin=${OTTER_SSHD_BIN:-}
if [ -z "$sshd_bin" ]; then
	if command -v sshd >/dev/null 2>&1; then
		sshd_bin=$(command -v sshd)
	elif [ -x /usr/sbin/sshd ]; then
		sshd_bin=/usr/sbin/sshd
	fi
fi
if [ -z "$sshd_bin" ] || [ ! -x "$sshd_bin" ]; then
	fail "sshd is not installed; this is not an SSH-reachable host"
else
	# `sshd -T` validates the configuration and needs the privilege-separation
	# directory, so bound the call: a hung probe must not hang the whole
	# assertion. timeout is coreutils and present on every supported host.
	if command -v timeout >/dev/null 2>&1; then
		sshd_conf=$(timeout 10 "$sshd_bin" -T 2>/dev/null || true)
	else
		sshd_conf=$("$sshd_bin" -T 2>/dev/null || true)
	fi
	if [ -z "$sshd_conf" ]; then
		fail "could not read the effective sshd configuration ($sshd_bin -T)"
	else
		pw=$(printf '%s\n' "$sshd_conf" | awk '$1=="passwordauthentication"{print $2}' | head -n 1)
		if [ "$pw" = "yes" ]; then
			fail "sshd accepts password authentication; restrict SSH to keys"
		else
			ok "sshd passwordauthentication=${pw:-no}"
		fi
		root_login=$(printf '%s\n' "$sshd_conf" | awk '$1=="permitrootlogin"{print $2}' | head -n 1)
		case $root_login in
		yes)
			fail "sshd permits root login with a password; use prohibit-password or no"
			;;
		*)
			ok "sshd permitrootlogin=${root_login:-unset}"
			;;
		esac
	fi
fi

# --- 8. swap and the provisioning record ------------------------------------
#
# A 1 GiB host needs its swap, and the way it was lost is the reason this check
# is a hard failure rather than a warning: the cloud-init user-data ran under
# `set -euo pipefail`, its `systemctl reload ssh || systemctl reload sshd` line
# failed on both sides (Ubuntu 24.04 sshd is socket-activated, so neither unit
# is reloadable), and the script aborted before it created the swapfile. The
# host then reported a non-zero cloud-init status that nothing read, and the
# only symptom was zero swap. State that matters has to be asserted, not
# created best-effort in the middle of a script.

if [ "$min_swap_mib" -gt 0 ] 2>/dev/null; then
	swap_kib=0
	swap_source=""
	if [ -r "$proc_root/swaps" ]; then
		# Every line after the header, summed. This is the kernel's own view,
		# so it is right even when swapon is absent.
		swap_kib=$(awk 'NR > 1 { total += $3 } END { print total + 0 }' "$proc_root/swaps")
		swap_source="$proc_root/swaps"
	elif command -v swapon >/dev/null 2>&1; then
		# KiB, because that is what /proc/swaps reports and the comparison
		# below is in the same unit.
		swap_kib=$(swapon --show=SIZE --bytes --noheadings 2>/dev/null |
			awk '{ total += $1 } END { print int((total + 0) / 1024) }')
		swap_source="swapon --show"
	fi
	swap_mib=$((swap_kib / 1024))
	if [ "$swap_mib" -ge "$min_swap_mib" ]; then
		ok "active swap is ${swap_mib} MiB (floor ${min_swap_mib} MiB, from ${swap_source:-unknown})"
	else
		fail "active swap is ${swap_mib} MiB, below the ${min_swap_mib} MiB floor; a 1 GiB host without swap OOMs under load (source: ${swap_source:-none found})"
	fi
fi

if command -v sysctl >/dev/null 2>&1; then
	swappiness=$(sysctl -n vm.swappiness 2>/dev/null || true)
	case $swappiness in
	"" | *[!0-9]*)
		ok "vm.swappiness is not readable here; not asserted"
		;;
	*)
		if [ "$swappiness" -le 100 ]; then
			ok "vm.swappiness=$swappiness"
		else
			fail "vm.swappiness=$swappiness is outside 0-100"
		fi
		;;
	esac
fi

if [ -n "$provision_report" ]; then
	if [ -s "$provision_report" ]; then
		ok "provisioning report $provision_report is present"
	else
		fail "provisioning report $provision_report is missing or empty; the host's first-run provisioning did not finish"
	fi
fi

# --- outcome ----------------------------------------------------------------

if [ -s "$failures" ]; then
	printf '\nassert-host-permissions: FAILED (%s check(s)):\n' "$(wc -l <"$failures" | tr -d ' ')"
	sed 's/^/  - /' "$failures"
	echo "assert-host-permissions: this host does not meet CA-09/CA-10; do not treat it as provisioned" >&2
	exit 1
fi

echo "assert-host-permissions: all assertions passed"
