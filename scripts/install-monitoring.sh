#!/bin/sh
# Install the dead-man monitoring for an Otter runtime host, and prove the
# checks run.
#
# Phase 0's monitoring is three shell scripts (heartbeat.sh, disk-check.sh and
# the otter-metric.sh they both call), four systemd units (the two services and
# their two timers) and two environment files under /etc/otter/. Until this
# script existed they were installed by hand, following the commands in the unit
# headers -- so nothing in `otter deploy` or scripts/provision.sh installed
# them, and a rebuilt host silently came up with no monitoring at all. The
# alarms live off the host and treat missing data as breaching, so a host with
# no checks does not stay quiet for long: it looks dead, about five minutes
# after the second `cdk deploy` that passes `-c alertEmail`.
#
# The deployer cannot own this. `//go:embed` can only embed files inside the
# embedding package (sdk/embed.go embeds sdk/python, migrations/embed.go embeds
# migrations/*.sql), so shipping scripts/*.sh inside the binary would mean a
# second copy of the canonical scripts -- and the copy in the checkout, not the
# one in the binary, is what the unit headers and the rebuild procedure name.
# This script lives beside them instead, and installs exactly what the headers
# describe: the three scripts into /usr/local/lib/otter/ mode 0755 root-owned,
# the four units into /etc/systemd/system/ mode 0644, and the two environment
# files into /etc/otter/ mode 0640 root:root.
#
# The order of a rebuild is: `cdk deploy` (the host, with no alertEmail), then
# `otter deploy` (the runtime -- it is what creates <workspace>/.otter/data),
# then `sh scripts/install-monitoring.sh --host ubuntu@<ip>`, then `cdk deploy`
# again with `-c alertEmail=<address>` once the checks are visibly publishing.
# Installing the checks after the runtime is not a preference: the disk check
# measures <workspace>/.otter/data, so installing it before `otter deploy` makes
# it fail from its first tick and raise the alarm on a healthy host. This script
# refuses that case by name rather than installing a check that lies.
#
# It refuses, before it changes anything, when:
#   * --host is missing;
#   * ssh does not answer, or the login is not root and has no passwordless sudo;
#   * the runtime's data directory does not exist on the host (the message names
#     `otter deploy`, which is the step that creates it);
#   * --metric-host is empty or holds a character outside [A-Za-z0-9_.-], because
#     it is used verbatim as the CloudWatch Host dimension the alarms match.
#
# Installing is not the claim; working monitoring is. After the files are in
# place this script asserts both timers active, runs each service once, requires
# ExecMainStatus=0 and prints the timer state. If either service fails it exits
# non-zero with that service's journal: an installed-but-failing check is worse
# than no check, because it alarms forever and the host looks monitored.
#
# No credential is handled here, and none is printed -- there is none to handle.
# The environment files carry a host label (OTTER_METRIC_HOST) and a data path
# (OTTER_DATA_DIR); the metric is authenticated by the instance role the host
# already has. That is stated rather than left implied, because a script that
# writes files under /etc/otter/ should say what is in them.
#
#   sh scripts/install-monitoring.sh --host ubuntu@203.0.113.10
#   sh scripts/install-monitoring.sh --host ubuntu@203.0.113.10 --identity ~/.ssh/castor
#   sh scripts/install-monitoring.sh --host ubuntu@203.0.113.10 \
#       --data-dir /opt/otter/workspaces/<workspace>/.otter/data
#   OTTER_INSTALL_DRY_RUN=1 sh scripts/install-monitoring.sh --host ubuntu@203.0.113.10
#
# The dry run prints the plan and still performs the read-only preflight
# (reachability, sudo, the data-directory probe): "would install" reported over a
# host that cannot be reached is not a plan an operator can act on. Nothing that
# changes the host is executed.
#
# Falsifiability: `sh scripts/test-install-monitoring.sh` stands in for ssh with
# a fake that answers from a fixture and records every command, then asserts the
# paths, modes and contents installed, that both services were run and asserted,
# that a service exiting non-zero fails the whole install, that a second run
# converges, and that a dry run mutates nothing.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# The canonical files are read from the directory this script lives in. The
# override exists so scripts/test-install-monitoring.sh can point the matrix at
# a mutated copy of this script in a temporary directory while still uploading
# the checkout's own scripts, the same way OTTER_ASSERT_SCRIPT exists in
# scripts/provision.sh. On a real run the default is the only correct answer.
source_dir=${OTTER_INSTALL_SOURCE_DIR:-$here}

