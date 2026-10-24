#!/bin/sh
# host-window-probe.sh -- read ONE host's maintenance-window lock and print a
# report `scripts/drill-host.sh` can parse. Read-only: it does not create,
# touch or remove anything, which is why it is the first thing the harness runs
# on the host and the only thing it runs when it refuses.
#
# It runs ON the host it describes. The harness pipes it over ssh:
#
#   ssh -o BatchMode=yes -- "$host" "sh -s -- '$lock'" < host-window-probe.sh
#
# Usage:
#   host-window-probe.sh <lock-path>
#
# Output is one `key=value` per line, the same shape lib/host-report.sh prints:
#
#   now=<epoch>            the HOST's clock; the expiry is judged against this,
#                          not the operator's clock, because the window is
#                          declared on the host
#   host=<uname -s -r -m>  what the drill is about to run on
#   lock_path=<path>       the file this probe was asked about
#   lock=absent            nothing at that path
#   lock=unreadable        it exists but cannot be read (a directory, a fifo, or
#                          a file this login has no read permission on)
#   lock=present           followed by one `content: <line>` per line of the
#                          lock file, verbatim
#
# The probe does not interpret the lock. Parsing lives in the harness, so the
# format has one definition; this program only answers "is there a file, can it
# be read, and exactly what does it say".
#
# Exit 0 whenever a report was produced, even a report saying the lock is
# absent: the caller decides, not the probe. Exit 2 means the probe could not
# be asked (bad usage), and then there is no report at all.
set -eu

[ "$#" -eq 1 ] || {
	echo "usage: host-window-probe.sh <lock-path>" >&2
	exit 2
}
LOCK=$1
[ -n "$LOCK" ] || {
	echo "host-window-probe: the lock path is empty" >&2
	exit 2
}

# Every measurement is guarded: an unmeasurable value prints `unknown` rather
# than aborting the probe, because a report that names what it could not read is
# more useful than no report.
printf 'now=%s\n' "$(date +%s 2>/dev/null || echo unknown)"
printf 'host=%s %s %s\n' \
	"$(uname -s 2>/dev/null || echo unknown)" \
	"$(uname -r 2>/dev/null || echo unknown)" \
	"$(uname -m 2>/dev/null || echo unknown)"
printf 'lock_path=%s\n' "$LOCK"

# -L as well as -e: a symlink the operator created counts as a declaration even
# when its target is missing, and the unreadable branch below then says so
# instead of reporting "absent".
if [ ! -e "$LOCK" ] && [ ! -L "$LOCK" ]; then
	printf 'lock=absent\n'
	exit 0
fi
if [ ! -f "$LOCK" ] || [ ! -r "$LOCK" ]; then
	printf 'lock=unreadable\n'
	exit 0
fi
if ! body=$(cat -- "$LOCK" 2>/dev/null); then
	printf 'lock=unreadable\n'
	exit 0
fi

printf 'lock=present\n'
# An empty file still prints one empty line so the harness sees a lock that is
# present and declares nothing, which is a refusal with a reason, not an absent
# window.
printf '%s\n' "$body" | while IFS= read -r line; do
	printf 'content: %s\n' "$line"
done
exit 0
