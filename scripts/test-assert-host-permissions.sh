#!/bin/sh
# Falsifiability for scripts/assert-host-permissions.sh.
#
# The assertion script is only evidence if it can fail. This harness builds a
# synthetic host in a temporary directory -- a passwd entry, a systemd unit and
# its loaded properties, a listening-socket table, an sshd dump, and a
# mode/owner table -- runs the assertion script against it, and then re-runs it
# with exactly one property broken. The clean fixture must pass, and every
# mutation must fail on the check that names it.
#
# The shims exist because this harness must run on the machines that author the
# script (macOS, whose `stat` and `getent` are not the host's) as well as on
# Linux. They are deliberately thin: each one reads a fixture file in the format
# the real tool prints, so the assertion script under test is exercised
# unchanged, and the fixture's numbers -- modes, owners, ports -- are what the
# script reads. A mutation is therefore a change to observed host state, not a
# change to the script.
#
#   sh scripts/test-assert-host-permissions.sh
#
# Exit status is 0 only when the clean run passed and every mutation failed.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# OTTER_ASSERT_SUBJECT exists so a reviewer can point the matrix at a mutated
# copy of the script and watch a case go red -- that is how the checks below are
# shown to be load-bearing rather than merely present.
subject=${OTTER_ASSERT_SUBJECT:-}
[ -n "$subject" ] || subject=$here/assert-host-permissions.sh
sh_bin=$(command -v sh)

[ -f "$subject" ] || {
	echo "test-assert-host-permissions: FAIL: missing subject $subject" >&2
	echo "test-assert-host-permissions: expected scripts/assert-host-permissions.sh beside this file," >&2
	echo "test-assert-host-permissions: or set OTTER_ASSERT_SUBJECT to the script to exercise" >&2
	exit 2
}

# The fixture owner is the real account running this harness, so that `id -u`
# finds it. The production default (`otter`) has no such shortcut and is
# checked by the fixture matrix itself.
service_user=$(id -u -n)
service_group=$service_user
service_uid=$(id -u)
# A second account the fixture must NOT find, for the "no such account" case.
missing_user=otter-missing-$$
missing_uid=$((service_uid + 31337))

work=$(mktemp -d "${TMPDIR:-/tmp}/otter-assert-test.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

# --- the fixture shims ------------------------------------------------------

mkdir -p "$work/bin"

# The shims read their values from files under $work, with that path written
# into them at generation time. They deliberately do not read the environment:
# `id` is setuid on some systems and drops it, and a shim that silently falls
# back to the host's tools would make the whole harness assert the host rather
# than the fixture.

write_id() {
	cat >"$work/bin/id" <<SHIM
#!/bin/sh
case \${1:-} in
-u) ;;
*) exec /usr/bin/id "\$@" ;;
esac
name=\${2:-}
awk -F: -v n="\$name" '\$1 == n { print \$3; found = 1 } END { exit found ? 0 : 1 }' $work/passwd
SHIM
	chmod +x "$work/bin/id"
}

write_getent() {
	cat >"$work/bin/getent" <<SHIM
#!/bin/sh
[ "\${1:-}" = "passwd" ] || exit 2
name=\${2:-}
awk -F: -v n="\$name" '\$1 == n { print; found = 1 } END { exit found ? 0 : 2 }' $work/passwd
SHIM
	chmod +x "$work/bin/getent"
}

write_stat() {
	cat >"$work/bin/stat" <<SHIM
#!/bin/sh
fmt=""
path=""
while [ \$# -gt 0 ]; do
	case \$1 in
	-c)
		fmt=\$2
		shift 2
		;;
	*)
		path=\$1
		shift
		;;
	esac
done
entry=\$(awk -F'\\t' -v p="\$path" '\$1 == p { print; found = 1 } END { exit found ? 0 : 1 }' $work/meta) || exit 1
case \$fmt in
'%a') printf '%s\\n' "\$(printf '%s' "\$entry" | cut -f2)" ;;
'%U:%G') printf '%s\\n' "\$(printf '%s' "\$entry" | cut -f3)" ;;
*) exit 2 ;;
esac
SHIM
	chmod +x "$work/bin/stat"
}

write_ss() {
	cat >"$work/bin/ss" <<SHIM
#!/bin/sh
cat $work/ss
SHIM
	chmod +x "$work/bin/ss"
}

