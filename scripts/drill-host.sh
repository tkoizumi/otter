#!/bin/sh
# drill-host.sh -- run one drill on a REAL host, inside a maintenance window an
# operator has declared on that host, and record the transcript.
#
# scripts/drill.sh runs drills that build their own world on this machine. Some
# drills cannot: change a systemd unit, restart the daemon, take a backup of a
# live runtime. Those are done to a machine somebody depends on, inside a window
# that has been announced, and the operator holding that window must be able to
# say afterwards exactly what was run, what it asserted, and what was put back.
# This is that wrapper -- the host counterpart of scripts/drill.sh, which
# established the shape: a header naming date/host/checkout, assert your own
# outcome, exit non-zero with the failing output, never skip quietly.
#
#   scripts/drill-host.sh <drill-name> --host <ssh-target> --window <name>
#   scripts/drill-host.sh <drill-name> --host <target> --window <name> --dry-run
#   scripts/drill-host.sh --list
#   scripts/drill-host.sh --help
#
# The stages, in order, and each one recorded:
#
#   precondition  the host answers ssh, the named drill exists locally, and the
#                 window lock exists on the host, is readable, names an operator,
#                 declares an unexpired `expires=<epoch>`, and (when the lock
#                 names itself) names the window the operator asked for. Every
#                 one of those is checked BEFORE any drill command is sent, so a
#                 refusal cannot have changed anything. --dry-run stops here,
#                 prints the plan and changes nothing.
#   run           the drill file is streamed to the host and run there as
#                 `sh -s -- run`. Output is streamed to the operator and to the
#                 transcript as it arrives.
#   assert        the drill's exit status IS the assertion. Non-zero fails the
#                 run; the transcript is kept either way.
#   restore       if the drill declares a restore step (`# drill-host-restore:
#                 <what it puts back>` in its header), it is run on the host as
#                 `sh -s -- restore` -- after a failure and after a pass, both.
#                 If it declares none, the transcript says so instead of
#                 pretending something was restored.
#
# WINDOW LOCK. The operator declares a window by creating a lock file ON THE
# HOST. Its format is line-oriented `key=value`, blank lines and `#` comments
# ignored:
#
#   window=maintenance          # optional; when --window is a NAME this must match
#   expires=1767225600          # required; seconds since the epoch, host clock
#   operator=alice@laptop       # required; who holds the window
#
# For example:
#
#   ssh root@203.0.113.7 "printf 'window=%s\nexpires=%s\noperator=%s\n' \
#       maintenance \$((\$(date +%s) + 3600)) alice@laptop \
#       > /var/lock/otter-window-maintenance"
#
# and the lock is removed when the window closes. The harness refuses -- before
# any drill command, with the transcript recording why -- when the lock is
# absent, unreadable, malformed (no expires= / no operator=), expired, or names
# a different window. `--lock-dir` (or DRILL_HOST_LOCK_DIR) moves the directory,
# which is how the offline suite avoids /var/lock, and `--window` also accepts
# an absolute path when a host keeps its locks somewhere else.
#
# THE DRILL CONTRACT (scripts/drill/host/<name>.sh, run on the host):
#
#   * it takes one argument, `run` or `restore`, and must handle both. The two
#     are SEPARATE ssh sessions, so anything they share has to live on the host
#     (a file, a systemd state), not in a shell variable;
#   * it declares its restore step with a header line
#     `# drill-host-restore: <what it puts back>`; without that line the harness
#     records that this drill declares no restore;
#   * it takes no other arguments: a host drill knows its host, and anything
#     site-specific comes from the host's own environment or from
#     DRILL_HOST_* variables that the operator exports into the drill itself;
#   * its exit status is the assertion: 0 means the behavior held, anything else
#     means it did not, and the harness exits 1 having kept the transcript;
#   * it must be safe to run `restore` after a `run` that failed half-way.
#
# TRANSCRIPT. Written to --transcript (default
# docs/evidence/host-windows/<date>-<window>-<drill>.txt) and its path printed at
# the start and at the end of every run. Header: the same fields scripts/drill.sh
# writes (date, host, checkout) plus the target, the window, the lock path, its
# expiry, the operator, the drill file and the transcript path; then the drill's
# own output, the assertion, the restore step and a final `result:` line. The
# header -- and any refusal after it -- is written even when the harness fails
# early: a run with no record is not evidence.
#
# EXIT. 0 passed; 1 the drill's assertion failed; 2 refused (usage, precondition,
# window); 3 the restore step failed, so the host is in an unknown state.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

