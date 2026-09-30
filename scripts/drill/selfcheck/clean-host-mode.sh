#!/bin/sh
# Selfcheck: the clean-host mode of scripts/drill/backup-restore.sh, without a
# host. This runs on a developer machine and in CI; the two-host drill itself
# runs only in the custodian's window (HW-7), against two real instances.
#
# What it can prove without a host, and does:
#
#   1. the mode's parameters are enforced. DRILL_CLEAN_HOST=1 with anything
#      missing refuses and names every missing value; clean-host parameters
#      without the opt-in REFUSE rather than silently running the local
#      same-host drill; an unknown opt-in value refuses; a parameter that could
#      not survive a remote shell refuses; a missing key file refuses.
#   2. the preflight probe (lib/host-report.sh) measures a real directory tree
#      correctly: an empty target reports clean, a leftover otter.db reports
#      dirty with the offender named, and so do a leftover otter.yaml and
#      .otter-id in the jobs root.
#   3. the preflight gate (lib/assert-clean-host.sh) refuses a target that is
#      not clean, refuses the same machine twice, passes its cleanliness check
#      when the target really is empty, and has a passing path at all.
#   4. the backup and restore halves (lib/hot-backup.sh, lib/restore-host.sh)
#      round-trip a runtime's on-disk state: every member that must be there
#      arrives, sdk/ WAL lock and pid files do not, the manifest catches a
#      corrupted archive, and a sabotage that omits the job sources leaves no
#      .otter-id to restore -- the red condition the clean-host identity
#      assertion exists to catch.
#   5. the two-host drill itself goes red at the cleanliness assertion, before
#      anything is written to the target, when the target is not clean; and the
#      same drill passes those checks when the target is empty. That run is NOT
#      the whole drill: it stops at the target-unit shape check, because this
#      machine has no systemd for the probe to report. The drill-level
#      source-live, token, version, backup, transfer and restore steps are not
#      reached here -- they are exercised a level down, by the probe cases in
#      section 2 and the gate cases in section 3. The run uses a test double for
#      the ssh transport only, the fields it substitutes are listed in
#      section 5, and it says in its own output that it is not host evidence.
#
# What it CANNOT prove, and does not claim: that a restore onto a second host
# works. That needs two hosts, and it is what the drill examines.
#
# How it runs. `make drill` does NOT run this file: scripts/drill.sh globs
# scripts/drill/*.sh, and this lives in the selfcheck/ subdirectory on purpose,
# so that a plain `make drill` cannot pick up the clean-host mode without its
# parameters and fail the suite. The selfcheck reaches CI through
# `go test ./...`, by way of internal/drill/selfcheck_test.go, which also reads
# every script this file runs so a green result cannot be a stale cache.
#
# Platform branches. The probe has Linux paths (GNU `stat -c`,
# `/etc/machine-id`) with portable fallbacks (BSD `stat -f`, `hostname`); the
# selfcheck runs wherever the developer is, so on macOS the fallbacks run and on
# Linux -- which is what CI uses -- the Linux branches are reached and executed
# for real. Rather than add a Docker dependency to the unit-test path, the
# live-daemon case asserts the branch each platform is *supposed* to take from
# the state of the machine: on a Linux host with a non-empty machine-id file the
# probe must name that file as its source, on a host without one it must name
# the hostname fallback, and the owner and mode must be measured either way. A
# regression in either branch fails here.
#
# Two things are deliberately NOT claimed. `getent`: the probe only runs
# `command -v getent`, nothing here invokes it, the gate's `getent=yes` comes
# from a hand-written fixture, and the drill's one real `getent passwd/group`
# call is reached by no test -- it runs for the first time at HW-7. And the
# two-host composition: see item 5 above.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
LIB="$root/scripts/drill/lib"
ENTRY="$root/scripts/drill/backup-restore.sh"
CLEAN_HOST="$root/scripts/drill/modes/backup-restore-clean-host.sh"

for tool in sqlite3 tar find; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "selfcheck: $tool is required and was not found on PATH" >&2
		echo "selfcheck: a selfcheck that cannot run is a failure, not a skip" >&2
		exit 2
	}
done

tmpbase=${TMPDIR:-/tmp}
while [ "$tmpbase" != "${tmpbase%/}" ]; do tmpbase=${tmpbase%/}; done
[ -n "$tmpbase" ] || tmpbase=/tmp
WORK=$(mktemp -d "$tmpbase/otter-selfcheck-clean-host.XXXXXX")
cleaning=0
cleanup() {
	status=$?
	[ "$cleaning" -eq 1 ] && exit "$status"
	cleaning=1
	# The live-daemon case starts a real otterd; it must not outlive the run.
	if [ -n "${LIVE_PID:-}" ]; then
		kill "$LIVE_PID" 2>/dev/null || true
		wait "$LIVE_PID" 2>/dev/null || true
	fi
	if [ -n "${OTTER_SELFCHECK_KEEP:-}" ] || { [ "$status" -ne 0 ] && [ -z "${CI:-}" ]; }; then
		echo "selfcheck: keeping $WORK" >&2
	else
		rm -rf "$WORK"
	fi
	exit "$status"
}
trap cleanup EXIT INT TERM

say() { echo "selfcheck: $*"; }
fail() {
	echo "selfcheck: FAILED: $*" >&2
	exit 1
}

# has / hasnot assert on a captured run's output. Passing the text in makes it
# impossible to assert against a stale capture by accident, and the failure
# carries the tail of the output so a red run is a diagnosis.
has() {
	case "$1" in
	*"$2"*) ;;
	*)
		fail "$3 (output did not contain: $2)
--- captured output ---
$(printf '%s' "$1" | tail -n 25)"
		;;
	esac
}
hasnot() {
	case "$1" in
	*"$2"*)
		fail "$3 (output contained: $2)
--- captured output ---
$(printf '%s' "$1" | tail -n 25)"
		;;
	esac
}

# The clean-host namespace this selfcheck clears before every case, so a
# leftover in the operator's shell cannot decide which mode runs.
CLEAR="-u DRILL_CLEAN_HOST -u DRILL_SOURCE_HOST -u DRILL_TARGET_HOST -u DRILL_SSH_KEY \
-u DRILL_REMOTE_DIR -u DRILL_REMOTE_JOBS_DIR -u DRILL_REMOTE_SERVICE -u DRILL_JOB \
-u DRILL_REMOTE_BIN -u DRILL_REMOTE_API_URL -u DRILL_REMOTE_BACKUP_DIR \
-u DRILL_REMOTE_SUDO -u DRILL_REMOTE_API_TOKEN -u DRILL_REMOTE_ENV_FILE \
-u DRILL_REMOTE_ETC_DIR -u DRILL_TRANSPORT \
-u DRILL_SABOTAGE -u DRILL_KEEP_TARGET -u DRILL_RUN_LIMIT"

# capture runs a command in the cleared namespace and records its merged output
# and exit status.
capture() {
	out=$(env $CLEAR "$@" 2>&1) && status=0 || status=$?
}

# A stub CLI so the probe does not depend on a built binary. When the caller
# supplies the checkout's real binary, the probe is exercised against that.
PROBE_BIN="$WORK/otter"
cat >"$PROBE_BIN" <<'STUB'
#!/bin/sh
case "${1:-}" in
--version)
	echo "otter v0.0.0-selfcheck"
	exit 0
	;;
*)
	echo "selfcheck stub otter: no daemon here" >&2
	exit 1
	;;
esac
STUB
chmod +x "$PROBE_BIN"
if [ -n "${OTTER_BIN:-}" ] && [ -x "${OTTER_BIN:-}" ]; then
	PROBE_BIN=$OTTER_BIN
fi

# --- 1. argument and precondition validation ---------------------------------

say "1/6 parameter enforcement (probe binary: $PROBE_BIN)"

capture DRILL_CLEAN_HOST=1 sh "$ENTRY"
[ "$status" -eq 2 ] || fail "DRILL_CLEAN_HOST=1 with no parameters exited $status, want 2"
for name in DRILL_SOURCE_HOST DRILL_TARGET_HOST DRILL_SSH_KEY DRILL_REMOTE_DIR \
	DRILL_REMOTE_JOBS_DIR DRILL_REMOTE_SERVICE DRILL_JOB; do
	has "$out" "$name" "the missing-parameter refusal did not name $name"
done
has "$out" "refusing to fall back" "the refusal must say it is not falling back"
hasnot "$out" "drill: workspace" "the missing-parameter run started the local same-host drill"
say "  ok  opt-in without parameters names all seven missing values and does not fall back"

capture DRILL_SOURCE_HOST=root@a DRILL_TARGET_HOST=root@b DRILL_SSH_KEY=/nonexistent \
	DRILL_REMOTE_DIR=/var/lib/otter DRILL_REMOTE_JOBS_DIR=/srv/otter/jobs \
	DRILL_REMOTE_SERVICE=otter DRILL_JOB=a-job sh "$ENTRY"
[ "$status" -eq 2 ] || fail "clean-host parameters without DRILL_CLEAN_HOST exited $status, want 2"
has "$out" "DRILL_CLEAN_HOST is not 1" "the refusal must name the missing opt-in"
has "$out" "evidence for the wrong claim" "the refusal must say why it will not fall back"
hasnot "$out" "drill: workspace" "the half-configured run started the local same-host drill"
say "  ok  parameters without the opt-in refuse instead of silently running the local drill"

# R4: the optional clean-host parameters count too. `DRILL_REMOTE_SUDO=1` alone
# is an attempt to run the clean-host drill, and falling through to the local
# mode would record local evidence under a clean-host claim.
capture DRILL_REMOTE_SUDO=1 sh "$ENTRY"
[ "$status" -eq 2 ] || fail "DRILL_REMOTE_SUDO without the opt-in exited $status, want 2"
has "$out" "DRILL_REMOTE_SUDO" "the refusal must name the optional parameter that was set"
has "$out" "DRILL_CLEAN_HOST is not 1" "the refusal must name the missing opt-in"
hasnot "$out" "drill: local same-host mode" "an optional parameter without the opt-in must not run the local drill"
say "  ok  an optional clean-host parameter without the opt-in refuses too"

