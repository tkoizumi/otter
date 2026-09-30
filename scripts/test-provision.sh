#!/bin/sh
# Falsifiability for scripts/provision.sh.
#
# provision.sh talks to a host over ssh and hands off to `otter deploy`. This
# harness stands in both: a fake ssh answers the preflight questions from a
# fixture and records every command, and a fake otter records the deploy
# arguments and exits with the status the case wants. The point is not to test
# ssh or the deployer -- it is to prove that *provision.sh* notices a host that
# is not ready, and that it never treats a host as provisioned when the deploy
# or the final assertion did not succeed.
#
# The case that matters most is the one this repository learned the hard way: a
# host whose provisioning silently stopped short. `no-swap` below is that host.
#
#   sh scripts/test-provision.sh
#
# OTTER_PROVISION_SUBJECT points the matrix at a mutated copy of provision.sh,
# the same way scripts/test-assert-host-permissions.sh does.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
subject=${OTTER_PROVISION_SUBJECT:-$here/provision.sh}

[ -f "$subject" ] || {
	echo "test-provision: missing $subject" >&2
	exit 2
}

work=$(mktemp -d "${TMPDIR:-/tmp}/otter-provision-test.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

mkdir -p "$work/bin" "$work/fixture"

# --- fake ssh ---------------------------------------------------------------
#
# Reads OTTER_FAKE_SSH_FIXTURE, a file of records:
#
#   match <glob> <exit> <output>
#
# The first record whose glob matches the remote command string is used. The
# command is appended to $work/log before it is answered, which is how the cases
# assert ordering.

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

# stdin may carry an uploaded script; consume it so the caller never blocks.
if [ ! -t 0 ]; then
	cat >/dev/null 2>&1 || true
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

# --- fake otter -------------------------------------------------------------

cat >"$work/bin/otter" <<'SHIM'
#!/bin/sh
printf 'otter %s\n' "$*" >>"$OTTER_TEST_LOG"
[ -z "${OTTER_FAKE_OTTER_OUT:-}" ] || printf '%s\n' "$OTTER_FAKE_OTTER_OUT"
exit "${OTTER_FAKE_OTTER_STATUS:-0}"
SHIM

chmod +x "$work/bin/ssh" "$work/bin/otter"

# --- fixture ---------------------------------------------------------------
#
# A correctly provisioned host: Ubuntu 24.04 aarch64, 904 MiB, swap converged,
# 19 GiB free, tools present, sshd locked down, nothing deployed yet, and the
# provisioning report present.

write_fixture() {
	{
		# The command is quoted (`id -u 'otter'`), so match on the shape the
		# script always produces rather than on punctuation that moves.
		printf 'match *id -u*otter*|%s|\n' "${FIX_USER_EXISTS:-1}"
		printf 'match *id -u*|%s|%s\n' "${FIX_LOGIN_UID_STATUS:-0}" "${FIX_LOGIN_UID:-1000}"
		printf 'match *sudo -n true*|%s|\n' "${FIX_SUDO_STATUS:-0}"
		printf 'match *uname -s*|0|%s\n' "${FIX_UNAME:-Linux\\naarch64}"
		printf 'match *. /etc/os-release*|0|%s\n' "${FIX_OS:-ubuntu 24.04}"
		printf 'match *missing=*|0|%s\n' "${FIX_TOOLS:-}"
		printf 'match *MemTotal*|0|%s\n' "${FIX_MEM_KIB:-926448}"
		printf 'match *df -Pk*|0|%s\n' "${FIX_DISK_KIB:-19000000}"
		printf 'match */proc/swaps*|0|%s\n' "${FIX_SWAP_KIB:-2097148}"
		printf 'match *sshd -T*|0|%s\n' "${FIX_SSHD:-passwordauthentication no\\npermitrootlogin no}"
		printf 'match *test -d*workspaces*|%s|\n' "${FIX_WS_ROOT_EXISTS:-1}"
		printf 'match *test -e /etc/otter/workspaces*|%s|\n' "${FIX_ENV_ROOT_EXISTS:-1}"
		# A missing report means the `test -s` half fails, so the command
		# produces no output as well as a non-zero status. A fixture that
		# printed text and exited 1 would be testing the wrong thing.
		if [ "${FIX_REPORT_STATUS:-0}" = "0" ]; then
			printf 'match *provision-report*|0|%s\n' "${FIX_REPORT:-otter host provisioning report}"
		else
			printf 'match *provision-report*|1|\n'
		fi
		# "none" is the sentinel for "the deploy installed nothing": an empty
		# value cannot be told apart from an unset one through ${VAR:-...}.
		if [ "${FIX_WS_OUT:-}" = "none" ]; then
			printf 'match *ls -d*workspaces*|0|\n'
		else
			printf 'match *ls -d*workspaces*|0|%s\n' "${FIX_WS_OUT:-/opt/otter/workspaces/castor-24856da9}"
		fi
		printf 'match *otter-assert-host-permissions.sh*--user*|%s|%s\n' "${FIX_ASSERT_STATUS:-0}" "${FIX_ASSERT_OUT:-assert-host-permissions: all assertions passed}"
		printf 'match *tee*/otter-assert-host-permissions.sh*|0|\n'
	} >"$work/fixture/ssh"
}

sh_bin=$(command -v sh)

# run executes provision.sh against the fixture. The project is a throwaway
# directory so the script can create its own otter.daemon.env without touching
# the checkout.
run() {
	project=$work/project
	mkdir -p "$project"
	# extra_args is set by expect; empty for the plain cases.
	: "${extra_args:=}"
	OTTER_TEST_LOG=$work/log \
		OTTER_FAKE_SSH_FIXTURE=$work/fixture/ssh \
		PATH="$work/bin:$PATH" \
		"$sh_bin" "$subject" --host ubuntu@10.0.0.5 --project "$project" \
		--otter-bin "$work/bin/otter" $extra_args 2>&1
}

passed=0
failed=0

# expect runs one case: $1 name, $2 0/1 (want success or failure), $3 a
# substring the output must contain (empty for none), then fixture overrides as
# FIX_NAME=value arguments.
expect() {
	name=$1
	want=$2
	must=$3
	shift 3
	extra_args=""
	for assignment in "$@"; do
		case $assignment in
		*=*) export "$assignment" ;;
		*) extra_args="$extra_args $assignment" ;;
		esac
	done
	write_fixture
	: >"$work/log"
	rm -rf "$work/project"
	status=0
	out=$(run) || status=$?

	unset FIX_LOGIN_UID_STATUS FIX_LOGIN_UID FIX_SUDO_STATUS FIX_UNAME FIX_OS \
		FIX_TOOLS FIX_MEM_KIB FIX_DISK_KIB FIX_SWAP_KIB FIX_SSHD \
		FIX_USER_EXISTS FIX_WS_ROOT_EXISTS FIX_ENV_ROOT_EXISTS \
		FIX_REPORT_STATUS FIX_REPORT FIX_WORKSPACE_DIR FIX_WS_OUT FIX_ASSERT_STATUS \
		FIX_ASSERT_OUT OTTER_FAKE_OTTER_STATUS OTTER_FAKE_OTTER_OUT || true

	if [ "$want" = "0" ] && [ "$status" -ne 0 ]; then
		echo "test-provision: FAIL: $name: want success, got exit $status" >&2
		printf '%s\n' "$out" | sed 's/^/    /' >&2
		failed=$((failed + 1))
		return
	fi
	if [ "$want" = "1" ] && [ "$status" -eq 0 ]; then
		echo "test-provision: FAIL: $name: want failure, got success" >&2
		printf '%s\n' "$out" | sed 's/^/    /' >&2
		failed=$((failed + 1))
		return
	fi
	if [ -n "$must" ] && ! printf '%s\n' "$out" | grep -Fq "$must"; then
		echo "test-provision: FAIL: $name: output does not contain the expected text" >&2
		echo "    want substring: $must" >&2
		printf '%s\n' "$out" | sed 's/^/    /' >&2
		failed=$((failed + 1))
		return
	fi
	passed=$((passed + 1))
	echo "ok: $name"
}

