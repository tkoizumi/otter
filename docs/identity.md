# Integration identity

Every registered integration has a durable identity that is separate from its
name and from the directory holding its source. This document explains the
model, what it fixes, and how to operate it.

- [Why](#why)
- [The three things](#the-three-things)
- [The registry](#the-registry)
- [The marker](#the-marker)
- [What identity keys](#what-identity-keys)
- [Resolving a reference](#resolving-a-reference)
- [Reconciliation rules](#reconciliation-rules)
- [Lifecycle commands](#lifecycle-commands)
- [Migrating an older workspace](#migrating-an-older-workspace)
- [API](#api)
- [Deployment](#deployment)
- [Trust boundary](#trust-boundary)

## Why

Integration identity used to be the manifest `name`. That made a human-chosen,
mutable, reusable string the primary key for state, run history, webhook tokens,
releases and Python environments. Two integrations that declared the same name
shared one identity, and deleting an integration did not retire its data, so a
later integration created under the same name inherited it.

The concrete failure: an integration was copied to a second directory, both
copies declared `name: counter`, and the copy read the original's accumulated
state. A name is a label; it cannot be a key.

## The three things

| Concept | What it is | How it changes |
| --- | --- | --- |
| **Identity** (`id`) | Opaque, daemon-minted, unique forever | Never |
| **Label** (`name`) | The `name:` field in `otter.yaml` | Edited by hand; duplicates allowed |
| **Location** | The canonical source directory | Only through `otter move` |

Identity is what state, runs, tokens, releases and environments belong to. The
label is what an operator types and reads. The location is where the code lives
right now.

## The registry

`integration_instances` in the data-directory database records one row per
identity that has ever been registered:

- `id` — the durable identity, the primary key.
- `name` — the current label.
- `canonical_path` — the current source directory, symlink-resolved.
- `status` — `active`, `retired`, `deleting` or `deleted`.
- `generation` — bumped whenever authority is revoked.

`integration_paths` records the single current owner of each canonical path, and
whether a path is suppressed because its identity was deleted.
`identity_operations` is a journal that makes a change spanning SQLite and the
filesystem recoverable across a crash.

Identities are never reused, including retired and deleted ones. That is what
stops a name from being recycled into somebody else's state.

## The marker

Each registered source directory carries a `.otter-id` file:

```text
0195a7c2-8e31-7b64-9f02-6dcb482ea510
```

One identifier and one newline. It is written by the runtime, never by hand, and
it is gitignored like `.otter/`.

The marker is what makes the directory's identity survive ordinary filesystem
work without the runtime having to guess:

| Event | Marker | Result |
| --- | --- | --- |
| `cp -r counter integrations/counter` | copied to the new path | The copy is a **new instance**: fresh identity, empty state |
| `mv counter counter-v2` (recorded in the registry) | unchanged | Same identity, same state |
| `rm -rf counter` then recreate | gone | **New instance** on the next scan, even if the daemon was stopped |
| replacing a tree with a different marker | different | The old identity is **retired**; the replacement registers fresh |
| restore an exact backup at the same path | unchanged | Continuity; use `otter reset` for a deliberate fresh start |

The registry is authoritative and the marker is a claim. A marker copied from
another integration never grants access to that integration's state: the second
path is a clone and gets its own identity.

Markers are read without following symlinks, verified on the opened descriptor,
and refused if multiply-linked. Writes go through a temporary file in the same
directory and an atomic rename, so a crash never leaves a half-written marker.

**Limit:** restoring a backup that contains the *same* marker to the *same*
path is indistinguishable from continuity. It keeps its identity. That is the
one case the design deliberately does not detect; use `otter reset` when a fresh
instance is what you want.

## What identity keys

- `integration_state` rows
- `runs.integration_id`, `run_queue.integration_id`
- `webhook_tokens.integration_id`
- release directories: `.releases/<id>/<digest>` and `.releases/active/<id>`
- prepared Python environments and their recorded identity
- per-run state tokens, and the child environment variable
  `OTTER_INTEGRATION_ID`

A run also records `integration_name` (the label at submission) and
`integration_generation`. The generation is checked when a queued run is claimed
and again when a run token writes state, so a run authorized before a reset,
move, retirement or deletion cannot execute or write afterwards.

Child processes receive both:

| Variable | Value |
| --- | --- |
| `OTTER_INTEGRATION_ID` | the durable identity |
| `OTTER_INTEGRATION_NAME` | the manifest label |

## Resolving a reference

One resolver is shared by the CLI and the API:

- `otter run counter` — a bare label. It succeeds only when exactly one *active*
  integration has that label. Two matches is a conflict that lists every
  candidate id and path.
- `otter run .`, `otter run ./integrations/counter` — a path. It is canonicalized
  and matched through path ownership, never by comparing manifest names. `.` and
  `..`, an absolute path, anything containing a path separator, or a literal
  `otter.yaml` are path references.
- `otter run id:<id>` — an explicit identity, always unambiguous. An opaque id
  typed without the prefix is accepted as a fallback.

Relative paths are made absolute by the CLI before being sent to the daemon,
because a relative path means different things to the two processes.

## Reconciliation rules

On every **complete** scan of the integrations root:

| Observation | Action |
| --- | --- |
| Registered path, matching marker | Preserve the identity; refresh the label |
| Registered path, marker missing or different | Retire the old identity and register a fresh one |
| Registered path, manifest invalid | Keep the identity, withhold execution, report the error |
| Unregistered path, valid manifest | Mint a fresh identity and write the marker |
| Unregistered path, valid copied marker | Mint a fresh identity and replace the marker |
| Malformed or symlinked marker | Block the path and report it; never reassign |
| Registered path whose directory is gone | Retire, keeping data and history |
| Suppressed path (its identity was deleted) | Report it; do not re-register |
| Incomplete scan | Change nothing; withhold execution from anything unverified |

A *partial* walk — an unreadable directory, a permission failure — is evidence
of a failed observation, never of deletion, so it retires nothing.

## Lifecycle commands

| Command | Effect |
| --- | --- |
| `otter register [<path>]` | Give a source directory an identity. Idempotent when the binding already matches. Clears a deletion suppression and mints fresh. |
| `otter reset <ref>` | Retire the identity and mint a fresh one at the same path. Old data is kept for inspection or deletion. |
| `otter delete <ref>` | Purge state, run history and logs, webhook token, releases and queue rows. Source files are left in place; the path is suppressed so a scan cannot silently re-register it. |
| `otter move <ref> <dest>` | Preserve the identity across a same-filesystem rename performed by the daemon. |
| `otter identity list [--all]` | Print registrations. Reads the registry directly, so it works with the runtime stopped. `--all` includes retired and deleted identities. |

### Removing an integration

There are two separate acts, and they are deliberately not one command:

1. **Remove the source.** Delete the directory. On the next complete scan the
   identity is *retired*: it stops accepting work, its path is released, and
   its state, run history, webhook token and releases are kept. This is the
   reversible half — nothing is destroyed, and restoring the directory with its
   marker brings the same identity back.
2. **Reclaim the data.** `otter delete <ref>` purges what the identity owns.
   Source files are left alone, and the path is suppressed so a scan cannot
   silently re-register it. The identity row survives as a tombstone and is
   never reused.

So:

```bash
# Stop using it, keep its data. Reversible.
rm -rf integrations/reporting

# Retire and purge. The source may or may not still exist.
otter delete reporting            # active, by label
otter delete integrations/reporting  # active, by path
otter delete id:<id>              # always works, active or retired
```

Order does not matter. `otter delete` resolves a retired identity from the
registry, so deleting the directory first and the data second works; a bare
label only resolves while the identity is active, which is what
`otter identity list --all` is for — it shows the id of a retired identity so
it can still be purged.

### Releases left behind

`otter delete` removes an identity's releases, but an integration whose
directory was deleted *outside* the registry leaves its release data with no
identity at all. `otter release --list --all` shows those rows with no id:

```
INTEGRATION     ID  ACTIVE RELEASE  REL  STATUS          PATH
tshirt_company  -   1b23f2551adf    1    (no identity)   -
```

The digest in the `ACTIVE RELEASE` column is a release, not an identity, so
`otter delete` cannot target it. Remove the leftovers with:

```bash
otter release --list --all --prune            # plan
otter release --list --all --prune --apply    # remove
```

Pruning refuses to run until the identity registry is bootstrapped, because
with an empty registry every release would look like an orphan.

`otter delete` does not remove the directory, and it is not how a *label*
collision is resolved: two active integrations sharing a label make the bare
label ambiguous, and the owner is chosen during migration with
`otter identity migrate --assign`, not by deleting data.

`otter reset` is the "start over" lever. Deleting `.otter-id` by hand has the
same effect on the next scan, but reset also revokes credentials and records the
change.

## Migrating an older workspace

A workspace whose durable rows are still keyed by manifest name is bootstrapped
once, keeping `id = old_name` so nothing has to be renamed:

```bash
otter identity migrate                 # dry run: print the plan
otter identity migrate --apply         # write markers and the registry
```

Every legacy key is either assigned to the one directory that declares it, or
reserved as retired if no directory claims it. A key declared by more than one
directory is a collision: there is only one old state namespace, so the operator
chooses its owner explicitly.

```bash
otter identity migrate --apply --assign counter=./integrations/counter
```

Startup bootstraps an unambiguous workspace automatically. A collision stops the
daemon with the command to run.

The command takes the data-directory lock, so it refuses while a runtime owns
the directory: stop the runtime, or run it against a copy.

Before migrating, take a backup that includes the write-ahead log — the daemon
checkpoints on a clean shutdown, so either stop it cleanly or copy the database
and its `-wal`/`-shm` files together. Markers are the only files written into
the integrations tree; the repository's own git history is the rollback for
those.

## API

Releases address integrations by identity. A reference is resolved once, with:

| Endpoint | Purpose |
| --- | --- |
| `GET /v1/integrations/resolve?ref=<ref>` | Resolve a label, path or id. `404` for no match, `409` listing candidates when a label is ambiguous. |
| `POST /v1/integrations` | Register a path: `{"path": "..."}`. |
| `POST /v1/integrations/{id}/reset` | Retire and mint fresh; returns `old_id` and `new_id`. |
| `POST /v1/integrations/{id}/move` | `{"destination": "..."}`. |
| `DELETE /v1/integrations/{id}` | Purge the identity's durable artifacts. |

Integration views carry both `id` and `name`, plus `generation` and `status`.
Run views carry `integration_id` and `integration_name`.

## Deployment

The destination registers its own identities; local and remote ids are
independent and need not match. `otter deploy`:

- excludes `.otter-id` from the staged tree, so a local identity never travels;
- excludes and protects `.otter-id` in the rsync step, so `--delete` cannot
  remove the destination's marker;
- releases each integration by its destination path, never by a directory
  basename that could be read as a label;
- records the destination identity of each integration in `.otter/deploy.json`,
  which is what lets a later deploy tell "same instance, new code" from "a new
  instance", and prints them in `otter deploy --status`.

The deployed tree must be writable by the runtime user, because registering a
new integration writes its marker.

## Trust boundary

The marker is a claim, not a credential. Anyone who can write into an
integration directory can write a marker, and anyone who can also remove the
registered directory could have a replacement adopt a retired identity's path
before the next scan. That is not an escalation: the same write access already
lets them change the code the runtime executes. State isolation between
integrations is protection against mistakes — a copy, a rename, a deletion and
recreation — not isolation from a hostile same-user process.

Shared artifacts are not deleted with an integration. Prepared Python
environments are content-addressed and may be shared by several integrations, so
`otter delete` removes only what the identity exclusively owns: state, run
history and logs, queue rows, its webhook token, and its release directories.
