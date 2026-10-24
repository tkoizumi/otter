#!/bin/sh
# Falsifiability for scripts/drill-host.sh.
#
# The harness's only job is to make a host window reproducible: refuse without a
# declared window, run the drill, take the drill's exit status as the assertion,
# put back what the drill declares, and leave a transcript that says all of it.
# None of that can be checked on a host in CI, so this suite stands in for the
# host: a fake `ssh` on PATH runs the remote command locally with the pushed
# script on stdin, which is exactly what `ssh host 'sh -s -- run' < drill.sh`
# does, and records every command it was asked to send.
#
# The lock lives in a fixture directory and reaches the harness through
# --lock-dir, so nothing here touches /var/lock. The fixture drills write their
# run/restore phases into a host-side file, so "the restore ran" is checked
# against what the drill recorded on the host, not against the harness's own
# claim that it ran it.
#
# What the cases prove:
#   * no lock, an unreadable lock, a lock with no expiry, a lock with no
#     operator, a lock for another window, and an expired lock each refuse with
#     exit 2, name the window -- and the expiry, when that is the reason -- and
#     send no drill command at all, only the read-only probe;
#   * every refusal still leaves a transcript with the header and the reason;
#   * --dry-run prints the plan and sends no drill command;
#   * a drill that exits 0 passes, its restore runs, and the transcript has the
#     scripts/drill.sh header plus the window identity, the output, the
#     assertion and the restore;
#   * a drill that exits non-zero fails with exit 1, keeps the transcript,
#     still restores, and the transcript names the drill and the status;
#   * a drill that declares no restore says so instead of pretending;
#   * DRILL_HOST_SUDO=1 sends the remote commands under `sudo -n`;
#   * --restore-only runs the restore and not the run, and does not let a closed
#     window block putting the host back;
#   * an unreachable host and a drill that does not exist refuse with a
#     transcript and without running a drill; OTTER_HOST supplies the host when
#     --host is not given;
#   * --help and --list need no host at all;
#   * the drill the repository actually ships, scripts/drill/host/
#     restart-service.sh, runs against a systemd stand-in -- and goes red when
#     the unit's PID does not change, which is the daemon that did not come back.
#
# OTTER_DRILL_HOST_SUBJECT points the matrix at a mutated copy of the harness.
# The last two blocks use it to demonstrate that this suite goes red when the
# window gate is removed and when the restore call is removed.
#
#   sh scripts/test-drill-host.sh
#
# Exit status is 0 only when every case passed and both mutations were detected.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
subject=${OTTER_DRILL_HOST_SUBJECT:-$here/drill-host.sh}
probe=$here/drill/lib/host-window-probe.sh
for needed in "$subject" "$probe"; do
	[ -f "$needed" ] || {
		echo "test-drill-host: missing $needed" >&2
		exit 2
	}
done

work=$(mktemp -d "${TMPDIR:-/tmp}/otter-drill-host-test.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

passed=0
failed=0
ok() {
	passed=$((passed + 1))
	echo "ok: $*"
}
bad() {
	failed=$((failed + 1))
	echo "FAIL: $*" >&2
}

# --- the fake host -----------------------------------------------------------
#
# ssh parses options the way OpenSSH does (an option with a value consumes two
# words, `--` ends the options), records the destination and the remote command
# it was asked to send, and then runs that command through `sh -c` with its stdin
# still connected. That is the whole transport contract the harness uses: the
# probe and the drill arrive on stdin and the remote command is `sh -s -- ...`.

mkdir -p "$work/bin" "$work/drills" "$work/locks" "$work/t"
cat >"$work/bin/ssh" <<'FAKE_SSH'
#!/bin/sh
log=${OTTER_FAKE_SSH_LOG:?OTTER_FAKE_SSH_LOG is not set}
while [ "$#" -gt 0 ]; do
	case "$1" in
	-o | -p | -l | -i | -F | -E | -c | -m | -S)
		shift 2
		continue
		;;
	--)
		shift
		break
		;;
	-*)
		shift
		continue
		;;
	*) break ;;
	esac
