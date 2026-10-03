package cli

import (
	"context"
	"flag"
	"fmt"
)

// cmdVersion prints this binary's version, or, with --json, the daemon's
// machine-readable contract document.
//
// The document is what a script or a control plane reads before it talks to the
// API: the JSON schema version, the runtime-contract version, the manifest
// schema, the embedded Python SDK version and the supported platforms. Those
// are the interfaces `v0.4.0` freezes, and the document is the one place to
// learn which of them a given daemon speaks.
func (a *App) cmdVersion(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() {
		fmt.Fprint(a.Stderr, "Usage: otter version [--json]\n\n"+
			"Print this binary's version, or the daemon's contract document with --json.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(a.Stderr, "otter: unexpected arguments: %v\n", fs.Args())
		return 2
	}

	if !g.jsonOut {
		fmt.Fprintln(a.Stdout, a.Version)
		return 0
	}
	doc, err := g.client().Version(ctx)
	if err != nil {
		return a.fail(err)
	}
	return a.printJSON(doc)
}