usage() {
	cat <<'EOF'
usage: scripts/drill-host.sh <drill-name> --host <ssh-target> --window <name|path> [options]
       scripts/drill-host.sh --list
       scripts/drill-host.sh --help

Run one drill on a real host inside a declared maintenance window, and record
the transcript. Stages: precondition -> run -> assert -> restore.

options:
  --host <ssh-target>   ssh destination, e.g. root@203.0.113.7 ($OTTER_HOST)
  --window <name|path>  the declared window. A bare name <n> is the lock
                        <lock-dir>/otter-window-<n> on the host; a value that
                        starts with '/' is used as the lock path itself.
  --lock-dir <dir>      where named locks live on the host
                        ($DRILL_HOST_LOCK_DIR, default /var/lock)
  --drill-dir <dir>     where host drills are read from
                        ($DRILL_HOST_DRILL_DIR, default scripts/drill/host)
  --transcript <path>   where to write the transcript ($DRILL_HOST_TRANSCRIPT,
                        default docs/evidence/host-windows/
                        <date>-<window>-<drill>.txt)
  --ssh <cmd>           the local ssh command line ($DRILL_HOST_SSH, default
                        `ssh`); it is word-split, so `DRILL_HOST_SSH='ssh -i
                        ~/.ssh/key'` works
  --dry-run             check every precondition, print the plan, change nothing
  --restore-only        run only the drill's restore step, and do not apply the
                        window gate to it: putting a host back is not the thing
                        a window gates
  --list                list the host drills; needs no host
  --help                this text; needs no host

window lock, on the host, line-oriented key=value ('#' comments, blanks ignored):
  window=<name>         optional; must match --window when that was a name
  expires=<epoch>       required; seconds since the epoch, judged on the host
  operator=<identity>   required; who holds the window

drill contract (scripts/drill/host/<name>.sh, executed ON the host):
  * takes one argument, 'run' or 'restore'; they are separate ssh sessions, so
    state shared between them must live on the host;
  * declares its restore step with a header line
    '# drill-host-restore: <what it puts back>';
  * takes no other arguments, and its exit status is the assertion: non-zero
    fails the run (the transcript is kept either way);
  * $DRILL_HOST_SUDO=1 runs every remote command as `sudo -n`.

transcript: the header scripts/drill.sh writes (date, host, checkout) plus the
target, window, lock path, expiry and operator; then the drill's output, the
assertion, the restore step and a result line. It is written even when the
harness refuses before running anything.

exit: 0 passed; 1 the drill's assertion failed; 2 refused (usage, precondition
or window); 3 the restore step failed, so the host is in an unknown state.
EOF
}

die_usage() {
	echo "drill-host: $*" >&2
	echo "drill-host: run scripts/drill-host.sh --help for the interface" >&2
	exit 2
}

