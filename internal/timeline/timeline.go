// Package timeline assembles the merged chronological view of a finished run:
// its lifecycle lines, its captured output, and its HTTP exchanges in one order.
//
// It is a read-only projection over data the daemon already stores. Nothing here
// writes, and nothing here reads a payload column: the HTTP side is limited to
// the metadata the request list already exposes, so opening a timeline can never
// disclose a header or body that `otter request` would not.
//
// Two properties matter more than convenience and drive the design:
//
//   - Ordering is a total order. Events are ordered by (at, source_rank, id) so
//     that equal timestamps across two independently sequenced tables still order
//     deterministically, and a page boundary can never skip or repeat an event.
//   - A continuation is bound to the evidence it started from. Pages are not a
//     database snapshot, so a cursor carries a digest of the evidence; if that
//     evidence changed, the continuation is refused instead of silently
//     returning a trace with a hole in it.
package timeline

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/runs"
)

// SchemaVersion identifies the shape of the page and cursor. A cursor from a
// different version is refused rather than reinterpreted.
const SchemaVersion = 1

// Source ranks fix the order of events that share a timestamp. Logs are ranked
// below HTTP so that a log line and an exchange recorded in the same instant
// always present in the same order.
const (
	RankLogs = 0
	RankHTTP = 1
)

// Event kinds.
const (
	KindLifecycle = "lifecycle"
	KindLog       = "log"
	KindHTTP      = "http"
)

// Sources name the table an event came from.
const (
	SourceRunLogs       = "run_logs"
	SourceHTTPExchanges = "http_exchanges"
)

// DefaultLimit and MaxLimit bound one page. The limit applies to events only,
// excluding context and page framing records.
const (
	DefaultLimit = 100
	MaxLimit     = 1000
)

// maxCursorBytes bounds a decoded continuation token. It is untrusted input, so
// it is rejected before any parsing work is done rather than after.
const maxCursorBytes = 4 * 1024

// ReadBudget bounds the whole database read for one page, starting before the
// connection is acquired and covering the revision work as well as the page
// queries. It exists because the daemon runs SQLite on a single connection: a
// trace that decided to take its time would stall the log writes and capture
// ingestion of every executing run. A read that cannot finish in this budget
// fails with ErrReadDeadline rather than holding the connection to completion.
//
// The database's 10s busy timeout is not this budget; it is the point at which a
// blocked driver call gives up.
const ReadBudget = 250 * time.Millisecond

// Errors the API layer maps onto status codes.
var (
	// ErrRunNotFound means the run id names no run at all.
	ErrRunNotFound = errors.New("run not found")
	// ErrRunNotTerminal means the attempt has not finished. A timeline is a
	// record of a finished attempt, so this is refused rather than followed.
	ErrRunNotTerminal = errors.New("run has not finished")
	// ErrCursorInvalid means the continuation token is malformed, oversized, of
	// an unknown version, or belongs to another run or another HTTP setting.
	ErrCursorInvalid = errors.New("cursor is invalid")
	// ErrEvidenceChanged means the evidence behind a continuation changed since
	// the first page. Continuing would silently skip, duplicate or replace
	// events, so the caller is told to start over.
	ErrEvidenceChanged = errors.New("timeline evidence changed since the first page")
	// ErrReadDeadline means the bounded read did not finish in time. It is a
	// deliberate 503 rather than a partial page.
	ErrReadDeadline = errors.New("timeline read exceeded its deadline")
)

// Position is where a page stopped: the ordering key of the last emitted event.
type Position struct {
	At   time.Time
	Rank int
	ID   int64
}

// Cursor is a continuation token. It is opaque to callers and must be treated as
// untrusted: it is a continuation hint, never an authorization mechanism.
type Cursor struct {
	Version  int
	RunID    string
	Include  bool
	Revision string
	Position Position
}