# --- defaults ---------------------------------------------------------------

host=${OTTER_INSTALL_HOST:-}
identity=${OTTER_INSTALL_IDENTITY:-}
port=${OTTER_INSTALL_PORT:-22}
data_dir=${OTTER_INSTALL_DATA_DIR:-}
# Castor's Host dimension; the stack's `-c monitoringHost` must agree with it.
# No colon: an explicitly empty value must stay empty and be refused below
# rather than fall back to the default in silence.
metric_host=${OTTER_INSTALL_METRIC_HOST-castor-runtime}
dry_run=${OTTER_INSTALL_DRY_RUN:-0}

# The install locations, taken from the headers of heartbeat.service and
# disk-check.service -- not invented here.
lib_dir=/usr/local/lib/otter
unit_dir=/etc/systemd/system
env_dir=/etc/otter
remote_workspace_root=/opt/otter/workspaces

usage() {
	cat <<'USAGE'
usage: install-monitoring.sh --host USER@HOST [options]

Install the Otter dead-man checks (three scripts, four systemd units, two
environment files) on a host `otter deploy` has already converged, then run
each check once and require it to succeed.

  --host USER@HOST       SSH destination (required; with no user, ubuntu@ is
                         assumed)
  --identity FILE        private key for SSH
  --port N               SSH port (default 22)
  --data-dir DIR         the runtime's data directory. Default: the single
                         /opt/otter/workspaces/<workspace>/.otter/data found on
                         the host; more than one workspace, or none, is an
                         error rather than a guess
  --metric-host NAME     the CloudWatch Host dimension (default castor-runtime);
                         [A-Za-z0-9_.-] only
  -h, --help             this text

Environment: OTTER_INSTALL_HOST, OTTER_INSTALL_IDENTITY, OTTER_INSTALL_PORT,
OTTER_INSTALL_DATA_DIR, OTTER_INSTALL_METRIC_HOST, OTTER_INSTALL_DRY_RUN=1.
OTTER_INSTALL_SOURCE_DIR exists for the fixture harness and is not meant to be
set on a real run.

Refuses, before it changes anything, when --host is missing, ssh is
unreachable, the login has no passwordless sudo and is not root, the data
directory does not exist on the host (`otter deploy` creates it), or
--metric-host is empty or malformed.
USAGE
}

while [ $# -gt 0 ]; do
	case $1 in
	--host)
		host=$2
		shift 2
		;;
	--identity)
		identity=$2
		shift 2
		;;
	--port)
		port=$2
		shift 2
		;;
	--data-dir)
		data_dir=$2
		shift 2
		;;
	--metric-host)
		metric_host=$2
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "install-monitoring: unknown argument: $1" >&2
		usage >&2
		exit 2
		;;
	esac
done

# --- refuse what can be refused locally, before the host is touched ---------

if [ -z "$host" ]; then
	echo "install-monitoring: --host is required, for example --host ubuntu@203.0.113.10" >&2
	exit 2
fi
case $host in
*@*) destination=$host ;;
*) destination=ubuntu@$host ;;
esac

case $metric_host in
"" | *[!A-Za-z0-9_.-]*)
	echo "install-monitoring: FAIL: --metric-host '$metric_host' is empty or is not a plain name ([A-Za-z0-9_.-]); it is used verbatim as the CloudWatch Host dimension the alarms match" >&2
	exit 2
	;;
esac