capture DRILL_REMOTE_ENV_FILE=/etc/otter/workspaces/app.env DRILL_REMOTE_ETC_DIR=/etc/otter sh "$ENTRY"
[ "$status" -eq 2 ] || fail "DRILL_REMOTE_ENV_FILE/DRILL_REMOTE_ETC_DIR without the opt-in exited $status, want 2"
has "$out" "DRILL_REMOTE_ENV_FILE" "the refusal must name every clean-host variable that was set"
has "$out" "DRILL_REMOTE_ETC_DIR" "the refusal must name every clean-host variable that was set"
say "  ok  the token-path parameters without the opt-in refuse too"

capture DRILL_CLEAN_HOST=yes sh "$ENTRY"
[ "$status" -eq 2 ] || fail "DRILL_CLEAN_HOST=yes exited $status, want 2"
has "$out" "must be 1 or unset" "an unknown opt-in value must be refused by name"
say "  ok  an unknown opt-in value refuses"

capture DRILL_CLEAN_HOST=1 DRILL_SOURCE_HOST=root@a DRILL_TARGET_HOST=root@b \
	DRILL_SSH_KEY=/nonexistent DRILL_REMOTE_DIR='/var/lib/otter; rm -rf /' \
	DRILL_REMOTE_JOBS_DIR=/srv/otter/jobs DRILL_REMOTE_SERVICE=otter DRILL_JOB=a-job sh "$ENTRY"
[ "$status" -eq 2 ] || fail "a parameter with shell metacharacters exited $status, want 2"
has "$out" "DRILL_REMOTE_DIR contains characters" "the metacharacter refusal must name the parameter"
say "  ok  a parameter that cannot cross a remote shell refuses"

KEY="$WORK/drill-key"
: >"$KEY"
capture DRILL_CLEAN_HOST=1 DRILL_SOURCE_HOST=root@a DRILL_TARGET_HOST=root@b \
	DRILL_SSH_KEY="$WORK/no-such-key" DRILL_REMOTE_DIR=/var/lib/otter \
	DRILL_REMOTE_JOBS_DIR=/srv/otter/jobs DRILL_REMOTE_SERVICE=otter DRILL_JOB=a-job sh "$ENTRY"
[ "$status" -eq 2 ] || fail "a missing key file exited $status, want 2"
has "$out" "is not a file" "the missing-key refusal must name the problem"
say "  ok  a missing ssh key refuses"

capture DRILL_CLEAN_HOST=1 DRILL_SOURCE_HOST=root@a DRILL_TARGET_HOST=root@a \
	DRILL_SSH_KEY="$KEY" DRILL_REMOTE_DIR=/var/lib/otter DRILL_REMOTE_JOBS_DIR=/srv/otter/jobs \
	DRILL_REMOTE_SERVICE=otter DRILL_JOB=a-job sh "$ENTRY"
[ "$status" -eq 2 ] || fail "the same host twice exited $status, want 2"
has "$out" "this drill needs two hosts" "the same-host refusal must say so"
say "  ok  the same host twice refuses before any connection"

# The default path must still be the local same-host drill: with no clean-host
# parameter and no opt-in, the entry point reaches the local mode's own
# preconditions. (OTTER_BIN is deliberately unusable here so the local drill
# stops before it builds a world -- this selfcheck is not the place to run it.)
capture OTTER_BIN=relative/otter sh "$ENTRY"
[ "$status" -eq 2 ] || fail "the default mode with an unusable OTTER_BIN exited $status, want 2"
has "$out" "OTTER_BIN must be an absolute path" "the default mode must still be the local same-host drill"
say "  ok  with no clean-host parameter the entry point is still the local drill"

# --- 2. the probe measures a real tree ---------------------------------------

say "2/6 probe (lib/host-report.sh) against real directories"

LOGICAL_DATA=/var/lib/otter
LOGICAL_JOBS=/srv/otter/jobs
# One local "host" directory, measured twice: first empty, then with a
# previous runtime's leftovers in it. Both reports therefore name the same
# paths, which is what lets section 3 test the cleanliness check rather than
# the path-equality check.
SBX_DATA="$WORK/host/data"
SBX_JOBS="$WORK/host/jobs"
mkdir -p "$SBX_JOBS"
report_get() { sed -n "s/^$2=//p" "$1"; }

CLEAN_REPORT="$WORK/clean.report"
sh "$LIB/host-report.sh" target "$SBX_DATA" "$SBX_JOBS" "$PROBE_BIN" \
	http://127.0.0.1:7999 otter-selfcheck >"$CLEAN_REPORT" ||
	fail "the probe exited non-zero for an empty target layout"
[ "$(report_get "$CLEAN_REPORT" data_clean)" = yes ] ||
	fail "the probe did not report an absent data directory as clean"
[ "$(report_get "$CLEAN_REPORT" jobs_clean)" = yes ] ||
	fail "the probe did not report an empty jobs root as clean"
[ "$(report_get "$CLEAN_REPORT" daemon)" = stopped ] ||
	fail "the probe did not report a closed API port as a stopped daemon"
[ "$(report_get "$CLEAN_REPORT" data_dir)" = "$SBX_DATA" ] ||
	fail "the probe did not echo the data directory it was given"
[ "$(report_get "$CLEAN_REPORT" kernel)" = "$(uname -s)" ] ||
	fail "the probe did not measure the kernel it is running on"
say "  ok  an absent data directory, an empty jobs root and a closed port report clean/stopped"

mkdir -p "$SBX_DATA" "$SBX_JOBS/app"
: >"$SBX_DATA/otter.db"
: >"$SBX_JOBS/app/otter.yaml"
printf 'a-b-c\n' >"$SBX_JOBS/app/.otter-id"
DIRTY_REPORT="$WORK/dirty.report"
sh "$LIB/host-report.sh" target "$SBX_DATA" "$SBX_JOBS" "$PROBE_BIN" \
	http://127.0.0.1:7999 otter-selfcheck >"$DIRTY_REPORT" ||
	fail "the probe exited non-zero for a dirty layout"
[ "$(report_get "$DIRTY_REPORT" data_clean)" = no ] ||
	fail "the probe called a data directory holding otter.db clean"
has "$(report_get "$DIRTY_REPORT" data_reason)" "otter.db" "the dirty data report did not name the offending entry"
[ "$(report_get "$DIRTY_REPORT" jobs_clean)" = no ] ||
	fail "the probe called a jobs root holding otter.yaml and .otter-id clean"
has "$(report_get "$DIRTY_REPORT" jobs_reason)" "otter.yaml" "the dirty jobs report did not name the offending entries"
[ "$(report_get "$DIRTY_REPORT" jobs_markers)" = 1 ] ||
	fail "the probe did not count the identity marker in the jobs root"
say "  ok  a leftover otter.db, otter.yaml or .otter-id is reported dirty, with the offender named"
# --- the token, and the unit that will serve the restore ----------------------
#
# R1. `otter deploy` writes the API token to /etc/otter/workspaces/<ws>.env and
# the CLI's own discovery glob (/etc/otter/*.env) does not cross `workspaces/`,
# so the probe must resolve it. R2: the unit, not the drill's parameters, decides
# where the restored daemon comes up.
#
# The "host" here is a temporary directory and `systemctl` is a test double; the
# parser, the token resolution, the auth classification and the gate are the
# real shipped code.

AUTHBIN="$WORK/auth-otter"
cat >"$AUTHBIN" <<'AUTHSTUB'
#!/bin/sh
case "${1:-}" in
--version)
	echo "otter v0.0.0-authstub"
	exit 0
	;;
status)
	echo "api:           ${OTTER_API_URL:-}"
	echo "version:       v0.0.0-authstub"
	echo "status:        ok"
	if [ "${OTTER_API_TOKEN:-}" = "GOODTOKEN" ]; then
		echo "jobs:          1 total, 1 valid, 0 invalid"
	else
		echo "otter: run counts unavailable: the daemon requires an API token" >&2
	fi
	exit 0
	;;
*)
	echo "authstub: no such command: ${1:-}" >&2
	exit 1
	;;
esac
AUTHSTUB
chmod +x "$AUTHBIN"

TOKETC="$WORK/probe/etc/otter"
EMPTY_ETC="$WORK/probe/empty/etc/otter"
mkdir -p "$TOKETC/workspaces" "$EMPTY_ETC/workspaces" "$WORK/probe/bin"
printf 'OTTER_API_TOKEN=GOODTOKEN\n' >"$TOKETC/workspaces/app-1a2b3c4d.env"
# Decoys in both other layouts, each with a token that is NOT the serving
# workspace's: another workspace beside it, and the flat /etc/otter/*.env that
# the CLI's own glob searches and that operations.md/security.md still document.
# The unit's own file must win over both, or a multi-workspace host
# authenticates as the wrong workspace and every call is a 401.
printf 'OTTER_API_TOKEN=OTHERTOKEN\n' >"$TOKETC/workspaces/decoy-9f8e7d6c.env"
printf 'OTTER_API_TOKEN=FLATTOKEN\n' >"$TOKETC/otter.env"
FAKE_UNIT_ENV="$TOKETC/workspaces/app-1a2b3c4d.env"
FAKE_UNIT_DAEMON_ENV="$TOKETC/workspaces/app-1a2b3c4d.daemon.env"
# The daemon-settings file, in the real layout: it is loaded first and holds no
# token at all.
printf 'OTTER_DATA_DIR=/should/not/win\n' >"$FAKE_UNIT_DAEMON_ENV"

