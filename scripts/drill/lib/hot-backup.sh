#!/bin/sh
# hot-backup.sh -- write a COMPLETE backup of one Otter runtime, from a live
# daemon, into a staging directory. This is the source half of the clean-host
# drill (scripts/drill/modes/backup-restore-clean-host.sh).
#
# What "complete" means here is defined by the restore that consumes it and by
# docs/operations.md "Backups": the database is copied atomically while the
# daemon keeps writing, and beside it go the release snapshots, the prepared
# environments, the reconstructable interpreter/uv/cache trees, and the job
# source directories that carry `.otter-id`. A database-only backup restores
# history whose code is gone.
#
# Deliberately NOT in the backup, because the runtime rebuilds them or they are
# process state: `sdk/` (re-extracted from the binary at startup), `otter.lock`,
# `serve.pid`, `listen.url`, `serve.data`, and `otter.db-wal`/`otter.db-shm`
# (folded into the database by `.backup`; copying a WAL from another database
# generation is how a restore gets silently corrupted).
#
# Deliberately NOT in the backup, because it must not travel with the state:
# `/etc/otter/*.env` holds job secrets. A job that needs a secret will fail on
# the restored host until the secrets are restored separately, encrypted. The
# drill only asserts a job that needs no secret, and says so.
#
# Usage:
#   hot-backup.sh <data_dir> <jobs_dir> <backup_dir> <sabotage> <source_id>
#
#   sabotage  "" (a complete backup) or one of drop-releases, drop-environments,
#             drop-tools, drop-jobs. A sabotage run MUST make the restored run
#             assertion go red; it exists so the drill is falsifiable on a real
#             host and not only in the local same-host mode. It is never set by
#             a clean run.
#   source_id a short label recorded in RECORD (machine id or hostname), so the
#             transcript says which host produced the archive.
#
# The staging directory must not exist or must be empty: this script never
# deletes anything it did not create.
set -eu

usage() {
	echo "usage: hot-backup.sh <data_dir> <jobs_dir> <backup_dir> <sabotage> <source_id>" >&2
	exit 2
}

[ "$#" -eq 5 ] || usage
DATA=$1
JOBS=$2
DEST=$3
SABOTAGE=$4
SOURCE_ID=$5

case "$SABOTAGE" in
"" | drop-releases | drop-environments | drop-tools | drop-jobs) ;;
*)
	echo "hot-backup: unknown sabotage '$SABOTAGE'" >&2
	echo "hot-backup: valid modes: (none), drop-releases, drop-environments, drop-tools, drop-jobs" >&2
	exit 2
	;;
esac

have() { command -v "$1" >/dev/null 2>&1; }

for tool in sqlite3 find tar; do
	have "$tool" || {
		echo "hot-backup: $tool is required and was not found on PATH" >&2
		exit 2
	}
done

# sha256_of lets the manifest be written on a host that ships shasum instead of
# GNU sha256sum.
if have sha256sum; then
	sha256_of() { sha256sum "$1"; }
elif have shasum; then
	sha256_of() { shasum -a 256 "$1"; }
else
	echo "hot-backup: sha256sum (or shasum) is required to write the backup manifest" >&2
	exit 2
fi

[ -n "$DATA" ] || usage
[ -n "$JOBS" ] || usage
[ -n "$DEST" ] || usage
[ -d "$DATA" ] || {
	echo "hot-backup: no data directory at $DATA" >&2
	exit 1
}
[ -d "$JOBS" ] || {
	echo "hot-backup: no jobs root at $JOBS" >&2
	exit 1
}
[ -f "$DATA/otter.db" ] || {
	echo "hot-backup: no database at $DATA/otter.db: this host is not a runtime, or the path is wrong" >&2
	exit 1
}
if [ -e "$DEST" ] && [ -n "$(ls -A "$DEST" 2>/dev/null)" ]; then
	echo "hot-backup: refusing to back up into non-empty $DEST" >&2
	exit 1
fi
install -d -m 0700 "$DEST" "$DEST/data" "$DEST/jobs"

# --- the database: atomic, while the daemon keeps writing ---------------------

# `.backup` is SQLite's online backup: transactional, and it restarts the copy
# if the source changes underneath it. A checkpoint plus `cp` would race the
# writers.
sqlite3 "$DATA/otter.db" ".backup '$DEST/otter.db'" || {
	echo "hot-backup: sqlite3 .backup failed" >&2
	exit 1
}

# --- everything the database references --------------------------------------

