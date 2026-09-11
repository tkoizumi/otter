// Package migrations holds the SQL schema migrations for Otter.
//
// The files are embedded into the daemon binary so that a single static
// binary can bootstrap an empty data directory with no external tooling.
package migrations

import "embed"

// FS contains every migration in this directory. Files are applied in
// lexicographic order, so they must be prefixed with a zero-padded number.
//
//go:embed *.sql
var FS embed.FS