cat >"$WORK/probe/bin/systemctl" <<'FAKESYSTEMCTL'
#!/bin/sh
# Test double for systemctl: the property output a real one prints for a
# deployed unit. The shape is captured from systemd 255 (Ubuntu 24.04) and
# systemd 252 (Amazon Linux 2023), including the fact that
# `systemctl show -p EnvironmentFiles` prints ONE LINE PER FILE:
#
#   EnvironmentFiles=/etc/otter/workspaces/app-1a2b3c4d.daemon.env (ignore_errors=yes)
#   EnvironmentFiles=/etc/otter/workspaces/app-1a2b3c4d.env (ignore_errors=yes)
#
# `otter deploy` renders the daemon-settings file first and the credentials
# file second, and only the second holds OTTER_API_TOKEN.
#
# FAKE_UNIT_SHAPE=drop-shared removes the credentials line, which is what the
# pre-R1 parse effectively saw when it kept only the first line: the mutation
# used to prove the token resolution's assertion can go red.
svc=""
for a in "$@"; do svc=$a; done
case "$svc" in
otterd-app-1a2b3c4d)
	echo "ActiveState=active"
	echo "LoadState=loaded"
	echo "ExecStart={ path=$FAKE_UNIT_BIN ; argv[]=$FAKE_UNIT_BIN --jobs $FAKE_UNIT_JOBS --data $FAKE_UNIT_DATA --listen $FAKE_UNIT_LISTEN --log-format json ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }"
	case "${FAKE_UNIT_SHAPE:-real}" in
	drop-shared)
		echo "EnvironmentFiles=$FAKE_UNIT_DAEMON_ENV (ignore_errors=yes)"
		;;
	*)
		echo "EnvironmentFiles=$FAKE_UNIT_DAEMON_ENV (ignore_errors=yes)"
		echo "EnvironmentFiles=$FAKE_UNIT_ENV (ignore_errors=yes)"
		;;
	esac
	;;
*)
	# Real systemd omits ExecStart= and EnvironmentFiles= for a unit it does not
	# know; inventing empty lines here would be a shape no host produces.
	echo "ActiveState=inactive"
	echo "LoadState=not-found"
	;;
esac
FAKESYSTEMCTL
chmod +x "$WORK/probe/bin/systemctl"

FAKE_UNIT_BIN=/opt/otter/workspaces/app-1a2b3c4d/bin/otterd
FAKE_UNIT_DATA=/var/lib/otter
FAKE_UNIT_JOBS=/srv/otter/jobs
FAKE_UNIT_LISTEN=127.0.0.1:7337

# probe_auth <out> <unit-env-file|-> <etc-dir> [api_token] [env_file] [shape]
# Runs the real probe with the fake systemctl on PATH. `shape=drop-shared` makes
# systemd report only the first EnvironmentFiles line, which is what the
# pre-R1 parse effectively kept.
probe_auth() {
	auth_out=$1
	auth_unit_env=$2
	auth_etc=$3
	auth_token=${4:-}
	auth_env_file=${5:-}
	auth_shape=${6:-real}
	auth_bin=${7:-$AUTHBIN}
	[ "$auth_unit_env" = "-" ] && auth_unit_env=""
	env PATH="$WORK/probe/bin:$PATH" \
		FAKE_UNIT_BIN="$FAKE_UNIT_BIN" FAKE_UNIT_DATA="$FAKE_UNIT_DATA" \
		FAKE_UNIT_JOBS="$FAKE_UNIT_JOBS" FAKE_UNIT_LISTEN="$FAKE_UNIT_LISTEN" \
		FAKE_UNIT_ENV="$auth_unit_env" FAKE_UNIT_DAEMON_ENV="$FAKE_UNIT_DAEMON_ENV" \
		FAKE_UNIT_SHAPE="$auth_shape" \
		sh "$LIB/host-report.sh" source "$FAKE_UNIT_DATA" "$FAKE_UNIT_JOBS" "$auth_bin" \
		http://127.0.0.1:7337 otterd-app-1a2b3c4d "$auth_token" "$auth_env_file" "$auth_etc" >"$auth_out"
}

UNIT_REPORT="$WORK/unit.report"
probe_auth "$UNIT_REPORT" "$FAKE_UNIT_ENV" "$TOKETC"
[ "$(report_get "$UNIT_REPORT" unit_data_dir)" = "$FAKE_UNIT_DATA" ] ||
	fail "the probe did not read --data out of the unit's ExecStart"
[ "$(report_get "$UNIT_REPORT" unit_jobs_dir)" = "$FAKE_UNIT_JOBS" ] ||
	fail "the probe did not read --jobs out of the unit's ExecStart"
[ "$(report_get "$UNIT_REPORT" unit_listen)" = "$FAKE_UNIT_LISTEN" ] ||
	fail "the probe did not read --listen out of the unit's ExecStart"
[ "$(report_get "$UNIT_REPORT" service_bin)" = "$FAKE_UNIT_BIN" ] ||
	fail "the probe did not read the unit's binary path"
[ "$(report_get "$UNIT_REPORT" service_state)" = active ] ||
	fail "the probe did not read ActiveState"
[ "$(report_get "$UNIT_REPORT" service_exists)" = yes ] ||
	fail "the probe did not read LoadState"
say "  ok  the probe reads the unit's data dir, jobs root, listen address, binary and state"

[ "$(report_get "$UNIT_REPORT" token_source)" = unit-env ] ||
	fail "the probe did not take the token from the serving unit's own EnvironmentFile (got $(report_get "$UNIT_REPORT" token_source))"
[ "$(report_get "$UNIT_REPORT" token_file)" = "$FAKE_UNIT_ENV" ] ||
	fail "the probe resolved the wrong token file: $(report_get "$UNIT_REPORT" token_file)"
[ "$(report_get "$UNIT_REPORT" token_candidates)" = 1 ] ||
	fail "the probe saw $(report_get "$UNIT_REPORT" token_candidates) token candidates, want the unit's one"
[ "$(report_get "$UNIT_REPORT" auth)" = ok ] ||
	fail "a good token must authenticate: auth=$(report_get "$UNIT_REPORT" auth)"
[ "$(report_get "$UNIT_REPORT" auth_required)" = no ] ||
	fail "a good token must not leave auth_required=yes"
say "  ok  the token comes from the unit's SECOND EnvironmentFiles line (two Workspace decoys, flat and nested, do not win)"

# D2 mutation: systemd reports only the daemon-settings line, which is what the
# pre-fix parse (head -n 1) effectively kept. The serving workspace's token is
# then invisible, the flat /etc/otter/*.env decoy wins, and the probe must say
# so -- the assertion above is what goes red.
D2_REPORT="$WORK/d2.report"
probe_auth "$D2_REPORT" "$FAKE_UNIT_ENV" "$TOKETC" "" "" drop-shared
[ "$(report_get "$D2_REPORT" token_source)" != unit-env ] ||
	fail "the D2 mutation still resolved the unit's token; the fixture no longer reproduces the defect"
[ "$(report_get "$D2_REPORT" auth)" = unauthorized ] ||
	fail "the D2 mutation should authenticate as the wrong workspace, got auth=$(report_get "$D2_REPORT" auth)"
say "  ok  D2 mutation: dropping the unit's second EnvironmentFiles line loses the token (source=$(report_get "$D2_REPORT" token_source), auth=$(report_get "$D2_REPORT" auth)) -- the assertion above is what catches it"

# Two workspace files and no unit evidence is the ambiguous case. (A flat
# /etc/otter/*.env is a different, older layout rather than a competing
# candidate; when there is no unit evidence the probe prefers it, exactly as the
# CLI's own lookup does, and says so. The case below covers both.)
AMBIG_ETC="$WORK/probe/ambig/etc/otter"
mkdir -p "$AMBIG_ETC/workspaces"
printf 'OTTER_API_TOKEN=OTHERTOKEN\n' >"$AMBIG_ETC/workspaces/one.env"
printf 'OTTER_API_TOKEN=THIRDTOKEN\n' >"$AMBIG_ETC/workspaces/two.env"
AMBIG_REPORT="$WORK/ambiguous.report"
probe_auth "$AMBIG_REPORT" "-" "$AMBIG_ETC"
[ "$(report_get "$AMBIG_REPORT" token_source)" = ambiguous ] ||
	fail "two token files and no unit evidence must be reported ambiguous, got $(report_get "$AMBIG_REPORT" token_source)"
[ "$(report_get "$AMBIG_REPORT" token_file)" = - ] ||
	fail "an ambiguous resolution must not pick a file"
[ "$(report_get "$AMBIG_REPORT" token_candidates)" = 2 ] ||
	fail "the ambiguous report did not count both candidates"
has "$(report_get "$AMBIG_REPORT" token_reason)" "several files hold a token" "the ambiguous report must say why"
say "  ok  two candidate token files are reported ambiguous instead of guessing one"

FLAT_REPORT="$WORK/flat-token.report"
probe_auth "$FLAT_REPORT" "-" "$TOKETC"
[ "$(report_get "$FLAT_REPORT" token_source)" = etc-otter ] ||
	fail "with no unit evidence the probe must use the CLI's own flat lookup, got $(report_get "$FLAT_REPORT" token_source)"
[ "$(report_get "$FLAT_REPORT" token_file)" = "$TOKETC/otter.env" ] ||
	fail "the flat lookup resolved the wrong file: $(report_get "$FLAT_REPORT" token_file)"
say "  ok  with no unit evidence the probe falls back to the CLI's own /etc/otter/*.env lookup, and names the file"

# The operator's DRILL_REMOTE_BIN default is deploy's host dispatcher, which
# refuses `--version` outside a workspace once the host holds two or more
# workspaces. The serving unit names the workspace's own otterd, and the CLI
# beside it is the right one; the probe must find it and say which it used.
cat >"$WORK/bin-dispatcher" <<'DISPATCHER'
#!/bin/sh
echo "otter: more than one workspace on this host; run me inside one" >&2
exit 1
DISPATCHER
chmod +x "$WORK/bin-dispatcher"
WSBIN="$WORK/probe/ws/bin"
mkdir -p "$WSBIN"
cp "$AUTHBIN" "$WSBIN/otter"
: >"$WSBIN/otterd"
chmod +x "$WSBIN/otterd"

saved_unit_bin=$FAKE_UNIT_BIN
FAKE_UNIT_BIN="$WSBIN/otterd"
BIN_REPORT="$WORK/bin-fallback.report"
probe_auth "$BIN_REPORT" "-" "$EMPTY_ETC" "" "" real "$WORK/bin-dispatcher"
FAKE_UNIT_BIN=$saved_unit_bin
[ "$(report_get "$BIN_REPORT" otter_bin_given)" = "$WORK/bin-dispatcher" ] ||
	fail "the probe did not report the CLI it was given"