MEMBERS=""
for member in .releases environments tools python cache; do
	[ -e "$DATA/$member" ] || continue
	if [ "$SABOTAGE" = "drop-$member" ]; then
		echo "hot-backup: sabotage: omitting $member"
		continue
	fi
	cp -a "$DATA/$member" "$DEST/data/$member" || {
		echo "hot-backup: could not copy $DATA/$member" >&2
		exit 1
	}
	MEMBERS="$MEMBERS $member"
done

# --- the job sources, and the identity markers inside them -------------------

JOB_FILES=0
if [ "$SABOTAGE" = "drop-jobs" ]; then
	echo "hot-backup: sabotage: omitting the job source directories"
else
	cp -a "$JOBS/." "$DEST/jobs/" || {
		echo "hot-backup: could not copy the jobs root $JOBS" >&2
		exit 1
	}
	JOB_FILES=$(find "$DEST/jobs" -type f 2>/dev/null | wc -l | tr -d ' ')
	MARKERS=$(find "$DEST/jobs" -name .otter-id -type f 2>/dev/null | wc -l | tr -d ' ')
	[ "$MARKERS" -gt 0 ] || {
		echo "hot-backup: the jobs root $JOBS holds no .otter-id marker: there is no job identity to back up" >&2
		exit 1
	}
fi

# --- generated metadata -------------------------------------------------------

# MANIFEST.sha256 covers every regular file, symlinks included via their
# targets' bytes, so the transfer can be verified on the other side. It is
# written with `./`-relative paths and checked with `sha256sum -c` from the
# backup root. It is built outside the backup and moved in, so it never lists
# itself: a manifest that includes its own (empty) file fails its own check.
MANIFEST_TMP=$(mktemp 2>/dev/null || echo /tmp/otter-manifest.$$)
(
	cd "$DEST"
	find . -type f | LC_ALL=C sort | while IFS= read -r f; do
		sha256_of "$f"
	done
) >"$MANIFEST_TMP" || {
	echo "hot-backup: could not write the manifest" >&2
	rm -f "$MANIFEST_TMP"
	exit 1
}
mv "$MANIFEST_TMP" "$DEST/MANIFEST.sha256" || {
	echo "hot-backup: could not install the manifest" >&2
	exit 1
}
if grep -q 'MANIFEST.sha256' "$DEST/MANIFEST.sha256"; then
	echo "hot-backup: the manifest lists itself; it would fail its own check" >&2
	exit 1
fi
MANIFEST_FILES=$(wc -l <"$DEST/MANIFEST.sha256" | tr -d ' ')

# SYMLINKS.txt records the link targets. A release's `active` symlink is an
# absolute path into the data directory: on a restore at the same path it stays
# valid, at a different path it must be repointed, and the drill reads this file
# to know what to expect.
SYMLINKS_TMP=$(mktemp 2>/dev/null || echo /tmp/otter-symlinks.$$)
(
	cd "$DEST"
	find . -type l | LC_ALL=C sort | while IFS= read -r l; do
		printf '%s -> %s\n' "$l" "$(readlink "$l" 2>/dev/null || echo '?')"
	done
) >"$SYMLINKS_TMP"
mv "$SYMLINKS_TMP" "$DEST/SYMLINKS.txt"
SYMLINK_COUNT=$(wc -l <"$DEST/SYMLINKS.txt" | tr -d ' ')

# A regression guard on the exclusions above: if any of these ever appears in a
# backup, the definition of "complete" has drifted.
for forbidden in otter.db-wal otter.db-shm otter.lock serve.pid listen.url serve.data sdk; do
	if [ -e "$DEST/$forbidden" ] || [ -e "$DEST/data/$forbidden" ]; then
		echo "hot-backup: refusing to keep $forbidden in the backup: it is transient or derived state" >&2
		exit 1
	fi
done

cat >"$DEST/RECORD" <<RECORD
data_dir=$DATA
jobs_dir=$JOBS
source_id=$SOURCE_ID
taken_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
members=$MEMBERS
db_bytes=$(wc -c <"$DEST/otter.db" | tr -d ' ')
job_files=$JOB_FILES
manifest_files=$MANIFEST_FILES
symlinks=$SYMLINK_COUNT
secrets=excluded
RECORD

echo "hot-backup: dest=$DEST"
echo "hot-backup: db=$(wc -c <"$DEST/otter.db" | tr -d ' ') bytes, members=${MEMBERS:-none}, job files=$JOB_FILES, files=$MANIFEST_FILES, symlinks=$SYMLINK_COUNT"
echo "hot-backup: secrets excluded (/etc/otter/*.env is not part of this archive)"
