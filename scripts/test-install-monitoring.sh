#!/bin/sh
# Falsifiability for scripts/install-monitoring.sh.
#
# install-monitoring.sh talks to a host over ssh and installs the dead-man
# checks there. This harness stands in for ssh: a fake answers the preflight
# questions from a fixture, records every command, and captures the stdin of
# every upload, so the cases can assert the exact paths, modes and contents that
# were installed, the two services that were run and asserted, and the two
# timers that were reloaded and enabled.
#
# The case that matters most is the one the monitoring exists to survive: a
# check that installs and then fails. `a check that exits non-zero fails the
# install` below is that host. A failing check publishes nothing, the alarm
# treats missing data as breaching, and the host alarms forever while looking
# monitored -- so the installer must fail the whole run, with the journal.
#
# The convergence probe is not stubbed. `install-monitoring.sh --print-probe`
# prints the probe a run would execute, with the paths that run is pointed at;
# the block below builds a fixture tree with the modes the install produces and
# runs that text against it, so "a converged host" is something the suite proves
# rather than something it is told. That is how the bug this block was written
# for stayed invisible: the old probe ran as the plain login, /etc/otter is 0750
# root:root, and a fully converged host reported mode=unknown forever while a
# fixture record that answered the probe by pattern kept the suite green.
#
# The same directory makes the env-file read that preserves an operator's line a
# privileged read. The fixture records are written for the login each case
# declares (`sudo -n cat '<env>'` when it is not root, `cat '<env>'` when it is),
# so an unprivileged read matches no record and the preservation assertions go
# red instead of quietly passing on an empty read.
#
#   sh scripts/test-install-monitoring.sh
#
# OTTER_INSTALL_SUBJECT points the matrix at a mutated copy of the installer,
# the same way scripts/test-provision.sh does, and the final block demonstrates
# the one mutation this suite's central refusal must not survive.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
subject=${OTTER_INSTALL_SUBJECT:-$here/install-monitoring.sh}
[ -f "$subject" ] || {
	echo "test-install-monitoring: missing $subject" >&2
	exit 2
}

# subject_in_use lets the mutation block below run the same runner against a
# mutated copy without a second copy of the runner.
subject_in_use=$subject
sh_bin=$(command -v sh)

work=$(mktemp -d "${TMPDIR:-/tmp}/otter-install-monitoring-test.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

mkdir -p "$work/bin" "$work/fixture"

# --- fake ssh ---------------------------------------------------------------
#
# Reads OTTER_FAKE_SSH_FIXTURE, a file of records:
#
#   match <glob> <exit> <output>
#
# The first record whose glob matches the remote command string is used. The
# command is appended to $OTTER_TEST_LOG before it is answered, which is how the
# cases assert ordering and exact steps.
#
# An upload arrives on stdin, and is recorded as $OTTER_TEST_STDIN.<n>, with the
# command that consumed it in $OTTER_TEST_STDIN.<n>.meta (newlines folded). The
# counter keeps each upload's bytes separate; the meta file is how a case finds
# the one it means without depending on call order.

cat >"$work/bin/ssh" <<'SHIM'
#!/bin/sh
# Drop the option arguments, then the destination; the rest is the command.
while [ $# -gt 0 ]; do
	case $1 in
	-o | -p | -i)
		shift 2
		;;
	*)
		break
		;;
	esac
done
shift # destination
cmd=$*
printf 'ssh %s\n' "$cmd" >>"$OTTER_TEST_LOG"

if [ ! -t 0 ]; then
	n=$(cat "$OTTER_TEST_STDIN.n" 2>/dev/null || echo 0)
	n=$((n + 1))
	printf '%s\n' "$n" >"$OTTER_TEST_STDIN.n"
	printf '%s\n' "$cmd" | tr '\n' ' ' >"$OTTER_TEST_STDIN.$n.meta"
	cat >"$OTTER_TEST_STDIN.$n"
fi

fixture=$OTTER_FAKE_SSH_FIXTURE
while IFS= read -r line; do
	case $line in
	"" | "#"*) continue ;;
	esac
	pattern=$(printf '%s' "$line" | sed 's/^match //' | cut -d'|' -f1)
	status=$(printf '%s' "$line" | cut -d'|' -f2)
	output=$(printf '%s' "$line" | cut -d'|' -f3-)
	# shellcheck disable=SC2254
	case $cmd in
	$pattern)
		[ -n "$output" ] && printf '%b\n' "$output"
		exit "$status"
		;;
	esac
done <"$fixture"
echo "fake-ssh: no fixture record matches: $cmd" >&2
exit 127
SHIM

chmod +x "$work/bin/ssh"

