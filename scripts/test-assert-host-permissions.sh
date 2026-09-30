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

# The fixture account is synthetic, and the id shim resolves it from the
# fixture's own passwd file. Using the account that runs the harness would make
# this pass or fail depending on the machine: run as root (a container) and
# "owned by the service account" is indistinguishable from "owned by root",
# which silently disables three cases.
service_user=otter-fixture
service_group=otter-fixture
service_uid=501
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

# The ss and sshd shims own their own absence: when the knob is set the shim is
# simply not written. Separate "remove it afterwards" helpers let the order of
# generator calls decide whether the host looked like it had the tool.
write_ss() {
	if [ "${FIX_SS_ABSENT:-}" = "yes" ]; then
		# A host without ss. `command -v ss` must fail the way it would on a
		# minimal image, so nothing named ss is left on the fixture PATH.
		rm -f "$work/bin/ss"
		return
	fi
	# FIX_SS_EMPTY models a real ss that answered with nothing. It must be a
	# successful command with empty output: a shim that merely exited non-zero
	# would be shadowed by the subject's own fallback to the real ss.
	if [ "${FIX_SS_EMPTY:-}" = "yes" ]; then
		cat >"$work/bin/ss" <<'SHIM'
#!/bin/sh
exit 0
SHIM
	else
		cat >"$work/bin/ss" <<SHIM
#!/bin/sh
cat $work/ss
SHIM
	fi
	chmod +x "$work/bin/ss"
}

write_sshd() {
	if [ "${FIX_SSHD_ABSENT:-}" = "yes" ]; then
		# A host with no sshd at all: the shim is removed and the subject's
		# `command -v sshd` / `/usr/sbin/sshd` checks both fail.
		rm -f "$work/bin/sshd"
		return
	fi
	if [ "${FIX_SSHD_EMPTY:-}" = "yes" ]; then
		# sshd -T prints nothing when the privilege-separation directory is
		# absent (a fresh boot before sshd has served), which must be a
		# failure rather than a pass.
		cat >"$work/bin/sshd" <<'SHIM'
#!/bin/sh
exit 0
SHIM
	else
		cat >"$work/bin/sshd" <<SHIM
#!/bin/sh
cat $work/sshd-conf
SHIM
	fi
	chmod +x "$work/bin/sshd"
}

write_swapon() {
	# The subject sums $proc_root/swaps and falls back to `swapon` when that is
	# unreadable. On a host with swap the real swapon would answer, so the
	# fixture provides one: bytes on stdout, nothing when the fixture has no
	# swap at all.
	cat >"$work/bin/swapon" <<SHIM
#!/bin/sh
cat $work/swap-bytes
SHIM
	chmod +x "$work/bin/swapon"
}