case $port in
"" | *[!0-9]*)
	echo "install-monitoring: FAIL: --port '$port' is not a port number" >&2
	exit 2
	;;
esac

case $data_dir in
"" | /*) ;;
*)
	echo "install-monitoring: FAIL: --data-dir '$data_dir' is not an absolute path; it names a directory on $destination" >&2
	exit 2
	;;
esac

case $dry_run in
"" | 0) dry_run=0 ;;
1) dry_run=1 ;;
*)
	echo "install-monitoring: FAIL: OTTER_INSTALL_DRY_RUN='$dry_run' is not 1 or 0; a dry run that is not understood must not be treated as one" >&2
	exit 2
	;;
esac

# The canonical files ship beside this script; a checkout missing one of them is
# a broken checkout, not a host to be half-installed.
for file in heartbeat.sh disk-check.sh otter-metric.sh \
	heartbeat.service heartbeat.timer disk-check.service disk-check.timer; do
	if [ ! -f "$source_dir/$file" ]; then
		echo "install-monitoring: FAIL: missing $source_dir/$file; run this from the checkout that holds scripts/" >&2
		exit 2
	fi
done

# --- output -----------------------------------------------------------------

say() {
	printf 'install-monitoring: %s\n' "$1"
}

die() {
	printf 'install-monitoring: FAIL: %s\n' "$1" >&2
	exit 1
}

# quote single-quotes a string for a POSIX shell. Every remote command is built
# through it, so a path with a space cannot turn into two commands.
quote() {
	printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

printf '\n===== install-monitoring =====\n'
printf 'date:        %s\n' "$(date '+%Y-%m-%d %H:%M:%S %Z')"
printf 'host:        %s %s (%s)\n' "$(uname -s)" "$(uname -r)" "$(uname -m)"
printf 'checkout:    %s\n' "$(git -C "$here/.." describe --tags --always --dirty 2>/dev/null || echo unknown)"
printf 'destination: %s\n' "$destination"
printf 'data dir:    %s\n' "${data_dir:-<the single workspace under $remote_workspace_root>}"
printf 'metric host: %s\n' "$metric_host"
printf 'dry run:     %s\n' "$dry_run"

# The plan is printed before the preflight, so an operator can read it even when
# the host does not answer -- which is exactly the case the dry run is for.
if [ "$dry_run" -eq 1 ]; then
	printf 'install-monitoring: dry run: this is the plan; nothing on %s will change:\n' "$destination"
	printf 'install-monitoring:   1. preflight: ssh answers, and the login has passwordless sudo when it is not root\n'
	printf 'install-monitoring:   2. require the runtime data directory %s to exist on the host\n' "${data_dir:-<the single workspace under $remote_workspace_root>}"
	printf 'install-monitoring:   3. install -d -m 0755 %s and install -d -m 0750 %s\n' "$lib_dir" "$env_dir"
	printf 'install-monitoring:   4. write heartbeat.sh, disk-check.sh and otter-metric.sh into %s, mode 0755 root:root\n' "$lib_dir"
	printf 'install-monitoring:   5. write heartbeat.service, heartbeat.timer, disk-check.service and disk-check.timer into %s, mode 0644 root:root\n' "$unit_dir"
	printf 'install-monitoring:   6. write %s/heartbeat.env (OTTER_METRIC_HOST=%s) and %s/disk-check.env (OTTER_METRIC_HOST=%s, OTTER_DATA_DIR=<the data dir>), mode 0640 root:root\n' "$env_dir" "$metric_host" "$env_dir" "$metric_host"
	printf 'install-monitoring:   7. systemctl daemon-reload, then systemctl enable --now heartbeat.timer disk-check.timer\n'
	printf 'install-monitoring:   8. assert both timers active, run each service once, require ExecMainStatus=0, print the timer state\n'
fi

# --- ssh plumbing -----------------------------------------------------------
#
# Every host interaction goes through `remote`, so a dry run has exactly one
# door to hold shut and the prerequisite probes are exactly the read-only ones.

ssh_args="-o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15"
ssh_args="$ssh_args -p $port"
[ -z "$identity" ] || ssh_args="$ssh_args -i $identity"

remote() {
	# shellcheck disable=SC2086
	ssh $ssh_args "$destination" "$1"
}

# mutate runs a remote command that changes the host, or prints it under a dry
# run. Read-only probes deliberately do not come through here.
mutate() {
	if [ "$dry_run" -eq 1 ]; then
		say "dry-run: would run: $1"
		return 0
	fi
	remote "$1" || die "the command failed on $destination: $1"
}

# upload streams a local file to a remote path as root and sets its mode. The
# file is created by a root shell (sudo -n), so it is root:root by construction;
# chown says so rather than relying on that. The umask is fixed so a run under an
# unusual login umask cannot leave a wider file behind for the chmod to fix late.
upload() { # local_file, remote_path, mode
	program="umask 022; cat > $2 && chown root:root $2 && chmod $3 $2"
	if [ "$dry_run" -eq 1 ]; then
		say "dry-run: would write $2 (mode $3 root:root) from $1"
		return 0
	fi
	# shellcheck disable=SC2086
	ssh $ssh_args "$destination" "$sudo sh -c $(quote "$program")" <"$1" ||
		die "could not write $2 on $destination"
}

tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/otter-install-monitoring.XXXXXX")
cleanup() {
	rm -rf "$tmpdir"
}
trap cleanup EXIT HUP INT TERM

# --- 1. the host answers, and root is available -----------------------------

say "checking $destination is reachable over SSH"
login_uid=$(remote 'id -u' 2>&1) || die "$destination is not reachable over SSH: $login_uid"
login_uid=$(printf '%s' "$login_uid" | tr -d '\r')
[ -n "$login_uid" ] || die "$destination returned nothing for 'id -u'"
if [ "$login_uid" = "0" ]; then
	sudo=
	say "the login on $destination is root; sudo is not needed"
else
	remote 'sudo -n true' >/dev/null 2>&1 ||
		die "the login on $destination is not root and has no passwordless sudo; installing the units and enabling the timers needs root"
	sudo='sudo -n'
	say "the login on $destination is not root; using sudo -n"
fi

# --- 2. the runtime exists, and it says where its data is -------------------
#
# The disk check measures <workspace>/.otter/data, so a host with no workspace
# has no data directory to measure and a check installed anyway would fail from
# its first tick. The workspace is discovered the same way
# scripts/provision.sh discovers it -- by listing the deploy root on the host --
# and a host with zero or several workspaces is an error rather than a guess.

if [ -z "$data_dir" ]; then
	say "discovering the workspace under $remote_workspace_root"
	workspaces=$(remote "ls -d $remote_workspace_root/*/ 2>/dev/null" 2>/dev/null) || workspaces=
	workspaces=$(printf '%s\n' "$workspaces" | tr -d '\r' | sed 's:/$::' | sed '/^$/d')
	count=$(printf '%s\n' "$workspaces" | sed '/^$/d' | wc -l | tr -d ' ')
	if [ "$count" -eq 0 ]; then
		die "$destination has no workspace under $remote_workspace_root, so there is no data directory to measure. 'otter deploy' is the step that creates <workspace>/.otter/data; run it, then run this script again, or pass --data-dir explicitly."
	fi
	if [ "$count" -ne 1 ]; then
		die "$destination has $count workspaces under $remote_workspace_root ($(printf '%s' "$workspaces" | tr '\n' ' ')); pass --data-dir to name the runtime's own <workspace>/.otter/data rather than have this script guess."
	fi
	data_dir=$workspaces/.otter/data