# A fake systemctl for the probe cases below. The convergence probe asks one
# question of systemd -- whether a timer is enabled -- and this answers it.
# OTTER_PROBE_TIMERS_DISABLED is a space-separated list of timer names systemd
# would report as disabled; every other name is enabled, as on a converged host.
cat >"$work/bin/systemctl" <<'SHIM'
#!/bin/sh
if [ "$1" != "is-enabled" ]; then
	exit 0
fi
for timer in $OTTER_PROBE_TIMERS_DISABLED; do
	for arg in "$@"; do
		if [ "$arg" = "$timer" ]; then
			exit 1
		fi
	done
done
exit 0
SHIM

chmod +x "$work/bin/systemctl"

# --- the fixture ------------------------------------------------------------

data_dir=/opt/otter/workspaces/castor-24856da9/.otter/data
workspace_dir=/opt/otter/workspaces/castor-24856da9
# A single quote, written once so the fixture records below can name the
# quoting the installer puts around a remote path.
sq="'"

# The install paths the fixture answers for. The defaults are the absolute paths
# the unit headers name; the case below that points OTTER_INSTALL_*_DIR at a tree
# of its own moves them, which is how the same variables the probe reads are
# shown to drive the install too.
lib_path=/usr/local/lib/otter
etc_path=/etc/otter
systemd_path=/etc/systemd/system

write_fixture() {
	# The env-file read carries `sudo -n` on a non-root login and nothing on a
	# root login, mirroring the installer's privileged() rule. It is derived from
	# the login the case declares, so the fixture cannot answer a read the
	# installer would not have been allowed to make.
	if [ "${FIX_LOGIN_UID-1000}" = "0" ]; then
		env_read_prefix=
	else
		env_read_prefix="sudo -n "
	fi
	{
		# A host whose checks are already installed answers the probe with
		# nothing stale; the default is a host where nothing is installed yet.
		# `-` rather than `:-`: an explicitly empty value is a case, not "unset".
		printf 'match *# install-monitoring-probe*|0|%s\n' \
			"${FIX_PROBE-$lib_path/heartbeat.sh(missing)}"
		printf 'match *id -u*|%s|%s\n' "${FIX_LOGIN_STATUS:-0}" "${FIX_LOGIN_UID-1000}"
		printf 'match *sudo -n true*|%s|\n' "${FIX_SUDO_STATUS:-0}"
		printf 'match *test -d*/opt/otter/workspaces*|%s|\n' "${FIX_DATA_STATUS:-0}"
		# An empty FIX_WS_LIST is "no workspace at all", not "unset".
		printf 'match *ls -d /opt/otter/workspaces/*|0|%s\n' "${FIX_WS_LIST-$workspace_dir/}"
		# The env-file read is a privileged read: the file is 0640 inside a 0750
		# /etc/otter, so the login that installed it has to read it back with
		# `sudo -n`. The records are written for the login the case declares, so
		# an unprivileged read on a non-root login matches no record at all and
		# the preservation assertions below go red instead of quietly passing on
		# an empty read.
		printf 'match *%scat %s%s/heartbeat.env%s*|%s|%s\n' \
			"$env_read_prefix" "$sq" "$etc_path" "$sq" \
			"${FIX_HB_ENV_STATUS:-1}" "${FIX_HB_ENV:-}"
		printf 'match *%scat %s%s/disk-check.env%s*|%s|%s\n' \
			"$env_read_prefix" "$sq" "$etc_path" "$sq" \
			"${FIX_DC_ENV_STATUS:-1}" "${FIX_DC_ENV:-}"
		printf 'match *install -d -m 0755 %s*|0|\n' "$lib_path"
		printf 'match *install -d -m 0750 %s*|0|\n' "$etc_path"
		printf 'match *cat > %s/heartbeat.sh*|0|\n' "$lib_path"
		printf 'match *cat > %s/disk-check.sh*|0|\n' "$lib_path"
		printf 'match *cat > %s/otter-metric.sh*|0|\n' "$lib_path"
		printf 'match *cat > %s/heartbeat.service*|0|\n' "$systemd_path"
		printf 'match *cat > %s/heartbeat.timer*|0|\n' "$systemd_path"
		printf 'match *cat > %s/disk-check.service*|0|\n' "$systemd_path"
		printf 'match *cat > %s/disk-check.timer*|0|\n' "$systemd_path"
		printf 'match *cat > %s/heartbeat.env*|0|\n' "$etc_path"
		printf 'match *cat > %s/disk-check.env*|0|\n' "$etc_path"
		printf 'match *systemctl daemon-reload*|0|\n'
		printf 'match *systemctl enable --now heartbeat.timer disk-check.timer*|0|\n'
		printf 'match *systemctl is-active heartbeat.timer*|0|%s\n' "${FIX_HB_TIMER:-active}"
		printf 'match *systemctl is-active disk-check.timer*|0|%s\n' "${FIX_DC_TIMER:-active}"
		printf 'match *systemctl is-enabled heartbeat.timer*|0|enabled\n'
		printf 'match *systemctl is-enabled disk-check.timer*|0|enabled\n'
		printf 'match *systemctl start heartbeat.service*|0|\n'
		printf 'match *systemctl start disk-check.service*|0|\n'
		printf 'match *show -p ExecMainStatus --value heartbeat.service*|0|%s\n' "${FIX_HB_STATUS:-0}"
		printf 'match *show -p ExecMainStatus --value disk-check.service*|0|%s\n' "${FIX_DC_STATUS:-0}"
		printf 'match *systemctl status*--no-pager*|0|%s\n' "${FIX_TIMER_STATUS:-}"
		printf 'match *journalctl*heartbeat.service*|0|%s\n' "${FIX_HB_JOURNAL:-}"
		printf 'match *journalctl*disk-check.service*|0|%s\n' "${FIX_DC_JOURNAL:-}"
	} >"$work/fixture/ssh"
}