done
host=${1:-}
[ "$#" -gt 0 ] && shift
printf 'ssh %s :: %s\n' "$host" "$*" >>"$log"
if [ "${OTTER_FAKE_SSH_UNREACHABLE:-}" = 1 ]; then
	printf 'ssh: connect to host %s port 22: Connection refused\n' "$host" >&2
	exit 255
fi
exec sh -c "$*"
FAKE_SSH
chmod +x "$work/bin/ssh"

# sudo is a pass-through here: the case that sets DRILL_HOST_SUDO=1 asserts the
# prefix in the recorded command, and this shim lets the command still run so
# the rest of the case is exercised too.
cat >"$work/bin/sudo" <<'FAKE_SUDO'
#!/bin/sh
[ "${1:-}" = -n ] && shift
exec "$@"
FAKE_SUDO
chmod +x "$work/bin/sudo"

host=root@host.test
drills=$work/drills
locks=$work/locks

# write_drill <name> <run-exit> <restore-description-or-empty>
#
# The fixture drill records each phase it runs into $work/events, a path that is
# on the "host" side of the fake transport. That file, not the harness's
# transcript, is what says whether a restore really happened.
write_drill() {
	name=$1
	run_exit=$2
	restore_desc=$3
	{
		printf '#!/bin/sh\n'
		printf '# fixture host drill for scripts/test-drill-host.sh\n'
		if [ -n "$restore_desc" ]; then
			printf '# drill-host-restore: %s\n' "$restore_desc"
		fi
		cat <<FIXTURE
phase=\${1:-}
echo "fixture-drill: $name phase \$phase"
printf '%s %s\n' '$name' "\$phase" >>'$work/events'
case "\$phase" in
run)
	echo "fixture-drill: $name asserts its behavior"
	exit $run_exit
	;;
restore)
	echo "fixture-drill: $name puts back what it changed"
	exit 0
	;;
esac
echo "fixture-drill: unknown phase '\$phase'" >&2
exit 2
FIXTURE
	} >"$drills/$name.sh"
}

write_drill pass 0 "turn the example unit back on"
write_drill fail 7 "turn the example unit back on"
write_drill no-restore 0 ""

# write_lock <name> <content>
write_lock() {
	printf '%s\n' "$2" >"$locks/otter-window-$1"
}
future=$(( $(date +%s) + 600 ))
past=$(( $(date +%s) - 60 ))
valid_lock="window=maintenance
expires=$future
operator=alice@laptop"

# --- the runner --------------------------------------------------------------
#
# subject_in_use is the harness under test: the real script, or a mutated copy
# in the two blocks at the end. HARNESS_ENV carries a case-specific variable
# (DRILL_HOST_SUDO, OTTER_FAKE_SSH_UNREACHABLE) through to the harness and to
# the fake ssh it starts.

subject_in_use=$subject
HARNESS_ENV=""
harness_status=0
casename=""
problems=""

run_harness() {
	: >"$work/log"
	: >"$work/events"
	harness_status=0
	# shellcheck disable=SC2086
	env PATH="$work/bin:$PATH" \
		DRILL_HOST_PROBE="$probe" \
		DRILL_HOST_SSH=ssh \
		OTTER_FAKE_SSH_LOG="$work/log" \
		$HARNESS_ENV \
		sh "$subject_in_use" "$@" >"$work/out" 2>&1 || harness_status=$?
}

problem() {
	problems="$problems
    - $1"
}

begin() {
	casename=$1
	problems=""
	transcript=$work/t/$2.txt
	rm -f "$transcript"
}

finish() {
	if [ -z "$problems" ]; then
		ok "$casename"
	else
		bad "$casename:$problems"
		echo "    --- output ---" >&2
		sed 's/^/    /' "$work/out" >&2
		if [ -f "$transcript" ]; then
			echo "    --- transcript ($transcript) ---" >&2
			sed 's/^/    /' "$transcript" >&2
		fi
	fi
}

