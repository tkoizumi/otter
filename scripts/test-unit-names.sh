#!/bin/sh
# Falsifiability for the systemd unit names in this directory.
#
# A timer starts the service named by its `Unit=` line, and systemd resolves that
# name against *installed unit files* -- not against these file names. Nothing
# local notices a mismatch: scripts/drill/liveness.sh invokes heartbeat.sh
# directly, so it exercises the check and never the unit.
#
# This repository shipped exactly that bug. heartbeat.timer said
# `Unit=otter-heartbeat.service` while the install commands place
# heartbeat.service; the service header enabled `otter-heartbeat.timer` while the
# timer installs as heartbeat.timer. On a host, `systemctl enable --now
# otter-heartbeat.timer` would have failed outright, and a timer that did load
# would have found no service to start: the check would never run, every ping
# would stop, and the external switch would raise a FALSE alarm on a healthy
# runtime -- while the liveness drill stayed green, because the script it tests
# works fine.
#
# The harness checks the pairings without a host:
#
#   - every scripts/*.timer resolves to a service file installed beside it
#     (an omitted Unit= is systemd's default: the timer's own name);
#   - every `enable --now <name>` in a service header names a file that exists
#     beside it, because that is the command an operator will actually paste.
#
# It is deliberately strict: a timer here must start a service here. A timer
# aimed at a system unit (logrotate.service, say) fails this check rather than
# silently relying on that unit existing on every host.
#
#   sh scripts/test-unit-names.sh
#
# OTTER_SCRIPTS_DIR points the same check at a mutated copy, so a case that must
# go red can be demonstrated rather than asserted.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
real=${OTTER_SCRIPTS_DIR:-$here}

for f in "$real"/*.timer "$real"/*.service; do
	[ -f "$f" ] || {
		echo "test-unit-names: no unit files under $real" >&2
		exit 2
	}
	break
done

work=$(mktemp -d "${TMPDIR:-/tmp}/otter-unit-names.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

passed=0
failed=0

# check_dir prints one ok/FAIL line per pairing and exits non-zero if any is
# broken. It is the whole assertion; the cases below only vary its input.
check_dir() {
	dir=$1
	problems=0

	for timer in "$dir"/*.timer; do
		[ -f "$timer" ] || continue
		base=$(basename "$timer" .timer)
		unit=$(sed -n 's/^[[:space:]]*Unit=[[:space:]]*\([^[:space:]]*\).*$/\1/p' "$timer" | head -1)
		# systemd's default is the timer's own name with .service appended.
		[ -n "$unit" ] || unit="$base.service"
		if [ -f "$dir/$unit" ]; then
			echo "ok:   $(basename "$timer") -> $unit"
		else
			echo "FAIL: $(basename "$timer") starts $unit, but $dir/$unit does not exist" >&2
			problems=$((problems + 1))
		fi
	done

	for service in "$dir"/*.service; do
		[ -f "$service" ] || continue
		while read -r enabled; do
			[ -n "$enabled" ] || continue
			if [ -f "$dir/$enabled" ]; then
				echo "ok:   $(basename "$service") enables $enabled"
			else
				echo "FAIL: $(basename "$service") says 'enable --now $enabled', but $dir/$enabled does not exist" >&2
				problems=$((problems + 1))
			fi
		done <<EOF
$(sed -n 's/.*enable --now[[:space:]]\{1,\}\([^[:space:]]*\).*/\1/p' "$service")
EOF
	done

	[ "$problems" -eq 0 ]
}

# expect <label> <dir> <ok|fail>
expect() {
	label=$1 dir=$2 want=$3
	if out=$(check_dir "$dir" 2>&1); then
		got=ok
	else
		got=fail
	fi
	if [ "$got" = "$want" ]; then
		passed=$((passed + 1))
		echo "ok:   $label"
	else
		failed=$((failed + 1))
		echo "FAIL: $label: expected $want, got $got" >&2
		echo "$out" | sed 's/^/    /' >&2
	fi
}

# mkfixture <name> copies the real units into a scratch directory, so a case can
# be broken on purpose without touching the checkout.
mkfixture() {
	dir="$work/$1"
	mkdir -p "$dir"
	for f in "$real"/*.timer "$real"/*.service; do
		[ -f "$f" ] || continue
		cp "$f" "$dir/"
	done
	echo "$dir"
}

# rewrite <file> <sed-expression>: portable in-place edit (macOS sed has no -i).
rewrite() {
	f=$1
	shift
	sed "$@" "$f" >"$f.tmp"
	mv "$f.tmp" "$f"
}

# --- the case that matters: the units this repository ships ------------------
expect "the shipped units name files that exist beside them" "$real" ok

# --- the bug this harness was written for ------------------------------------
dir=$(mkfixture bad-unit)
rewrite "$dir/heartbeat.timer" 's|^Unit=.*|Unit=otter-heartbeat.service|'
expect "a timer naming a service that is not installed" "$dir" fail

dir=$(mkfixture bad-enable)
rewrite "$dir/heartbeat.service" 's|enable --now .*|enable --now otter-heartbeat.timer|'
expect "a service header enabling a timer that is not installed" "$dir" fail

# --- unit-name defaults and strictness ---------------------------------------
dir="$work/implicit"
mkdir -p "$dir"
printf '[Service]\nType=oneshot\nExecStart=/bin/true\n' >"$dir/foo.service"
printf '[Timer]\nOnUnitActiveSec=1min\n' >"$dir/foo.timer"
expect "a timer with no Unit= takes the service beside it" "$dir" ok

dir="$work/implicit-missing"
mkdir -p "$dir"
printf '[Timer]\nOnUnitActiveSec=1min\n' >"$dir/foo.timer"
expect "a timer with no Unit= and no service of that name" "$dir" fail

dir="$work/system-unit"
mkdir -p "$dir"
printf '[Timer]\nOnUnitActiveSec=1min\nUnit=logrotate.service\n' >"$dir/foo.timer"
expect "a timer aimed at a system unit rather than one installed here" "$dir" fail

# --- outcome -----------------------------------------------------------------
if [ "$failed" -ne 0 ]; then
	echo "test-unit-names: FAILED: $failed case(s), $passed passed" >&2
	exit 1
fi

echo "test-unit-names: all $passed cases passed"