# list_drills prints the host drill names, one per line, and fails when there are
# none -- the same contract scripts/drill.sh --list has, because a silent empty
# list is how a window gets recorded for a drill that no longer exists.
list_drills() {
	found=0
	for candidate in "$DRILL_DIR"/*.sh; do
		[ -f "$candidate" ] || continue
		found=1
		basename "$candidate" .sh
	done
	[ "$found" -eq 1 ]
}

# --- arguments ---------------------------------------------------------------
#
# Everything is parsed before anything is written or run: a mistyped option must
# not leave a lock, a transcript or a remote command behind.

DRILL=
HOST=${OTTER_HOST:-}
WINDOW=
LOCK_DIR=${DRILL_HOST_LOCK_DIR:-/var/lock}
DRILL_DIR=${DRILL_HOST_DRILL_DIR:-$root/scripts/drill/host}
TRANSCRIPT=${DRILL_HOST_TRANSCRIPT:-}
SSH_CMD=${DRILL_HOST_SSH:-ssh}
PROBE=${DRILL_HOST_PROBE:-$root/scripts/drill/lib/host-window-probe.sh}
SUDO=${DRILL_HOST_SUDO:-}
DRY_RUN=0
RESTORE_ONLY=0
DO_LIST=0
DO_HELP=0

while [ "$#" -gt 0 ]; do
	case "$1" in
	--host)
		[ "$#" -ge 2 ] || die_usage "--host needs a value"
		HOST=$2
		shift 2
		;;
	--window)
		[ "$#" -ge 2 ] || die_usage "--window needs a value"
		WINDOW=$2
		shift 2
		;;
	--lock-dir)
		[ "$#" -ge 2 ] || die_usage "--lock-dir needs a value"
		LOCK_DIR=$2
		shift 2
		;;
	--drill-dir)
		[ "$#" -ge 2 ] || die_usage "--drill-dir needs a value"
		DRILL_DIR=$2
		shift 2
		;;
	--transcript)
		[ "$#" -ge 2 ] || die_usage "--transcript needs a value"
		TRANSCRIPT=$2
		shift 2
		;;
	--ssh)
		[ "$#" -ge 2 ] || die_usage "--ssh needs a value"
		SSH_CMD=$2
		shift 2
		;;
	--dry-run)
		DRY_RUN=1
		shift
		;;
	--restore-only)
		RESTORE_ONLY=1
		shift
		;;
	--list | -l)
		DO_LIST=1
		shift
		;;
	--help | -h)
		DO_HELP=1
		shift
		;;
	--)
		shift
		while [ "$#" -gt 0 ]; do
			[ -z "$DRILL" ] || die_usage "unexpected argument: $1"
			DRILL=$1
			shift
		done
		;;
	-*) die_usage "unknown option: $1" ;;
	*)
		[ -z "$DRILL" ] || die_usage "unexpected argument: $1"
		DRILL=$1
		shift
		;;
	esac
done

# --help and --list answer without a host, a window or a transcript. They are
# the two things an operator may run before any window exists.
if [ "$DO_HELP" -eq 1 ]; then
	usage
	exit 0
fi
if [ "$DO_LIST" -eq 1 ]; then
	if names=$(list_drills); then
		printf '%s\n' "$names"
	else
		echo "drill-host: no host drills defined yet — add $DRILL_DIR/<name>.sh" >&2
		exit 1
	fi
	exit 0
fi

[ -n "$DRILL" ] || die_usage "no drill named (scripts/drill-host.sh <drill-name> --host ... --window ...)"
case "$DRILL" in
*[!A-Za-z0-9._-]*) die_usage "drill names are [A-Za-z0-9._-]; got '$DRILL'" ;;
esac
[ -n "$HOST" ] || die_usage "no host (--host <ssh-target>, or OTTER_HOST)"
[ -n "$WINDOW" ] || die_usage "no window (--window <name|path>): a drill needs a declared window"
case "$SUDO" in
"" | 0 | 1) ;;
*) die_usage "DRILL_HOST_SUDO must be 1 or unset, got '$SUDO'" ;;
esac

# A name composes the lock path; a path is the lock. A relative path is refused
# because the remote shell would resolve it against a login directory nobody
# named, and the refusal would then be about the wrong file.
case "$WINDOW" in
*/*)
	case "$WINDOW" in
	/*) ;;
	*) die_usage "--window is a path only when it is absolute; got '$WINDOW'" ;;
	esac
	LOCK_PATH=$WINDOW
	WINDOW_LABEL=$WINDOW
	WINDOW_IS_PATH=1
	;;
*)
	LOCK_PATH=$LOCK_DIR/otter-window-$WINDOW
	WINDOW_LABEL=$WINDOW
	WINDOW_IS_PATH=0
	;;
esac
# The lock path is embedded in a remote command. Restricting it to a charset
# with no quotes, spaces or metacharacters is what makes that safe; the probe
# then reads exactly the file this operator named.
case "$LOCK_PATH" in
*[!A-Za-z0-9._/-]*) die_usage "the lock path may only contain [A-Za-z0-9._/-]; got '$LOCK_PATH'" ;;
esac

drill_file=$DRILL_DIR/$DRILL.sh

if [ -z "$TRANSCRIPT" ]; then
	stamp=$(date '+%Y%m%dT%H%M%S')
	label=$(printf '%s' "$WINDOW_LABEL" | sed 's/[^A-Za-z0-9._-]/-/g')
	TRANSCRIPT=$root/docs/evidence/host-windows/$stamp-$label-$DRILL.txt
fi

# --- the transcript exists before any precondition is checked ---------------
#
# A refusal that left no record is indistinguishable from a run that was never
# attempted, so the file is opened here, right after the arguments, and every
# later stage appends to it through the helpers below.

transcript_dir=$(dirname -- "$TRANSCRIPT")
[ "$transcript_dir" = . ] || mkdir -p -- "$transcript_dir" 2>/dev/null || true
if ! : >>"$TRANSCRIPT" 2>/dev/null; then
	echo "drill-host: cannot write the transcript $TRANSCRIPT" >&2
	echo "drill-host: refusing: a window run with no record is not evidence" >&2
	exit 2
fi

field() { printf '%-10s %s\n' "$1" "$2" | tee -a "$TRANSCRIPT"; }
say() { printf 'drill-host: %s\n' "$*" | tee -a "$TRANSCRIPT"; }
stage() { printf '\n--- %s ---\n' "$*" | tee -a "$TRANSCRIPT"; }
# Refusals go to stderr as well, and are recorded: the transcript is the
# evidence, the terminal is the notification.
refuse() { printf 'drill-host: refusing: %s\n' "$*" | tee -a "$TRANSCRIPT" >&2; }
hint() { printf 'drill-host:   %s\n' "$*" | tee -a "$TRANSCRIPT" >&2; }

work=$(mktemp -d "${TMPDIR:-/tmp}/otter-drill-host.XXXXXX") || {
	say "cannot create a temporary directory for this run"
	exit 3
}

FINISHED=0
cleanup() {
	status=$?
	rm -rf "${work:?}"
	# An unexpected exit (a bug, a signal, a full disk) still leaves a result
	# line, so the transcript cannot be read as a run that quietly passed.
	if [ "$FINISHED" -eq 0 ] && [ -s "$TRANSCRIPT" ]; then
		printf 'drill-host: result: ABORTED — the harness exited %s without a verdict; nothing here says the drill passed\n' \
			"$status" >>"$TRANSCRIPT"
	fi
}
trap cleanup EXIT

interrupted() {
	say "interrupted: the window on $HOST may be unrestored"
	say "interrupted: finish it with: sh $0 $DRILL --host $HOST --window $WINDOW_LABEL --restore-only"
	result "INTERRUPTED — no verdict, and the host may be left half-changed"
	exit 130
}
trap interrupted INT TERM

result() {
	FINISHED=1
	printf 'drill-host: result: %s\n' "$1" | tee -a "$TRANSCRIPT"
	printf 'drill-host: transcript %s\n' "$TRANSCRIPT" | tee -a "$TRANSCRIPT"
}
refused_result() {
	FINISHED=1
	printf 'drill-host: result: REFUSED — %s\n' "$1" | tee -a "$TRANSCRIPT" >&2
	printf 'drill-host: transcript %s\n' "$TRANSCRIPT" | tee -a "$TRANSCRIPT" >&2
	exit 2
}

# --- header ------------------------------------------------------------------
#
# Same fields as scripts/drill.sh (date, host, checkout), plus what makes a host
# window different: which machine, which window, which lock, which drill, and
# where this record is being written.

START=$(date +%s)
{
	printf '\n===== drill-host: %s =====\n' "$DRILL"
	printf 'date:     %s\n' "$(date '+%Y-%m-%d %H:%M:%S %Z')"
	printf 'host:     %s %s (%s)  [local, where this harness ran]\n' \
		"$(uname -s)" "$(uname -r)" "$(uname -m)"
	printf 'checkout: %s\n' "$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo unknown)"
	printf 'target:   %s\n' "$HOST"
	printf 'window:   %s\n' "$WINDOW_LABEL"
	printf 'lock:     %s (on %s)\n' "$LOCK_PATH" "$HOST"
	printf 'drill:    %s\n' "$drill_file"
	printf 'transcript: %s\n' "$TRANSCRIPT"
} | tee -a "$TRANSCRIPT"

# --- precondition: the named drill exists here -------------------------------

stage "precondition: the drill"
if [ ! -f "$drill_file" ]; then
	refuse "no such host drill: $DRILL (looked for $drill_file)"
	if names=$(list_drills); then
		hint "available: $(printf '%s' "$names" | tr '\n' ' ')"
	else
		hint "no host drills are defined yet — add $DRILL_DIR/<name>.sh"
	fi
	refused_result "the drill '$DRILL' does not exist, so nothing was run"
fi
[ -r "$drill_file" ] || refused_result "the drill $drill_file is not readable, so nothing was run"
if [ ! -f "$PROBE" ]; then
	refuse "the window probe is missing: $PROBE"
	hint "it ships with the repository as scripts/drill/lib/host-window-probe.sh"
	refused_result "the harness cannot read a window lock, so nothing was run"
fi
say "drill:    $drill_file"

# --- precondition: the window, read ON the host ------------------------------
#
# One read-only probe answers both questions the harness must answer before it
# may touch anything: does the host answer, and what does the window lock say?
# It is `sh -s -- <lock>` with the probe streamed on stdin, so no quoting of a
# script body is needed and the only interpolated value is a validated path.

stage "precondition: host and window"
remote_prefix=""
if [ "$SUDO" = 1 ]; then
	remote_prefix="sudo -n "
fi
probe_remote="${remote_prefix}sh -s -- '$LOCK_PATH'"

say "probe:    $SSH_CMD -o BatchMode=yes -- $HOST $probe_remote"
# DRILL_HOST_SSH is a command line on purpose (an operator passes `ssh -i key`),
# so word splitting here is the interface, not an accident.
# shellcheck disable=SC2086
if $SSH_CMD -o BatchMode=yes -- "$HOST" "$probe_remote" <"$PROBE" >"$work/probe.out" 2>"$work/probe.err"; then
	probe_status=0
else
	probe_status=$?
fi
if [ "$probe_status" -ne 0 ]; then
	refuse "the host $HOST did not answer the window probe (ssh exited $probe_status)"
	while IFS= read -r line; do
		hint "ssh: $line"
	done <"$work/probe.err"
	hint "a drill needs a reachable host; nothing was run"
	refused_result "the host $HOST is unreachable, so nothing was run"
fi
# The probe's report is evidence in its own right: replay it into the transcript.
while IFS= read -r line; do
	field 'probe:' "$line"
done <"$work/probe.out"

# probe_value reads one key the probe printed. lock_value reads one key out of
# the lock's raw content, with the carriage return and trailing blanks stripped
# so a lock written by a `printf` with \r, or with a trailing space, still parses.
probe_value() { sed -n "s/^$1=//p" "$work/probe.out" | head -n 1; }
lock_value() {
	probe_value "content: $1" | tr -d '\r' | sed 's/[[:space:]]*$//'
}

HOST_FACTS=$(probe_value host)
HOST_NOW=$(probe_value now)
LOCK_STATE=$(probe_value lock)
LOCK_EXPIRES=$(lock_value expires)
LOCK_OPERATOR=$(lock_value operator)
LOCK_WINDOW=$(lock_value window)

say "host:     ${HOST_FACTS:-unknown}"
case "$HOST_NOW" in
'' | *[!0-9]*)
	refuse "the host $HOST did not report a clock, so an expiry cannot be judged"
	hint "the probe printed now='${HOST_NOW:-}'"
	refused_result "the window's expiry cannot be judged on $HOST"
	;;
esac
say "clock:    host epoch $HOST_NOW"

# Record what the lock says before judging it: a refusal transcript must show
# the declaration the operator actually made, not just the complaint.
if [ "$LOCK_STATE" = present ]; then
	[ -n "$LOCK_EXPIRES" ] && field 'expires:' "$LOCK_EXPIRES"
	[ -n "$LOCK_OPERATOR" ] && field 'operator:' "$LOCK_OPERATOR"
	[ -n "$LOCK_WINDOW" ] && field 'declares:' "window=$LOCK_WINDOW"
fi

# require_window is the gate the whole harness exists for. Under --restore-only
# it records the declaration but does not gate on it: a host that was left
# half-changed must be put back even by an operator whose window has since
# closed, and refusing that would be the dangerous reading of "refuse without a
# window".
require_window() {
	gate_mode=$1
	if [ "$gate_mode" = restore ]; then
		say "restore-only: the window gate is not applied to a restore"
		if [ "$LOCK_STATE" = present ]; then
			say "restore-only: the lock declares expires=$LOCK_EXPIRES operator=$LOCK_OPERATOR; restoring anyway, because a host put back is not gated"
		else
			say "restore-only: the lock is $LOCK_STATE; restoring anyway, because a host put back is not gated"
		fi
		return 0
	fi

	case "$LOCK_STATE" in
	absent)
		refuse "no window lock at $LOCK_PATH on $HOST"
		hint "the window '$WINDOW_LABEL' is not declared, so the harness will not touch $HOST"
		hint "declare it on the host first, for example:"
		hint "  ssh $HOST \"printf 'window=%s\\nexpires=%s\\noperator=%s\\n' $WINDOW_LABEL \$((\$(date +%s) + 3600)) \$(id -un)@\$(hostname) > $LOCK_PATH\""
		hint "or point --lock-dir / --window at where this host keeps its locks"
		refused_result "window '$WINDOW_LABEL' has no lock at $LOCK_PATH, so nothing was run"
		;;
	unreadable)
		refuse "the window lock $LOCK_PATH on $HOST cannot be read"
		hint "it exists but is not a readable file (a directory, a fifo, or a mode this login cannot read)"
		hint "without reading the declaration the harness cannot check the expiry, so nothing was run"
		refused_result "the lock for window '$WINDOW_LABEL' cannot be read, so nothing was run"
		;;
	present) ;;
	*)
		refuse "the probe reported lock='$LOCK_STATE', which the harness does not understand"
		refused_result "the window could not be read, so nothing was run"
		;;
	esac

	if [ -z "$LOCK_EXPIRES" ]; then
		refuse "the window lock $LOCK_PATH declares no expires=<epoch>"
		hint "an expiration is what makes the window a window: a declaration nobody can expire is not one"
		refused_result "the lock for window '$WINDOW_LABEL' declares no expiry, so nothing was run"
	fi
	case "$LOCK_EXPIRES" in
	*[!0-9]*)
		refuse "the window lock $LOCK_PATH declares expires=$LOCK_EXPIRES, which is not an epoch in seconds"
		refused_result "the lock for window '$WINDOW_LABEL' has an unreadable expiry, so nothing was run"
		;;
	esac
	if [ -z "$LOCK_OPERATOR" ]; then
		refuse "the window lock $LOCK_PATH declares no operator=<identity>"
		hint "who holds the window is part of the declaration, and it goes in the transcript"
		refused_result "the lock for window '$WINDOW_LABEL' names no operator, so nothing was run"
	fi
	# The expiry is judged before the window's name: if both are wrong, "the window
	# you asked for has closed" is the fact the operator needs first.
	remaining=$((LOCK_EXPIRES - HOST_NOW))
	if [ "$remaining" -le 0 ]; then
		refuse "window '$WINDOW_LABEL' expired at $LOCK_EXPIRES, $((-remaining))s ago (host clock $HOST_NOW)"
		hint "the lock is $LOCK_PATH on $HOST; extend it or declare a new window before running"
		hint "nothing was run: an expired window is not a window"
		refused_result "window '$WINDOW_LABEL' expired at $LOCK_EXPIRES, so nothing was run"
	fi
	if [ "$WINDOW_IS_PATH" -eq 0 ] && [ -n "$LOCK_WINDOW" ] && [ "$LOCK_WINDOW" != "$WINDOW_LABEL" ]; then
		refuse "the lock $LOCK_PATH declares window=$LOCK_WINDOW, but this run asked for window '$WINDOW_LABEL'"
		hint "one lock is one window; running under another window's declaration is evidence for the wrong claim"
		refused_result "the lock belongs to window '$LOCK_WINDOW', not '$WINDOW_LABEL', so nothing was run"
	fi
	say "window:   '$WINDOW_LABEL' is open: ${remaining}s remaining (expires $LOCK_EXPIRES, operator $LOCK_OPERATOR)"
}

if [ "$RESTORE_ONLY" -eq 1 ]; then
	require_window restore
else
	require_window run
fi

# --- the drill's declared restore step ---------------------------------------
#
# Read here, before the run, for two reasons: the plan can say what will be put
# back before anything is changed, and a drill that never runs (a failed
# precondition) cannot leave the operator wondering whether a restore was owed.

restore_desc=
restore_declared=0
if grep -q '^# drill-host-restore:' "$drill_file" 2>/dev/null; then
	restore_declared=1
	restore_desc=$(sed -n 's/^# drill-host-restore:[[:space:]]*//p' "$drill_file" | head -n 1)
	[ -n "$restore_desc" ] || restore_desc="(the drill gave no description)"
