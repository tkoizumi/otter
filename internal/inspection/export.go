package inspection

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tkoizumi/otter/internal/database"
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
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
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

// ErrOutboxFull means the local queue is at its bound. The store records the
// error and continues; dropping diagnostic records is preferable to delaying or
// failing a job.
var ErrOutboxFull = errors.New("inspection: capture outbox is full")

// DefaultMaxOutboxRows bounds the local queue. A run is limited to 1,000
// exchanges, so this holds at least fifty runs' worth while a Cloud outage is
// drained with backoff.
const DefaultMaxOutboxRows = 50_000

// OutboxExporter writes pending export records to the runtime's local queue.
//
// It writes in the caller's transaction: an exchange that was stored is an
// exchange that has an export row. The document itself is assembled when the
// relay pulls the row, so the queue never holds a second copy of a body.
type OutboxExporter struct {
	// MaxRows overrides DefaultMaxOutboxRows; zero means the default.
	MaxRows int
}

// Export implements Exporter by inserting pending rows.
func (e OutboxExporter) Export(ctx context.Context, tx DBTX, records []ExportRecord) error {
	if len(records) == 0 {
		return nil
	}
	limit := e.MaxRows
	if limit <= 0 {
		limit = DefaultMaxOutboxRows
	}
	var pending int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM capture_exports WHERE state = 'pending'`).Scan(&pending); err != nil {
		return fmt.Errorf("inspection: count capture outbox: %w", err)
	}

	now := database.FormatTime(time.Now().UTC())
	for _, record := range records {
		if err := record.validate(); err != nil {
			return err
		}
		if pending >= limit {
			return fmt.Errorf("%w: %d pending (limit %d)", ErrOutboxFull, pending, limit)
		}
		result, err := tx.ExecContext(ctx,
			`INSERT INTO capture_exports
			   (run_id, kind, request_id, state, attempts, last_error, next_attempt_at, created_at, updated_at)
			 VALUES (?, ?, ?, 'pending', 0, '', 0, ?, ?)
			 ON CONFLICT(run_id, kind, request_id) DO NOTHING`,
			record.RunID, string(record.Kind), record.RequestID, now, now)
		if err != nil {
			return fmt.Errorf("inspection: queue capture export: %w", err)
		}
		if added, err := result.RowsAffected(); err == nil {
			pending += int(added)
		}
	}
	return nil
}

// Valid reports whether the kind is one this build defines.
func (k ExportKind) Valid() bool {
	switch k {
	case ExportExchange, ExportReplay, ExportManifest:
		return true
	default:
		return false
	}
}

func (r ExportRecord) validate() error {
	if strings.TrimSpace(r.RunID) == "" {
		return fmt.Errorf("inspection: export record has no run id")
	}
	if !r.Kind.Valid() {
		return fmt.Errorf("inspection: export record has unknown kind %q", r.Kind)
	}
	if r.Kind == ExportManifest {
		if r.RequestID != "" {
			return fmt.Errorf("inspection: a manifest export carries no request id")
		}
		return nil
	}
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("inspection: %s export has no request id", r.Kind)
	}
	return nil
}

// listRequestIDs returns every exchange recorded for a run. At finalization an
// incomplete exchange is written once, so the relay needs the complete list,
// not only the exchanges that completed during this batch.
func listRequestIDs(ctx context.Context, tx *sql.Tx, runID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT request_id FROM http_exchanges WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("inspection: list exchanges for export: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("inspection: scan exchange for export: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inspection: list exchanges for export: %w", err)
	}
	return ids, nil
}

// ParseExportMode normalizes the configured exporter name.
func ParseExportMode(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", ExportModeNone:
		return ExportModeNone, nil
	case ExportModeOutbox:
		return ExportModeOutbox, nil
	default:
		return "", fmt.Errorf("must be %q or %q; got %q", ExportModeNone, ExportModeOutbox, value)
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
	case ExportModeOutbox:
		return OutboxExporter{}, nil
	default:
		return nil, fmt.Errorf("inspection: unsupported capture exporter %q", normalized)
	}
}