out_has() { grep -Fq -- "$1" "$work/out"; }
transcript_has() { [ -f "$transcript" ] && grep -Fq -- "$1" "$transcript"; }
events_has() { grep -Fq -- "$1" "$work/events" 2>/dev/null; }
remote_count() { wc -l <"$work/log" | tr -d ' '; }
# A drill command is what mutates the host; the probe is `sh -s -- '<lock>'`,
# the drill phases are `sh -s -- 'run'` and `sh -s -- 'restore'`. The suite
# asserts on the phases, so "no remote command was run" is precise: the
# read-only probe may have been sent (it is how the harness learns there is no
# lock), and nothing else may have been.
drill_commands() {
	n=0
	grep -Fq -- "sh -s -- 'run'" "$work/log" 2>/dev/null && n=$((n + 1))
	grep -Fq -- "sh -s -- 'restore'" "$work/log" 2>/dev/null && n=$((n + 1))
	printf '%s' "$n"
}

# --- --help and --list need no host -----------------------------------------

begin "help needs no host" help
run_harness --help
[ "$harness_status" -eq 0 ] || problem "exit $harness_status, want 0"
out_has "usage: scripts/drill-host.sh" || problem "the usage does not name the interface"
out_has "--window <name|path>" || problem "the usage does not document --window"
out_has "drill-host-restore" || problem "the usage does not document the restore declaration"
[ "$(remote_count)" -eq 0 ] || problem "help sent a remote command"
finish

begin "list needs no host and lists the drills" list
run_harness --list --drill-dir "$drills"
[ "$harness_status" -eq 0 ] || problem "exit $harness_status, want 0"
out_has "pass" || problem "the list omits the pass fixture"
out_has "no-restore" || problem "the list omits the no-restore fixture"
[ "$(remote_count)" -eq 0 ] || problem "list sent a remote command"
finish

# --- the window gate ---------------------------------------------------------

begin "no lock refuses, names the missing window, runs no drill" no-lock
run_harness pass --host "$host" --window maintenance --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript"
[ "$harness_status" -eq 2 ] || problem "exit $harness_status, want 2"
out_has "no window lock at $locks/otter-window-maintenance" ||
	problem "the refusal does not name the missing lock"
out_has "window 'maintenance' is not declared" ||
	problem "the refusal does not name the missing window"
[ "$(drill_commands)" -eq 0 ] || problem "a drill command was sent despite the refusal"
[ "$(remote_count)" -eq 1 ] || problem "sent $(remote_count) remote commands, want only the read-only probe"
transcript_has "===== drill-host: pass =====" || problem "the refusal left no header in the transcript"
transcript_has "target:   $host" || problem "the refusal transcript has no target"
transcript_has "window:   maintenance" || problem "the refusal transcript has no window"
transcript_has "result: REFUSED" || problem "the refusal transcript has no REFUSED result"
finish

begin "an unreadable lock refuses" lock-unreadable
mkdir -p "$locks/otter-window-blocked"
run_harness pass --host "$host" --window blocked --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript"
[ "$harness_status" -eq 2 ] || problem "exit $harness_status, want 2"
out_has "cannot be read" || problem "the refusal does not say the lock is unreadable"
[ "$(drill_commands)" -eq 0 ] || problem "a drill command was sent despite the refusal"
transcript_has "result: REFUSED" || problem "no refusal recorded in the transcript"
finish

begin "an expired lock refuses and names the expiry" lock-expired
write_lock expired "window=expired
expires=$past
operator=alice@laptop"
run_harness pass --host "$host" --window expired --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript"
[ "$harness_status" -eq 2 ] || problem "exit $harness_status, want 2"
out_has "expired at $past" || problem "the refusal does not name the expiry epoch $past"
out_has "expired window is not a window" || problem "the refusal does not say why that matters"
[ "$(drill_commands)" -eq 0 ] || problem "a drill command was sent despite the expired lock"
transcript_has "expires:   $past" || problem "the transcript does not record the expiry it judged"
transcript_has "result: REFUSED" || problem "no refusal recorded in the transcript"
finish

begin "a lock with no expiry refuses" lock-no-expires
write_lock no-expires "window=maintenance
operator=alice@laptop"
run_harness pass --host "$host" --window no-expires --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript"
[ "$harness_status" -eq 2 ] || problem "exit $harness_status, want 2"
out_has "declares no expires=<epoch>" || problem "the refusal does not name the missing expires="
[ "$(drill_commands)" -eq 0 ] || problem "a drill command was sent despite the refusal"
finish

