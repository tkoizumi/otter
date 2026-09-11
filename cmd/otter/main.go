// Command otter is the Otter command-line interface.
//
// It talks to a running otterd over the local HTTP API. The only command that
// does not need a daemon is `otter validate`.
package main

import (
	"context"
	"os"

	"github.com/otter-runtime/otter/internal/cli"
)

// version is injected at build time.
var version = "dev"

func main() {
	app := cli.New(version, os.Stdout, os.Stderr)
	os.Exit(app.Run(context.Background(), os.Args[1:]))
}
