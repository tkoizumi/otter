package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tkoizumi/otter/internal/runs"
)

// finishJournalName is the append-only sidecar that holds attempt outcomes
// which could not be written to SQLite.
//
// It exists for one narrow, severe case: the child has already run, so the
// attempt must not be executed again, but the transaction that would record its
// terminal state (and any successor) cannot be committed. Leaving the run
// `running` and returning would make the next startup treat it as interrupted
// and re-run a successful child, duplicating its external effects. The journal
// is what recovery reads instead.
const finishJournalName = "pending-finishes.jsonl"

// finishRecord is one attempt outcome the database refused to record. It is
// durable enough to terminalise the run and, when the decision taken before the
// write was to retry, to create the successor later.
type finishRecord struct {
	RunID      string      `json:"run_id"`
	Status     runs.Status `json:"status"`
	ExitCode   *int        `json:"exit_code,omitempty"`
	Error      string      `json:"error,omitempty"`
	FinishedAt time.Time   `json:"finished_at"`
	// Retry is the decision taken before the failed write, not a policy to
	// re-evaluate from scratch: recovery replays it, still bounded by the
	// manifest that exists at restart.
	Retry      bool      `json:"retry"`
	RecordedAt time.Time `json:"recorded_at"`
}

// finishJournalPath is where the fallback lives inside the data directory.
func (d *Daemon) finishJournalPath() string {
	return filepath.Join(d.cfg.DataDir, finishJournalName)
}

// appendFinishRecord durably appends one record. It fsyncs before returning, so
// a daemon crash cannot lose the fallback that the database refused to hold.
func (d *Daemon) appendFinishRecord(rec finishRecord) error {
	if d.cfg.DataDir == "" {
		return errors.New("finish journal: no data directory")
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("finish journal: encode %s: %w", rec.RunID, err)
	}
	line = append(line, '\n')

	f, err := os.OpenFile(d.finishJournalPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("finish journal: open: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("finish journal: write %s: %w", rec.RunID, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("finish journal: sync %s: %w", rec.RunID, err)
	}
	return nil
}

// readFinishJournal returns the most recent record per run. A torn or malformed
// line -- the ordinary result of a crash midway through an append -- is skipped
// rather than treated as corruption; a record is only ever rewritten in full.
func (d *Daemon) readFinishJournal() (map[string]finishRecord, error) {
	if d.cfg.DataDir == "" {
		return map[string]finishRecord{}, nil
	}

	f, err := os.Open(d.finishJournalPath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]finishRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]finishRecord{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec finishRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.RunID == "" || !rec.Status.Terminal() {
			continue
		}
		out[rec.RunID] = rec
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// rewriteFinishJournal replaces the journal with keep. Records that were
// applied -- or that can never be applied because their run is no longer
// running -- are dropped so nothing is replayed twice. An empty keep removes
// the file.
func (d *Daemon) rewriteFinishJournal(keep map[string]finishRecord) error {
	if d.cfg.DataDir == "" {
		return nil
	}
	path := d.finishJournalPath()
	if len(keep) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("finish journal: remove: %w", err)
		}
		return nil
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), finishJournalName+".*")
	if err != nil {
		return fmt.Errorf("finish journal: temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	writer := bufio.NewWriter(tmp)
	for _, rec := range keep {
		line, err := json.Marshal(rec)
		if err != nil {
			_ = tmp.Close()
			return fmt.Errorf("finish journal: encode %s: %w", rec.RunID, err)
		}
		if _, err := writer.Write(append(line, '\n')); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("finish journal: write %s: %w", rec.RunID, err)
		}
	}
	if err := writer.Flush(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("finish journal: flush: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("finish journal: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("finish journal: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("finish journal: replace: %w", err)
	}
	return syncDir(filepath.Dir(path))
}

// syncDir fsyncs a directory so a rename survives a power failure, not only a
// process crash.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