# --- running the subject ----------------------------------------------------

default_run_args="--host ubuntu@10.0.0.5"
run_args=$default_run_args
extra_args=""

# run invokes the subject against the fixture. An upload redirects its own
# stdin, so /dev/null here only keeps a probe from ever waiting on the harness's
# stdin. OTTER_INSTALL_SOURCE_DIR points at this checkout: a mutated copy of the
# subject in a temporary directory must still upload the canonical scripts, not
# nothing.
run() {
	OTTER_TEST_LOG=$work/log \
		OTTER_TEST_STDIN=$work/stdin \
		OTTER_FAKE_SSH_FIXTURE=$work/fixture/ssh \
		OTTER_INSTALL_SOURCE_DIR=$here \
		PATH="$work/bin:$PATH" \
		"$sh_bin" "$subject_in_use" $run_args $extra_args </dev/null 2>&1
}

# reset_transcript clears the transcript and the captured uploads. The argument
# list and the fixture are set by the caller: a fixture built before a case's
# overrides would answer the previous case's questions, and an argument list
# reset after a case's flags would drop them.
reset_transcript() {
	: >"$work/log"
	rm -f "$work"/stdin.* 2>/dev/null || true
}

passed=0
failed=0

ok() {
	passed=$((passed + 1))
	echo "ok: $1"
}

bad() {
	failed=$((failed + 1))
	echo "test-install-monitoring: FAIL: $1" >&2
}

unset_knobs() {
	unset FIX_PROBE FIX_LOGIN_STATUS FIX_LOGIN_UID FIX_SUDO_STATUS \
		FIX_DATA_STATUS FIX_WS_LIST FIX_HB_ENV FIX_HB_ENV_STATUS \
		FIX_DC_ENV FIX_DC_ENV_STATUS FIX_HB_TIMER FIX_DC_TIMER \
		FIX_HB_STATUS FIX_DC_STATUS FIX_TIMER_STATUS \
		FIX_HB_JOURNAL FIX_DC_JOURNAL 2>/dev/null || true
	unset OTTER_INSTALL_HOST OTTER_INSTALL_IDENTITY OTTER_INSTALL_PORT \
		OTTER_INSTALL_DATA_DIR OTTER_INSTALL_METRIC_HOST \
		OTTER_INSTALL_DRY_RUN OTTER_INSTALL_LIB_DIR \
		OTTER_INSTALL_SYSTEMD_DIR OTTER_INSTALL_ETC_DIR 2>/dev/null || true
}

# expect runs one case: $1 name, $2 0/1 (want success or failure), $3 a
# substring the output must contain (empty for none), then fixture overrides as
# FIX_NAME=value assignments, flags as bare arguments (only --no-host so far).
expect() {
	name=$1
	want=$2
	must=$3
	shift 3
	run_args=$default_run_args
	extra_args=""
	for assignment in "$@"; do
		case $assignment in
		--no-host)
			run_args=""
			;;
		*=*)
			export "$assignment"
			;;
		*)
			run_args="$run_args $assignment"
			;;
		esac
	done
	write_fixture
	reset_transcript
	status=0
	out=$(run) || status=$?
	unset_knobs

	if [ "$want" = "0" ] && [ "$status" -ne 0 ]; then
		bad "$name: want success, got exit $status"
		printf '%s\n' "$out" | sed 's/^/    /' >&2
		return
	fi
	if [ "$want" = "1" ] && [ "$status" -eq 0 ]; then
		bad "$name: want failure, got success"
		printf '%s\n' "$out" | sed 's/^/    /' >&2
		return
	fi
	if [ -n "$must" ] && ! printf '%s\n' "$out" | grep -Fq -e "$must"; then
		bad "$name: output does not contain the expected text"
		echo "    want substring: $must" >&2
		printf '%s\n' "$out" | sed 's/^/    /' >&2
		return
	fi
	ok "$name"
}

# --- the matrix -------------------------------------------------------------
#
# Every refusal composes with the next: the run that has no --host never reaches
# ssh, the run whose data directory is missing never installs, and the run whose
# check exits non-zero must fail the whole install.