write_sysctl() {
	# The $work path is expanded when the shim is written; \${1:-} must not be,
	# because it is the shim's own argument at run time.
	cat >"$work/bin/sysctl" <<SHIM
#!/bin/sh
case "\${1:-}" in
-n) cat $work/swappiness ;;
*) exit 1 ;;
esac
SHIM
	chmod +x "$work/bin/sysctl"
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
	fx_db_mode=${FIX_DB_MODE:-600}
	# The data directory and the env files each have their own knobs. Sharing
	# one meant a case named for the data directory was satisfied by the env
	# files' failure output, so deleting the data-owner check kept the suite
	# green.
	fx_data_owner=${FIX_DATA_OWNER:-$service_user:$service_group}
	fx_env_mode=${FIX_ENV_MODE:-600}
	# The env directory and files are root-owned: systemd reads the file as
	# root before dropping to User=otter, and a service account that owns its
	# own credentials can rewrite them.
	fx_env_owner=${FIX_ENV_OWNER:-root:root}
	fx_env_dir_mode=${FIX_ENV_DIR_MODE:-700}
	fx_env_dir_owner=${FIX_ENV_DIR_OWNER:-root:root}
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
	# FIX_RW_ABSENT drops the property from the unit entirely, which is the
	# only way to exercise the loop's "does not set ReadWritePaths" branch: an
	# empty value still comes back from systemctl as an empty string.
	fx_rw_absent=${FIX_RW_ABSENT:-}
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
	fx_swappiness=${FIX_SWAPPINESS:-60}

	# FIX_MISSING_DATA_DIR / FIX_MISSING_UNIT exercise the branches that report
	# an absent path rather than a wrong one.
	mkdir -p "$fx_ws" "$env_dir" "$(dirname "$unit_file")"
	if [ "${FIX_MISSING_ENV_DIR:-}" = "yes" ]; then
		rm -rf "$env_dir"
	fi
	if [ "${FIX_MISSING_DATA_DIR:-}" = "yes" ]; then
		rm -rf "$fx_data"
	else
		mkdir -p "$fx_data"
		# The subject checks a present database's mode; the fixture must
		# actually have one, or the check is skipped and a case named for it
		# proves nothing.
		: >"$fx_data/otter.db"
	fi
	# The fixture directory is reused between cases, so a "missing" path has to
	# be removed rather than merely not created.
	if [ "${FIX_MISSING_UNIT:-}" = "yes" ]; then
		rm -f "$unit_file"
	else
		: >"$unit_file"
	fi
	# The literal record `otter deploy` writes (keys unit/name/listen and no
	# data_dir), so the derivation path is what the harness exercises.
	printf '{"jobs_layout":"jobs","id":"e0309b8c-1111-2222-3333-444455556666","slug":"castor","name":"castor-24856da9","unit":"%s","listen":"%s","created_at":"2026-09-30T00:00:00Z"}\n' "$fx_unit" "$fx_listen" >"$fx_ws/workspace.json"
	if [ "${FIX_MISSING_ENV_DIR:-}" = "yes" ]; then
		: # nothing to create; the directory is gone
	else
		: >"$env_dir/castor-24856da9.env"
		: >"$env_dir/castor-24856da9.daemon.env"
	fi
	# The env files are real files so the subject's existence test passes; their
	# recorded modes and owners, which the stat shim serves, are the fixture's.
	[ "${FIX_MISSING_ENV_DIR:-}" = "yes" ] ||
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
		# A second account, so the fixture is not a one-user host. It must not
		# be named "root": the harness runs as root in a container, and a
		# duplicate name made `getent passwd <user>` return two lines.
		printf 'nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n'
	} >"$work/passwd"

	{
		printf 'User=%s\n' "$fx_unit_user"
		printf 'NoNewPrivileges=%s\n' "$fx_nnp"
		printf 'ProtectSystem=%s\n' "$fx_protect_system"
		printf 'ProtectHome=%s\n' "$fx_protect_home"
		printf 'PrivateTmp=%s\n' "$fx_private_tmp"
		if [ "$fx_rw_absent" = "yes" ]; then
			: # the property is not present at all
		else
			printf 'ReadWritePaths=%s\n' "$fx_rw"
		fi
		printf 'ExecStart=%s\n' "$fx_exec_start"
	} >"$work/unit-props"

	printf '%s\n' "$fx_ss_table" >"$work/ss"

	{
		printf 'passwordauthentication %s\n' "$fx_pw_auth"
		printf 'permitrootlogin %s\n' "$fx_root_login"
		printf 'pubkeyauthentication yes\n'
	} >"$work/sshd-conf"

	{
		printf '%s\t%s\t%s\n' "$fx_data" "$fx_dir_mode" "$fx_data_owner"
		printf '%s\t%s\t%s\n' "$fx_data/otter.db" "$fx_db_mode" "$fx_data_owner"
		printf '%s\t%s\t%s\n' "$env_dir" "$fx_env_dir_mode" "$fx_env_dir_owner"
		printf '%s\t%s\t%s\n' "$env_dir/castor-24856da9.env" "$fx_env_mode" "$fx_env_owner"
		printf '%s\t%s\t%s\n' "$env_dir/castor-24856da9.daemon.env" "$fx_env_mode" "$fx_env_owner"
	} >"$work/meta"

	# swappiness, read through the sysctl shim below.
	printf '%s\n' "$fx_swappiness" >"$work/swappiness"

	# The swap table and swappiness, in the kernel's own format. "none" means
	# the host has no /proc/swaps at all, so the subject's only source is a
	# swapon that reports nothing.
	mkdir -p "$work/proc"
	swap_kib=${FIX_SWAP_KIB:-2097148}
	if [ "$swap_kib" = "none" ]; then
		rm -f "$work/proc/swaps"
		: >"$work/swap-bytes"
	else
		# swapon --bytes prints bytes; the subject divides by 1024.
		printf '%s\n' "$((swap_kib * 1024))" >"$work/swap-bytes"
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
	if [ "${FIX_NOVALUE:-}" = "yes" ]; then
		: >"$work/novalue"
	fi

	write_id
	write_getent
	write_stat
	write_ss
	write_sshd
	write_systemctl
	write_swapon
	write_sysctl
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
		OTTER_SSHD_BIN=${FIX_SSHD_BIN:-$work/bin/sshd} \
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
# expect runs one case.
#
#   expect NAME WANT REQUIRED... [-- MUTATION...]
#
# WANT is 0 (the subject must pass) or 1 (it must fail). Every REQUIRED
# argument is a substring the output must contain, so a case can name more than
# one check. Each MUTATION is a single NAME=VALUE assignment applied to the
# fixture environment before the fixture is rebuilt; they are exported one at a
# time, because `export "A=1 B=2"` sets A to "1 B=2" and leaves B unset.
#
# The mutation names one FIX_ variable; it is set through the environment rather
# than eval'd as shell, so a test name cannot accidentally execute a command.
expect() {
	name=$1
	want=$2
	shift 2

	# Required substrings and mutations both travel one-per-line in files. A
	# space-joined string cannot carry a substring that contains spaces -- and
	# re-splitting it on whitespace checked each word separately, so "unit file
	# /path does not exist" was satisfied by the word "exist" in "exists". A
	# pipe is not used to read them back because a piped `while` body runs in a
	# subshell, where `export` would not reach the fixture.
	: >"$work/required"
	: >"$work/mutations"
	seen_sep=0
	for arg in "$@"; do
		if [ "$arg" = "--" ]; then
			seen_sep=1
			continue
		fi
		if [ "$seen_sep" -eq 1 ]; then
			printf '%s\n' "$arg" >>"$work/mutations"
		else
			printf '%s\n' "$arg" >>"$work/required"
		fi
	done

	# Reset every fixture variable first, so a mutation cannot leak into the
	# next case.
	unset_mutations
	while IFS= read -r assignment; do
		[ -n "$assignment" ] || continue
		name_part=${assignment%%=*}
		encoded=${assignment#*=}
		# %b decodes the \n escapes, so a multi-line value survives the
		# line-oriented transport above.
		value_part=$(printf '%b' "$encoded")
		case $assignment in
		*=*) export "$name_part"="$value_part" ;;
		*)
			echo "test-assert-host-permissions: FAIL: $name: malformed mutation $assignment" >&2
			failed=$((failed + 1))
			return
			;;
		esac
	done <"$work/mutations"
	populate

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
	while IFS= read -r want_sub; do
		[ -n "$want_sub" ] || continue
		if ! printf '%s\n' "$out" | grep -Fq -- "$want_sub"; then
			echo "test-assert-host-permissions: FAIL: $name: output does not name the broken check" >&2
			echo "    want substring: $want_sub" >&2
			printf '%s\n' "$out" | sed 's/^/    /' >&2
			failed=$((failed + 1))
			return
		fi
	done <"$work/required"
	passed=$((passed + 1))
	echo "ok: $name"
}

