# Host drills

Drills here run on a **real host**, not on the machine that checks out the
repository, so they are not part of `scripts/drill.sh --list` and never run in
`make test`. They are driven by [`scripts/drill-host.sh`](../../drill-host.sh),
which refuses to run one unless an operator has declared a maintenance window on
that host:

```sh
scripts/drill-host.sh restart-service --host root@203.0.113.7 --window maintenance --dry-run
scripts/drill-host.sh restart-service --host root@203.0.113.7 --window maintenance
```

Why a second entry point: the drills in the parent directory build their own
world and clean it up, and evidence for them is a transcript anyone can
reproduce offline. A drill here changes a machine other people depend on, so it
needs the one thing that cannot be checked offline — a declared window, on the
host, with an expiry — and a record of what was changed and put back.

## The window

The window is a lock file on the host, `/var/lock/otter-window-<name>`:

```
# comments and blank lines are ignored
window=maintenance        # optional; must match --window when that is a name
expires=1767225600        # required; epoch seconds, judged on the host's clock
operator=alice@laptop     # required; who holds the window
```

`--window` takes a name (composed under `--lock-dir`, default `/var/lock`) or an
absolute path. The harness refuses, before sending any drill command, when the
lock is absent, unreadable, missing `expires=` or `operator=`, expired, or
declares a different window. `--lock-dir` exists so tests (and hosts that keep
their locks elsewhere) never touch `/var/lock`.

## Writing a drill

A host drill is a POSIX `sh` script that runs **on the host** in two separate
sessions, and takes one argument:

```sh
sh -s -- run       # do the thing, assert it, exit non-zero when it does not hold
sh -s -- restore   # put back what the window changed
```

* The two phases are separate ssh sessions. State shared between them must live
  on the host — a file, a systemd unit, a backup you made — not in a variable.
* Declare the restore step in the header, and say what it puts back:

  ```sh
  # drill-host-restore: re-enable and restart the unit
  ```

  No such line means "nothing to restore", and the transcript says so rather
  than pretending.
* The exit status **is** the assertion. Zero means the behavior held; anything
  else fails the window, keeps the transcript, and still runs the restore.
* Take no arguments and do not reach the network. Site values belong in
  `/etc/otter/drill-host.env` on the host (see `restart-service.sh`), which
  keeps them out of this repository.
* A drill that cannot run on the host must fail and say why. There is no skip: a
  drill that did not run has produced no evidence.

[`restart-service.sh`](restart-service.sh) is the worked example and the
template. It changes no file, so its restore is "leave the unit enabled and
active" rather than a repaired configuration — a drill that edits a unit must
back the file up in `run` and put it back in `restore`.

## What a window still needs from a human

The harness records the transcript, but it cannot promise the things the
transcript does not contain: that no run was in flight, that the window was
announced to whoever watches the alerts, and that the host's clock and the
operator's agree. Those stay the custodian's job, which is what the window lock
is for.