fi

data_dir_exists() {
	remote "test -d $(quote "$data_dir")" >/dev/null 2>&1
}

say "checking the runtime data directory $data_dir exists on $destination"
if ! data_dir_exists; then
	die "$destination has no data directory at $data_dir. 'otter deploy' is the step that creates it (the runtime's state lives in <workspace>/.otter/data); installing the disk check before the runtime exists makes it alarm on a healthy host."
fi

# --- 3. what is already there ----------------------------------------------
#
# A second run has to converge and say so. The probe is read-only and reports
# what is stale, and a converged host is still re-asserted below: the point of a
# re-run is a working timer, not a file that merely exists.

installed_probe=$(cat <<'PROBE'
# install-monitoring-probe
missing=''
check() {
	[ -f "$1" ] || { missing="$missing $1(missing)"; return 0; }
	mode=$(stat -c '%a' "$1" 2>/dev/null) || mode=unknown
	[ "$mode" = "$2" ] || missing="$missing $1(mode=$mode)"
}
check /usr/local/lib/otter/heartbeat.sh 755
check /usr/local/lib/otter/disk-check.sh 755
check /usr/local/lib/otter/otter-metric.sh 755
check /etc/systemd/system/heartbeat.service 644
check /etc/systemd/system/heartbeat.timer 644
check /etc/systemd/system/disk-check.service 644
check /etc/systemd/system/disk-check.timer 644
check /etc/otter/heartbeat.env 640
check /etc/otter/disk-check.env 640
systemctl is-enabled --quiet heartbeat.timer 2>/dev/null || missing="$missing heartbeat.timer(not-enabled)"
systemctl is-enabled --quiet disk-check.timer 2>/dev/null || missing="$missing disk-check.timer(not-enabled)"
printf '%s' "$missing"
PROBE
)
# A probe that could not run is not a converged host: it reports nothing stale
# for the same reason it reports nothing at all, and claiming "already
# installed" off a failed probe is the kind of green that hides a broken host.
already=0
if probe_out=$(remote "$installed_probe" 2>/dev/null); then
	probe_out=$(printf '%s' "$probe_out" | tr -d '\r')
	if [ -z "$probe_out" ]; then
		already=1
	fi