expect "no --host refuses" 1 "--host is required" --no-host
expect "an unreachable host refuses" 1 "not reachable over SSH" \
	"FIX_LOGIN_STATUS=255" "FIX_LOGIN_UID="
expect "a login without passwordless sudo refuses" 1 "no passwordless sudo" \
	"FIX_SUDO_STATUS=1"
expect "a relative --data-dir refuses before ssh" 1 "not an absolute path" \
	"OTTER_INSTALL_DATA_DIR=relative/data"
expect "an empty --metric-host refuses" 1 "is not a plain name" \
	"OTTER_INSTALL_METRIC_HOST="
expect "a malformed --metric-host refuses" 1 "is not a plain name" \
	"OTTER_INSTALL_METRIC_HOST=bad host"
expect "a --metric-host with a slash refuses" 1 "is not a plain name" \
	"OTTER_INSTALL_METRIC_HOST=castor/runtime"
expect "a --port that is not a number refuses" 1 "not a port number" \
	"OTTER_INSTALL_PORT=ssh"

# The refusal that keeps the disk check off a host with no runtime: the message
# must name `otter deploy`, because that is the step that creates the data
# directory and the operator needs the remedy, not just the symptom.
expect "a missing data directory refuses" 1 "otter deploy" \
	"OTTER_INSTALL_DATA_DIR=$data_dir" "FIX_DATA_STATUS=1"
expect "a missing data directory names the path it wanted" 1 "$data_dir" \
	"OTTER_INSTALL_DATA_DIR=$data_dir" "FIX_DATA_STATUS=1"
expect "no workspace at all refuses, naming otter deploy" 1 "otter deploy" \
	"FIX_WS_LIST="
expect "several workspaces refuse rather than guess" 1 "pass --data-dir" \
	"FIX_WS_LIST=/opt/otter/workspaces/a/\n/opt/otter/workspaces/b/"

# The install itself.
expect "the happy path installs and asserts" 0 "install-monitoring: OK" \
	"OTTER_INSTALL_DATA_DIR=$data_dir"
expect "the workspace is discovered when --data-dir is absent" 0 "install-monitoring: OK"

# Convergence: a host that already has the monitors must report that and
# converge, not fail, and a converged run is still re-asserted.
expect "a converged host reports convergence and re-asserts" 0 "already installed" \
	"FIX_PROBE=" "OTTER_INSTALL_DATA_DIR=$data_dir"

# The case that matters: the check installs, then fails. The install must fail
# with the journal rather than leave an alarm that fires forever.
expect "a check that exits non-zero fails the install" 1 "exited 1" \
	"OTTER_INSTALL_DATA_DIR=$data_dir" "FIX_HB_STATUS=1"
expect "the failing check's journal is printed" 1 "status=1/FAILURE" \
	"OTTER_INSTALL_DATA_DIR=$data_dir" "FIX_HB_STATUS=1" \
	"FIX_HB_JOURNAL=heartbeat.service: Main process exited, code=exited, status=1/FAILURE"
expect "the disk check failing also fails the install" 1 "exited 7" \
	"OTTER_INSTALL_DATA_DIR=$data_dir" "FIX_DC_STATUS=7" \
	"FIX_DC_JOURNAL=disk-check.service: disk-check: BREACH free space is 12MB"
expect "a timer that is not active fails" 1 "not active" \
	"OTTER_INSTALL_DATA_DIR=$data_dir" "FIX_HB_TIMER=inactive" \
	"FIX_TIMER_STATUS=heartbeat.timer: inactive (dead)"

# --- the recorded install, asserted step by step ----------------------------
#
# Re-run the happy path and read the transcript: the point of this block is that
# the paths, modes and contents are what the unit headers specify, not what the
# script happens to do.

export OTTER_INSTALL_DATA_DIR=$data_dir
write_fixture
run_args=$default_run_args
extra_args=""
reset_transcript
status=0
out=$(run) || status=$?
unset OTTER_INSTALL_DATA_DIR
if [ "$status" -ne 0 ]; then
	bad "the recorded install did not succeed (exit $status)"
	printf '%s\n' "$out" | sed 's/^/    /' >&2
else
	ok "the recorded install succeeded"
fi

# require_log asserts a recorded ssh command contains a substring.
require_log() {
	if grep -Fq -e "$2" "$work/log"; then
		ok "$1"
	else
		bad "$1 (no recorded command contains: $2)"
		sed 's/^/    /' "$work/log" >&2
	fi
}

require_log "the install root is created 0755" "install -d -m 0755 /usr/local/lib/otter"
require_log "the env directory is created 0750" "install -d -m 0750 /etc/otter"
# /etc/otter is 0750 root:root, so a probe running as the plain login cannot stat
# the two env files: it would report them mode=unknown on a converged host, and
# "already installed" could never be said. The probe must therefore run under the
# same privilege the install uses -- and the directory and files must stay 0750
# and 0640, which the two assertions above and below already pin.
require_log "the convergence probe runs under the install's sudo" \
	"sudo -n sh -c '# install-monitoring-probe"
