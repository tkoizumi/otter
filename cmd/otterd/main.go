// Command otterd is the Otter daemon.
//
// It discovers integrations under --integrations, registers their triggers,
// executes them as child processes, persists runs, logs and state in SQLite
// and serves the HTTP API on --listen.
package main

import (
	"context"
	"os"

	"github.com/tkoizumi/otter/internal/cli"
)

// version is injected at build time:
//
//	go build -ldflags "-X main.version=1.2.3" ./cmd/otterd
var version = "dev"

func main() {
	os.Exit(cli.RunDaemon(context.Background(), version, os.Args[1:], os.Stdout, os.Stderr))
}