else
	probe_out=""
fi
if [ "$already" -eq 1 ]; then
	say "the monitors are already installed and converged on $destination; re-asserting them"
else
	say "installing the monitors on $destination (not yet converged:${probe_out})"
fi

# --- 4. install -------------------------------------------------------------

say "creating $lib_dir (0755) and $env_dir (0750)"
mutate "$sudo install -d -m 0755 $lib_dir"
mutate "$sudo install -d -m 0750 $env_dir"

say "installing the three check scripts into $lib_dir"
for script in heartbeat.sh disk-check.sh otter-metric.sh; do
	upload "$source_dir/$script" "$lib_dir/$script" 0755
done

say "installing the four systemd units into $unit_dir"
for unit in heartbeat.service heartbeat.timer disk-check.service disk-check.timer; do
	upload "$source_dir/$unit" "$unit_dir/$unit" 0644
done

# write_env writes an environment file whole, but keeps any line an operator
# added that this script does not manage: the unit headers invite an
# OTTER_METRIC_REGION to be added there, and a re-run that dropped it would
# change how the metric is signed while looking like a no-op.
write_env() { # remote_path, with_data_dir (yes/no)
	env_tmp=$tmpdir/$(basename "$1")
	if [ "$dry_run" -eq 1 ]; then
		if [ "$2" = "yes" ]; then
			say "dry-run: would write $1 (mode 0640 root:root) with OTTER_METRIC_HOST=$metric_host and OTTER_DATA_DIR=$data_dir"
		else
			say "dry-run: would write $1 (mode 0640 root:root) with OTTER_METRIC_HOST=$metric_host"
		fi
		return 0
	fi
	existing=$(remote "cat $(quote "$1") 2>/dev/null" 2>/dev/null) || existing=
	{
		printf 'OTTER_METRIC_HOST=%s\n' "$metric_host"
		if [ "$2" = "yes" ]; then
			printf 'OTTER_DATA_DIR=%s\n' "$data_dir"
		fi
		printf '%s\n' "$existing" | tr -d '\r' | while IFS= read -r line; do
			[ -n "$line" ] || continue
			key=${line%%=*}
			case $key in
			OTTER_METRIC_HOST | OTTER_DATA_DIR) continue ;;
			esac
			printf '%s\n' "$line"
		done
	} >"$env_tmp"
	upload "$env_tmp" "$1" 0640
}