[ "$(report_get "$BIN_REPORT" otter_bin)" = "$WSBIN/otter" ] ||
	fail "the probe did not fall back to the serving workspace's CLI, got $(report_get "$BIN_REPORT" otter_bin)"
[ "$(report_get "$BIN_REPORT" otter_version)" = "otter v0.0.0-authstub" ] ||
	fail "the fallback CLI's version was not read: $(report_get "$BIN_REPORT" otter_version)"
say "  ok  a dispatcher that refuses --version falls back to the serving workspace's CLI, named in the report"

saved_unit_bin=$FAKE_UNIT_BIN
FAKE_UNIT_BIN="$WORK/probe/ws/bin/otterd"
rm -f "$WSBIN/otter"
probe_auth "$BIN_REPORT" "-" "$EMPTY_ETC" "" "" real "$WORK/bin-dispatcher"
FAKE_UNIT_BIN=$saved_unit_bin
[ "$(report_get "$BIN_REPORT" otter_version)" = missing ] ||
	fail "with no runnable CLI anywhere the probe must report missing, got $(report_get "$BIN_REPORT" otter_version)"
say "  ok  with no runnable CLI anywhere the probe reports missing, so the gate refuses"

WRONG_REPORT="$WORK/wrong-token.report"
probe_auth "$WRONG_REPORT" "-" "$EMPTY_ETC" "" "$TOKETC/workspaces/decoy-9f8e7d6c.env"
[ "$(report_get "$WRONG_REPORT" token_source)" = given-file ] ||
	fail "an env file named by the drill must be used: $(report_get "$WRONG_REPORT" token_source)"
[ "$(report_get "$WRONG_REPORT" token_file)" = "$TOKETC/workspaces/decoy-9f8e7d6c.env" ] ||
	fail "the probe resolved the wrong file: $(report_get "$WRONG_REPORT" token_file)"
[ "$(report_get "$WRONG_REPORT" auth)" = unauthorized ] ||
	fail "a token that does not authenticate must be reported unauthorized, got $(report_get "$WRONG_REPORT" auth)"
say "  ok  a token from the wrong workspace is reported unauthorized, not as a missing job"

NOAUTH_REPORT="$WORK/no-token.report"
probe_auth "$NOAUTH_REPORT" "-" "$EMPTY_ETC"
[ "$(report_get "$NOAUTH_REPORT" token_source)" = none ] ||
	fail "an empty etc directory must resolve no token, got $(report_get "$NOAUTH_REPORT" token_source)"
[ "$(report_get "$NOAUTH_REPORT" token_file)" = - ] ||
	fail "no token file must be reported as -"
[ "$(report_get "$NOAUTH_REPORT" auth)" = token-missing ] ||
	fail "a daemon that wants a token and did not get one must be reported token-missing"
say "  ok  a daemon that requires a token, with none found, is reported token-missing"

EXPLICIT_REPORT="$WORK/explicit-token.report"
probe_auth "$EXPLICIT_REPORT" "-" "$EMPTY_ETC" GOODTOKEN
[ "$(report_get "$EXPLICIT_REPORT" token_source)" = explicit ] ||
	fail "an explicitly passed token must be reported as explicit"
[ "$(report_get "$EXPLICIT_REPORT" auth)" = ok ] ||
	fail "the explicit token must authenticate"
say "  ok  an explicitly passed token authenticates without any file"

# lib/run-otter.sh is what every remote CLI call now goes through. The fake
# transport never lets it run, so exercise it directly: it must hand the token
# from the file to the CLI's environment, and pass the CLI's own arguments on
# unchanged.
cat >"$WORK/otter-echo" <<'ECHO'
#!/bin/sh
echo "api=${OTTER_API_URL:-unset}"
echo "token=${OTTER_API_TOKEN:-unset}"
echo "args=$*"
ECHO
chmod +x "$WORK/otter-echo"
printf 'OTTER_API_TOKEN=GOODTOKEN\n' >"$WORK/token.env"
out=$(sh "$LIB/run-otter.sh" "$WORK/token.env" http://127.0.0.1:7337 "$WORK/otter-echo" inspect selfcheck-job)
has "$out" "token=GOODTOKEN" "run-otter.sh must hand the file's token to the CLI"
has "$out" "api=http://127.0.0.1:7337" "run-otter.sh must set the API URL"
has "$out" "args=inspect selfcheck-job" "run-otter.sh must pass the CLI's arguments through"
out=$(sh "$LIB/run-otter.sh" - http://127.0.0.1:7337 "$WORK/otter-echo" status)
has "$out" "token=unset" "with no token file the CLI must see no token at all"
say "  ok  run-otter.sh reads the token file into the CLI environment and passes arguments through"


# --- 2b. the probe against a LIVE daemon --------------------------------------
#
# Everything above measures directories. That is not enough: a probe that never
# sees a running daemon can pass while its daemon measurement is broken, which
# is how one round's D4 -- `otter status` asked of the wrong binary -- survived
# the whole suite. So this case starts a REAL otterd and probes it.
#
# The binaries are the checkout's own. `make drill` supplies OTTER_BIN; a bare
# selfcheck builds the pair into its work directory, so the case always runs
# rather than skipping.

mkdir -p "$WORK/live/bin" "$WORK/live/jobs"
LIVE_CLI=${OTTER_BIN:-}
LIVE_DAEMON=${OTTERD_BIN:-}
if [ -n "$LIVE_CLI" ] && [ -x "$(dirname "$LIVE_CLI")/otterd" ]; then
	LIVE_DAEMON=${LIVE_DAEMON:-$(dirname "$LIVE_CLI")/otterd}
fi
if [ -z "$LIVE_CLI" ] || [ ! -x "$LIVE_CLI" ]; then
	"$root/scripts/go" build -trimpath -o "$WORK/live/bin/otter" ./cmd/otter ||
		fail "could not build the otter CLI for the live-daemon case"
	LIVE_CLI="$WORK/live/bin/otter"
fi
if [ -z "$LIVE_DAEMON" ] || [ ! -x "$LIVE_DAEMON" ]; then
	"$root/scripts/go" build -trimpath -o "$WORK/live/bin/otterd" ./cmd/otterd ||
		fail "could not build otterd for the live-daemon case"
	LIVE_DAEMON="$WORK/live/bin/otterd"
fi

LIVE_DATA="$WORK/live/data"
LIVE_JOBS="$WORK/live/jobs"
LIVE_PORT=$(( 47000 + ($$ % 700) ))
LIVE_PID=""
i=0
while [ "$i" -lt 8 ]; do
	LIVE_PORT=$((LIVE_PORT + 7))
	i=$((i + 1))
	# A port something already answers on is taken; try the next one.
	if "$LIVE_CLI" --api "http://127.0.0.1:$LIVE_PORT" status >/dev/null 2>&1; then
		continue
	fi
	"$LIVE_DAEMON" -jobs "$LIVE_JOBS" -data "$LIVE_DATA" \
		-listen "127.0.0.1:$LIVE_PORT" -log-format json >"$WORK/live/otterd.log" 2>&1 &
	LIVE_PID=$!
	j=0
	while [ "$j" -lt 100 ]; do
		"$LIVE_CLI" --api "http://127.0.0.1:$LIVE_PORT" status >/dev/null 2>&1 && break
		kill -0 "$LIVE_PID" 2>/dev/null || break
		j=$((j + 1))
		sleep 0.1
	done
	if "$LIVE_CLI" --api "http://127.0.0.1:$LIVE_PORT" status >/dev/null 2>&1; then
		break
	fi
	kill "$LIVE_PID" 2>/dev/null || true
	wait "$LIVE_PID" 2>/dev/null || true
	LIVE_PID=""
done
[ -n "$LIVE_PID" ] || fail "could not start a real otterd for the live-daemon case (see $WORK/live/otterd.log)"

LIVE_REPORT="$WORK/live.report"
sh "$LIB/host-report.sh" source "$LIVE_DATA" "$LIVE_JOBS" "$LIVE_CLI" \
	"http://127.0.0.1:$LIVE_PORT" otter-live "" "" "$WORK/live/etc" >"$LIVE_REPORT" ||
	fail "the probe exited non-zero against a live daemon"
[ "$(report_get "$LIVE_REPORT" daemon)" = running ] ||
	fail "a real probe against a live daemon reported daemon=$(report_get "$LIVE_REPORT" daemon), want running"
LIVE_CLI_VERSION=$("$LIVE_CLI" --version 2>/dev/null || echo missing)
LIVE_DAEMON_VERSION=$(report_get "$LIVE_REPORT" daemon_version)
[ -n "$LIVE_DAEMON_VERSION" ] && [ "$LIVE_DAEMON_VERSION" != "-" ] ||
	fail "a live daemon's version was not reported"
[ "$LIVE_DAEMON_VERSION" = "${LIVE_CLI_VERSION#otter }" ] ||
	fail "the live daemon reports $LIVE_DAEMON_VERSION and its CLI $LIVE_CLI_VERSION: those are the same build"
[ "$(report_get "$LIVE_REPORT" auth)" = ok ] ||
	fail "a daemon with no token must answer with counters: auth=$(report_get "$LIVE_REPORT" auth)"
[ "$(report_get "$LIVE_REPORT" uid)" = "$(id -u)" ] ||
	fail "the probe reported uid=$(report_get "$LIVE_REPORT" uid)"
say "  ok  a real probe against a live otterd reports daemon=running, version $LIVE_DAEMON_VERSION, auth=ok"

# Platform branches the fixtures cannot cover. On Linux (which is what CI runs)
# this exercises /etc/machine-id and GNU stat; on macOS the hostname fallback
# and BSD stat. The assertion is the same either way, so a branch that stops
# working on either platform is caught here.
# Assert the branch this machine is supposed to take, not merely that some
# branch was taken: a Linux-only regression in the /etc/machine-id path would
# otherwise pass unnoticed, because the hostname fallback also produces a
# non-empty id.
LIVE_MI_SOURCE=$(report_get "$LIVE_REPORT" machine_id_source)
case "$(uname -s)" in
Linux)
	if [ -s /etc/machine-id ] || [ -s /var/lib/dbus/machine-id ]; then
		case "$LIVE_MI_SOURCE" in
		/etc/machine-id | /var/lib/dbus/machine-id) ;;
		*)
			fail "this Linux host has a non-empty machine-id file, but the probe reported machine_id_source=$LIVE_MI_SOURCE"
			;;
		esac
	else
		[ "$LIVE_MI_SOURCE" = hostname ] ||
			fail "this Linux host has no non-empty machine-id file, so the probe must fall back to the hostname; it reported $LIVE_MI_SOURCE"
	fi
	;;