write_sshd() {
	cat >"$work/bin/sshd" <<SHIM
#!/bin/sh
cat $work/sshd-conf
SHIM
	chmod +x "$work/bin/sshd"
}

write_systemctl() {
	cat >"$work/bin/systemctl" <<SHIM
#!/bin/sh
sub=\${1:-}
shift || true
case \$sub in
is-enabled)
	[ -f $unit_file ] || exit 1
	cat $work/is-enabled
	;;
is-active)
	[ -f $unit_file ] || exit 3
	cat $work/is-active
	;;
show)
	prop=""
	# Real systemd prints bare values with --value and Property=Value without
	# it. Writing "novalue" into $work/novalue makes this shim behave like a
	# systemd older than 253, which prints the prefixed form and ignores
	# --value; that is what exercises the subject's fallback read.
	bare=1
	[ -f $work/novalue ] && bare=0
	if [ "\${1:-}" = "-p" ]; then
		prop=\${2:-}
		# --value is systemd >= 253. An older systemd does not understand it
		# and prints nothing, which is exactly the case the subject's fallback
		# read exists for.
		if [ "\${3:-}" = "--value" ] && [ -f $work/novalue ]; then
			exit 0
		fi
	fi
	awk -F= -v p="\$prop" -v bare="\$bare" '
		p == "" || \$1 == p {
			if (!p) { print }
			else if (bare) { sub("^" p "=", ""); print }
			else { print }
		}' $work/unit-props
	;;
*) exit 1 ;;
esac
SHIM
	chmod +x "$work/bin/systemctl"
}

# --- fixture state ----------------------------------------------------------
#
# populate writes every fixture file from the environment, so a mutation is one
# variable assignment before the call. Anything not overridden gets the value a
# correctly provisioned host would have.