begin "a lock naming no operator refuses" lock-no-operator
write_lock no-operator "window=maintenance
expires=$future"
run_harness pass --host "$host" --window no-operator --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript"
[ "$harness_status" -eq 2 ] || problem "exit $harness_status, want 2"
out_has "declares no operator=<identity>" || problem "the refusal does not name the missing operator="
[ "$(drill_commands)" -eq 0 ] || problem "a drill command was sent despite the refusal"
finish

begin "a lock for another window refuses" lock-mismatch
write_lock mismatch "window=someone-elses-window
expires=$future
operator=alice@laptop"
run_harness pass --host "$host" --window mismatch --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript"
[ "$harness_status" -eq 2 ] || problem "exit $harness_status, want 2"
out_has "declares window=someone-elses-window" || problem "the refusal does not name the window the lock declares"
[ "$(drill_commands)" -eq 0 ] || problem "a drill command was sent despite the refusal"
finish

# --- dry run -----------------------------------------------------------------

begin "a valid lock and --dry-run print the plan and mutate nothing" dry-run
write_lock maintenance "$valid_lock"
run_harness pass --host "$host" --window maintenance --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript" --dry-run
[ "$harness_status" -eq 0 ] || problem "exit $harness_status, want 0"
out_has "plan: target      $host" || problem "the plan does not name the host"
out_has "plan: window      'maintenance'" || problem "the plan does not name the window"
out_has "expires $future" || problem "the plan does not name the expiry"
out_has "operator alice@laptop" || problem "the plan does not name the operator"
out_has "plan: drill       $drills/pass.sh" || problem "the plan does not name the drill"
out_has "plan: run         ssh" || problem "the plan does not print the run command"
out_has "plan: restore     turn the example unit back on" ||
	problem "the plan does not print the declared restore"
out_has "dry run: no drill command was sent" || problem "the dry run does not say nothing was sent"
[ "$(drill_commands)" -eq 0 ] || problem "the dry run sent a drill command"
[ "$(remote_count)" -eq 1 ] || problem "the dry run sent $(remote_count) remote commands, want only the probe"
[ -s "$work/events" ] && problem "the dry run ran the fixture drill"
transcript_has "result: DRY RUN" || problem "the transcript has no dry-run result"
finish

# --- run, assert, restore ----------------------------------------------------

begin "a drill that exits 0 passes, restores, and records everything" pass
write_lock maintenance "$valid_lock"
run_harness pass --host "$host" --window maintenance --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript"
[ "$harness_status" -eq 0 ] || problem "exit $harness_status, want 0"
# the header scripts/drill.sh writes, plus the window identity
transcript_has "===== drill-host: pass =====" || problem "no drill-host header"
transcript_has "date:     " || problem "the header has no date"
transcript_has "host:     " || problem "the header has no host"
transcript_has "checkout: " || problem "the header has no checkout describe"
transcript_has "target:   $host" || problem "the header has no target"
transcript_has "window:   maintenance" || problem "the header has no window"
transcript_has "lock:     $locks/otter-window-maintenance (on $host)" || problem "the header has no lock path"
transcript_has "expires:   $future" || problem "the header has no expiry"
transcript_has "operator:  alice@laptop" || problem "the header has no operator"
transcript_has "drill:    $drills/pass.sh" || problem "the header has no drill path"
transcript_has "transcript: $transcript" || problem "the header does not name the transcript"
# the drill's own output, streamed
transcript_has "fixture-drill: pass phase run" || problem "the run output is not in the transcript"
transcript_has "fixture-drill: pass phase restore" || problem "the restore output is not in the transcript"
# the assertion and the restore, recorded as such
transcript_has "assert:   pass exited 0" || problem "the transcript does not record the assertion"
transcript_has "restore: turn the example unit back on" || problem "the transcript does not name the restore"
transcript_has "restore:  exit 0" || problem "the transcript does not record the restore status"
transcript_has "result: PASS" || problem "the transcript has no PASS result"
# and the host-side truth: run then restore, in that order
want="pass run
pass restore"
[ "$(cat "$work/events")" = "$want" ] ||
	problem "the fixture recorded '$(tr '\n' ' ' <"$work/events")', want 'pass run pass restore'"
[ "$(drill_commands)" -eq 2 ] || problem "sent $(drill_commands) drill commands, want run and restore"
finish