# The same privilege rule covers the read that keeps an operator's line: the env
# files are 0640 inside the 0750 directory, so a plain-login `cat` reads nothing
# and write_env cannot tell that apart from "the file has no extra lines".
require_log "the existing heartbeat.env is read under the install's sudo" \
	"sudo -n cat '/etc/otter/heartbeat.env'"
require_log "the existing disk-check.env is read under the install's sudo" \
	"sudo -n cat '/etc/otter/disk-check.env'"

for script in heartbeat.sh disk-check.sh otter-metric.sh; do
	require_log "$script is written to /usr/local/lib/otter" \
		"cat > /usr/local/lib/otter/$script"
	require_log "$script is owned by root" \
		"chown root:root /usr/local/lib/otter/$script"
	require_log "$script is mode 0755" \
		"chmod 0755 /usr/local/lib/otter/$script"
done

for unit in heartbeat.service heartbeat.timer disk-check.service disk-check.timer; do
	require_log "$unit is written to /etc/systemd/system" \
		"cat > /etc/systemd/system/$unit"
	require_log "$unit is mode 0644" "chmod 0644 /etc/systemd/system/$unit"
done

require_log "heartbeat.env is written mode 0640" "chmod 0640 /etc/otter/heartbeat.env"
require_log "disk-check.env is written mode 0640" "chmod 0640 /etc/otter/disk-check.env"
require_log "systemd is reloaded" "systemctl daemon-reload"
require_log "both timers are enabled and started" \
	"systemctl enable --now heartbeat.timer disk-check.timer"
require_log "the heartbeat timer's state is read" "systemctl is-active heartbeat.timer"
require_log "the disk-check timer's state is read" "systemctl is-active disk-check.timer"
require_log "the heartbeat service is run once" "systemctl start heartbeat.service"
require_log "the disk-check service is run once" "systemctl start disk-check.service"
require_log "the heartbeat service's exit status is read" \
	"show -p ExecMainStatus --value heartbeat.service"
require_log "the disk-check service's exit status is read" \
	"show -p ExecMainStatus --value disk-check.service"

if grep -Fq -e "journalctl" "$work/log"; then
	bad "a successful install read the journal (nothing failed, so nothing to explain)"
else
	ok "a successful install does not read the journal"
fi

# stdin_for finds the upload whose recorded command contains $1 and prints what
# was on ssh's stdin -- the file's bytes.
stdin_for() {
	for meta in "$work"/stdin.*.meta; do
		[ -f "$meta" ] || continue
		if grep -Fq -e "$1" "$meta"; then
			cat "${meta%.meta}"
			return 0
		fi
	done
	return 1
}

# The canonical scripts must be what was sent, not a second copy. This is the
# check that would catch the installer drifting from the checkout.
for script in heartbeat.sh disk-check.sh otter-metric.sh \
	heartbeat.service heartbeat.timer disk-check.service disk-check.timer; do
	case $script in
	*.service | *.timer) remote_path=/etc/systemd/system/$script ;;
	*) remote_path=/usr/local/lib/otter/$script ;;
	esac
	if body=$(stdin_for "cat > $remote_path"); then
		if [ "$body" = "$(cat "$here/$script")" ]; then
			ok "the uploaded $script is the checkout's own file"
		else
			bad "the uploaded $script differs from $here/$script"
		fi
	else
		bad "no upload of $script was recorded"
	fi
done

if body=$(stdin_for "cat > /etc/otter/heartbeat.env"); then
	if [ "$body" = "OTTER_METRIC_HOST=castor-runtime" ]; then
		ok "heartbeat.env carries only OTTER_METRIC_HOST"
	else
		bad "heartbeat.env content is not OTTER_METRIC_HOST=castor-runtime"
		printf '%s\n' "$body" | sed 's/^/    /' >&2
	fi
else
	bad "no upload of /etc/otter/heartbeat.env was recorded"
fi

if body=$(stdin_for "cat > /etc/otter/disk-check.env"); then
	want=$(printf 'OTTER_METRIC_HOST=castor-runtime\nOTTER_DATA_DIR=%s' "$data_dir")
	if [ "$body" = "$want" ]; then
		ok "disk-check.env carries the host dimension and the data directory"
	else
		bad "disk-check.env content is not the metric host plus $data_dir"
		printf '%s\n' "$body" | sed 's/^/    /' >&2
	fi
else
	bad "no upload of /etc/otter/disk-check.env was recorded"
fi

