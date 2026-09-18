package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/pyenv"
)

// cmdPrepare prepares managed integrations before they can execute.
func (a *App) cmdPrepare(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	integrations := fs.String("integrations", config.DefaultIntegrations, "integrations root")
	data := fs.String("data", config.DefaultDataDir, "Otter data directory")
	// Empty means "let the manager choose": a vendored copy under the data
	// directory is preferred, and PATH is the fallback. Defaulting to the bare
	// name "uv" would skip the vendored copy on a host that has no global uv.
	uv := fs.String("uv", "", "uv executable used only during preparation (default: the vendored copy, then PATH)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(a.Stderr, "usage: otter prepare [--integrations DIR] [--data DIR] [integration]")
		return 2
	}
	items, err := config.Discover(*integrations)
	if err != nil {
		return a.fail(err)
	}
	wanted := ""
	if fs.NArg() == 1 {
		wanted = fs.Arg(0)
	}
	seen := false
	count := 0
	for _, item := range items {
		if wanted != "" && item.ID != wanted {
			continue
		}
		seen = true
		if !item.Valid {
			fmt.Fprintf(a.Stderr, "otter: %s: %s\n", item.ID, item.Error)
			return 1
		}
		if item.Manifest.Python.Mode != "managed" {
			if wanted != "" {
				fmt.Fprintf(a.Stderr, "otter: %s uses external Python; nothing to prepare\n", item.ID)
			}
			continue
		}
		ready, err := (pyenv.Manager{DataDir: *data}).Prepare(ctx, item.Dir, item.ID, *uv)
		if err != nil {
			fmt.Fprintf(a.Stderr, "otter: prepare %s: %v\n", item.ID, err)
			return 1
		}
		fmt.Fprintf(a.Stdout, "%s: Python %s, environment %s\n", item.ID, ready.Python, ready.Digest[:12])
		count++
	}
	if wanted != "" && !seen {
		fmt.Fprintf(a.Stderr, "otter: integration %s not found\n", wanted)
		return 1
	}
	if wanted == "" && count == 0 {
		fmt.Fprintln(a.Stderr, "otter: no managed integrations found")
	}
	return 0
}