*)
	# Anywhere else (macOS, the developer's machine) there is no machine-id
	# file, so the fallback is the correct branch and is asserted as such.
	[ "$LIVE_MI_SOURCE" = hostname ] ||
		fail "this host has no /etc/machine-id, so the probe must fall back to the hostname; it reported $LIVE_MI_SOURCE"
	;;
esac
[ -n "$(report_get "$LIVE_REPORT" machine_id)" ] || fail "the probe reported no machine id"
[ "$(report_get "$LIVE_REPORT" machine_id)" != "-" ] || fail "the probe reported an empty machine id"
[ "$(report_get "$LIVE_REPORT" data_owner)" != "-" ] ||
	fail "the probe could not measure the data directory's owner (stat branch)"
[ "$(report_get "$LIVE_REPORT" data_mode)" != "-" ] ||
	fail "the probe could not measure the data directory's mode (stat branch)"
say "  ok  machine id ($(report_get "$LIVE_REPORT" machine_id_source)), owner and mode are measured on this platform"

# D4: the same live daemon, but with deploy's host dispatcher as the CLI the
# operator named and the unit naming the workspace's own binaries. `otter
# status` must be asked of the resolved CLI: a probe that measures the daemon
# with the dispatcher reports "no daemon answers" for a running runtime, which
# is exactly what a two-workspace host did.
UNIT_BIN_DIR="$WORK/live/unit/bin"
mkdir -p "$UNIT_BIN_DIR"
ln -s "$LIVE_DAEMON" "$UNIT_BIN_DIR/otterd"
ln -s "$LIVE_CLI" "$UNIT_BIN_DIR/otter"
cat >"$WORK/live/dispatcher" <<'DISPATCH'
#!/bin/sh
echo "otter: not inside a workspace, and this host holds several" >&2
exit 1
DISPATCH
chmod +x "$WORK/live/dispatcher"
DISP_REPORT="$WORK/live-dispatcher.report"
env PATH="$WORK/probe/bin:$PATH" \
	FAKE_UNIT_BIN="$UNIT_BIN_DIR/otterd" FAKE_UNIT_DATA="$LIVE_DATA" \
	FAKE_UNIT_JOBS="$LIVE_JOBS" FAKE_UNIT_LISTEN="127.0.0.1:$LIVE_PORT" \
	FAKE_UNIT_ENV="" FAKE_UNIT_DAEMON_ENV="" FAKE_UNIT_SHAPE=real \
	sh "$LIB/host-report.sh" source "$LIVE_DATA" "$LIVE_JOBS" "$WORK/live/dispatcher" \
	"http://127.0.0.1:$LIVE_PORT" otterd-app-1a2b3c4d "" "" "$WORK/live/etc" >"$DISP_REPORT" ||
	fail "the probe failed with a dispatcher as the named CLI"
[ "$(report_get "$DISP_REPORT" otter_bin_given)" = "$WORK/live/dispatcher" ] ||
	fail "the probe did not report the CLI it was given"
[ "$(report_get "$DISP_REPORT" otter_bin)" = "$UNIT_BIN_DIR/otter" ] ||
	fail "the probe did not fall back to the unit's workspace CLI: $(report_get "$DISP_REPORT" otter_bin)"
[ "$(report_get "$DISP_REPORT" daemon)" = running ] ||
	fail "with a dispatcher as the named CLI the probe reported daemon=$(report_get "$DISP_REPORT" daemon): the daemon was measured with the wrong binary (D4)"
[ "$(report_get "$DISP_REPORT" daemon_version)" = "$LIVE_DAEMON_VERSION" ] ||
	fail "the fallback measured daemon version $(report_get "$DISP_REPORT" daemon_version), want $LIVE_DAEMON_VERSION"
say "  ok  a real daemon is measured with the serving workspace's CLI, not the dispatcher that cannot answer (D4)"

kill "$LIVE_PID" 2>/dev/null || true
wait "$LIVE_PID" 2>/dev/null || true
LIVE_PID=""

# --- 3. the preflight gate ----------------------------------------------------

say "3/6 gate (lib/assert-clean-host.sh)"

sh "$LIB/host-report.sh" source "$SBX_DATA" "$SBX_JOBS" "$PROBE_BIN" \
	http://127.0.0.1:7999 otter-selfcheck >"$WORK/source.report" || fail "source probe failed"

out=$(sh "$LIB/assert-clean-host.sh" "$WORK/source.report" "$CLEAN_REPORT" 0 2>&1) && status=0 || status=$?
[ "$status" -ne 0 ] || fail "the gate accepted the same machine as both hosts"
has "$out" "not a second host" "the same-machine refusal must say so"
say "  ok  two reports from one machine are refused as 'not a second host'"

# A dirty target that is otherwise a legal pair must be refused AT the
# cleanliness check. The machine identity is the only substituted field: the
# reports come from the real probe over real directories.
sed -e 's/^machine_id=.*/machine_id=selfcheck-source/' \
	-e 's/^ssh_host_key=.*/ssh_host_key=selfcheck-source-key/' \
	"$WORK/source.report" >"$WORK/source.fake.report"
sed -e 's/^machine_id=.*/machine_id=selfcheck-target/' \
	-e 's/^ssh_host_key=.*/ssh_host_key=selfcheck-target-key/' \
	"$DIRTY_REPORT" >"$WORK/dirty.fake.report"
out=$(sh "$LIB/assert-clean-host.sh" "$WORK/source.fake.report" "$WORK/dirty.fake.report" 0 2>&1) && status=0 || status=$?
[ "$status" -ne 0 ] || fail "the gate accepted a target whose data directory holds otter.db"
has "$out" "is not clean" "the dirty-target refusal must say the target is not clean"
has "$out" "otter.db" "the dirty-target refusal must name the offending entry"
say "  ok  a dirty target is refused at the cleanliness assertion, with the offender named"

# ...and an empty target PASSES that same check, so the gate is not a blanket
# failure. Here it fails later, on this machine's missing systemd unit.
sed -e 's/^machine_id=.*/machine_id=selfcheck-target/' \
	-e 's/^ssh_host_key=.*/ssh_host_key=selfcheck-target-key/' \
	"$CLEAN_REPORT" >"$WORK/clean.fake.report"
out=$(sh "$LIB/assert-clean-host.sh" "$WORK/source.fake.report" "$WORK/clean.fake.report" 0 2>&1) && status=0 || status=$?
has "$out" "clean      target data directory is empty or absent" "an empty target must pass the cleanliness assertion"
hasnot "$out" "is not clean" "an empty target must not be refused as dirty"
say "  ok  an empty target passes the cleanliness assertion (then failed on: $(printf '%s' "$out" | sed -n 's/^drill: FAILED: //p' | head -n 1 | cut -c1-70))"

# A pair of reports that satisfies every condition must pass the whole gate.
# These two are synthesized fixtures: no host can make a developer machine
# report Linux, root, systemd and a live daemon, and the point is that the gate
# has a passing path at all.
cat >"$WORK/good-source.report" <<'REPORT'
role=source
hostname=source.example
machine_id=source-machine
machine_id_source=/etc/machine-id
ssh_host_key=source-host-key
kernel=Linux
release=6.8.0
arch=x86_64
uid=0
otter_bin=/opt/otter/workspaces/app-1a2b3c4d/bin/otter
otter_bin_given=/usr/local/bin/otter
otter_version=otter v0.2.0-47-g2a32ca7
data_dir=/var/lib/otter
data_real=/var/lib/otter
data_exists=yes
data_kind=dir
data_entries=4
data_clean=no
data_reason=not empty
data_owner=otter:otter
data_mode=700
db_present=yes
jobs_dir=/srv/otter/jobs
jobs_real=/srv/otter/jobs
jobs_exists=yes
jobs_kind=dir
jobs_entries=1
jobs_clean=no
jobs_reason=not empty
find=yes
# D1: these two strings are captured from a live otterd, verbatim.
# `otter --version` prints the program name; `otter status` reports the daemon's
# health version without it. The gate must treat them as the same build.
daemon=running
daemon_version=v0.2.0-47-g2a32ca7
daemon_detail=-
auth=ok
auth_required=no
api_url=http://127.0.0.1:7337
token_source=unit-env
token_file=/etc/otter/workspaces/app.env
token_candidates=1
token_reason=-
etc_dir=/etc/otter
service=otter
service_state=active
service_exists=yes
service_bin=/opt/otter/workspaces/app-1a2b3c4d/bin/otterd
unit_data_dir=/var/lib/otter
unit_jobs_dir=/srv/otter/jobs
unit_listen=127.0.0.1:7337
unit_env_files=/etc/otter/workspaces/app.daemon.env /etc/otter/workspaces/app.env 
sqlite3=yes
sha256sum=yes
tar=yes
readlink=yes
timeout=yes
getent=yes
REPORT
sed -e 's/^role=source/role=target/' \
	-e 's/^hostname=source.example/hostname=target.example/' \
	-e 's/^machine_id=source-machine/machine_id=target-machine/' \
	-e 's/^ssh_host_key=source-host-key/ssh_host_key=target-host-key/' \
	-e 's/^data_exists=yes/data_exists=no/' \
	-e 's/^data_clean=no/data_clean=yes/' \
	-e 's/^data_reason=.*/data_reason=-/' \
	-e 's/^data_entries=4/data_entries=0/' \
	-e 's/^data_owner=otter:otter/data_owner=-/' \
	-e 's/^db_present=yes/db_present=no/' \
	-e 's/^jobs_exists=yes/jobs_exists=no/' \
	-e 's/^jobs_clean=no/jobs_clean=yes/' \
	-e 's/^jobs_reason=.*/jobs_reason=-/' \
	-e 's/^jobs_entries=1/jobs_entries=0/' \
	-e 's/^daemon=running/daemon=stopped/' \
	-e 's/^daemon_version=.*/daemon_version=-/' \
	-e 's/^auth=ok/auth=unknown/' \
	-e 's/^auth_required=no/auth_required=unknown/' \
	-e 's/^service_state=active/service_state=inactive/' \
	"$WORK/good-source.report" >"$WORK/good-target.report"