# --- the convergence probe, executed against a fixture tree ------------------
#
# The probe is what decides whether a host is reported as already converged, so
# it is the one part of the installer a stubbed answer cannot vouch for. The
# installer prints its own probe with --print-probe, pointing it at the same
# OTTER_INSTALL_*_DIR variables the install uses; these cases build a tree with
# the modes the install produces and execute that text against it. A probe that
# called a converged tree stale, or a stale one converged, fails here -- and so
# does a probe that cannot stat inside a 0750 directory, which is the bug this
# block was written for.
#
# The fixture is owned by the user running this suite, so its 0750 directory does
# not model the permission that stopped the old probe; the privilege assertion in
# the recorded install above is what pins that half.

probe_root=$work/probe-tree
probe_lib=$probe_root/usr/local/lib/otter
probe_etc=$probe_root/etc/otter
probe_systemd=$probe_root/etc/systemd/system

# build_probe_fixture writes a converged tree: three 0755 scripts, four 0644
# units, and a 0750 environment directory holding two 0640 env files.
build_probe_fixture() {
	rm -rf "$probe_root"
	mkdir -p "$probe_lib" "$probe_etc" "$probe_systemd"
	for script in heartbeat.sh disk-check.sh otter-metric.sh; do
		printf '#!/bin/sh\n' >"$probe_lib/$script"
		chmod 0755 "$probe_lib/$script"
	done
	for unit in heartbeat.service heartbeat.timer disk-check.service disk-check.timer; do
		printf '[Unit]\nDescription=probe fixture\n' >"$probe_systemd/$unit"
		chmod 0644 "$probe_systemd/$unit"
	done
	for envfile in heartbeat.env disk-check.env; do
		printf 'OTTER_METRIC_HOST=castor-runtime\n' >"$probe_etc/$envfile"
		chmod 0640 "$probe_etc/$envfile"
	done
	chmod 0750 "$probe_etc"
}

# probe_text prints the probe the installer would run for this fixture tree. It
# is the installer's own text, not a copy: a suite that restated the probe would
# still be green if the installer's copy drifted.
probe_text() {
	OTTER_INSTALL_LIB_DIR=$probe_lib \
		OTTER_INSTALL_ETC_DIR=$probe_etc \
		OTTER_INSTALL_SYSTEMD_DIR=$probe_systemd \
		"$sh_bin" "$subject" --print-probe
}

# run_probe executes it with the fake systemctl on PATH. $1 is the space-
# separated list of timers systemd would report as disabled.
run_probe() {
	probe=$(probe_text) || return 1
	OTTER_PROBE_TIMERS_DISABLED=$1 PATH="$work/bin:$PATH" \
		"$sh_bin" -c "$probe"
}

# expect_probe requires the probe's report to be empty, or to contain $2, after
# $3's timers are the disabled ones. The tree is repaired between cases, so each
# case starts from a converged fixture rather than from the previous case's
# damage.
expect_probe() { # name, want (empty for nothing stale), disabled timers
	name=$1
	want=$2
	if ! out=$(run_probe "$3"); then
		bad "$name: the probe did not run"
		return
	fi
	if [ -z "$want" ]; then
		if [ -z "$out" ]; then
			ok "$name"
		else
			bad "$name: want nothing stale, got:$out"
		fi
		return
	fi
	case $out in
	*"$want"*) ok "$name" ;;
	*)
		bad "$name: no stale report contains '$want', got:${out:-<nothing>}"
		;;
	esac
}

build_probe_fixture
if [ "$(stat -c '%a' "$probe_etc")" = "750" ] &&
	[ "$(stat -c '%a' "$probe_etc/heartbeat.env")" = "640" ]; then
	ok "the probe fixture has the modes the install produces (0750 directory, 0640 files)"
else
	bad "the probe fixture is not the converged tree it claims"
fi

expect_probe "a converged fixture reports nothing stale" "" ""

chmod 0755 "$probe_etc"
expect_probe "a widened 0750 environment directory is reported, with its mode" \
	"$probe_etc(mode=755)" ""
chmod 0750 "$probe_etc"

chmod 0644 "$probe_etc/heartbeat.env"
expect_probe "an env file with the wrong mode is reported, with its mode" \
	"$probe_etc/heartbeat.env(mode=644)" ""
chmod 0640 "$probe_etc/heartbeat.env"

chmod 0600 "$probe_systemd/disk-check.timer"
expect_probe "a unit file with the wrong mode is reported, with its mode" \
	"$probe_systemd/disk-check.timer(mode=600)" ""
chmod 0644 "$probe_systemd/disk-check.timer"

rm -f "$probe_lib/disk-check.sh"
expect_probe "a missing script is reported as missing" \
	"$probe_lib/disk-check.sh(missing)" ""
printf '#!/bin/sh\n' >"$probe_lib/disk-check.sh"
chmod 0755 "$probe_lib/disk-check.sh"

expect_probe "a disabled disk-check timer is reported" \
	"disk-check.timer(not-enabled)" "disk-check.timer"