contains_line() {
	# -e keeps a pattern that starts with a dash from being read as an option.
	grep -Fq -e "$1" "$work/log"
}

# --- the matrix -------------------------------------------------------------

# The happy path: preflight passes, the deploy runs, the host assertion runs.
expect "a ready host is provisioned" 0 "provision: OK"

expect "no swap fails" 1 "below the " "FIX_SWAP_KIB=0"
expect "no swap fails: the message names the cause" 1 "cloud-init" "FIX_SWAP_KIB=0"
expect "swap below the floor fails" 1 "below the " "FIX_SWAP_KIB=131072"
expect "too little RAM fails" 1 "below the " "FIX_MEM_KIB=524288"
expect "too little disk fails" 1 "below the " "FIX_DISK_KIB=1048576"
expect "a missing tool fails" 1 "missing on " "FIX_TOOLS= rsync find"
expect "a non-Ubuntu host fails" 1 "not 'ubuntu 24.04'" "FIX_OS=debian 12"
expect "sshd accepting passwords fails" 1 "passwordauthentication=yes" \
	"FIX_SSHD=passwordauthentication yes\npermitrootlogin no"
expect "sshd permitting root login fails" 1 "permitrootlogin=yes" \
	"FIX_SSHD=passwordauthentication no\npermitrootlogin yes"
