package daemon

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/release"
	"github.com/tkoizumi/otter/internal/runs"
)

// OT-010: a submission that binds a run must not outlive the release it bound
// to. Retention renames a doomed release aside inside its own immediate
// transaction, and the submit transaction is also immediate, so the two order
// rather than interleave. This test drives that ordering deterministically: a
// second connection holds the write lock while the submit reads the active
// release and blocks entering its transaction, the release is renamed away
// exactly as a prune would, and the lock is then released. The submit must
// refuse the run, naming the vanished release, rather than queue work against a
// snapshot that no longer exists.
func TestSubmitRefusesAReleasePrunedUnderTheWriteLock(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "plain", noCronJob, noopPython)
	dataDir := t.TempDir()
	d := newDaemon(t, root, dataDir, nil, nil)
	ctx := context.Background()
	jobID := runtimeID(t, d, "plain")

	releaseDir, _, ok, err := release.ActiveSourceDir(dataDir, jobID)
	if err != nil || !ok {
		t.Fatalf("active release: ok=%v err=%v", ok, err)
	}

	// A second connection stands in for the retention pass: it takes the
	// database write lock and holds it.
	holder, err := database.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	defer func() { _ = holder.Close() }()

	held := make(chan struct{})
	releaseLock := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- holder.Tx(ctx, func(tx *sql.Tx) error {
			close(held)
			<-releaseLock
			return nil
		})
	}()
	<-held

	// Start the submit. It reads the active release (still present), then blocks
	// at BEGIN IMMEDIATE because the holder owns the write lock.
	type submitResult struct {
		id  string
		err error
	}
	submit := make(chan submitResult, 1)
	go func() {
		id, err := d.SubmitRun(ctx, "id:"+jobID, api.TriggerPayload{Type: api.TriggerManual})
		submit <- submitResult{id: id, err: err}
	}()

	// Give the submit time to reach the transaction, then do what a prune does
	// to a doomed release: rename it out of the live tree. The rename is outside
	// the holder's transaction, exactly as Cleanup is outside the prune's.
	time.Sleep(300 * time.Millisecond)
	gone := releaseDir + ".pruned"
	if err := os.Rename(releaseDir, gone); err != nil {
		t.Fatalf("rename release aside: %v", err)
	}
	close(releaseLock)
	if err := <-holderDone; err != nil {
		t.Fatalf("holder transaction: %v", err)
	}

	result := <-submit
	if result.err == nil {
		t.Fatalf("a submit bound to a pruned release succeeded with run %s", result.id)
	}
	if !errors.Is(result.err, api.ErrConflict) {
		t.Fatalf("submit error = %v, want a conflict", result.err)
	}
	if !strings.Contains(result.err.Error(), "no longer on disk") {
		t.Fatalf("submit refused for the wrong reason: %v", result.err)
	}

	list, err := d.ListRuns(ctx, runs.Filter{JobID: jobID})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("a refused submit queued %d run(s)", len(list))
	}
}