fi

# --restore-only asks for a step this drill may not have. Saying "restored" when
# nothing ran would be the same lie as claiming a restore that was never
# declared, so it is refused before the plan.
if [ "$RESTORE_ONLY" -eq 1 ] && [ "$restore_declared" -eq 0 ]; then
	refuse "$DRILL declares no restore step, so there is nothing to restore"
	hint "the drill has no '# drill-host-restore:' line; if the host needs putting back, do it by hand"
	refused_result "nothing to restore: $DRILL declares no restore step"
fi

# --- the plan, for --dry-run -------------------------------------------------

if [ "$DRY_RUN" -eq 1 ] || [ "$RESTORE_ONLY" -eq 1 ]; then
	stage "plan"
	say "plan: target      $HOST (${HOST_FACTS:-unknown})"
	say "plan: window      '$WINDOW_LABEL' via $LOCK_PATH, expires $LOCK_EXPIRES (operator $LOCK_OPERATOR)"
	say "plan: drill       $drill_file"
	if [ "$restore_declared" -eq 1 ]; then
		say "plan: restore     $restore_desc"
	else
		say "plan: restore     none declared by this drill"
	fi
	if [ "$RESTORE_ONLY" -eq 0 ]; then
		say "plan: run         $SSH_CMD -o BatchMode=yes -- $HOST ${remote_prefix}sh -s -- run < $drill_file"
		say "plan: assert      exit 0; any other status fails the run"
	fi
	if [ "$restore_declared" -eq 1 ]; then
		say "plan: restore cmd $SSH_CMD -o BatchMode=yes -- $HOST ${remote_prefix}sh -s -- restore < $drill_file"
	fi
	say "plan: transcript  $TRANSCRIPT"
	if [ "$DRY_RUN" -eq 1 ]; then
		say "dry run: no drill command was sent; the only remote command was the read-only probe"
		result "DRY RUN — the plan is above, nothing was changed on $HOST"
		exit 0
	fi
	stage "restore-only: no run, no assertion"
