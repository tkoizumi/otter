package daemon

import (
	"context"
	"testing"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/runs"
)

// A control command is delivered at least once, so the RUNTIME -- the component
// that actually accepts work -- must deduplicate it. The same caller key returns
// the first run instead of starting a second; a different key is a different
// command; and an unkeyed submit keeps the historical no-dedup behaviour.
func TestSubmitRunIsIdempotentOnTheCallerKey(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "plain", noCronJob, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "plain")

	first, err := d.SubmitRunWithOptions(ctx, "id:"+id,
		api.TriggerPayload{Type: api.TriggerManual}, api.SubmitRunOptions{IdempotencyKey: "cmd-1"})
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}

	// The retry of the SAME command must not start a second run.
	retry, err := d.SubmitRunWithOptions(ctx, "id:"+id,
		api.TriggerPayload{Type: api.TriggerManual}, api.SubmitRunOptions{IdempotencyKey: "cmd-1"})
	if err != nil {
		t.Fatalf("retry submit: %v", err)
	}
	if retry != first {
		t.Fatalf("retry returned %s, want the first run %s", retry, first)
	}

	// A different command is a different run.
	second, err := d.SubmitRunWithOptions(ctx, "id:"+id,
		api.TriggerPayload{Type: api.TriggerManual}, api.SubmitRunOptions{IdempotencyKey: "cmd-2"})
	if err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if second == first {
		t.Fatal("a different idempotency key must create a different run")
	}

	// An unkeyed submit is not deduplicated against a keyed one.
	unkeyed, err := d.SubmitRunWithOptions(ctx, "id:"+id,
		api.TriggerPayload{Type: api.TriggerManual}, api.SubmitRunOptions{})
	if err != nil {
		t.Fatalf("unkeyed submit: %v", err)
	}
	if unkeyed == first || unkeyed == second {
		t.Fatal("an unkeyed submit must not be folded into a keyed command")
	}

	// Exactly three run rows exist: the retry added none.
	got, err := d.runs.List(ctx, runs.Filter{JobID: id})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("run rows = %d, want 3 (first, second, unkeyed): %+v", len(got), runIDs(got))
	}
}

func runIDs(list []*runs.Run) []string {
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, r.ID)
	}
	return out
}