# unset_mutations drops every FIX_* knob the matrix can set, so each case starts
# from the correct fixture.
unset_mutations() {
	unset FIX_WS FIX_DATA_DIR FIX_UNIT FIX_DATA_OWNER FIX_OWNER FIX_USER \
		FIX_GROUP FIX_UID FIX_SHELL FIX_DIR_MODE FIX_DB_MODE FIX_ENV_MODE \
		FIX_ENV_OWNER FIX_ENABLED FIX_ACTIVE FIX_UNIT_USER FIX_NNP \
		FIX_PROTECT_SYSTEM FIX_PROTECT_HOME FIX_PRIVATE_TMP FIX_RW FIX_LISTEN \
		FIX_EXEC_START FIX_SS FIX_PW_AUTH FIX_ROOT_LOGIN FIX_NOVALUE \
		FIX_QUIET_SYSTEMCTL FIX_SWAP_KIB FIX_MISSING_REPORT \
		FIX_MISSING_DATA_DIR FIX_MISSING_UNIT FIX_MISSING_ENV_DIR FIX_SS_ABSENT FIX_SS_EMPTY FIX_SSHD_EMPTY \
		FIX_SSHD_BIN FIX_ENV_DIR_MODE FIX_ENV_DIR_OWNER FIX_RW_ABSENT \
		FIX_SSHD_ABSENT FIX_SWAPPINESS || true
}

