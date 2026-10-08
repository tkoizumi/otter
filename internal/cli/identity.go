package cli

import (
	"context"
	"flag"
	"fmt"
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

// cmdDelete implements `otter delete <job>`.
func (a *App) cmdDelete(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter delete <job>")
		return 2
	}
	ref, err := daemonRef(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}

	// The daemon resolves the reference, so a retired identity can still be
	// purged by id after its directory has been removed.
	result, err := g.client().DeleteJob(ctx, ref)
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
