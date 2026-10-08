package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
)

// This file implements the identity lifecycle commands. Every one of them goes
// through the daemon rather than editing the registry directly: the daemon
// owns the data directory and the identity service, so a second writer is
// exactly what the design forbids.
//
// The commands are deliberately about identity, not about files:
//
//	register  give a source directory a durable identity
//	reset     retire the identity and mint a fresh one (fresh state)
//	delete    retire the identity and purge everything it owns
//	move      preserve the identity across a directory rename
//
// Removing a directory with rm -rf is not one of them. It retires the
// identity on the next complete scan, keeping the data; only an explicit
// delete reclaims it.

// cmdRegister implements `otter register [<path>]`.
func (a *App) cmdRegister(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter register [<path>|.]")
		return 2
	}
	path := "."
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}
	absolute, err := absoluteDir(path)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}

	view, err := g.client().RegisterJob(ctx, absolute)
	if err != nil {
		return a.fail(err)
	}
	if g.jsonOut {
		return a.printJSON(view)
	}
	fmt.Fprintf(a.Stdout, "registered  %s\n", view.Name)
	fmt.Fprintf(a.Stdout, "id          %s\n", view.ID)
	fmt.Fprintf(a.Stdout, "path        %s\n", view.Path)
	return 0
}

// cmdReset implements `otter reset <job>`.
func (a *App) cmdReset(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter reset <job>")
		return 2
	}
	ref, err := daemonRef(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}

	result, err := g.client().ResetJob(ctx, ref)
	if err != nil {
		return a.fail(err)
	}
	if g.jsonOut {
		return a.printJSON(result)
	}
	fmt.Fprintf(a.Stdout, "reset       %s\n", result.Name)
	fmt.Fprintf(a.Stdout, "old id      %s\n", result.OldID)
	fmt.Fprintf(a.Stdout, "new id      %s\n", result.NewID)
	fmt.Fprintf(a.Stdout, "path        %s\n", result.Path)
	fmt.Fprintln(a.Stdout, "note: the previous identity's state and history are kept until `otter delete`")
	return 0
}

