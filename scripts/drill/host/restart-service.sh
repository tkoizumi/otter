#!/bin/sh
# Drill: a deployed Otter unit restarts, comes back, and the daemon answers
# again -- the smallest real host window, and the template for writing others.
# drill-host-restore: leave the unit enabled and active, which is the state the window opened in
#
# This drill runs ON the host. scripts/drill-host.sh streams it over ssh and
# runs it twice, in two separate sessions:
#
#   sh -s -- run      restart the unit, then assert it came back
#   sh -s -- restore  put it back: enabled and active
#
# What it changes: it restarts the daemon, which drops in-flight runs. That is
# the whole point of running it inside a declared window, and it is why the
# harness refuses without a lock.
#
# What it asserts:
#   * the unit's main PID differs after the restart, so the daemon really
#     restarted rather than systemd deciding the unit was already fine;
#   * `systemctl is-active` reports `active` within DRILL_READY_SECONDS;
#   * the unit is enabled, so the next boot brings the runtime back;
#   * when DRILL_HEALTH_URL is set, the daemon answers it with HTTP 200.
#
# What it does NOT assert: that no run was in flight (a window's job), that the
# job output is intact, or anything about the unit FILE -- this drill changes no
# file, so its restore is "ensure the unit is enabled and active" rather than a
# repaired configuration. A drill that edits a unit must back it up and restore
# it in its own restore phase.
#
# Configuration is read from /etc/otter/drill-host.env on the host when that
# file exists, so site values live with the host rather than in this
# repository:
#
#   DRILL_SERVICE=otterd                      systemd unit name
#   DRILL_HEALTH_URL=http://127.0.0.1:7337/health
#   DRILL_READY_SECONDS=30                    how long to wait for `active`
#
# scripts/test-drill-host.sh exercises this drill against a systemd stand-in,
# including the case where the PID does not change and the drill must go red.
# What that cannot prove is systemd itself: the stand-in answers the questions,
# and the transcript of the first real window is the evidence for the host.
set -eu

phase=${1:-}
case "$phase" in
run | restore) ;;
*)
	echo "usage: restart-service.sh run|restore (scripts/drill-host.sh runs it)" >&2
	exit 2
	;;
esac

ENV_FILE=${DRILL_ENV_FILE:-/etc/otter/drill-host.env}
if [ -r "$ENV_FILE" ]; then
	# shellcheck disable=SC1090
	. "$ENV_FILE"
fi
SERVICE=${DRILL_SERVICE:-otterd}
HEALTH_URL=${DRILL_HEALTH_URL:-}
READY_SECONDS=${DRILL_READY_SECONDS:-30}

say() { echo "restart-service: $*"; }
fail() {
	echo "restart-service: FAILED: $*" >&2
	exit 1
}
have() { command -v "$1" >/dev/null 2>&1; }

[ "$(id -u)" = 0 ] ||
	fail "restarting $SERVICE needs root; run the window with DRILL_HOST_SUDO=1"
have systemctl ||
	fail "systemctl is not installed on this host, so there is no unit to restart"

# wait_active waits up to READY_SECONDS for the unit to report active, and says
# what it last saw rather than timing out silently.
wait_active() {
	label=$1
	i=0
	state=""
	while [ "$i" -lt "$READY_SECONDS" ]; do
		state=$(systemctl is-active "$SERVICE" 2>/dev/null || true)
		[ "$state" = active ] && return 0
		i=$((i + 1))
		sleep 1
	done
	fail "$label: $SERVICE is '$state' after ${READY_SECONDS}s, not active"
}

# check_health asserts the daemon answers, when the host says where to ask. A
# URL that cannot be checked is a failure, not a skip: a claim nobody verified
# is exactly what this harness exists to prevent.
check_health() {
	[ -n "$HEALTH_URL" ] || return 0
	if ! have curl; then
		fail "DRILL_HEALTH_URL is set to $HEALTH_URL but curl is not installed, so the daemon's answer cannot be asserted"
	fi
	code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 "$HEALTH_URL" 2>/dev/null) ||
		fail "$HEALTH_URL did not answer (curl failed)"
	[ "$code" = 200 ] || fail "$HEALTH_URL answered HTTP $code, want 200"
	say "health: $HEALTH_URL is 200"
}

unit_pid() {
	pid=$(systemctl show -p MainPID --value "$SERVICE" 2>/dev/null || true)
	case "$pid" in
	'' | *[!0-9]* | 0)
		fail "could not read $SERVICE's main PID from systemd (got '${pid:-}')"
		;;
	esac
	printf '%s' "$pid"
}

if [ "$phase" = restore ]; then
	# The window opened with the unit enabled and active; that is what it ends
	# with. `enable` is idempotent, and a failure here is real: a host that
	# cannot re-enable its unit does not survive a reboot.
	say "restore: ensuring $SERVICE is enabled and active"
	systemctl enable "$SERVICE" >/dev/null 2>&1 ||
		fail "could not enable $SERVICE"
	if [ "$(systemctl is-active "$SERVICE" 2>/dev/null || true)" != active ]; then
		systemctl start "$SERVICE" >/dev/null 2>&1 || fail "could not start $SERVICE"
	fi
	wait_active "restore"
	check_health
	say "restore: $SERVICE is enabled and active (pid $(unit_pid))"
	exit 0
fi

# --- run: the mutation, then the assertions ----------------------------------

enabled=$(systemctl is-enabled "$SERVICE" 2>/dev/null || true)
[ "$enabled" = enabled ] ||
	fail "$SERVICE is '$enabled', not enabled: restarting it would prove nothing about the unit this host boots"

before=$(unit_pid)
say "restart: $SERVICE (pid $before, enabled)"
systemctl restart "$SERVICE" >/dev/null 2>&1 ||
	fail "systemctl restart $SERVICE failed"
wait_active "restart"

after=$(unit_pid)
[ "$after" != "$before" ] ||
	fail "$SERVICE reports the same main PID ($before) after a restart, so it did not restart"
say "assert:  $SERVICE is active with a new pid $after (was $before)"
check_health
say "ok: $SERVICE restarted and came back"