// cursorWire is the encoded form. Field names are short because the token is
// echoed in shell commands.
type cursorWire struct {
	Version  int    `json:"v"`
	RunID    string `json:"r"`
	Include  bool   `json:"i"`
	Revision string `json:"d"`
	At       string `json:"t"`
	Rank     int    `json:"k"`
	ID       int64  `json:"n"`
}

// Encode renders a cursor as URL-safe base64 JSON.
func (c Cursor) Encode() (string, error) {
	wire := cursorWire{
		Version:  c.Version,
		RunID:    c.RunID,
		Include:  c.Include,
		Revision: c.Revision,
		At:       database.FormatTime(c.Position.At),
		Rank:     c.Position.Rank,
		ID:       c.Position.ID,
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("timeline: encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

// DecodeCursor parses and validates a continuation token. Every failure is
// reported as ErrCursorInvalid so the API layer answers 400 without leaking
// which part of the token was wrong.
func DecodeCursor(token string) (Cursor, error) {
	if token == "" {
		return Cursor{}, fmt.Errorf("%w: empty", ErrCursorInvalid)
	}
	if len(token) > maxCursorBytes {
		return Cursor{}, fmt.Errorf("%w: too long", ErrCursorInvalid)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: not base64", ErrCursorInvalid)
	}
	var wire cursorWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Cursor{}, fmt.Errorf("%w: not a cursor object", ErrCursorInvalid)
	}
	if wire.Version != SchemaVersion {
		return Cursor{}, fmt.Errorf("%w: version %d is not supported", ErrCursorInvalid, wire.Version)
	}
	if wire.Rank != RankLogs && wire.Rank != RankHTTP {
		return Cursor{}, fmt.Errorf("%w: unknown source rank", ErrCursorInvalid)
	}
	if wire.ID < 0 {
		return Cursor{}, fmt.Errorf("%w: negative position", ErrCursorInvalid)
	}
	at, err := database.ParseTime(wire.At)
	if err != nil || at.IsZero() {
		return Cursor{}, fmt.Errorf("%w: unreadable position timestamp", ErrCursorInvalid)
	}
	return Cursor{
		Version:  wire.Version,
		RunID:    wire.RunID,
		Include:  wire.Include,
		Revision: wire.Revision,
		Position: Position{At: at, Rank: wire.Rank, ID: wire.ID},
	}, nil
}

// Request asks for one page of a run's timeline.
type Request struct {
	RunID       string
	IncludeHTTP bool
	// After is the raw continuation token, empty for a first page.
	After string
	Limit int
	// Deadline bounds the whole database read, starting before the connection is
	// acquired. Zero means no internal deadline.
	Deadline time.Duration
}

// Context is the header the page always carries, even when it has no events. Run
// status and error live here rather than being inferred from a lifecycle line:
// the log is prunable and the terminal line can be missing, but the run record
// is authoritative.
type Context struct {
	SchemaVersion   int                    `json:"schema_version"`
	RunID           string                 `json:"run_id"`
	IntegrationID   string                 `json:"integration_id"`
	IntegrationName string                 `json:"integration_name,omitempty"`
	Status          string                 `json:"status"`
	Attempt         int                    `json:"attempt"`
	ParentRunID     string                 `json:"parent_run_id,omitempty"`
	TriggerType     string                 `json:"trigger_type,omitempty"`
	Error           string                 `json:"error,omitempty"`
	ExitCode        *int                   `json:"exit_code,omitempty"`
	ReleaseDigest   string                 `json:"release_digest,omitempty"`
	CapturePolicy   string                 `json:"capture_policy,omitempty"`
	CreatedAt       time.Time              `json:"created_at"`
	StartedAt       *time.Time             `json:"started_at,omitempty"`
	FinishedAt      *time.Time             `json:"finished_at,omitempty"`
	Capture         *inspection.RunCapture `json:"capture"`
	IncludeHTTP     bool                   `json:"include_http"`
}

// Event is one item on the timeline.
type Event struct {
	Kind   string    `json:"kind"`
	At     time.Time `json:"at"`
	Source string    `json:"source"`
	ID     int64     `json:"id"`
	RunID  string    `json:"run_id"`

	// Log and lifecycle events carry the stored message verbatim. Splitting the
	// message's trailing structured fields is a rendering concern and stays in
	// the CLI, so there is exactly one interpretation of a log line.
	Stream  string `json:"stream,omitempty"`
	Message string `json:"message,omitempty"`

	// HTTP events carry the exchange summary, never a payload.
	HTTP *HTTPEvent `json:"http,omitempty"`

	// positionAt is the event's own timestamp, never serialized, and it is what a
	// cursor position is built from. At carries the same instant in the storage
	// format for the wire, and today that format keeps nanoseconds, so the two
	// agree exactly. Keeping the source value separate means cursor construction
	// never depends on that being true: if the rendered form ever loses
	// precision, a page boundary would still resume from the real instant rather
	// than from a truncated one.
	positionAt time.Time
}

// HTTPEvent is the exchange projection the timeline shows.
//
// It is a summary placed at the exchange's first recorded occurrence, usually
// its start. Status and duration are the latest retained values and were not
// necessarily known at that timestamp: this is not a response event. Late reports
// when the daemon recorded the exchange after the producer stamped it, which
// means an adjacent log line is not evidence of a causal order.
type HTTPEvent struct {
	RequestID  string `json:"request_id"`
	Method     string `json:"method,omitempty"`
	URL        string `json:"url,omitempty"`
	StatusCode *int   `json:"status_code,omitempty"`
	ErrorClass string `json:"transport_error_class,omitempty"`
	DurationMS *int64 `json:"duration_total_ms,omitempty"`
	Phase      string `json:"phase"`
	Complete   bool   `json:"complete"`
	Payloads   string `json:"payloads,omitempty"`
	CallSite   string `json:"call_site,omitempty"`
	// ErrorCode and ErrorMessage state why a failed exchange failed, taken from
	// the sanitized response body at ingestion. Present only when a body was
	// captured and a recognised error shape was found.
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	IngestedAt   time.Time `json:"ingested_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Late         bool      `json:"late,omitempty"`
}

// Page is one page of a run's timeline.
type Page struct {
	Context    Context `json:"context"`
	Events     []Event `json:"events"`
	HasMore    bool    `json:"has_more"`
	NextCursor string  `json:"next_cursor,omitempty"`
	// SnapshotAt is informational only. Nothing is pinned: it records when this
	// page was read, and the revision carried by the cursor is what actually
	// guards a continuation against changed evidence.
	SnapshotAt time.Time `json:"snapshot_at"`
}

// Reader assembles timeline pages from the daemon's database.
type Reader struct {
	db         *database.DB
	runs       *runs.Store
	logs       *runs.LogStore
	inspection *inspection.Store
}

// NewReader wires the stores the timeline reads.
func NewReader(db *database.DB, runsStore *runs.Store, logsStore *runs.LogStore, inspectionStore *inspection.Store) *Reader {
	return &Reader{db: db, runs: runsStore, logs: logsStore, inspection: inspectionStore}
}

// Page reads one page of a run's timeline.
//
// A first page establishes the evidence revision a continuation will be checked
// against. A continuation must present that same revision; if the evidence moved,
// ErrEvidenceChanged is returned rather than a page that is missing or repeating
// events.
func (r *Reader) Page(ctx context.Context, req Request) (*Page, error) {
	if req.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Deadline)
		defer cancel()
	}

	limit := req.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	var (
		cursor    Cursor
		firstPage = true
	)
	// A first page has no position yet, so both sources must expose their
	// equal-timestamp branch. Rank -1 is below every real rank, which is exactly
	// the "nothing has been passed yet" position; a zero value would claim rank 0
	// and silently drop log rows that share the first event's timestamp.
	cursor.Position.Rank = -1
	if req.After != "" {
		decoded, err := DecodeCursor(req.After)
		if err != nil {
			return nil, err
		}
		if decoded.RunID != req.RunID {
			return nil, fmt.Errorf("%w: cursor belongs to a different run", ErrCursorInvalid)
		}
		if decoded.Include != req.IncludeHTTP {
			return nil, fmt.Errorf("%w: cursor was issued with a different HTTP inclusion setting", ErrCursorInvalid)
		}
		cursor = decoded
		firstPage = false
	}

	page := &Page{Events: []Event{}, SnapshotAt: time.Now().UTC()}

	err := r.db.Tx(ctx, func(tx *sql.Tx) error {
		run, err := r.runs.TimelineRunTx(ctx, tx, req.RunID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRunNotFound
		}
		if err != nil {
			return err
		}
		if !run.Status.Terminal() {
			return fmt.Errorf("%w: status is %s", ErrRunNotTerminal, run.Status)
		}

		capture, err := r.captureTx(ctx, tx, req.RunID)
		if err != nil {
			return err
		}

		revision, err := r.revisionTx(ctx, tx, run, capture, req.IncludeHTTP)
		if err != nil {
			return err
		}
		if !firstPage && cursor.Revision != revision {
			return ErrEvidenceChanged
		}

		// Each source is asked for one row beyond the page so the merge can tell
		// whether a further page exists without reading the source to its end.
		//
		// The bound is the ordering tuple (at, rank, id). A cursor position is
		// "the last event already emitted", so a source must still be eligible at
		// the cursor's own timestamp when that source orders at or above the
		// cursor's rank: those rows come after the cursor in the merged order.
		// Rows of a lower-ranked source at that timestamp have already been
		// passed, and only strictly later timestamps qualify. A first page has no
		// position (rank -1), so every source takes its equal-timestamp branch.
		probe := limit + 1
		logs, err := r.logs.LogPageTx(ctx, tx, req.RunID, runs.LogCursor{
			At:   cursor.Position.At,
			Rank: cursor.Position.Rank,
			ID:   cursor.Position.ID,
		}, probe)
		if err != nil {
			return err
		}

		var exchanges []inspection.TimelineExchange
		if req.IncludeHTTP && r.inspection != nil {
			exchanges, err = r.inspection.TimelineExchangeTx(ctx, tx, req.RunID, inspection.TimelineCursor{
				At:   cursor.Position.At,
				Rank: cursor.Position.Rank,
				ID:   cursor.Position.ID,
			}, probe)
			if err != nil {
				return err
			}
		}

		merged := r.merge(run.ID, logs, exchanges)
		page.HasMore = len(merged) > limit
		if len(merged) > limit {
			merged = merged[:limit]
		}
		page.Events = merged
		page.Context = Context{
			SchemaVersion:   SchemaVersion,
			RunID:           run.ID,
			IntegrationID:   run.IntegrationID,
			IntegrationName: run.IntegrationName,
			Status:          string(run.Status),
			Attempt:         run.Attempt,
			TriggerType:     run.TriggerType,
			Error:           derefString(run.Error),
			ExitCode:        run.ExitCode,
			ReleaseDigest:   run.ReleaseDigest,
			CapturePolicy:   run.CapturePolicy,
			CreatedAt:       run.CreatedAt,
			StartedAt:       run.StartedAt,
			FinishedAt:      run.FinishedAt,
			Capture:         capture,
			IncludeHTTP:     req.IncludeHTTP,
		}
		if run.ParentRunID != nil {
			page.Context.ParentRunID = *run.ParentRunID
		}

		if page.HasMore {
			last := page.Events[len(page.Events)-1]
			next := Cursor{
				Version:  SchemaVersion,
				RunID:    req.RunID,
				Include:  req.IncludeHTTP,
				Revision: revision,
				Position: Position{At: positionOf(last), Rank: rankOf(last), ID: last.ID},
			}
			token, err := next.Encode()
			if err != nil {
				return err
			}
			page.NextCursor = token
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("%w: %v", ErrReadDeadline, err)
		}
		return nil, err
	}
	return page, nil
}

// captureTx reads the capture summary, tolerating a daemon without inspection.
func (r *Reader) captureTx(ctx context.Context, tx *sql.Tx, runID string) (*inspection.RunCapture, error) {
	if r.inspection == nil {
		return inspection.UnavailableCapture(runID), nil
	}
	return r.inspection.TimelineCaptureTx(ctx, tx, runID)
}

// revisionTx folds the evidence a page displays into one digest.
//
// It deliberately does not count log rows. MIN(id) and MAX(id) are two covering
// index seeks, and they detect every mutation the log store can perform: an
// append raises MAX, and every deletion path removes whole rows, changing MIN or
// clearing both. Log rows are never updated, and run_logs.id is AUTOINCREMENT so
// an id is never reused. Counting would be O(rows) per page and quadratic across
// a paged trace, which is the opposite of what a diagnostic command should cost.
func (r *Reader) revisionTx(ctx context.Context, tx *sql.Tx, run *runs.TimelineRun, capture *inspection.RunCapture, includeHTTP bool) (string, error) {
	logs, err := r.logs.LogEvidenceTx(ctx, tx, run.ID)
	if err != nil {
		return "", err
	}

	hash := sha256.New()
	write := func(values ...string) {
		for i, value := range values {
			if i > 0 {
				hash.Write([]byte{0})
			}
			hash.Write([]byte(value))
		}
		hash.Write([]byte{0, '\n'})
	}

	// The context is part of the revision because it is part of what a page
	// displays: a status change must invalidate a continuation just as a new
	// event does.
	write(
		"v="+strconv.Itoa(SchemaVersion),
		"run="+runFingerprint(run),
		"capture="+captureFingerprint(capture),
		"logs="+strconv.FormatInt(logs.MinID, 10)+":"+strconv.FormatInt(logs.MaxID, 10),
		"logs_present="+strconv.FormatBool(logs.Present),
	)

	if includeHTTP {
		evidence, err := r.inspectionEvidenceTx(ctx, tx, run.ID)
		if err != nil {
			return "", err
		}
		digest := ""
		if r.inspection != nil {
			digest, err = r.inspection.TimelineDigestTx(ctx, tx, run.ID)
			if err != nil {
				return "", err
			}
		}
		write(
			"http="+strconv.FormatInt(evidence.MinID, 10)+":"+strconv.FormatInt(evidence.MaxID, 10),
			"http_present="+strconv.FormatBool(evidence.Present),
			"http_digest="+digest,
		)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// runFingerprint reduces the run context a page displays to a stable string. The
// header is authoritative for status and error, so a change to either must
// invalidate a continuation even when no new event arrived.
func runFingerprint(run *runs.TimelineRun) string {
	parent := ""
	if run.ParentRunID != nil {
		parent = *run.ParentRunID
	}
	exit := ""
	if run.ExitCode != nil {
		exit = strconv.Itoa(*run.ExitCode)
	}
	started, finished := "", ""
	if run.StartedAt != nil {
		started = database.FormatTime(*run.StartedAt)
	}
	if run.FinishedAt != nil {
		finished = database.FormatTime(*run.FinishedAt)
	}
	return strings.Join([]string{
		run.ID,
		run.IntegrationID,
		run.IntegrationName,
		string(run.Status),
		strconv.Itoa(run.Attempt),
		parent,
		run.TriggerType,
		derefString(run.Error),
		exit,
		run.ReleaseDigest,
		run.CapturePolicy,
		database.FormatTime(run.CreatedAt),
		started,
		finished,
	}, "|")
}

func (r *Reader) inspectionEvidenceTx(ctx context.Context, tx *sql.Tx, runID string) (inspection.Evidence, error) {
	if r.inspection == nil {
		return inspection.Evidence{}, nil
	}
	return r.inspection.TimelineEvidenceTx(ctx, tx, runID)
}

// captureFingerprint reduces the capture summary to the facts a trace displays.
// It is written explicitly rather than by marshalling the struct so that adding
// a field elsewhere cannot silently change cursor validity.
func captureFingerprint(capture *inspection.RunCapture) string {
	if capture == nil {
		return "none"
	}
	finalized := ""
	if capture.FinalizedAt != nil {
		finalized = database.FormatTime(*capture.FinalizedAt)
	}
	return string(capture.State) + "|" + string(capture.Policy) + "|" + capture.Coverage + "|" +
		string(capture.Finalization) + "|" + finalized + "|" +
		strconv.Itoa(capture.RequestCount) + "|" +
		strconv.Itoa(capture.IncompleteCount) + "|" +
		strconv.FormatInt(capture.DroppedEvents, 10) + "|" +
		strconv.FormatBool(capture.PayloadsExpired)
}

// merge interleaves the two ordered sources into one ordered slice.
//
// Both inputs are already chronological. Ties are broken by source rank, so the
// comparison is exactly the (at, rank, id) order the cursor encodes.
func (r *Reader) merge(runID string, logs []runs.LogEntry, exchanges []inspection.TimelineExchange) []Event {
	out := make([]Event, 0, len(logs)+len(exchanges))
	i, j := 0, 0
	for i < len(logs) && j < len(exchanges) {
		logAt := logs[i].Timestamp
		httpAt := exchanges[j].OccurredAt
		// Logs are rank 0, so an equal timestamp emits the log first.
		if !httpAt.Before(logAt) {
			out = append(out, logEvent(runID, logs[i]))
			i++
			continue
		}
		out = append(out, httpEvent(runID, exchanges[j]))
		j++
	}
	for ; i < len(logs); i++ {
		out = append(out, logEvent(runID, logs[i]))
	}
	for ; j < len(exchanges); j++ {
		out = append(out, httpEvent(runID, exchanges[j]))
	}
	return out
}

func logEvent(runID string, entry runs.LogEntry) Event {
	// The kind comes from who wrote the line, not from the stream it landed on:
	// the daemon's narration and the SDK's ctx.log output share the `otter`
	// stream, and calling an integration's own log line a lifecycle event would
	// misdescribe it.
	kind := KindLog
	if entry.IsLifecycle() {
		kind = KindLifecycle
	}
	return Event{
		Kind:       kind,
		At:         entry.Timestamp,
		Source:     SourceRunLogs,
		ID:         entry.ID,
		RunID:      runID,
		Stream:     entry.Stream,
		Message:    entry.Message,
		positionAt: entry.Timestamp,
	}
}

func httpEvent(runID string, exchange inspection.TimelineExchange) Event {
	return Event{
		Kind:       KindHTTP,
		At:         exchange.OccurredAt,
		Source:     SourceHTTPExchanges,
		ID:         exchange.ID,
		RunID:      runID,
		positionAt: exchange.OccurredAt,
		HTTP: &HTTPEvent{
			RequestID:    exchange.RequestID,
			Method:       exchange.Method,
			URL:          exchange.URL,
			StatusCode:   exchange.StatusCode,
			ErrorClass:   exchange.Class,
			DurationMS:   exchange.DurationMS,
			Phase:        string(exchange.Phase),
			Complete:     exchange.Complete,
			Payloads:     exchange.Payloads,
			CallSite:     exchange.CallSite,
			ErrorCode:    exchange.ErrorCode,
			ErrorMessage: exchange.ErrorMessage,
			IngestedAt:   exchange.IngestedAt,
			UpdatedAt:    exchange.UpdatedAt,
			Late:         !exchange.IngestedAt.IsZero() && exchange.IngestedAt.Before(exchange.OccurredAt),
		},
	}
}

func rankOf(event Event) int {
	if event.Source == SourceHTTPExchanges {
		return RankHTTP
	}
	return RankLogs
}

// positionOf returns the timestamp a cursor must use for this event. The
// unrounded source timestamp is authoritative; At is only the rendered form.
// A page built by an older caller that did not populate positionAt still gets a
// usable value rather than a zero time.
func positionOf(event Event) time.Time {
	if !event.positionAt.IsZero() {
		return event.positionAt
	}
	return event.At
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