expect_probe "a disabled heartbeat timer is reported" \
	"heartbeat.timer(not-enabled)" "heartbeat.timer"
expect_probe "both timers disabled are both reported" \
	"heartbeat.timer(not-enabled)" "heartbeat.timer disk-check.timer"

# The defaults are the absolute paths the unit headers name. This is the check
# that keeps the harness hook from becoming the only supported configuration: a
# default that drifted would probe (and install into) a path no host has.
default_probe=$("$sh_bin" "$subject" --print-probe)
for want in "check /etc/otter 750 dir" \
	"check /usr/local/lib/otter/heartbeat.sh 755" \
	"check /etc/systemd/system/heartbeat.timer 644" \
	"check /etc/otter/disk-check.env 640"; do
	if printf '%s\n' "$default_probe" | grep -Fq -e "$want"; then
		ok "the default probe checks the absolute path: $want"
	else
		bad "the default probe does not check: $want"
		printf '%s\n' "$default_probe" | sed 's/^/    /' >&2
	fi
done

# The same variables move the install, not only the probe. Pointed at a tree this
# case owns, the installer must write there: otherwise the probe could follow
# OTTER_INSTALL_*_DIR while the install quietly kept writing to /etc/otter, and
# the probe would then call the real host permanently stale.
alt=$work/alt-install
lib_path=$alt/usr/local/lib/otter
etc_path=$alt/etc/otter
systemd_path=$alt/etc/systemd/system
export OTTER_INSTALL_LIB_DIR=$lib_path
export OTTER_INSTALL_ETC_DIR=$etc_path
export OTTER_INSTALL_SYSTEMD_DIR=$systemd_path
export OTTER_INSTALL_DATA_DIR=$data_dir
write_fixture
run_args=$default_run_args
extra_args=""
reset_transcript
status=0
out=$(run) || status=$?
unset OTTER_INSTALL_LIB_DIR OTTER_INSTALL_ETC_DIR OTTER_INSTALL_SYSTEMD_DIR \
	OTTER_INSTALL_DATA_DIR
lib_path=/usr/local/lib/otter
etc_path=/etc/otter
systemd_path=/etc/systemd/system
if [ "$status" -ne 0 ]; then
	bad "the relocated install did not succeed (exit $status)"
	printf '%s\n' "$out" | sed 's/^/    /' >&2
elif grep -Fq -e "cat > $alt/usr/local/lib/otter/heartbeat.sh" "$work/log" &&
	grep -Fq -e "cat > $alt/etc/systemd/system/heartbeat.timer" "$work/log" &&
	grep -Fq -e "cat > $alt/etc/otter/disk-check.env" "$work/log" &&
	! grep -Fq -e "cat > /usr/local/lib/otter/" "$work/log"; then
	ok "the install follows OTTER_INSTALL_*_DIR, the same variables the probe reads"
else
	bad "the install did not write under OTTER_INSTALL_*_DIR"
	sed 's/^/    /' "$work/log" >&2
fi

# On a root login there is no sudo to use and none is needed, but the probe must
# still run as the install's privilege. It must not be quietly skipped because
# $sudo is empty.
export FIX_LOGIN_UID=0
write_fixture
run_args=$default_run_args
extra_args=""
reset_transcript
status=0
out=$(run) || status=$?
unset FIX_LOGIN_UID
if [ "$status" -ne 0 ]; then
	bad "the root-login install did not succeed (exit $status)"
	printf '%s\n' "$out" | sed 's/^/    /' >&2
elif grep -Fq -e "sudo -n" "$work/log"; then
	bad "a root login still ran a command through sudo"
	sed 's/^/    /' "$work/log" >&2
elif ! grep -Fq -e "sh -c '# install-monitoring-probe" "$work/log"; then
	bad "the root-login run recorded no probe"
	sed 's/^/    /' "$work/log" >&2
elif ! grep -Fq -e "cat '/etc/otter/heartbeat.env'" "$work/log"; then
	bad "the root-login run recorded no env read"
	sed 's/^/    /' "$work/log" >&2
else
	ok "on a root login the probe and the env read run as root without sudo"
fi

# --- an operator's extra env line survives a re-run -------------------------
#
# The unit headers invite OTTER_METRIC_REGION to be added to these files. A
# re-run that dropped it would change how the metric is signed while looking
# like a no-op, so the managed keys are replaced and everything else is kept.

export OTTER_INSTALL_DATA_DIR=$data_dir
export FIX_HB_ENV_STATUS=0
export FIX_HB_ENV='OTTER_METRIC_HOST=an-old-name\nOTTER_METRIC_REGION=eu-west-1\n'
write_fixture
run_args=$default_run_args
extra_args=""
reset_transcript
status=0
out=$(run) || status=$?
unset OTTER_INSTALL_DATA_DIR FIX_HB_ENV_STATUS FIX_HB_ENV
if [ "$status" -ne 0 ]; then
	bad "the preservation run did not succeed (exit $status)"
	printf '%s\n' "$out" | sed 's/^/    /' >&2
