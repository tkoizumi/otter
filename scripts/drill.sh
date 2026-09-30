#!/bin/sh
# Run operating drills and assert their outcomes.
#
# A drill is evidence, not a procedure. Each one builds its own world, asserts
# the behavior it claims, and exits non-zero with the failing output when the
# behavior does not hold — so its transcript can be recorded as the evidence a
# task is closed by (R-19, CA-26, docs/phase-0-tasks.md).
#
#   scripts/drill.sh                 run every drill in scripts/drill/
#   scripts/drill.sh backup-restore  run one drill
#   scripts/drill.sh --list          list the drills
#
# Each drill is a file scripts/drill/<name>.sh taking no required arguments. It
# must be self-contained: assume nothing about the operator's shell state, clean
# up what it creates, and never reach a real vendor API or a real destination.
#
# A drill that cannot run on this platform exits non-zero and says why. It does
# not skip quietly: a drill that did not run has produced no evidence, and a
# silent pass is the failure mode this whole mechanism exists to prevent.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
dir="$root/scripts/drill"

# A transcript is only evidence if it says where and when it ran and against
# which build, so every drill run is preceded by this header.
header() {
	printf '\n===== drill: %s =====\n' "$1"
	printf 'date:     %s\n' "$(date '+%Y-%m-%d %H:%M:%S %Z')"
	printf 'host:     %s %s (%s)\n' "$(uname -s)" "$(uname -r)" "$(uname -m)"
	printf 'checkout: %s\n' "$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo unknown)"
}

# list prints the drill names, one per line, and fails when there are none.
list() {
	found=0
	for candidate in "$dir"/*.sh; do
		[ -f "$candidate" ] || continue
		found=1
		basename "$candidate" .sh
	done
	[ "$found" -eq 1 ]
}

run_one() {
	name=$1
	script=$dir/$name.sh
	if [ ! -f "$script" ]; then
		echo "drill: no such drill: $name" >&2
		if available=$(list); then
			echo "drill: available: $(printf '%s' "$available" | tr '\n' ' ')" >&2
		else
			echo "drill: no drills defined yet — add scripts/drill/<name>.sh" >&2
		fi
		exit 2
	fi
	header "$name"
	sh "$script"
}

case "${1:-}" in
--list | -l)
	list || { echo "drill: no drills defined yet" >&2; exit 1; }
	;;
"")
	names=$(list) || {
		echo "drill: no drills defined yet — add scripts/drill/<name>.sh" >&2
		exit 1
	}
	failed=""
	for name in $names; do
		run_one "$name" || failed="$failed $name"
	done
	if [ -n "$failed" ]; then
		echo "drill: FAILED:$failed" >&2
		exit 1
	fi
	echo "drill: all drills passed"
	;;
*)
	run_one "$1"
	;;
esac