say "installing the environment files into $env_dir"
write_env "$env_dir/heartbeat.env" no
write_env "$env_dir/disk-check.env" yes

say "reloading systemd and enabling both timers"
mutate "$sudo systemctl daemon-reload"
mutate "$sudo systemctl enable --now heartbeat.timer disk-check.timer"

# --- 5. assert the install is a working one ---------------------------------
#
# The install is not the claim. Read the loaded units, not the files that were
# just written: systemd ignores an unknown directive silently, and a timer that
# exists but is not active runs nothing.

# sysprop reads one property off a loaded unit. On systemd >= 253 --value is
# enough; the fallback keeps an older host from reporting every property as
# unset, which would fail the assertion for the wrong reason.
sysprop() { # unit, property
	value=$(remote "$sudo systemctl show -p $2 --value $1" 2>/dev/null) || value=
	value=$(printf '%s' "$value" | tr -d '\r')
	if [ -z "$value" ]; then
		value=$(remote "$sudo systemctl show -p $2 $1" 2>/dev/null | sed -n "s/^$2=//p" | head -n 1) || value=
		value=$(printf '%s' "$value" | tr -d '\r')
	fi
	printf '%s' "$value"
}

require_timer() { # timer unit
	if [ "$dry_run" -eq 1 ]; then
		say "dry-run: would require $1 active and print its state"
		return 0
	fi
	state=$(remote "$sudo systemctl is-active $1" 2>/dev/null) || state=
	state=$(printf '%s' "$state" | tr -d '\r')
	enabled=$(remote "$sudo systemctl is-enabled $1" 2>/dev/null) || enabled=
	enabled=$(printf '%s' "$enabled" | tr -d '\r')
	say "timer $1: active=${state:-unknown} enabled=${enabled:-unknown}"
	if [ "$state" != "active" ]; then
		status_out=$(remote "$sudo systemctl status $1 --no-pager -l" 2>&1) || status_out=
		printf 'install-monitoring: FAIL: %s is %s, not active; the timer that should run the check is not running:\n' \
			"$1" "${state:-unknown}" >&2
		printf '%s\n' "$status_out" >&2
		exit 1
	fi
}

run_service() { # service unit
	if [ "$dry_run" -eq 1 ]; then
		say "dry-run: would run $1 once and require exit 0"
		return 0
	fi
	started=$(remote "$sudo systemctl start $1" 2>&1) || true
	status=$(sysprop "$1" ExecMainStatus)
	if [ "$status" != "0" ]; then
		journal=$(remote "$sudo journalctl -u $1 -n 50 --no-pager" 2>&1) || journal=
		printf 'install-monitoring: FAIL: %s exited %s, so the installed check is failing. An installed-but-failing check alarms forever and the host still looks monitored; the journal says why:\n' \
			"$1" "${status:-unknown}" >&2
		[ -z "$started" ] || printf '%s\n' "$started" >&2
		printf '%s\n' "$journal" >&2
		exit 1
	fi
	say "$1 ran once and exited 0"
}

say "asserting the installed monitoring"
require_timer heartbeat.timer
require_timer disk-check.timer
run_service heartbeat.service
run_service disk-check.service

if [ "$dry_run" -eq 1 ]; then
	printf 'install-monitoring: dry run complete — nothing on %s was changed\n' "$destination"
else
	printf 'install-monitoring: OK — %s is monitored: both timers are active and each check ran once with exit 0\n' "$destination"
	say "next: confirm the Otter-namespace metrics are arriving, then run the second cdk deploy with -c alertEmail=<address>; the alarms treat missing data as breaching, so they are only useful once these checks publish"
fi