out=$(sh "$LIB/assert-clean-host.sh" "$WORK/good-source.report" "$WORK/good-target.report" 0 2>&1) ||
	fail "the gate refused reports that satisfy every condition:$out"
has "$out" "ok: the target is a clean, empty host" "the passing path must say what it concluded"
has "$out" "token      unit-env" "the passing path must report the token it resolved"
has "$out" "the source daemon and its CLI agree" "the real daemon/CLI version pair must be treated as one build (D1)"
say "      D1: with the raw strings the old comparison failed every pair; here it agrees on v0.2.0-47-g2a32ca7"
say "  ok  a fully good pair passes the gate (fixture reports; the gate's own happy path)"

# R1/R2: each mutation below is one broken measurement away from that good pair,
# and the gate has to name it rather than let the drill fail later with a message
# that points somewhere else.
mutation() {
	mut_name=$1
	mut_source_sed=$2
	mut_target_sed=$3
	if [ -n "$mut_source_sed" ]; then
		sed -e "$mut_source_sed" "$WORK/good-source.report" >"$WORK/mut-source.report"
	else
		cp "$WORK/good-source.report" "$WORK/mut-source.report"
	fi
	if [ -n "$mut_target_sed" ]; then
		sed -e "$mut_target_sed" "$WORK/good-target.report" >"$WORK/mut-target.report"
	else
		cp "$WORK/good-target.report" "$WORK/mut-target.report"
	fi
	out=$(sh "$LIB/assert-clean-host.sh" "$WORK/mut-source.report" "$WORK/mut-target.report" 0 2>&1) && status=0 || status=$?
	[ "$status" -ne 0 ] || fail "the gate passed a mutated pair: $mut_name"
	# Print the refusal, so the recorded transcript shows what each mutation
	# produced rather than only that something did.
	say "      $mut_name -> $(printf '%s\n' "$out" | sed -n 's/^drill: FAILED: //p' | head -n 1 | cut -c1-120)"
}

mutation "unit data dir" 's|^unit_data_dir=.*|unit_data_dir=/var/lib/somewhere-else|' ""
has "$out" "serves data directory /var/lib/somewhere-else" "the unit-data-dir mismatch must name the unit's path"
say "  ok  a unit that serves a different data directory is refused, by name"

mutation "unit jobs dir" 's|^unit_jobs_dir=.*|unit_jobs_dir=/srv/other-jobs|' ""
has "$out" "serves jobs root /srv/other-jobs" "the unit-jobs-dir mismatch must name the unit's path"
say "  ok  a unit that serves a different jobs root is refused, by name"

mutation "unit port" 's|^unit_listen=.*|unit_listen=127.0.0.1:7444|' ""
has "$out" "listens on 127.0.0.1:7444" "the port mismatch must name the unit's address"
say "  ok  a unit that listens on another port is refused, by name"

mutation "unit paths unreadable" 's|^unit_data_dir=.*|unit_data_dir=-|' ""
has "$out" "names no data directory" "an undeterminable unit must say so"
say "  ok  a unit the drill cannot read is refused with instructions, not guessed at"

mutation "daemon and CLI builds differ" 's|^daemon_version=.*|daemon_version=v0.2.0-46-gdeadbee|' ""
has "$out" "different builds" "a genuinely different daemon build must still refuse (D1's check must bite)"
say "  ok  D1 mutation: a daemon on another build is refused, so the comparison was not weakened into agreement"

mutation "ambiguous token" 's|^token_source=.*|token_source=ambiguous|
s|^token_file=.*|token_file=-|
s|^auth=ok|auth=token-missing|
s|^token_reason=.*|token_reason=several files hold a token: /a /b|' ""
has "$out" "several environment files" "the ambiguous token must be refused, naming the candidates"
say "  ok  an ambiguous token on the source is refused before any call"

mutation "wrong token" 's|^auth=ok|auth=unauthorized|' ""
has "$out" "does not authenticate" "a wrong token must be refused as unauthorized"
say "  ok  a token that does not authenticate is refused as the token, not the job"

mutation "no token found" 's|^token_source=.*|token_source=none|
s|^token_file=.*|token_file=-|
s|^auth=ok|auth=token-missing|' ""
has "$out" "requires an API token and none was found" "a missing token must name the lookups"
has "$out" "DRILL_REMOTE_ENV_FILE" "a missing token must name the escape hatch"
say "  ok  a daemon that needs a token and has none is refused with the two overrides named"

mutation "target has no token" "" 's|^token_source=.*|token_source=none|
s|^token_file=.*|token_file=-|'
has "$out" "none was found on the target" "a target that cannot be driven must be refused"
say "  ok  a target with no resolvable token is refused before the restore"

# --- 4. backup and restore halves round-trip locally --------------------------

say "4/6 backup + restore halves (lib/hot-backup.sh, lib/restore-host.sh)"

JOBID=11111111-2222-3333-4444-555555555555
REL_DIGEST=aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888
ENV_DIGEST=9999aaaa8888bbbb7777cccc6666dddd5555eeee4444ffff3333aaaa2222bbbb
SRC="$WORK/roundtrip/src"
RT_DATA="$SRC/data"
RT_JOBS="$SRC/jobs"
mkdir -p "$RT_DATA/.releases/$JOBID/$REL_DIGEST" "$RT_DATA/.releases/active" \
	"$RT_DATA/environments/$ENV_DIGEST/bin" "$RT_DATA/tools/uv" "$RT_DATA/python/3.13/bin" \
	"$RT_DATA/cache/uv" "$RT_DATA/sdk/python/otter" "$RT_JOBS/app"

sqlite3 "$RT_DATA/otter.db" \
	"create table t(k text primary key, v text); insert into t values ('cursor','42'); insert into t values ('note','restored');"
cat >"$RT_DATA/.releases/$JOBID/$REL_DIGEST/otter-release.json" <<JSON
{"job":"$JOBID","digest":"$REL_DIGEST","environment":"$ENV_DIGEST","source":"$RT_JOBS/app"}
JSON
cat >"$RT_DATA/environments/$ENV_DIGEST/otter-ready.json" <<JSON
{"job":"$JOBID","digest":"$ENV_DIGEST","interpreter":"$RT_DATA/environments/$ENV_DIGEST/bin/python"}
JSON
printf '#!/bin/sh\necho "Python 3.13.5"\n' >"$RT_DATA/environments/$ENV_DIGEST/bin/python"
printf '#!/bin/sh\necho "uv selfcheck"\n' >"$RT_DATA/tools/uv/uv"
printf 'python\n' >"$RT_DATA/python/3.13/bin/python"
printf 'wheel\n' >"$RT_DATA/cache/uv/wheel"
printf 'sdk\n' >"$RT_DATA/sdk/python/otter/__init__.py"
printf 'stale wal\n' >"$RT_DATA/otter.db-wal"
printf 'transient\n' >"$RT_DATA/otter.lock"
printf 'name: app\n' >"$RT_JOBS/app/otter.yaml"
printf '%s\n' "$JOBID" >"$RT_JOBS/app/.otter-id"
printf 'print("hi")\n' >"$RT_JOBS/app/main.py"
ln -s "$RT_DATA/.releases/$JOBID/$REL_DIGEST" "$RT_DATA/.releases/active/$JOBID"

BACKUP="$WORK/roundtrip/backup"
sh "$LIB/hot-backup.sh" "$RT_DATA" "$RT_JOBS" "$BACKUP" "" selfcheck-source >"$WORK/hot-backup.out" 2>&1 ||
	fail "hot-backup.sh failed on a well-formed runtime:$(cat "$WORK/hot-backup.out")"

[ -f "$BACKUP/otter.db" ] || fail "the backup has no otter.db"
[ -f "$BACKUP/RECORD" ] || fail "the backup has no RECORD"
[ -f "$BACKUP/MANIFEST.sha256" ] || fail "the backup has no MANIFEST.sha256"
[ -f "$BACKUP/data/.releases/$JOBID/$REL_DIGEST/otter-release.json" ] || fail "the release snapshot was not backed up"
[ -f "$BACKUP/data/environments/$ENV_DIGEST/otter-ready.json" ] || fail "the environment was not backed up"
[ -f "$BACKUP/data/tools/uv/uv" ] || fail "tools/ was not backed up"
[ -f "$BACKUP/data/python/3.13/bin/python" ] || fail "python/ was not backed up"
[ -f "$BACKUP/data/cache/uv/wheel" ] || fail "cache/ was not backed up"
[ -f "$BACKUP/jobs/app/.otter-id" ] || fail "the job sources (with .otter-id) were not backed up"
[ ! -e "$BACKUP/data/sdk" ] || fail "the derived sdk/ tree was backed up"
[ ! -e "$BACKUP/data/otter.db-wal" ] || fail "a stale WAL was backed up"
[ ! -e "$BACKUP/data/otter.lock" ] || fail "the lock file was backed up"
grep -q 'otter-id' "$BACKUP/MANIFEST.sha256" || fail "the identity marker is not in the manifest"
if grep -q 'MANIFEST.sha256' "$BACKUP/MANIFEST.sha256"; then
	fail "the manifest lists itself; it would fail its own verification"
fi
grep -q 'active' "$BACKUP/SYMLINKS.txt" || fail "the active release symlink is not recorded"
grep -q "data_dir=$RT_DATA" "$BACKUP/RECORD" || fail "RECORD does not carry the data directory"
grep -q "jobs_dir=$RT_JOBS" "$BACKUP/RECORD" || fail "RECORD does not carry the jobs root"
say "  ok  a complete backup holds the database, releases, environments, tools, python, cache and job sources"