begin "a drill that exits non-zero fails, keeps the transcript, and still restores" fail
write_lock maintenance "$valid_lock"
run_harness fail --host "$host" --window maintenance --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript"
[ "$harness_status" -eq 1 ] || problem "exit $harness_status, want 1"
[ -f "$transcript" ] || problem "the failed run left no transcript"
transcript_has "fixture-drill: fail phase run" || problem "the failing output is not in the transcript"
transcript_has "fixture-drill: fail puts back what it changed" ||
	problem "the restore did not run after the failure"
transcript_has "assert:   fail exited 7" || problem "the transcript does not record the failing status"
transcript_has "result: FAIL" || problem "the transcript has no FAIL result"
transcript_has "fail exited 7 in window 'maintenance'" || problem "the FAIL result does not name the drill and the status"
out_has "result: FAIL" || problem "the failure is not reported to the operator"
[ "$(cat "$work/events")" = "fail run
fail restore" ] || problem "the fixture recorded '$(tr '\n' ' ' <"$work/events")', want a run and a restore"
finish

begin "a drill that declares no restore says so" no-restore
write_lock maintenance "$valid_lock"
run_harness no-restore --host "$host" --window maintenance --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript"
[ "$harness_status" -eq 0 ] || problem "exit $harness_status, want 0"
transcript_has "no-restore declares no restore step" ||
	problem "the transcript does not say the drill declares no restore"
transcript_has "nothing was restored by this harness" ||
	problem "the transcript does not say that nothing was restored"
transcript_has "restore none declared" || problem "the PASS result pretends a restore ran"
[ "$(cat "$work/events")" = "no-restore run" ] ||
	problem "the fixture recorded '$(tr '\n' ' ' <"$work/events")', want only the run"
[ "$(drill_commands)" -eq 1 ] || problem "a restore command was sent for a drill that declares none"
finish

begin "DRILL_HOST_SUDO=1 sends the remote commands under sudo -n" sudo
write_lock maintenance "$valid_lock"
HARNESS_ENV="DRILL_HOST_SUDO=1"
run_harness pass --host "$host" --window maintenance --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript"
HARNESS_ENV=""
[ "$harness_status" -eq 0 ] || problem "exit $harness_status, want 0"
grep -Fq ":: sudo -n sh -s -- 'run'" "$work/log" ||
	problem "the run was not sent under sudo -n"
grep -Fq ":: sudo -n sh -s -- 'restore'" "$work/log" ||
	problem "the restore was not sent under sudo -n"
transcript_has "sudo -n sh -s -- 'run'" || problem "the transcript does not record the sudo prefix"
finish

begin "--restore-only runs the restore and not the run" restore-only
write_lock maintenance "$valid_lock"
run_harness pass --host "$host" --window maintenance --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript" --restore-only
[ "$harness_status" -eq 0 ] || problem "exit $harness_status, want 0"
[ "$(cat "$work/events")" = "pass restore" ] ||
	problem "the fixture recorded '$(tr '\n' ' ' <"$work/events")', want only the restore"
transcript_has "restore-only" || problem "the transcript does not say it was a restore-only run"
transcript_has "result: RESTORED" || problem "the transcript has no RESTORED result"
finish

begin "--restore-only still restores when the window has closed" restore-only-expired
write_lock closed "window=maintenance
expires=$past
operator=alice@laptop"
run_harness pass --host "$host" --window closed --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript" --restore-only
[ "$harness_status" -eq 0 ] || problem "exit $harness_status, want 0: putting a host back must not be gated by the window"
[ "$(cat "$work/events")" = "pass restore" ] || problem "the restore did not run"
transcript_has "restoring anyway" || problem "the transcript does not say the gate was not applied"
finish

begin "--restore-only on a drill that declares no restore refuses" restore-only-none
write_lock maintenance "$valid_lock"
run_harness no-restore --host "$host" --window maintenance --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript" --restore-only
[ "$harness_status" -eq 2 ] || problem "exit $harness_status, want 2"
out_has "nothing to restore" || problem "the refusal does not say there is nothing to restore"
[ "$(drill_commands)" -eq 0 ] || problem "a drill command was sent despite the refusal"
transcript_has "result: REFUSED" || problem "no refusal recorded in the transcript"
finish

# --- refusal without a transcript-worthy host --------------------------------

begin "an unreachable host refuses with a transcript" unreachable
write_lock maintenance "$valid_lock"
HARNESS_ENV="OTTER_FAKE_SSH_UNREACHABLE=1"
run_harness pass --host "$host" --window maintenance --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript"
HARNESS_ENV=""
[ "$harness_status" -eq 2 ] || problem "exit $harness_status, want 2"
out_has "did not answer the window probe" || problem "the refusal does not say the host did not answer"
out_has "Connection refused" || problem "the refusal does not carry ssh's own error"
[ "$(drill_commands)" -eq 0 ] || problem "a drill command was sent to an unreachable host"
transcript_has "===== drill-host: pass =====" || problem "the refusal left no header"
transcript_has "result: REFUSED" || problem "no refusal recorded in the transcript"
finish

begin "a drill that does not exist refuses before touching the host" missing-drill
write_lock maintenance "$valid_lock"
run_harness no-such-drill --host "$host" --window maintenance --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript"
[ "$harness_status" -eq 2 ] || problem "exit $harness_status, want 2"
out_has "no such host drill: no-such-drill" || problem "the refusal does not name the missing drill"
out_has "available: " || problem "the refusal does not list the drills that exist"
[ "$(remote_count)" -eq 0 ] || problem "the harness contacted the host for a drill that does not exist"
transcript_has "===== drill-host: no-such-drill =====" || problem "the refusal left no header"
transcript_has "result: REFUSED" || problem "no refusal recorded in the transcript"
finish

begin "OTTER_HOST supplies the host when --host is not given" otter-host
write_lock maintenance "$valid_lock"
HARNESS_ENV="OTTER_HOST=$host"
run_harness pass --window maintenance --lock-dir "$locks" \
	--drill-dir "$drills" --transcript "$transcript"
HARNESS_ENV=""
[ "$harness_status" -eq 0 ] || problem "exit $harness_status, want 0: OTTER_HOST was not used"
transcript_has "target:   $host" || problem "the transcript does not name OTTER_HOST as the target"
grep -Fq "ssh $host :: sh -s -- 'run'" "$work/log" || problem "the run was not sent to OTTER_HOST"
finish

# --- the window as an absolute path -----------------------------------------

begin "--window accepts an absolute lock path" window-path
write_lock maintenance "$valid_lock"
run_harness pass --host "$host" --window "$locks/otter-window-maintenance" \
	--drill-dir "$drills" --transcript "$transcript"
[ "$harness_status" -eq 0 ] || problem "exit $harness_status, want 0"
out_has "lock:     $locks/otter-window-maintenance (on $host)" ||
	problem "the path was not used as the lock"
grep -Fq "sh -s -- '$locks/otter-window-maintenance'" "$work/log" ||
	problem "the probe was not pointed at the path"
transcript_has "window:   $locks/otter-window-maintenance" || problem "the transcript does not name the path as the window"
finish

# --- usage refusals ----------------------------------------------------------

begin "no window is a usage error" usage-no-window
run_harness pass --host "$host" --drill-dir "$drills"
[ "$harness_status" -eq 2 ] || problem "exit $harness_status, want 2"
out_has "no window (--window <name|path>)" || problem "the error does not name the missing --window"
[ "$(remote_count)" -eq 0 ] || problem "a usage error sent a remote command"
finish

begin "no host is a usage error" usage-no-host
run_harness pass --window maintenance --lock-dir "$locks" --drill-dir "$drills"
[ "$harness_status" -eq 2 ] || problem "exit $harness_status, want 2"
out_has "no host (--host <ssh-target>" || problem "the error does not name the missing --host"
[ "$(remote_count)" -eq 0 ] || problem "a usage error sent a remote command"
finish

begin "a relative window path is refused" usage-relative
run_harness pass --host "$host" --window var/lock/otter-window-x \
	--drill-dir "$drills" --transcript "$transcript"
[ "$harness_status" -eq 2 ] || problem "exit $harness_status, want 2"
out_has "only when it is absolute" || problem "the refusal does not say the path must be absolute"
finish

# --- the shipped drill, against a systemd stand-in -----------------------------
#
# scripts/drill/host/restart-service.sh is the drill the repository actually
# ships: it restarts a unit. A suite has no systemd, so it gets a stand-in that
# keeps the three answers the drill asks for -- enabled, active and MainPID --
# in a file, and a fake `id` that satisfies the drill's root check. What this
# proves is not that systemd works; it is that the shipped drill runs, asserts
# and restores, and that its central assertion fails when the daemon does NOT
# come back, which is the failure a window exists to catch.

systemd=$work/systemd
mkdir -p "$systemd"
cat >"$work/bin/systemctl" <<'FAKE_SYSTEMCTL'
#!/bin/sh
state=${OTTER_FAKE_SYSTEMD_STATE:?}
get() { sed -n "s/^$1=//p" "$state" | head -n 1; }
set_kv() {
	sed "s/^$1=.*/$1=$2/" "$state" >"$state.new"
	mv "$state.new" "$state"
}
cmd=${1:-}
shift || true
case "$cmd" in
is-enabled) get enabled ;;
is-active) get active ;;
show) get pid ;;
restart)
	# FAKE_SYSTEMD_NO_RESTART=1 leaves the PID alone: the unit's answer never
	# changes, which is the daemon that did not actually come back.
	if [ "${FAKE_SYSTEMD_NO_RESTART:-}" != 1 ]; then
		pid=$(get pid)
		set_kv pid "$((pid + 1))"
	fi
	set_kv active active
	;;