fi

# --- run and restore ---------------------------------------------------------
#
# phase_remote streams one phase of the drill to the host, tees its output to the
# operator and the transcript as it arrives, and puts its exit status in
# PHASE_STATUS. The status has to travel through a file, not a shell variable:
# the producer is the left side of a pipeline, which is a subshell, and POSIX sh
# has no PIPESTATUS. Streaming is why the status cannot simply be captured with
# $( ): the operator watching a window needs the output as it happens.

PHASE_STATUS=0
phase_remote() {
	phase=$1
	status_file=$work/phase.status
	rm -f "$status_file"
	remote="${remote_prefix}sh -s -- '$phase'"
	say "remote:   $SSH_CMD -o BatchMode=yes -- $HOST $remote"
	# shellcheck disable=SC2086
	{
		set +e
		$SSH_CMD -o BatchMode=yes -- "$HOST" "$remote" <"$drill_file" 2>&1
		printf '%s\n' "$?" >"$status_file"
	} | tee -a "$TRANSCRIPT"
	if [ ! -s "$status_file" ]; then
		say "transport: the remote $phase did not report an exit status (ssh transport failed)"
		PHASE_STATUS=255
		return 0
	fi
	PHASE_STATUS=$(cat "$status_file")
}

drill_status=0
if [ "$RESTORE_ONLY" -eq 0 ]; then
	stage "run: $DRILL on $HOST"
	phase_remote run
	drill_status=$PHASE_STATUS
	if [ "$drill_status" -eq 0 ]; then
		say "assert:   $DRILL exited 0; the behavior it asserts held"
	else
		say "assert:   $DRILL exited $drill_status; that exit status IS the assertion, so this run failed"
	fi