# The archive's SHAPE is an interface between lib/hot-backup.sh and the mode's
# assertions, and nothing tested it. The first real HW-7 run looked for the job
# marker at <archive>/<job>/.otter-id instead of <archive>/jobs/<job>/.otter-id
# and failed after a clean backup and transfer -- a path bug that four
# verification rounds could not see, because no case ever reached that line.
# So the mode reads every member through backup_path(), and this case extracts
# those paths from the mode's own source and asserts each one resolves inside an
# archive that hot-backup.sh really produced (the one above, from a real data
# directory and jobs root).
LAYOUT_TEMPLATES=$(sed -n 's/.*backup_path "\([^"]*\)".*/\1/p' "$CLEAN_HOST" | sort -u)
[ -n "$LAYOUT_TEMPLATES" ] ||
	fail "the mode no longer reads the archive through backup_path, so the layout contract is untested"
# A raw member path is the shape of the HW-7 defect: "$WORK/backup/<member>".
# The helper is the only place allowed to name the expansion root, and it names
# it without a trailing slash, so any occurrence of the former is a read that
# bypassed the helper -- and a read this case cannot see.
ROOT_REFS=$(grep -c '[$]WORK/backup/' "$CLEAN_HOST" || true)
[ "$ROOT_REFS" = 0 ] ||
	fail "the mode builds $ROOT_REFS archive path(s) as \$WORK/backup/<member>; every member read must go through backup_path so this case can check the layout"
for tmpl in $LAYOUT_TEMPLATES; do
	rel=$(printf '%s' "$tmpl" | sed -e 's/[$]JOB_REL/app/g' -e "s/[$]SRC_JOBID/$JOBID/g" \
		-e "s/[$]REL_DIGEST/$REL_DIGEST/g" -e "s/[$]ENV_DIGEST/$ENV_DIGEST/g")
	case "$rel" in
	*'$'*)
		fail "the layout case cannot resolve the mode's archive path '$tmpl'; teach it the new placeholder"
		;;
	esac
	[ -e "$BACKUP/$rel" ] ||
		fail "the mode reads $rel from the archive, but hot-backup.sh did not put it there"
done
say "  ok  every archive path the mode reads exists in a real archive ($(printf '%s' "$LAYOUT_TEMPLATES" | tr '\n' ' '))"

# A corrupted archive must be refused before anything is written.
printf 'corrupt\n' >>"$BACKUP/data/.releases/$JOBID/$REL_DIGEST/otter-release.json"
out=$(sh "$LIB/restore-host.sh" "$BACKUP" "$RT_DATA" "$RT_JOBS" - 2>&1) && status=0 || status=$?
[ "$status" -ne 0 ] || fail "restore-host.sh restored a backup that does not match its manifest"
has "$out" "does not match its manifest" "the corruption refusal must say the manifest caught it"
[ -f "$RT_DATA/.releases/$JOBID/$REL_DIGEST/otter-release.json" ] ||
	fail "the corruption refusal removed the source tree it refused to overwrite"
say "  ok  a corrupted archive is refused by MANIFEST.sha256 before anything is written"

rm -rf "$BACKUP"
sh "$LIB/hot-backup.sh" "$RT_DATA" "$RT_JOBS" "$BACKUP" "" selfcheck-source >/dev/null 2>&1 ||
	fail "re-taking the clean backup failed"

SAB="$WORK/roundtrip/backup-sabotage"
sh "$LIB/hot-backup.sh" "$RT_DATA" "$RT_JOBS" "$SAB" drop-jobs selfcheck-source >"$WORK/hot-backup-sabotage.out" 2>&1 ||
	fail "hot-backup.sh drop-jobs failed:$(cat "$WORK/hot-backup-sabotage.out")"
[ ! -e "$SAB/jobs/app/.otter-id" ] || fail "the drop-jobs sabotage still produced a .otter-id in the archive"
if [ -e "$SAB/jobs" ] && [ -n "$(find "$SAB/jobs" -type f 2>/dev/null)" ]; then
	fail "the drop-jobs sabotage still produced job files"
fi
say "  ok  the drop-jobs sabotage leaves no job source and no .otter-id to restore"

# Restoring the sabotaged archive must leave a jobs root with no identity: this
# is the red condition the clean-host identity assertion fires on.
mv "$SRC" "$WORK/roundtrip/src-aside"
sh "$LIB/restore-host.sh" "$SAB" "$RT_DATA" "$RT_JOBS" - >/dev/null 2>&1 ||
	fail "restoring the sabotaged archive failed"
MARKERS=$(find "$RT_JOBS" -name .otter-id -type f 2>/dev/null | wc -l | tr -d ' ')
[ "$MARKERS" = 0 ] || fail "the sabotage restore produced $MARKERS .otter-id markers, want 0"
rm -rf "$SRC"
mv "$WORK/roundtrip/src-aside" "$SRC"
say "  ok  restoring the sabotage yields a jobs root with no identity marker (the identity assertion's red condition)"

# The clean backup must survive the whole round trip.
mv "$SRC" "$WORK/roundtrip/src-aside"
sh "$LIB/restore-host.sh" "$BACKUP" "$RT_DATA" "$RT_JOBS" - >"$WORK/restore.out" 2>&1 ||
	fail "restore-host.sh failed on the clean backup:$(cat "$WORK/restore.out")"
mv "$WORK/roundtrip/src-aside" "$WORK/roundtrip/src-original"

[ -f "$RT_DATA/.releases/$JOBID/$REL_DIGEST/otter-release.json" ] || fail "the restored release snapshot is missing"
[ -f "$RT_DATA/environments/$ENV_DIGEST/otter-ready.json" ] || fail "the restored environment is missing"
[ -f "$RT_DATA/tools/uv/uv" ] || fail "the restored tools/ tree is missing"
[ -f "$RT_JOBS/app/main.py" ] || fail "the restored job source is missing"
[ "$(cat "$RT_JOBS/app/.otter-id")" = "$JOBID" ] || fail "the restored identity marker does not match"
[ "$(sqlite3 "$RT_DATA/otter.db" "select v from t where k='cursor';")" = 42 ] ||
	fail "the restored database does not hold the backed-up rows"
RESOLVED=$(readlink -f "$RT_DATA/.releases/active/$JOBID")
EXPECTED_LINK=$(readlink -f "$RT_DATA/.releases/$JOBID/$REL_DIGEST")
[ "$RESOLVED" = "$EXPECTED_LINK" ] || fail "the restored active release link resolves to $RESOLVED"
grep -q "verified" "$WORK/restore.out" || fail "the restore did not report a manifest verification"
[ ! -e "$RT_DATA/sdk" ] || fail "the restore created the derived sdk/ tree"
[ ! -e "$RT_DATA/otter.db-wal" ] || fail "the restore left a WAL beside the database"
say "  ok  the clean backup round-trips: identity, releases, environments, sources and database rows"

out=$(sh "$LIB/restore-host.sh" "$BACKUP" "$RT_DATA" "$RT_JOBS" - 2>&1) && status=0 || status=$?
[ "$status" -ne 0 ] || fail "restore-host.sh restored into a non-empty target"
has "$out" "refusing to restore into non-empty" "the non-empty-target refusal must say so"
say "  ok  a restore into a non-empty target refuses"

# --- 5. the drill goes red on a dirty target, without a host ------------------

# The transport is a test double; the reports it returns are the real probe's
# measurement of real directories, with exactly these fields substituted,
# because a selfcheck cannot be two machines and cannot be Linux+systemd+root:
#
#   machine_id, ssh_host_key, hostname    -> the two "hosts"
#   data_dir/data_real/jobs_dir/jobs_real -> rewritten from the per-host sandbox
#                                            back to the logical path
#
# Everything else is measured by lib/host-report.sh against a real tree --
# including `daemon` and `daemon_version`, which the double used to overwrite
# and no longer does. That is why the source in these runs reports `stopped`:
# there is no daemon in the sandbox, and the honest measurement is what makes
# the run stop at the gate's checks instead of sailing through them.
say "5/6 the drill refuses a dirty target before writing anything"

FAKE="$WORK/fake-ssh"
cat >"$FAKE" <<'FAKE_SSH'
#!/bin/sh
# Test double for ssh: it answers the preflight probe from a per-host sandbox
# and refuses every other step, so a bug that skipped the gate shows up as a
# call to a step that must never have run.
#
# Two things it deliberately does NOT do, because doing them hid real defects:
#   * it does not overwrite the probe's `daemon`/`daemon_version` fields. Those
#     come from whatever the probe actually measured;
#   * it does not re-split the command line by hand. It parses it exactly as the
#     remote shell would (`eval "set -- ..."`), so an argument the drill failed
#     to quote disappears here just as it does over ssh. D5 -- empty arguments
#     vanishing out of `sh -s -- $*` -- was invisible while the double split the
#     string itself.
set -eu
prev=""
last=""
for arg in "$@"; do
	prev=$last
	last=$arg
done
host=$prev
cmd=$last
printf 'CMD %s\n' "$cmd" >>"$FAKE_LOG"
stdin=$(mktemp)
cat >"$stdin"

if [ "$host" = "$FAKE_SRC_HOST" ]; then
	data=$FAKE_SRC_DATA
	jobs=$FAKE_SRC_JOBS
	machine=fake-source-machine
	hostkey=fake-source-hostkey
	hostname=source.drill.invalid
elif [ "$host" = "$FAKE_TGT_HOST" ]; then
	data=$FAKE_TGT_DATA
	jobs=$FAKE_TGT_JOBS
	machine=fake-target-machine
	hostkey=fake-target-hostkey
	hostname=target.drill.invalid
else
	echo "fake-ssh: unknown host $host" >&2
	rm -f "$stdin"
	exit 96
fi