start) set_kv active active ;;
enable) set_kv enabled enabled ;;
*)
	echo "fake systemctl: unsupported '$cmd $*'" >&2
	exit 1
	;;
esac
FAKE_SYSTEMCTL
chmod +x "$work/bin/systemctl"

cat >"$work/bin/id" <<'FAKE_ID'
#!/bin/sh
case "${1:-}" in
-u)
	echo 0
	exit 0
	;;
esac
exec /usr/bin/id "$@"
FAKE_ID
chmod +x "$work/bin/id"

printf 'enabled=enabled\nactive=active\npid=100\n' >"$systemd/state"
printf 'DRILL_SERVICE=otterd\n' >"$systemd/drill-host.env"

begin "the shipped restart-service drill runs, asserts and restores" shipped-drill-run
write_lock maintenance "$valid_lock"
HARNESS_ENV="OTTER_FAKE_SYSTEMD_STATE=$systemd/state DRILL_ENV_FILE=$systemd/drill-host.env"
run_harness restart-service --host "$host" --window maintenance --lock-dir "$locks" \
	--drill-dir "$here/drill/host" --transcript "$transcript"
HARNESS_ENV=""
[ "$harness_status" -eq 0 ] || problem "exit $harness_status, want 0"
transcript_has "restart: otterd (pid 100, enabled)" || problem "the drill did not restart from the stand-in's PID"
transcript_has "assert:  otterd is active with a new pid 101 (was 100)" ||
	problem "the drill did not assert the PID changed"