# --- the matrix -------------------------------------------------------------
#
# Expectations and mutations are separated by `--`: everything before it is a
# substring the output must contain, everything after is a fixture assignment.
# A case can therefore require several checks and set several knobs, and no
# case depends on another check's failure to satisfy it.

# The clean host: every assertion holds, so a failure here means the script's
# own checks are wrong, not the fixture.
expect "clean fixture passes" 0 "all assertions passed"

# --- the service account ---

expect "interactive shell on the service account fails" 1 \
	"want a nologin shell" -- "FIX_SHELL=/bin/bash"
expect "service account in the interactive uid range fails" 1 \
	"interactive account's range" -- "FIX_UID=4242"
expect "missing service account fails" 1 "does not exist" \
	-- "FIX_USER=$missing_user" "FIX_UID=$missing_uid"

# --- the data directory ---

expect "data directory 0755 fails" 1 "want 0700" -- "FIX_DIR_MODE=755"
# The data directory has its own owner knob; sharing one with the env files
# meant this case was satisfied by their failure output.
expect "data directory owned by root fails" 1 \
	"data directory $data_dir is owned by root:nogroup, want $service_user:$service_user" \
	-- "FIX_DATA_OWNER=root:nogroup"
expect "data directory owner is not the service group fails" 1 \
	"data directory $data_dir is owned by" -- "FIX_DATA_OWNER=$service_user:not-$service_group"
expect "a missing data directory fails" 1 "data directory $data_dir does not exist" \
	-- "FIX_MISSING_DATA_DIR=yes"
# The database's own mode is a recommendation: the 0700 directory contains it,
# so a 0644 otter.db meets the documented bar and is reported, not failed.
expect "a 0644 otter.db is reported, not failed" 0 \
	"otter.db mode is 644; owner-only is tighter" -- "FIX_DB_MODE=644"
expect "a 0600 otter.db passes" 0 "otter.db mode is 0600" -- "FIX_DB_MODE=600"

# --- the environment files ---

expect "a missing environment directory fails" 1 \
	"environment directory $env_dir does not exist" -- "FIX_MISSING_ENV_DIR=yes"
expect "a 0755 environment directory fails" 1 \
	"environment directory $env_dir is mode 755, want 0700" -- "FIX_ENV_DIR_MODE=755"
expect "an environment directory owned by the service account fails" 1 \
	"environment directory $env_dir is owned by $service_user:$service_group, want root:root" \
	-- "FIX_ENV_DIR_OWNER=$service_user:$service_group"
expect "env file 0644 fails" 1 "want 0600" -- "FIX_ENV_MODE=644"
# The env files are root-owned, which is what `otter deploy` writes and the
# stronger posture: systemd reads them as root, and a service account that owns
# its credentials can rewrite them.
expect "an env file owned by the service account fails" 1 \
	"$env_dir/castor-24856da9.env is owned by $service_user:$service_group, want root:root" \
	-- "FIX_ENV_OWNER=$service_user:$service_group"
expect "the daemon env file is checked too" 1 \
	"$env_dir/castor-24856da9.daemon.env is mode 640, want 0600" -- "FIX_ENV_MODE=640"

# --- the systemd unit ---

expect "a missing unit file fails" 1 "unit file $unit_file does not exist" \
	-- "FIX_MISSING_UNIT=yes"
expect "unit not enabled fails" 1 "want enabled" -- "FIX_ENABLED=disabled"
expect "unit not active fails" 1 "want active" -- "FIX_ACTIVE=failed"
expect "unit running as root fails" 1 "want $service_user" -- "FIX_UNIT_USER=root"
expect "a systemctl that answers nothing fails" 1 \
	"want enabled" "want active" -- "FIX_QUIET_SYSTEMCTL=yes"
expect "NoNewPrivileges removed fails" 1 "does not set NoNewPrivileges" -- "FIX_NNP=off"
expect "ProtectSystem removed fails" 1 "does not set ProtectSystem" -- "FIX_PROTECT_SYSTEM=off"
expect "ProtectHome removed fails" 1 "does not set ProtectHome" -- "FIX_PROTECT_HOME=off"
expect "PrivateTmp removed fails" 1 "does not set PrivateTmp" -- "FIX_PRIVATE_TMP=off"
expect "ReadWritePaths absent fails" 1 "does not set ReadWritePaths" -- "FIX_RW_ABSENT=yes"
expect "an empty ReadWritePaths fails the presence check" 1 \
	"does not set ReadWritePaths" -- "FIX_RW=off"