fi

restore_status=0
if [ "$restore_declared" -eq 1 ]; then
	stage "restore: $restore_desc"
	phase_remote restore
	restore_status=$PHASE_STATUS
	if [ "$restore_status" -eq 0 ]; then
		say "restore:  exit 0; the drill reports the host is back the way it found it"
	else
		say "restore:  exit $restore_status; the drill could not put the host back"
	fi
else
	stage "restore: none declared"
	say "restore:  $DRILL declares no restore step (no '# drill-host-restore:' line)"
	say "restore:  nothing was restored by this harness; any change the drill made is still on $HOST"
	if [ "$drill_status" -ne 0 ]; then
		say "restore:  the run failed AND nothing was restored: check $HOST by hand"
	fi
fi

# --- verdict -----------------------------------------------------------------
#
# A failed restore outranks a failed assertion: an operator (or a CI job keying
# off the status) has to know the host is in an unknown state first.

elapsed=$(($(date +%s) - START))
say "elapsed:  ${elapsed}s"
if [ "$restore_status" -ne 0 ]; then
	if [ "$drill_status" -ne 0 ]; then
		say "result:   the drill failed (exit $drill_status) and its restore failed (exit $restore_status)"
	else
		say "result:   the drill passed, but its restore failed (exit $restore_status)"
	fi
	result "FAIL — restore failed on $HOST; the host is in an unknown state, check it by hand"
	exit 3
fi
if [ "$drill_status" -ne 0 ]; then
	result "FAIL — $DRILL exited $drill_status in window '$WINDOW_LABEL'; the transcript is kept"
	exit 1
fi
if [ "$RESTORE_ONLY" -eq 1 ]; then
	result "RESTORED — $DRILL's restore step ran in window '$WINDOW_LABEL' (no run, no assertion)"
	exit 0
fi
if [ "$restore_declared" -eq 1 ]; then
	restore_verdict=ran
else
	restore_verdict="none declared"
fi
result "PASS — $DRILL in window '$WINDOW_LABEL' on $HOST; restore $restore_verdict"
exit 0