transcript_has "ok: otterd restarted and came back" || problem "the drill's success line is missing"
transcript_has "restore: otterd is enabled and active" || problem "the restore did not run"
transcript_has "result: PASS" || problem "the harness did not report a PASS"
[ "$(grep '^pid=' "$systemd/state")" = "pid=101" ] || problem "the stand-in did not see a restart"
finish

begin "the shipped drill fails when the unit did not actually restart" shipped-drill-no-restart
printf 'enabled=enabled\nactive=active\npid=100\n' >"$systemd/state"
write_lock maintenance "$valid_lock"
HARNESS_ENV="OTTER_FAKE_SYSTEMD_STATE=$systemd/state DRILL_ENV_FILE=$systemd/drill-host.env FAKE_SYSTEMD_NO_RESTART=1"
run_harness restart-service --host "$host" --window maintenance --lock-dir "$locks" \
	--drill-dir "$here/drill/host" --transcript "$transcript"
HARNESS_ENV=""
[ "$harness_status" -eq 1 ] || problem "exit $harness_status, want 1"
transcript_has "reports the same main PID" || problem "the drill did not name the unchanged PID"
transcript_has "result: FAIL" || problem "the harness did not report a FAIL"
transcript_has "restore: otterd is enabled and active" || problem "the restore did not run after the failure"
finish