expect "ReadWritePaths without the data dir fails" 1 "does not include" -- "FIX_RW=/tmp/elsewhere"
# An older systemd has no `show --value`, so the fallback read must still find
# the directives. The fixture's shim is switched to the older behaviour here.
expect "properties read without --value still assert" 0 \
	"all assertions passed" -- "FIX_NOVALUE=yes"

# --- the API and the listening sockets ---

expect "API on a wildcard address fails" 1 "not loopback" -- "FIX_LISTEN=0.0.0.0:7337"
expect "a wildcard 0.0.0.0 listener fails" 1 \
	"wildcard (0.0.0.0) listener exists on port 8999" \
	-- "FIX_SS=LISTEN 0 4096 127.0.0.1:$api_port 0.0.0.0:*\nLISTEN 0 4096 0.0.0.0:8999 0.0.0.0:*"
# The v6 wildcard is a separate branch and needs its own case.
expect "a wildcard :: listener fails" 1 \
	"wildcard (::) listener exists on port 8999" \
	-- "FIX_SS=LISTEN 0 4096 127.0.0.1:$api_port 0.0.0.0:*\nLISTEN 0 4096 0.0.0.0:22 0.0.0.0:*\nLISTEN 0 4096 [::]:8999 [::]:*"
expect "an unapproved non-loopback listener fails" 1 "unapproved listener" \
	-- "FIX_SS=LISTEN 0 4096 127.0.0.1:$api_port 0.0.0.0:*\nLISTEN 0 4096 10.0.0.5:8999 0.0.0.0:*\nLISTEN 0 4096 0.0.0.0:22 0.0.0.0:*"
expect "the daemon's own port not listening fails" 1 "nothing is listening" \
	-- "FIX_SS=LISTEN 0 4096 0.0.0.0:22 0.0.0.0:*"
expect "empty ss output fails" 1 "no TCP listeners at all" -- "FIX_SS_EMPTY=yes"
expect "unparsable ss output fails" 1 "no parsable listening sockets" \
	-- "FIX_SS=State Recv-Q Send-Q"
expect "an absent ss fails" 1 "ss is not installed" -- "FIX_SS_ABSENT=yes"

# --- swap and the provisioning record ---

# The CW-0 finding: swap was the silent casualty of a provisioning script that
# aborted early, and the first-run report went with it.
expect "no swap fails" 1 "below the" -- "FIX_SWAP_KIB=0"
expect "a 512 MiB swapfile passes the 256 MiB floor" 0 \
	"active swap is 512 MiB" -- "FIX_SWAP_KIB=524288"
expect "swap below the floor fails" 1 "below the" -- "FIX_SWAP_KIB=131072"
expect "a host with no /proc/swaps and no swapon fails" 1 \
	"active swap is 0 MiB" -- "FIX_SWAP_KIB=none"
expect "vm.swappiness out of range fails" 1 "outside 0-100" -- "FIX_SWAPPINESS=200"
expect "vm.swappiness in range passes" 0 "vm.swappiness=10" -- "FIX_SWAPPINESS=10"
expect "a missing provisioning report fails" 1 "provisioning report" \
	-- "FIX_MISSING_REPORT=yes"

# --- sshd ---

expect "sshd accepting passwords fails" 1 "password authentication" -- "FIX_PW_AUTH=yes"
expect "sshd permitting root password login fails" 1 "root login" -- "FIX_ROOT_LOGIN=yes"
expect "empty sshd -T output fails" 1 "could not read the effective sshd configuration" \
	-- "FIX_SSHD_EMPTY=yes"
expect "an absent sshd fails" 1 "sshd is not installed" \
	-- "FIX_SSHD_ABSENT=yes" "FIX_SSHD_BIN=$work/bin/no-such-sshd"

# --- outcome ----------------------------------------------------------------

if [ "$failed" -ne 0 ]; then
	echo "test-assert-host-permissions: FAILED: $failed case(s), $passed passed" >&2
	exit 1
fi

echo "test-assert-host-permissions: all $passed cases passed"
