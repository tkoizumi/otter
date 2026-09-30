#!/bin/sh
# restore-host.sh -- restore a complete backup (hot-backup.sh) onto a host, at
# the same absolute paths it was taken from. This is the target half of the
# clean-host drill.
#
# It refuses rather than guesses. In particular:
#
#   * it verifies the backup against its manifest before writing anything, so a
#     truncated transfer is reported as a truncated transfer;
#   * it refuses when RECORD's data/jobs paths differ from the paths it was
#     asked to restore into. Releases record their absolute source directory and
#     environment markers record an absolute interpreter path, so a restore into
#     different paths silently serves the wrong instance (docs/operations.md,
#     "Restore onto a clean host");
#   * it re-checks that the target is empty immediately before writing, because
#     the drill's preflight probe ran some time earlier.
#
# Usage:
#   restore-host.sh <backup_dir> <data_dir> <jobs_dir> <owner|->
#
#   owner  `user:group` to chown the restored trees to (the source's owner), or
#          `-` to leave ownership alone. A real run always passes the owner; `-`
#          is for a selfcheck running as an ordinary user, and a restore that
#          will not run as the service account is not a restore.
set -eu

usage() {
	echo "usage: restore-host.sh <backup_dir> <data_dir> <jobs_dir> <owner|->" >&2
	exit 2
}

[ "$#" -eq 4 ] || usage
BACKUP=$1
DATA=$2
JOBS=$3
OWNER=$4

have() { command -v "$1" >/dev/null 2>&1; }

[ -d "$BACKUP" ] || {
	echo "restore-host: no backup at $BACKUP" >&2
	exit 1
}
for required in otter.db MANIFEST.sha256 RECORD; do
	[ -f "$BACKUP/$required" ] || {
		echo "restore-host: the backup has no $required; hot-backup.sh did not finish" >&2
		exit 1
	}
done
[ -n "$DATA" ] || usage
[ -n "$JOBS" ] || usage

record_value() { sed -n "s/^$1=//p" "$BACKUP/RECORD" | head -n 1; }

SRC_DATA=$(record_value data_dir)
SRC_JOBS=$(record_value jobs_dir)
[ -n "$SRC_DATA" ] || {
	echo "restore-host: RECORD has no data_dir" >&2
	exit 1
}
[ -n "$SRC_JOBS" ] || {
	echo "restore-host: RECORD has no jobs_dir" >&2
	exit 1
}
[ "$SRC_DATA" = "$DATA" ] || {
	echo "restore-host: the backup was taken from data directory $SRC_DATA, this restore targets $DATA" >&2
	echo "restore-host: restore at the path it was backed up from, or repoint the absolute paths it records first" >&2
	exit 1
}
[ "$SRC_JOBS" = "$JOBS" ] || {
	echo "restore-host: the backup was taken from jobs root $SRC_JOBS, this restore targets $JOBS" >&2
	echo "restore-host: job identity and each release's source path are path-bound and cannot be repointed" >&2
	exit 1
}

# --- verify the bytes before writing any of them ------------------------------

if have sha256sum; then
	verify_cmd="sha256sum -c MANIFEST.sha256"
elif have shasum; then
	verify_cmd="shasum -a 256 -c MANIFEST.sha256"
else
	echo "restore-host: sha256sum (or shasum) is required to verify the backup" >&2
	exit 1
fi
VERIFY_OUT=$(mktemp 2>/dev/null || echo /tmp/otter-restore-verify.$$)
if ! (cd "$BACKUP" && eval "$verify_cmd") >"$VERIFY_OUT" 2>&1; then
	echo "restore-host: the backup does not match its manifest; refusing to restore a corrupt archive" >&2
	grep -v ': OK$' "$VERIFY_OUT" | head -n 20 >&2 || true
	rm -f "$VERIFY_OUT"
	exit 1
fi
VERIFIED=$(grep -c ': OK$' "$VERIFY_OUT" 2>/dev/null || true)
rm -f "$VERIFY_OUT"
[ "${VERIFIED:-0}" -gt 0 ] || {
	echo "restore-host: the manifest verified nothing; the backup is not worth restoring" >&2
	exit 1
}

# --- the target must still be empty ------------------------------------------

if [ -e "$DATA" ] && [ -n "$(ls -A "$DATA" 2>/dev/null)" ]; then
	echo "restore-host: refusing to restore into non-empty $DATA ($(ls -A "$DATA" | head -n 5 | tr '\n' ' '))" >&2
	exit 1
fi
if [ -e "$JOBS" ] && [ -n "$(ls -A "$JOBS" 2>/dev/null)" ]; then
	echo "restore-host: refusing to restore into non-empty jobs root $JOBS ($(ls -A "$JOBS" | head -n 5 | tr '\n' ' '))" >&2
	exit 1
fi

# --- write it ----------------------------------------------------------------

install -d -m 0700 "$DATA"
if [ -d "$BACKUP/data" ]; then
	cp -a "$BACKUP/data/." "$DATA/" || {
		echo "restore-host: could not restore the data directory members" >&2
		exit 1
	}
fi
cp -a "$BACKUP/otter.db" "$DATA/otter.db" || {
	echo "restore-host: could not restore the database" >&2
	exit 1
}
# A WAL or SHM from another database generation must never sit beside a
# restored file. hot-backup.sh never copies them; this is the belt to that
# braces.
rm -f "$DATA/otter.db-wal" "$DATA/otter.db-shm"

install -d -m 0750 "$JOBS"
if [ -d "$BACKUP/jobs" ]; then
	cp -a "$BACKUP/jobs/." "$JOBS/" || {
		echo "restore-host: could not restore the job source directories" >&2
		exit 1
	}
fi

if [ "$OWNER" != "-" ]; then
	[ "$(id -u)" = 0 ] || {
		echo "restore-host: asked to restore ownership to $OWNER but not running as root" >&2
		exit 1
	}
	chown -R "$OWNER" "$DATA" "$JOBS" || {
		echo "restore-host: could not chown the restored trees to $OWNER" >&2
		exit 1
	}
fi

MARKERS=$(find "$JOBS" -name .otter-id -type f 2>/dev/null | wc -l | tr -d ' ')
MEMBERS=$(record_value members)
LINK_TARGET=$(find "$DATA/.releases/active" -type l -print 2>/dev/null | head -n 1 || true)

echo "restore-host: data=$DATA jobs=$JOBS owner=$OWNER"
echo "restore-host: verified $VERIFIED files against MANIFEST.sha256"
echo "restore-host: members=${MEMBERS:-none}, identity markers=$MARKERS, db=$(wc -c <"$DATA/otter.db" | tr -d ' ') bytes"
if [ -n "$LINK_TARGET" ]; then
	echo "restore-host: active release link $(basename "$(dirname "$LINK_TARGET")")/$(basename "$LINK_TARGET") -> $(readlink "$LINK_TARGET")"
fi