// cmdDelete implements `otter delete <name|id>` and
// `otter delete --cloud <name|id>`.
//
// Both paths destroy data and neither can be undone, so both go through the one
// confirmation in confirm.go before anything is sent: a declined prompt leaves
// the local store and the Cloud command queue untouched. --yes (or -y) skips
// the prompt, which is now what a script must pass -- a non-interactive stdin
// without it refuses rather than waiting on a pipe or proceeding silently.
//
// The reference is a job name or id and never a directory. Every other command
// that accepts a reference also accepts a path -- `otter run .` is the point --
// but a delete has nothing to gain from one: the prompt would echo the raw
// reference, so `otter delete .` read "deleting . destroys, irreversibly:",
// which names no job a human can check. looksLikePath -- the same predicate
// that decides whether the other commands read a manifest, so the rule cannot
// drift -- refuses one here, before the cloud/local split, and therefore before
// the prompt, before a credential is read and before any request is sent.
func (a *App) cmdDelete(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	cloudMode := fs.Bool("cloud", false, "delete the job from Otter Cloud instead of this workspace")
	runtimeID := fs.String("runtime", "", "with --cloud: the runtime that owns the job; needed only when the job's label is ambiguous")
	yes := fs.Bool("yes", false, "skip the confirmation prompt; deleting is irreversible")
	shortYes := fs.Bool("y", false, "shorthand for --yes")
	fs.Usage = func() {
		fmt.Fprint(a.Stderr, `Usage:
  otter delete <name|id> [--yes]          purge a workspace job and everything it owns
  otter delete --cloud <name|id> [--yes]  delete the job from Otter Cloud
  otter delete --cloud <name|id> --runtime <id>
                                          disambiguate the job on a chosen runtime

Deleting is irreversible. It removes every run and the logs those runs
produced, every captured request and response payload, the job's stored state,
every schedule, the job's configuration, and every release including the active
one. Source files in the jobs directory are left in place; a workspace job's
path is suppressed until an explicit register, and a job known only from its
releases has no tombstone, so releasing the same id brings it back.

The argument is a job name or id, never a directory: "otter delete ." and
"otter delete jobs/counter" are refused, because the confirmation can only name
a job the operator recognises.

The command asks for confirmation before it does any of that. --yes (or -y)
skips the prompt and is what a script must pass: with no --yes and a stdin that
is not a terminal the delete refuses instead of waiting for input, and a
declined prompt deletes nothing and exits 1.

With --cloud the credential comes from otter login, and the name or id is
resolved by id or label through the Cloud API.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprint(a.Stderr, `
Examples:
  otter delete counter
  otter delete counter --yes
  otter delete --cloud counter
  otter delete --cloud counter --runtime rt_123 --yes
`)
	}

	// The flag package stops at the first positional argument, so
	// `otter delete counter --yes` would otherwise leave --yes as a second
	// positional and fail. --runtime is the only flag that takes a value.
	takesValue := func(arg string) bool {
		name := strings.TrimLeft(arg, "-")
		if i := strings.Index(name, "="); i >= 0 {
			name = name[:i]
		}
		return name == "runtime"
	}
	args = flagsFirst(normalizeLongFlags(args), takesValue)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter delete <name|id> [--cloud] [--runtime <id>] [--yes]")
		return 2
	}
	ref := fs.Arg(0)
	assumeYes := *yes || *shortYes

	// A delete names a job, never a directory. This is a usage error and it is
	// checked before the cloud/local split so that it is also checked before
	// the prompt and before the first request on either path: a bad reference
	// prints guidance, touches no daemon, reads no credential and calls no API.
	if looksLikePath(ref) {
		fmt.Fprintf(a.Stderr, "otter: delete takes a job name or id, not a path: %q\n", ref)
		fmt.Fprintln(a.Stderr, "otter: pass the name otter jobs lists or the job's id")
		return 2
	}

	if *cloudMode {
		return a.cmdDeleteCloud(ctx, g, ref, *runtimeID, assumeYes)
	}
	if strings.TrimSpace(*runtimeID) != "" {
		fmt.Fprintln(a.Stderr, "otter: --runtime selects a Cloud runtime; it needs --cloud")
		return 2
	}

	// The daemon resolves the reference, so a retired identity can still be
	// purged by id after its directory has been removed.
	daemonReference, err := daemonRef(ref)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}

	// The prompt is the last thing before the delete and the first thing after
	// argument checking: declining it makes no request at all.
	if proceed, code := a.confirmDelete(ref, assumeYes); !proceed {
		return code
	}

	result, err := g.client().DeleteJob(ctx, daemonReference)
	if err != nil {
		return a.fail(err)
	}
	if g.jsonOut {
		return a.printJSON(result)
	}
	name := result.Name
	if name == "" {
		name = result.ID
	}
	fmt.Fprintf(a.Stdout, "deleted     %s (%s)\n", name, result.ID)
	// The daemon reports the scope of the delete, because it differs by job
	// shape: a released-only job has no source to leave and no tombstone to
	// write. An older daemon that does not report it gets the historical note.
	note := result.Note
	if note == "" {
		note = "source files were left in place; the path is suppressed until an explicit register."
	}
	fmt.Fprintf(a.Stdout, "note        %s\n", note)
	return 0
}

// cmdMove implements `otter move <job> <destination>`.
func (a *App) cmdMove(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("move", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter move <job> <destination>")
		return 2
	}
	ref, err := daemonRef(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}
	destination, err := absoluteDir(fs.Arg(1))
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}

	view, err := g.client().MoveJob(ctx, ref, destination)
	if err != nil {
		return a.fail(err)
	}
	if g.jsonOut {
		return a.printJSON(view)
	}
	fmt.Fprintf(a.Stdout, "moved       %s\n", view.Name)
	fmt.Fprintf(a.Stdout, "id          %s\n", view.ID)
	fmt.Fprintf(a.Stdout, "path        %s\n", view.Path)
	return 0
}

// daemonRef turns a command-line reference into something the daemon can
// resolve unambiguously.
//
// A path is made absolute and canonical here, because a relative path means
// different things to the CLI's working directory and the daemon's. Everything
// else -- a bare label or an explicit id -- is passed through untouched, and
// the daemon decides whether it is unique.
func daemonRef(ref string) (string, error) {
	if !looksLikePath(ref) {
		return ref, nil
	}
	return absoluteDir(ref)
}

// resolveViaDaemon resolves a reference to the job view the daemon
// holds, so a command can act on the durable identity rather than on the label
// the operator typed.
func (a *App) resolveViaDaemon(ctx context.Context, g globals, ref string) (id, name string, code int) {
	daemonReference, err := daemonRef(ref)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return "", "", 2
	}
	view, err := g.client().ResolveJob(ctx, daemonReference)
	if err != nil {
		return "", "", a.fail(err)
	}
	return view.ID, view.Name, 0
}