case "$cmd" in
*"sh -s --"*)
	# Only the preflight probe is answered. The drill also pipes other scripts
	# over ssh (lib/run-otter.sh, lib/hot-backup.sh, lib/restore-host.sh);
	# refusing those is what makes "the gate refused a dirty target before
	# touching it" checkable, so the probe is identified by the script actually
	# on stdin rather than by the shape of the command line.
	if ! head -n 3 "$stdin" 2>/dev/null | grep -q 'host-report.sh'; then
		echo "fake-ssh: not the preflight probe; this selfcheck refuses: $(head -n 1 "$stdin" 2>/dev/null)" >&2
		rm -f "$stdin"
		exit 97
	fi
	# Parse the remote command line the way the remote shell would, so empty
	# arguments survive. Then log exactly what the remote program received.
	args=$(printf '%s' "$cmd" | sed -e 's/^ *//' -e 's/^sudo -n env //' -e 's/^sudo -n //' -e 's/^sh -s --//')
	eval "set -- $args"
	printf 'ARGC %s' "$#" >>"$FAKE_LOG"
	for a in "$@"; do printf ' [%s]' "$a" >>"$FAKE_LOG"; done
	printf '\n' >>"$FAKE_LOG"
	translated=""
	for a in "$@"; do
		case "$a" in
		"$FAKE_LOGICAL_DATA") a=$data ;;
		"$FAKE_LOGICAL_JOBS") a=$jobs ;;
		esac
		case "$a" in
		*"'"*) a=$(printf '%s' "$a" | sed "s/'/'\\''/g") ;;
		esac
		translated="$translated '$a'"
	done
	probe_out=$(sh -c "sh -s --$translated" <"$stdin")
	printf '%s' "$probe_out" | sed \
		-e "s|^data_dir=.*|data_dir=$FAKE_LOGICAL_DATA|" \
		-e "s|^data_real=.*|data_real=$FAKE_LOGICAL_DATA|" \
		-e "s|^jobs_dir=.*|jobs_dir=$FAKE_LOGICAL_JOBS|" \
		-e "s|^jobs_real=.*|jobs_real=$FAKE_LOGICAL_JOBS|" \
		-e "s|^machine_id=.*|machine_id=$machine|" \
		-e "s|^ssh_host_key=.*|ssh_host_key=$hostkey|" \
		-e "s|^hostname=.*|hostname=$hostname|"
	;;
*)
	echo "fake-ssh: the selfcheck only answers the preflight probe; got: $cmd" >&2
	rm -f "$stdin"
	exit 97
	;;
esac
rm -f "$stdin"
FAKE_SSH
chmod +x "$FAKE"

FAKE_SRC="$WORK/fake/source"
FAKE_TGT="$WORK/fake/target"
mkdir -p "$FAKE_SRC/data" "$FAKE_SRC/jobs" "$FAKE_TGT/data" "$FAKE_TGT/jobs"
# The source is a runtime: it has a database.
: >"$FAKE_SRC/data/otter.db"
# The target is dirty: a previous runtime left its database behind.
: >"$FAKE_TGT/data/otter.db"

fake_run() {
	FAKE_LOG=$1
	FAKE_SUDO=${2:-0}
	out=$(env $CLEAR DRILL_TRANSPORT="$FAKE" FAKE_LOG="$FAKE_LOG" \
		DRILL_REMOTE_SUDO="$FAKE_SUDO" \
		FAKE_SRC_HOST=root@source.drill.invalid FAKE_TGT_HOST=root@target.drill.invalid \
		FAKE_SRC_DATA="$FAKE_SRC/data" FAKE_SRC_JOBS="$FAKE_SRC/jobs" \
		FAKE_TGT_DATA="$FAKE_TGT/data" FAKE_TGT_JOBS="$FAKE_TGT/jobs" \
		FAKE_LOGICAL_DATA="$LOGICAL_DATA" FAKE_LOGICAL_JOBS="$LOGICAL_JOBS" \
		DRILL_CLEAN_HOST=1 \
		DRILL_SOURCE_HOST=root@source.drill.invalid \
		DRILL_TARGET_HOST=root@target.drill.invalid \
		DRILL_SSH_KEY="$KEY" \
		DRILL_REMOTE_DIR="$LOGICAL_DATA" \
		DRILL_REMOTE_JOBS_DIR="$LOGICAL_JOBS" \
		DRILL_REMOTE_SERVICE=otter-selfcheck \
		DRILL_REMOTE_BIN="$PROBE_BIN" \
		DRILL_REMOTE_API_URL=http://127.0.0.1:7999 \
		DRILL_JOB=selfcheck-job \
		sh "$CLEAN_HOST" 2>&1) && status=0 || status=$?
}

fake_run "$WORK/fake-dirty.log"
[ "$status" -ne 0 ] || fail "the drill accepted a dirty target"
has "$out" "is not clean" "the drill must go red at the cleanliness assertion"
has "$out" "otter.db" "the red run must name the offending entry"
has "$out" "TRANSPORT OVERRIDDEN" "the substituted-transport run must announce itself"
hasnot "$out" "the target is a clean, empty host" "the drill printed the clean-host conclusion while refusing a dirty target"
CALLS=$(grep -c '^CMD ' "$WORK/fake-dirty.log" || true)
[ "$CALLS" = 2 ] ||
	fail "the dirty-target run made $CALLS remote calls, want only the two probes; it touched the target"
case "$(cat "$WORK/fake-dirty.log")" in
*hot-backup* | *restore* | *install\ -d*)
	fail "the dirty-target run reached a restore step:$(cat "$WORK/fake-dirty.log")"
	;;
esac
DIRTY_REASON=$(printf '%s\n' "$out" | sed -n 's/^drill: FAILED: //p' | head -n 1 | cut -c1-120)
[ -n "$DIRTY_REASON" ] || fail "the dirty-target run did not report why it stopped"
say "  ok  a dirty target goes red at the cleanliness assertion after exactly the two probes"
say "      red reason: $DIRTY_REASON"

# D5: this run is the DOCUMENTED invocation -- no DRILL_REMOTE_API_TOKEN, no
# DRILL_REMOTE_ENV_FILE -- so two of the probe's nine parameters are empty and
# sit in the middle of the list. `sh -s -- $*` dropped them, and `/etc/otter`
# landed in the token slot. The transport double parses its command line the way
# the remote shell does, so a reintroduced bug shows up right here.
PROBE_ARGS=$(grep '^ARGC ' "$WORK/fake-dirty.log" | head -n 1)
[ "$(printf '%s' "$PROBE_ARGS" | awk '{print $2}')" = 9 ] ||
	fail "the probe was called with $(printf '%s' "$PROBE_ARGS" | awk '{print $2}') parameters, want 9: $PROBE_ARGS"
case "$PROBE_ARGS" in
*"[source]"*"[]"*"[]"*"[/etc/otter]"*)
	say "  ok  the no-token invocation passes all nine probe parameters, the two empty ones included"
	;;
*)
	fail "the probe's empty parameters did not survive the remote shell: $PROBE_ARGS"
	;;
esac

# And the same quoting carries hot-backup.sh's empty sabotage, which is what
# makes the clean path runnable at all: the library requires five parameters.
BACKUP_ARGS=$(sh -c '. "'"$LIB"'/remote-args.sh"; remote_args /d /j /stage "" abcdef123456')
eval "set -- $BACKUP_ARGS"
[ "$#" = 5 ] || fail "remote_args turned the five hot-backup parameters into $#"
[ -z "$4" ] || fail "remote_args did not preserve the empty sabotage parameter"
[ "$5" = abcdef123456 ] || fail "remote_args shifted the parameters after the empty one"
say "  ok  remote_args preserves hot-backup.sh's empty sabotage: five parameters, the fourth empty"
out=$(sh -c '. "'"$LIB"'/remote-args.sh"; remote_args "a b" "" "c'"'"'d"')
eval "set -- $out"
[ "$#" = 3 ] && [ "$1" = "a b" ] && [ -z "$2" ] && [ "$3" = "c'd" ] ||
	fail "remote_args did not round-trip spaces and a quote: $out"
say "  ok  remote_args round-trips a space and a single quote through a real shell parse"

# The same drill with an empty target passes the gate: the refusal above is
# about the dirt, not about running at all. It then fails on the next unmet
# precondition, which on this machine is the missing systemd unit -- the point
# is that it got past the cleanliness checks.
rm -f "$FAKE_TGT/data/otter.db"
fake_run "$WORK/fake-clean.log"
has "$out" "clean      target data directory is empty or absent" "an empty target must pass the drill's cleanliness gate"
has "$out" "clean      target jobs root is empty or absent" "an empty jobs root must pass the drill's cleanliness gate"
hasnot "$out" "is not clean" "the drill called an empty target dirty"
[ "$status" -ne 0 ] || fail "the drill 'succeeded' against a test double that cannot run a restore"
NEXT=$(printf '%s\n' "$out" | sed -n 's/^drill: FAILED: //p' | head -n 1 | cut -c1-70)
[ -n "$NEXT" ] || fail "the empty-target run did not report why it stopped"
case "$NEXT" in
*clean* | *empty*)
	fail "the empty-target run stopped at the cleanliness gate: $NEXT"
	;;
esac
say "  ok  an empty target passes the gate; the run then stops on the next precondition ($NEXT)"

# R8: with DRILL_REMOTE_SUDO=1 every remote command has to run as
# `sudo -n env ...`. Bare `VAR=value` after sudo is only accepted when the
# sudoers rule grants SETENV, and on a rule that does not, the failure is
# "command not found: OTTER_API_URL=..." from the target. `env` needs no grant.
: >"$FAKE_TGT/data/otter.db"
fake_run "$WORK/fake-sudo.log" 1
has "$out" "sudo=1" "the run must report the privilege mode it was given"
first_cmd=$(sed -n 's/^CMD //p' "$WORK/fake-sudo.log" | head -n 1)
case "$first_cmd" in
"sudo -n env sh -s -- "*)
	say "  ok  DRILL_REMOTE_SUDO=1 prefixes remote commands with 'sudo -n env' (the first call: $first_cmd)"
	;;
*)
	fail "with DRILL_REMOTE_SUDO=1 the first remote command was '$first_cmd', want it to start with 'sudo -n env'"
	;;
esac
CALLS=$(grep -c '^CMD ' "$WORK/fake-sudo.log" || true)
[ "$CALLS" = 2 ] ||
	fail "the sudo run made $CALLS remote calls, want the two probes before the dirty target was refused"

say "ok: the clean-host mode validates, refuses, gates, backs up and restores without a host"