expect "an unreachable host fails" 1 "not reachable" "FIX_LOGIN_UID_STATUS=255" "FIX_LOGIN_UID="
expect "a login without passwordless sudo fails" 1 "no passwordless sudo" "FIX_SUDO_STATUS=1"
expect "an x86 host is accepted" 0 "host architecture is amd64" "FIX_UNAME=Linux\\nx86_64"
expect "an unsupported architecture fails" 1 "not an amd64 or arm64 host" "FIX_UNAME=Linux\\nriscv64"

expect "an already-provisioned host fails" 1 "already provisioned" \
	"FIX_USER_EXISTS=0" "FIX_WS_ROOT_EXISTS=0" "FIX_ENV_ROOT_EXISTS=0"
expect "an already-provisioned host converges with --allow-existing" 0 "converging" \
	"--allow-existing" "FIX_USER_EXISTS=0" "FIX_WS_ROOT_EXISTS=0"

expect "a missing provisioning report fails" 1 "provisioning report" "FIX_REPORT_STATUS=1" "FIX_REPORT="
expect "a deploy that fails is fatal" 1 "otter deploy failed" "OTTER_FAKE_OTTER_STATUS=1"
expect "a host assertion that fails is fatal" 1 "does not meet CA-09/CA-10" "FIX_ASSERT_STATUS=1"
expect "a deploy that installed no workspace is fatal" 1 "installed nothing" \
	"FIX_ASSERT_STATUS=0" "FIX_WS_OUT=none"

# Ordering and handoff: these are what make the script a provisioning step
# rather than a wrapper around a shell.
write_fixture
: >"$work/log"
rm -rf "$work/project"
run >/dev/null 2>&1 || true
if contains_line "otter deploy" && contains_line "--daemon-env"; then
	passed=$((passed + 1))
	echo "ok: the deploy is driven by otter deploy"
else
	echo "test-provision: FAIL: the deploy did not invoke otter deploy" >&2
	sed 's/^/    /' "$work/log" >&2
	failed=$((failed + 1))
fi

deploy_line=$(grep -n "otter deploy" "$work/log" | head -n 1 | cut -d: -f1)
assert_line=$(grep -n "otter-assert-host-permissions.sh.*--user" "$work/log" | head -n 1 | cut -d: -f1)
if [ -n "$deploy_line" ] && [ -n "$assert_line" ] && [ "$deploy_line" -lt "$assert_line" ]; then
	passed=$((passed + 1))
	echo "ok: the host assertion runs after the deploy"
else
	echo "test-provision: FAIL: the host assertion did not run after the deploy (deploy line ${deploy_line:-none}, assert line ${assert_line:-none})" >&2
	sed 's/^/    /' "$work/log" >&2
	failed=$((failed + 1))
fi

if grep -q "OTTER_WORKERS=1" "$work/project/otter.daemon.env"; then
	passed=$((passed + 1))
	echo "ok: OTTER_WORKERS=1 is written before the deploy"
else
	echo "test-provision: FAIL: otter.daemon.env does not set OTTER_WORKERS=1" >&2
	cat "$work/project/otter.daemon.env" 2>/dev/null | sed 's/^/    /' >&2
	failed=$((failed + 1))
fi

# --- outcome ----------------------------------------------------------------

if [ "$failed" -ne 0 ]; then
	echo "test-provision: FAILED: $failed case(s), $passed passed" >&2
	exit 1
fi

echo "test-provision: all $passed cases passed"