workspace_dir=$work/opt/otter/workspaces/castor-24856da9
unit_name=otterd-castor-24856da9
data_dir=$workspace_dir/.otter/data
env_dir=$work/etc/otter/workspaces
unit_file=$work/etc/systemd/system/$unit_name.service
api_addr=127.0.0.1:7337
api_port=${api_addr##*:}

populate() {
	fx_ws=${FIX_WS:-$workspace_dir}
	fx_data=${FIX_DATA_DIR:-$data_dir}
	fx_unit=${FIX_UNIT:-$unit_name}
	fx_owner=${FIX_OWNER:-$service_user:$service_group}
	fx_user=${FIX_USER:-$service_user}
	fx_group=${FIX_GROUP:-$service_group}
	fx_uid=${FIX_UID:-$service_uid}
	fx_shell=${FIX_SHELL:-/usr/sbin/nologin}
	fx_dir_mode=${FIX_DIR_MODE:-700}
	fx_env_mode=${FIX_ENV_MODE:-600}
	fx_env_owner=${FIX_ENV_OWNER:-$fx_owner}
	fx_enabled=${FIX_ENABLED:-enabled}
	fx_active=${FIX_ACTIVE:-active}
	fx_unit_user=${FIX_UNIT_USER:-$service_user}
	# "off" is the sentinel for "the directive is absent": an empty value
	# would be indistinguishable from an unset one under ${VAR:-default}.
	fx_nnp=${FIX_NNP:-yes}
	[ "$fx_nnp" = "off" ] && fx_nnp=""
	fx_protect_system=${FIX_PROTECT_SYSTEM:-strict}
	[ "$fx_protect_system" = "off" ] && fx_protect_system=""
	fx_protect_home=${FIX_PROTECT_HOME:-true}
	[ "$fx_protect_home" = "off" ] && fx_protect_home=""
	fx_private_tmp=${FIX_PRIVATE_TMP:-true}
	[ "$fx_private_tmp" = "off" ] && fx_private_tmp=""
	fx_rw=${FIX_RW:-$fx_data}
	# "off" is the sentinel for "the directive is absent"; an empty value would
	# be indistinguishable from an unset one under ${VAR:-default}.
	[ "$fx_rw" = "off" ] && fx_rw="" 
	fx_listen=${FIX_LISTEN:-$api_addr}
	fx_exec_start=${FIX_EXEC_START:-/opt/otter/workspaces/castor-24856da9/bin/otterd \
--jobs /opt/otter/workspaces/castor-24856da9/jobs \
--data $fx_data \
--listen $fx_listen --log-format json}
	fx_ss_table=${FIX_SS:-LISTEN 0 4096 127.0.0.1:$api_port 0.0.0.0:*
LISTEN 0 4096 0.0.0.0:22 0.0.0.0:*
LISTEN 0 4096 [::1]:$api_port [::]:*}
	fx_pw_auth=${FIX_PW_AUTH:-no}
	fx_root_login=${FIX_ROOT_LOGIN:-prohibit-password}

	mkdir -p "$fx_ws" "$fx_data" "$env_dir" "$(dirname "$unit_file")"
	printf '{"service_name":"%s","data_dir":"%s"}\n' "$fx_unit" "$fx_data" >"$fx_ws/workspace.json"
	: >"$unit_file"
	: >"$env_dir/castor-24856da9.env"
	: >"$env_dir/castor-24856da9.daemon.env"
	# The env files are real files so the subject's existence test passes; their
	# recorded modes and owners, which the stat shim serves, are the fixture's.
	chmod 0600 "$env_dir"/castor-24856da9.env "$env_dir"/castor-24856da9.daemon.env
	# A systemctl that prints nothing for is-enabled/is-active: the subject has
	# to treat silence as "not enabled"/"not active", not as a pass.
	if [ "${FIX_QUIET_SYSTEMCTL:-}" = "yes" ]; then
		: >"$work/is-enabled"
		: >"$work/is-active"
	else
		printf '%s\n' "$fx_enabled" >"$work/is-enabled"
		printf '%s\n' "$fx_active" >"$work/is-active"
	fi

	{
		printf '%s:x:%s:%s::/opt/otter/workspaces/castor-24856da9:%s\n' \
			"$fx_user" "$fx_uid" "$fx_group" "$fx_shell"
		printf 'root:x:0:0:root:/root:/bin/bash\n'
	} >"$work/passwd"

	{
		printf 'User=%s\n' "$fx_unit_user"
		printf 'NoNewPrivileges=%s\n' "$fx_nnp"
		printf 'ProtectSystem=%s\n' "$fx_protect_system"
		printf 'ProtectHome=%s\n' "$fx_protect_home"
		printf 'PrivateTmp=%s\n' "$fx_private_tmp"
		printf 'ReadWritePaths=%s\n' "$fx_rw"
		printf 'ExecStart=%s\n' "$fx_exec_start"
	} >"$work/unit-props"

	printf '%s\n' "$fx_ss_table" >"$work/ss"

	{
		printf 'passwordauthentication %s\n' "$fx_pw_auth"
		printf 'permitrootlogin %s\n' "$fx_root_login"
		printf 'pubkeyauthentication yes\n'
	} >"$work/sshd-conf"

	{
		printf '%s\t%s\t%s\n' "$fx_data" "$fx_dir_mode" "$fx_owner"
		printf '%s\t%s\t%s\n' "$fx_data/otter.db" "600" "$fx_owner"
		printf '%s\t%s\t%s\n' "$env_dir/castor-24856da9.env" "$fx_env_mode" "$fx_env_owner"
		printf '%s\t%s\t%s\n' "$env_dir/castor-24856da9.daemon.env" "$fx_env_mode" "$fx_env_owner"
	} >"$work/meta"

	# The swap table and swappiness, in the kernel's own format. "none" means
	# the host has no /proc/swaps at all, so the subject's only source is a
	# swapon that reports nothing.
	mkdir -p "$work/proc"
	swap_kib=${FIX_SWAP_KIB:-2097148}
	if [ "$swap_kib" = "none" ]; then
		rm -f "$work/proc/swaps"
	else
		{
			printf 'Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n'
			printf '/swapfile                               file\t\t%s\t\t0\t\t-2\n' "$swap_kib"
		} >"$work/proc/swaps"
	fi
	printf 'MemTotal:        926448 kB\n' >"$work/proc/meminfo"

	if [ "${FIX_MISSING_REPORT:-}" = "yes" ]; then
		rm -f "$work/provision-report.txt"
	else
		printf 'otter host provisioning report\n' >"$work/provision-report.txt"
	fi

	rm -f "$work/novalue"
	[ "${FIX_NOVALUE:-}" = "yes" ] && : >"$work/novalue"

	write_id
	write_getent
	write_stat
	write_ss
	write_sshd
	write_systemctl
}

# run executes the assertion script against the fixture. The fixture bin comes
# first so its shims are found before the author's tools; PATH is passed
# explicitly because a shell clears it for a setuid-ish environment otherwise.
run() {
	# The interpreter is resolved with the *real* PATH first: prepending the
	# fixture directory ahead of `sh` would make the shell itself unfindable.
	PATH="$work/bin:$PATH" OTTER_SERVICE_USER=$service_user \
		OTTER_ENV_DIR=$env_dir OTTER_SYSTEMD_UNIT_DIR=$work/etc/systemd/system \
		OTTER_PROC_ROOT=$work/proc \
		OTTER_PROVISION_REPORT=$work/provision-report.txt \
		"$sh_bin" "$subject" --workspace-dir "$workspace_dir" 2>&1
}

passed=0
failed=0

# expect runs one case. $1 is the case name, $2 is 0 or 1 (want success or
# failure), $3 is the substring the output must contain, and $4 -- optional --
# is one mutation applied to the fixture variables before it is rebuilt, as a
# single shell assignment (FIX_DIR_MODE=755). Further arguments are additional
# substrings the output must contain, for a case whose fault is noticed by
# more than one check.
#
# populate is called in this shell rather than through `env` because it is a
# function and `env` cannot call one. That means a mutation would leak into the
# next case, so every fixture variable is dropped again afterwards.
expect() {
	name=$1
	want=$2
	must_contain=$3
	mutation=${4:-}
	shift 4 2>/dev/null || shift $#
	extra="$*"

	# The mutation names one FIX_ variable; it is set through the environment
	# rather than eval'd as shell, so the fixture cannot accidentally execute
	# a command that a test name happens to contain.
	if [ -n "$mutation" ]; then
		export "$mutation"
	fi
	populate
	unset FIX_WS FIX_DATA_DIR FIX_UNIT FIX_OWNER FIX_USER FIX_GROUP FIX_UID \
		FIX_SHELL FIX_DIR_MODE FIX_ENV_MODE FIX_ENV_OWNER FIX_ENABLED FIX_ACTIVE \
		FIX_UNIT_USER FIX_NNP FIX_PROTECT_SYSTEM FIX_PROTECT_HOME \
		FIX_PRIVATE_TMP FIX_RW FIX_LISTEN FIX_EXEC_START FIX_SS FIX_PW_AUTH \
		FIX_ROOT_LOGIN FIX_NOVALUE FIX_QUIET_SYSTEMCTL FIX_SWAP_KIB \
		FIX_MISSING_REPORT || true

	status=0
	out=$(run) || status=$?

	if [ "$want" = "0" ] && [ "$status" -ne 0 ]; then
		echo "test-assert-host-permissions: FAIL: $name: want success, got exit $status" >&2
		printf '%s\n' "$out" | sed 's/^/    /' >&2
		failed=$((failed + 1))
		return
	fi
	if [ "$want" = "1" ] && [ "$status" -eq 0 ]; then
		echo "test-assert-host-permissions: FAIL: $name: want failure, got success" >&2
		printf '%s\n' "$out" | sed 's/^/    /' >&2
		failed=$((failed + 1))
		return
	fi
	for want_sub in "$must_contain" $extra; do
		[ -n "$want_sub" ] || continue
		if ! printf '%s\n' "$out" | grep -Fq "$want_sub"; then
			echo "test-assert-host-permissions: FAIL: $name: output does not name the broken check" >&2
			echo "    want substring: $want_sub" >&2
			printf '%s\n' "$out" | sed 's/^/    /' >&2
			failed=$((failed + 1))
			return
		fi
	done
	passed=$((passed + 1))
	echo "ok: $name"
}

# --- the matrix -------------------------------------------------------------
#
# Each override is passed as ONE argument: it is a shell assignment that expect
# evaluates before rebuilding the fixture. Multiple assignments therefore go in
# one quoted string, not as separate words.

# The clean host: every assertion holds, so a failure here means the script's
# own checks are wrong, not the fixture.
expect "clean fixture passes" 0 "all assertions passed"

# Each mutation below is one property of a correctly provisioned host, broken.
# The substring is the check that must notice.
expect "interactive shell on the service account fails" 1 "want a nologin shell" \
	"FIX_SHELL=/bin/bash"
expect "service account in the interactive uid range fails" 1 "interactive account's range" \
	"FIX_UID=4242"
expect "missing service account fails" 1 "does not exist" \
	"FIX_USER=$missing_user FIX_UID=$missing_uid"

expect "data directory 0755 fails" 1 "want 0700" "FIX_DIR_MODE=755"
expect "data directory owned by another group fails" 1 "want $service_user:$service_group" \
	"FIX_OWNER=root:$service_group"
expect "env file 0644 fails" 1 "want 0600" "FIX_ENV_MODE=644"
expect "env file owned by root fails" 1 "want $service_user:$service_group" \
	"FIX_ENV_OWNER=root:root"

expect "unit not enabled fails" 1 "want enabled" "FIX_ENABLED=disabled"
expect "unit not active fails" 1 "want active" "FIX_ACTIVE=failed"
expect "unit running as root fails" 1 "want $service_user" "FIX_UNIT_USER=root"
expect "NoNewPrivileges removed fails" 1 "does not set NoNewPrivileges" "FIX_NNP=off"
expect "ProtectSystem removed fails" 1 "does not set ProtectSystem" "FIX_PROTECT_SYSTEM=off"
expect "ProtectHome removed fails" 1 "does not set ProtectHome" "FIX_PROTECT_HOME=off"
expect "PrivateTmp removed fails" 1 "does not set PrivateTmp" "FIX_PRIVATE_TMP=off"
expect "ReadWritePaths without the data dir fails" 1 "does not include" \
	"FIX_RW=/tmp/elsewhere"
expect "ReadWritePaths absent fails" 1 "does not set ReadWritePaths" "FIX_RW=off"

expect "API on a wildcard address fails" 1 "not loopback" "FIX_LISTEN=0.0.0.0:7337"
expect "a wildcard listener fails" 1 "wildcard (0.0.0.0) listener exists" \
	"FIX_SS=LISTEN 0 4096 127.0.0.1:$api_port 0.0.0.0:*
LISTEN 0 4096 0.0.0.0:8999 0.0.0.0:*"
expect "an unapproved non-loopback listener fails" 1 "unapproved listener" \
	"FIX_SS=LISTEN 0 4096 127.0.0.1:$api_port 0.0.0.0:*
LISTEN 0 4096 10.0.0.5:8999 0.0.0.0:*
LISTEN 0 4096 0.0.0.0:22 0.0.0.0:*"
expect "the daemon's own port not listening fails" 1 "nothing is listening" \
	"FIX_SS=LISTEN 0 4096 0.0.0.0:22 0.0.0.0:*"

# An older systemd has no `show --value`, so the fallback read must still find
# the directives. The fixture's shim is switched to the older behaviour here.
expect "properties read without --value still assert" 0 "all assertions passed" \
	"FIX_NOVALUE=yes"

expect "a systemctl that answers nothing fails" 1 "want enabled" \
	"FIX_QUIET_SYSTEMCTL=yes" "want active"

# The CW-0 finding: swap was the silent casualty of a provisioning script that
# aborted early, and the first-run report went with it.
expect "no swap fails" 1 "below the" "FIX_SWAP_KIB=0"
expect "a 512 MiB swapfile fails the 256 MiB floor only as configured" 0 "all assertions passed" \
	"FIX_SWAP_KIB=524288"
expect "swap below the floor fails" 1 "below the" "FIX_SWAP_KIB=131072"
expect "a host with no /proc/swaps and no swapon fails" 1 "active swap is 0 MiB" \
	"FIX_SWAP_KIB=none"
expect "a missing provisioning report fails" 1 "provisioning report" \
	"FIX_MISSING_REPORT=yes"

expect "sshd accepting passwords fails" 1 "password authentication" "FIX_PW_AUTH=yes"
expect "sshd permitting root password login fails" 1 "root login" "FIX_ROOT_LOGIN=yes"

# --- outcome ----------------------------------------------------------------

if [ "$failed" -ne 0 ]; then
	echo "test-assert-host-permissions: FAILED: $failed case(s), $passed passed" >&2
	exit 1
fi

echo "test-assert-host-permissions: all $passed cases passed"
