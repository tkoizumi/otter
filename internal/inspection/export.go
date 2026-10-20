package inspection

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ExportKind is the kind of capture record that leaves the runtime.
type ExportKind string

const (
	// ExportExchange is one completed request/response pair.
	ExportExchange ExportKind = "exchange"
	// ExportReplay is one replay of a captured exchange.
	ExportReplay ExportKind = "replay"
	// ExportManifest is the summaries-only manifest written at run finalization.
	ExportManifest ExportKind = "manifest"
)

// ExportRecord is one pending export. The exporter turns it into a document;
// the record deliberately carries no body, so a queued export can never hold a
// payload outside the capture tables.
type ExportRecord struct {
	Kind      ExportKind
	RunID     string
	RequestID string
}

// DBTX is the part of *sql.Tx an exporter may use. The outbox is written in the
// same transaction as the capture ingest, so the two cannot disagree about what
// exists: an exchange that was stored is an exchange that will be exported.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Exporter receives capture records after they are stored.
//
// The default is the no-op exporter: a runtime that has not configured a sink
// behaves exactly as it did before capture export existed. An exporter error is
// logged and swallowed by the store; a diagnostic feature must never fail a job.
type Exporter interface {
	Export(ctx context.Context, tx DBTX, records []ExportRecord) error
}

// ExportModeNone is the default: no sink, no outbox, no outbound traffic.
const ExportModeNone = "none"

// ExportModeOutbox writes pending exports to the runtime's local outbox. The
// agent relays them later (S4).
const ExportModeOutbox = "outbox"

// NoopExporter is the default exporter.
type NoopExporter struct{}

// Export implements Exporter and does nothing.
func (NoopExporter) Export(context.Context, DBTX, []ExportRecord) error { return nil }

// ParseExportMode normalizes the configured exporter name.
func ParseExportMode(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", ExportModeNone:
		return ExportModeNone, nil
	default:
		return "", fmt.Errorf("must be %q; got %q", ExportModeNone, value)
	}
}

// NewExporter builds the configured exporter. A mode this build does not
// implement is refused rather than silently replaced by the no-op: a silent
// fallback would make an operator believe capture was leaving the runtime.
func NewExporter(mode string) (Exporter, error) {
	normalized, err := ParseExportMode(mode)
	if err != nil {
		return nil, err
	}
	switch normalized {
	case ExportModeNone:
		return NoopExporter{}, nil
	default:
		return nil, fmt.Errorf("inspection: unsupported capture exporter %q", normalized)
	}
}
