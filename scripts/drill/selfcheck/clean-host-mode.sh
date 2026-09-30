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
#      same drill passes that gate when the target is empty. That run uses a
#      test double for the ssh transport only: the reports it consumes are the
#      real probe's measurement of real directories. The substituted fields are
#      listed in section 5, and the run says in its own output that it is not
#      host evidence.
#
# What it CANNOT prove, and does not claim: that a restore onto a second host
# works. That needs two hosts, and it is what the drill exists for.
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
-u DRILL_REMOTE_SUDO -u DRILL_REMOTE_API_TOKEN -u DRILL_TRANSPORT \
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
otter_bin=/usr/local/bin/otter
otter_version=otter v0.0.0-selfcheck
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
daemon=running
daemon_detail=-
api_url=http://127.0.0.1:7337
service=otter
service_state=active
service_exists=yes
sqlite3=yes
sha256sum=yes
tar=yes
readlink=yes
timeout=yes
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
	-e 's/^service_state=active/service_state=inactive/' \
	"$WORK/good-source.report" >"$WORK/good-target.report"
out=$(sh "$LIB/assert-clean-host.sh" "$WORK/good-source.report" "$WORK/good-target.report" 0 2>&1) ||
	fail "the gate refused reports that satisfy every condition:$out"
has "$out" "ok: the target is a clean, empty host" "the passing path must say what it concluded"
say "  ok  a fully good pair passes the gate (fixture reports; the gate's own happy path)"

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
# measurement of real directories, with these fields substituted because a
# selfcheck cannot be two machines and cannot be Linux+systemd+root:
#
#   machine_id, ssh_host_key, hostname    -> the two "hosts"
#   daemon                                -> the source answers, the target does not
#   data_dir/data_real/jobs_dir/jobs_real -> rewritten from the per-host sandbox
#                                            back to the logical path
#
# Everything else -- data_clean, jobs_clean, the reasons, kernel, versions,
# tooling, ownership -- is measured by lib/host-report.sh against a real tree.
say "5/6 the drill refuses a dirty target before writing anything"

FAKE="$WORK/fake-ssh"
cat >"$FAKE" <<'FAKE_SSH'
#!/bin/sh
# Test double for ssh: answers the preflight probe from a per-host sandbox and
# refuses everything else, so a bug that skipped the gate shows up as a call to
# a step that should never have run.
set -eu
prev=""
last=""
for arg in "$@"; do
	prev=$last
	last=$arg
done
host=$prev
cmd=$last
printf '%s\n' "$cmd" >>"$FAKE_LOG"
stdin=$(mktemp)
cat >"$stdin"

if [ "$host" = "$FAKE_SRC_HOST" ]; then
	data=$FAKE_SRC_DATA
	jobs=$FAKE_SRC_JOBS
	machine=fake-source-machine
	hostkey=fake-source-hostkey
	hostname=source.drill.invalid
	daemon=running
elif [ "$host" = "$FAKE_TGT_HOST" ]; then
	data=$FAKE_TGT_DATA
	jobs=$FAKE_TGT_JOBS
	machine=fake-target-machine
	hostkey=fake-target-hostkey
	hostname=target.drill.invalid
	daemon=stopped
else
	echo "fake-ssh: unknown host $host" >&2
	rm -f "$stdin"
	exit 96
fi

case "$cmd" in
*"sh -s --"*)
	args=$(printf '%s' "$cmd" | sed -e 's/^ *//' -e 's/^sudo -n //' -e 's/^sh -s -- //')
	translated=""
	for a in $args; do
		case "$a" in
		"$FAKE_LOGICAL_DATA") a=$data ;;
		"$FAKE_LOGICAL_JOBS") a=$jobs ;;
		esac
		translated="$translated $a"
	done
	probe_out=$(sh -s -- $translated <"$stdin")
	# A running daemon on the source runs the same build as its CLI, which is
	# what the real probe would report.
	cli_version=$(printf '%s' "$probe_out" | sed -n 's/^otter_version=//p')
	if [ "$daemon" = running ]; then daemon_version=$cli_version; else daemon_version=-; fi
	printf '%s' "$probe_out" | sed \
		-e "s|^data_dir=.*|data_dir=$FAKE_LOGICAL_DATA|" \
		-e "s|^data_real=.*|data_real=$FAKE_LOGICAL_DATA|" \
		-e "s|^jobs_dir=.*|jobs_dir=$FAKE_LOGICAL_JOBS|" \
		-e "s|^jobs_real=.*|jobs_real=$FAKE_LOGICAL_JOBS|" \
		-e "s|^machine_id=.*|machine_id=$machine|" \
		-e "s|^ssh_host_key=.*|ssh_host_key=$hostkey|" \
		-e "s|^hostname=.*|hostname=$hostname|" \
		-e "s|^daemon=.*|daemon=$daemon|" \
		-e "s|^daemon_version=.*|daemon_version=$daemon_version|"
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
	out=$(env $CLEAR DRILL_TRANSPORT="$FAKE" FAKE_LOG="$FAKE_LOG" \
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
CALLS=$(wc -l <"$WORK/fake-dirty.log" | tr -d ' ')
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

say "ok: the clean-host mode validates, refuses, gates, backs up and restores without a host"