begin "the shipped drill's restore phase runs alone" shipped-drill-restore
printf 'enabled=disabled\nactive=inactive\npid=100\n' >"$systemd/state"
write_lock maintenance "$valid_lock"
HARNESS_ENV="OTTER_FAKE_SYSTEMD_STATE=$systemd/state DRILL_ENV_FILE=$systemd/drill-host.env"
run_harness restart-service --host "$host" --window maintenance --lock-dir "$locks" \
	--drill-dir "$here/drill/host" --transcript "$transcript" --restore-only
HARNESS_ENV=""
[ "$harness_status" -eq 0 ] || problem "exit $harness_status, want 0"
[ "$(grep '^enabled=' "$systemd/state")" = "enabled=enabled" ] ||
	problem "the restore did not re-enable the unit"
[ "$(grep '^active=' "$systemd/state")" = "active=active" ] ||
	problem "the restore did not make the unit active"
transcript_has "restore: otterd is enabled and active" || problem "the restore did not report the state"
transcript_has "result: RESTORED" || problem "the harness did not report a RESTORED result"
finish

# --- the mutation the window gate must not survive ---------------------------
#
# Remove the gate from a copy of the harness and run the no-lock case against it.
# A harness that still refuses proves nothing about the refusal above; this one
# must now run the drill with no window declared at all, which is exactly the
# behavior the gate exists to prevent -- and exactly what the no-lock case would
# report as a pass if the gate were ever removed.

mkdir -p "$work/mutants"
gate_mutant=$work/mutants/no-gate.sh
sed 's/^require_window() {$/require_window() { return 0;/' "$subject" >"$gate_mutant"
if cmp -s "$subject" "$gate_mutant"; then
	bad "the window gate was not found in $subject, so the mutation demonstrates nothing"
else
	begin "removing the window gate lets the no-lock case run, so that case is load-bearing" mutant-gate
	subject_in_use=$gate_mutant
	run_harness pass --host "$host" --window never-declared --lock-dir "$locks" \
		--drill-dir "$drills" --transcript "$transcript"
	subject_in_use=$subject
	if [ "$harness_status" -ne 0 ]; then
		problem "the mutant still refused (exit $harness_status); the mutation did not remove the gate"
	fi
	events_has "pass run" || problem "the mutant did not run the drill, so the mutation did not remove the gate"
	[ "$(drill_commands)" -ge 1 ] || problem "the mutant sent no drill command"
	finish
fi

# --- the mutation the restore assertion must not survive ---------------------
#
# Replace the restore phase's invocation with a no-op. The run still passes and
# the transcript still claims a restore, so the harness alone cannot be trusted
# here: the assertion that catches it is the fixture's host-side record. This
# block proves that record is what the suite checks, by showing the mutant
# produces a transcript that looks green while nothing was restored.

restore_mutant=$work/mutants/no-restore-call.sh
sed 's/^	phase_remote restore$/	: restore skipped by the mutant/' "$subject" >"$restore_mutant"
if cmp -s "$subject" "$restore_mutant"; then
	bad "the restore invocation was not found in $subject, so the mutation demonstrates nothing"
else
	begin "removing the restore call leaves the host-side record empty, so the restore assertion is load-bearing" mutant-restore
	subject_in_use=$restore_mutant
	write_lock maintenance "$valid_lock"
	run_harness pass --host "$host" --window maintenance --lock-dir "$locks" \
		--drill-dir "$drills" --transcript "$transcript"
	subject_in_use=$subject
	if [ "$harness_status" -ne 0 ]; then
		problem "the mutant failed (exit $harness_status); it was supposed to look green"
	fi
	if events_has "pass restore"; then
		problem "the mutant still restored, so the mutation did not remove the restore call"
	fi
	# The surprising part, and the reason the suite does not trust the
	# transcript alone: the mutant's transcript still claims the restore ran.
	transcript_has "restore:  exit 0" ||
		problem "the mutant did not produce its misleading restore claim, so this block is not measuring what it says"
	finish
fi

# --- outcome ----------------------------------------------------------------

if [ "$failed" -ne 0 ]; then
	echo "test-drill-host: FAILED: $failed case(s), $passed passed" >&2
	exit 1
fi
echo "test-drill-host: all $passed cases passed"