elif body=$(stdin_for "cat > /etc/otter/heartbeat.env"); then
	case $body in
	*"OTTER_METRIC_REGION=eu-west-1"*)
		ok "an operator's OTTER_METRIC_REGION is preserved across a re-run"
		;;
	*)
		bad "the preserved env file lost OTTER_METRIC_REGION"
		printf '%s\n' "$body" | sed 's/^/    /' >&2
		;;
	esac
	case $body in
	*"an-old-name"*)
		bad "the stale OTTER_METRIC_HOST was not replaced"
		;;
	*)
		ok "the managed OTTER_METRIC_HOST is replaced, not duplicated"
		;;
	esac
else
	bad "no upload of /etc/otter/heartbeat.env was recorded"
fi

# Preservation rests on the read being privileged: on a non-root login a plain
# `cat` of the 0750 directory returns nothing, write_env takes that for "no extra
# lines", and the re-run silently drops the operator's OTTER_METRIC_REGION. The
# fixture now matches only the privileged shape, but this asserts the transcript
# itself so a regression names the cause rather than just the symptom.
if grep -Fq -e "sudo -n cat '/etc/otter/heartbeat.env'" "$work/log"; then
	ok "the preserved line is read back under the install's sudo, not as the plain login"
else
	bad "the existing env file was read without sudo, so a preserved line would be dropped"
	sed 's/^/    /' "$work/log" >&2
fi

# --- a dry run changes nothing ----------------------------------------------
#
# The dry run still runs the read-only preflight, so its plan is a plan for the
# host that is actually there; it must record no mutation.

export OTTER_INSTALL_DRY_RUN=1
export OTTER_INSTALL_DATA_DIR=$data_dir
write_fixture
run_args=$default_run_args
extra_args=""
reset_transcript
status=0
out=$(run) || status=$?
unset OTTER_INSTALL_DRY_RUN OTTER_INSTALL_DATA_DIR
if [ "$status" -ne 0 ]; then
	bad "the dry run did not succeed (exit $status)"
	printf '%s\n' "$out" | sed 's/^/    /' >&2
elif ! printf '%s\n' "$out" | grep -Fq "dry run complete"; then
	bad "the dry run did not say that nothing was changed"
elif ! printf '%s\n' "$out" | grep -Fq "would write /usr/local/lib/otter/heartbeat.sh"; then
	bad "the dry run did not print the plan for the scripts"
elif ! grep -Fq -e "id -u" "$work/log"; then
	bad "the dry run did not run the read-only preflight"
else
	ok "the dry run prints the plan and still preflights"
fi

mutations=""
for pattern in "cat > " "install -d" "chmod " "chown " "systemctl daemon-reload" \
	"systemctl enable" "systemctl start" "journalctl"; do
	if grep -Fq -e "$pattern" "$work/log"; then
		mutations="$mutations '$pattern'"
	fi
done
if [ -n "$mutations" ]; then
	bad "the dry run recorded a mutation:$mutations"
	sed 's/^/    /' "$work/log" >&2
else
	ok "the dry run recorded no mutation and no upload"
fi

# --- the mutation the suite's central refusal must not survive --------------
#
# Remove the data-directory refusal from a copy of the subject and run the same
# data-missing scenario against it: it must now install and exit 0. That is what
# makes the case above load-bearing -- pointing OTTER_INSTALL_SUBJECT at this
# mutant turns the suite red on that case.

mutant=$work/install-monitoring-mutant.sh
sed 's/^if ! data_dir_exists; then$/if false; then/' "$subject" >"$mutant"
if cmp -s "$subject" "$mutant"; then
	bad "the data-directory refusal was not found, so the mutation demonstrates nothing"
else
	export OTTER_INSTALL_DATA_DIR=$data_dir
	export FIX_DATA_STATUS=1
	write_fixture
	run_args=$default_run_args
	extra_args=""
	reset_transcript
	subject_in_use=$mutant
	status=0
	out=$(run) || status=$?
	subject_in_use=$subject
	unset OTTER_INSTALL_DATA_DIR FIX_DATA_STATUS
	if [ "$status" -eq 0 ]; then
		ok "removing the data-directory refusal lets the mutant install on a host with no runtime, so the case is load-bearing"
	else
		bad "the mutant still failed (exit $status); the mutation did not remove the refusal"
		printf '%s\n' "$out" | sed 's/^/    /' >&2
	fi
fi

# --- outcome ----------------------------------------------------------------

if [ "$failed" -ne 0 ]; then
	echo "test-install-monitoring: FAILED: $failed case(s), $passed passed" >&2
	exit 1
fi

echo "test-install-monitoring: all $passed cases passed"
