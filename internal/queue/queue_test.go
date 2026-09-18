package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/database"
)

// fakeCapacity is a deterministic test double for Capacity. An integration
// absent from limit (or with a non-positive limit) is unlimited.
type fakeCapacity struct {
	mu      sync.Mutex
	limit   map[string]int
	running map[string]int
}

func newFakeCapacity(limits map[string]int) *fakeCapacity {
	return &fakeCapacity{limit: limits, running: map[string]int{}}
}

func (c *fakeCapacity) Reserve(integrationID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	limit, hasLimit := c.limit[integrationID]
	if hasLimit && limit > 0 && c.running[integrationID] >= limit {
		return false
	}
	c.running[integrationID]++
	return true
}

func (c *fakeCapacity) Release(integrationID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running[integrationID] > 0 {
		c.running[integrationID]--
	}
}

func (c *fakeCapacity) runningFor(integrationID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running[integrationID]
}

func newTestQueue(t *testing.T) (*Queue, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("database.Migrate() error = %v", err)
	}
	return New(db.DB), ctx
}

// testBase is a fixed timestamp so ordering assertions are deterministic.
var testBase = time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)

func mustEnqueue(t *testing.T, q *Queue, ctx context.Context, runID, integrationID string, availableAt time.Time) {
	t.Helper()
	if err := q.Enqueue(ctx, runID, integrationID, availableAt); err != nil {
		t.Fatalf("Enqueue(%q) error = %v", runID, err)
	}
}

func mustDepth(t *testing.T, q *Queue, ctx context.Context) int {
	t.Helper()
	n, err := q.Depth(ctx)
	if err != nil {
		t.Fatalf("Depth() error = %v", err)
	}
	return n
}

func TestEnqueueContainsRemove(t *testing.T) {
	q, ctx := newTestQueue(t)
	mustEnqueue(t, q, ctx, "run-1", "int-a", testBase)

	found, err := q.Contains(ctx, "run-1")
	if err != nil {
		t.Fatalf("Contains() error = %v", err)
	}
	if !found {
		t.Errorf("Contains(run-1) = false, want true")
	}
	if n := mustDepth(t, q, ctx); n != 1 {
		t.Errorf("Depth() = %d, want 1", n)
	}

	removed, err := q.Remove(ctx, "run-1")
	if err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if !removed {
		t.Errorf("first Remove() = false, want true")
	}
	removed, err = q.Remove(ctx, "run-1")
	if err != nil {
		t.Fatalf("second Remove() error = %v", err)
	}
	if removed {
		t.Errorf("second Remove() = true, want false")
	}

	found, err = q.Contains(ctx, "run-1")
	if err != nil {
		t.Fatalf("Contains() after remove error = %v", err)
	}
	if found {
		t.Errorf("Contains(run-1) after removal = true, want false")
	}
}

func TestEnqueueIsIdempotent(t *testing.T) {
	q, ctx := newTestQueue(t)
	mustEnqueue(t, q, ctx, "run-1", "int-a", testBase)
	mustEnqueue(t, q, ctx, "run-1", "int-a", testBase.Add(time.Hour))

	if n := mustDepth(t, q, ctx); n != 1 {
		t.Errorf("Depth() after duplicate enqueue = %d, want 1", n)
	}
	items, err := q.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List() returned %d items, want 1", len(items))
	}
	if !items[0].AvailableAt.Equal(testBase.Add(time.Hour)) {
		t.Errorf("AvailableAt = %s, want the upserted %s", items[0].AvailableAt, testBase.Add(time.Hour))
	}
}

func TestClaimEmptyQueue(t *testing.T) {
	q, ctx := newTestQueue(t)

	if _, err := q.Claim(ctx, testBase, nil); !errors.Is(err, ErrEmpty) {
		t.Errorf("Claim(nil capacity) error = %v, want ErrEmpty", err)
	}
	if _, err := q.Claim(ctx, testBase, newFakeCapacity(nil)); !errors.Is(err, ErrEmpty) {
		t.Errorf("Claim(fake capacity) error = %v, want ErrEmpty", err)
	}
}

func TestClaimRespectsAvailableAt(t *testing.T) {
	t.Run("future run is not claimable yet", func(t *testing.T) {
		q, ctx := newTestQueue(t)
		future := testBase.Add(time.Hour)
		mustEnqueue(t, q, ctx, "run-future", "int-a", future)

		if _, err := q.Claim(ctx, testBase, nil); !errors.Is(err, ErrEmpty) {
			t.Fatalf("Claim(now) error = %v, want ErrEmpty for a future run", err)
		}

		item, err := q.Claim(ctx, future, nil)
		if err != nil {
			t.Fatalf("Claim(available_at) error = %v", err)
		}
		if item.RunID != "run-future" {
			t.Errorf("Claim(available_at).RunID = %q, want run-future", item.RunID)
		}
	})

	t.Run("past-due run is claimable", func(t *testing.T) {
		q, ctx := newTestQueue(t)
		mustEnqueue(t, q, ctx, "run-past", "int-a", testBase.Add(-time.Hour))

		item, err := q.Claim(ctx, testBase, nil)
		if err != nil {
			t.Fatalf("Claim() error = %v", err)
		}
		if item.RunID != "run-past" {
			t.Errorf("Claim().RunID = %q, want run-past", item.RunID)
		}
	})
}

func TestClaimReturnsOldestFirst(t *testing.T) {
	q, ctx := newTestQueue(t)
	// Enqueue out of chronological order.
	mustEnqueue(t, q, ctx, "run-3", "int-a", testBase.Add(2*time.Minute))
	mustEnqueue(t, q, ctx, "run-1", "int-a", testBase)
	mustEnqueue(t, q, ctx, "run-2", "int-a", testBase.Add(time.Minute))

	now := testBase.Add(time.Hour)
	want := []string{"run-1", "run-2", "run-3"}
	for i, wantID := range want {
		item, err := q.Claim(ctx, now, nil)
		if err != nil {
			t.Fatalf("Claim #%d error = %v", i+1, err)
		}
		if item.RunID != wantID {
			t.Errorf("Claim #%d RunID = %q, want %q", i+1, item.RunID, wantID)
		}
	}
	if _, err := q.Claim(ctx, now, nil); !errors.Is(err, ErrEmpty) {
		t.Errorf("Claim() on drained queue error = %v, want ErrEmpty", err)
	}
}

func TestClaimRemovesRow(t *testing.T) {
	q, ctx := newTestQueue(t)
	mustEnqueue(t, q, ctx, "run-1", "int-a", testBase)
	mustEnqueue(t, q, ctx, "run-2", "int-a", testBase.Add(time.Minute))

	item, err := q.Claim(ctx, testBase.Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if item.RunID != "run-1" {
		t.Fatalf("Claim().RunID = %q, want run-1", item.RunID)
	}
	if found, err := q.Contains(ctx, "run-1"); err != nil {
		t.Fatalf("Contains() error = %v", err)
	} else if found {
		t.Errorf("Contains(claimed run) = true, want false")
	}
	if n := mustDepth(t, q, ctx); n != 1 {
		t.Errorf("Depth() after claim = %d, want 1", n)
	}
}

func TestClaimEnforcesCapacity(t *testing.T) {
	q, ctx := newTestQueue(t)
	capacity := newFakeCapacity(map[string]int{"a": 1})
	mustEnqueue(t, q, ctx, "a-1", "a", testBase)
	mustEnqueue(t, q, ctx, "a-2", "a", testBase.Add(time.Minute))

	now := testBase.Add(time.Hour)
	first, err := q.Claim(ctx, now, capacity)
	if err != nil {
		t.Fatalf("first Claim() error = %v", err)
	}
	if first.RunID != "a-1" {
		t.Fatalf("first Claim().RunID = %q, want a-1", first.RunID)
	}
	if got := capacity.runningFor("a"); got != 1 {
		t.Errorf("reserved slots = %d, want 1", got)
	}

	// The second run belongs to a saturated integration: it must not be handed
	// out, even though it is available and at the head of the queue.
	if _, err := q.Claim(ctx, now, capacity); !errors.Is(err, ErrEmpty) {
		t.Fatalf("second Claim() error = %v, want ErrEmpty while at capacity", err)
	}
	if got := capacity.runningFor("a"); got != 1 {
		t.Errorf("reserved slots after failed claim = %d, want 1", got)
	}
	if found, err := q.Contains(ctx, "a-2"); err != nil {
		t.Fatalf("Contains(a-2) error = %v", err)
	} else if !found {
		t.Errorf("a-2 was removed despite capacity rejection")
	}

	capacity.Release("a")
	if got := capacity.runningFor("a"); got != 0 {
		t.Errorf("reserved slots after Release = %d, want 0", got)
	}

	second, err := q.Claim(ctx, now, capacity)
	if err != nil {
		t.Fatalf("Claim() after Release error = %v", err)
	}
	if second.RunID != "a-2" {
		t.Errorf("second Claim().RunID = %q, want a-2", second.RunID)
	}
}

func TestClaimCapacityIsPerIntegration(t *testing.T) {
	q, ctx := newTestQueue(t)
	capacity := newFakeCapacity(map[string]int{"a": 1})
	mustEnqueue(t, q, ctx, "a-1", "a", testBase)
	mustEnqueue(t, q, ctx, "a-2", "a", testBase.Add(time.Minute))
	mustEnqueue(t, q, ctx, "b-1", "b", testBase.Add(2*time.Minute))

	now := testBase.Add(time.Hour)
	first, err := q.Claim(ctx, now, capacity)
	if err != nil {
		t.Fatalf("first Claim() error = %v", err)
	}
	if first.RunID != "a-1" {
		t.Fatalf("first Claim().RunID = %q, want a-1", first.RunID)
	}

	// "a" is saturated, but "b" has spare capacity, so the queue skips the
	// blocked candidate and hands out b-1.
	next, err := q.Claim(ctx, now, capacity)
	if err != nil {
		t.Fatalf("second Claim() error = %v", err)
	}
	if next.RunID != "b-1" {
		t.Errorf("second Claim().RunID = %q, want b-1", next.RunID)
	}
	if got := capacity.runningFor("a"); got != 1 {
		t.Errorf("reserved slots for a = %d, want 1", got)
	}
	if got := capacity.runningFor("b"); got != 1 {
		t.Errorf("reserved slots for b = %d, want 1", got)
	}
}

func TestClaimConcurrentNoDoubleExecution(t *testing.T) {
	q, ctx := newTestQueue(t)
	const runs = 8

	ids := make([]string, runs)
	for i := 0; i < runs; i++ {
		ids[i] = fmt.Sprintf("run-%d", i)
		mustEnqueue(t, q, ctx, ids[i], "int-a", testBase)
	}

	var (
		mu      sync.Mutex
		claimed = map[string]int{}
		errs    []error
		wg      sync.WaitGroup
	)
	now := testBase.Add(time.Hour)
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			item, err := q.Claim(ctx, now, nil)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			claimed[item.RunID]++
		}()
	}
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("concurrent Claim() errors = %v", errs)
	}
	if len(claimed) != runs {
		t.Errorf("claimed %d distinct runs, want %d", len(claimed), runs)
	}
	for _, id := range ids {
		if got := claimed[id]; got != 1 {
			t.Errorf("run %q was claimed %d times, want exactly 1", id, got)
		}
	}
	for id, count := range claimed {
		if count != 1 {
			t.Errorf("run %q was claimed %d times, want exactly 1", id, count)
		}
	}

	if _, err := q.Claim(ctx, now, nil); !errors.Is(err, ErrEmpty) {
		t.Errorf("Claim() after concurrent drain error = %v, want ErrEmpty", err)
	}
}

func TestDepthByIntegrationListAndDiscardAll(t *testing.T) {
	q, ctx := newTestQueue(t)
	mustEnqueue(t, q, ctx, "a-2", "a", testBase.Add(time.Minute))
	mustEnqueue(t, q, ctx, "a-1", "a", testBase)
	mustEnqueue(t, q, ctx, "b-1", "b", testBase.Add(30*time.Second))

	byIntegration, err := q.DepthByIntegration(ctx)
	if err != nil {
		t.Fatalf("DepthByIntegration() error = %v", err)
	}
	wantDepths := map[string]int{"a": 2, "b": 1}
	if len(byIntegration) != len(wantDepths) {
		t.Fatalf("DepthByIntegration() = %v, want %v", byIntegration, wantDepths)
	}
	for id, want := range wantDepths {
		if got := byIntegration[id]; got != want {
			t.Errorf("DepthByIntegration()[%q] = %d, want %d", id, got, want)
		}
	}

	items, err := q.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	wantOrder := []string{"a-1", "b-1", "a-2"}
	if len(items) != len(wantOrder) {
		t.Fatalf("List() returned %d items, want %d", len(items), len(wantOrder))
	}
	for i, wantID := range wantOrder {
		if items[i].RunID != wantID {
			t.Errorf("List()[%d].RunID = %q, want %q (oldest first)", i, items[i].RunID, wantID)
		}
	}

	removed, err := q.DiscardAll(ctx)
	if err != nil {
		t.Fatalf("DiscardAll() error = %v", err)
	}
	if removed != 3 {
		t.Errorf("DiscardAll() removed %d, want 3", removed)
	}
	if n := mustDepth(t, q, ctx); n != 0 {
		t.Errorf("Depth() after DiscardAll = %d, want 0", n)
	}

	removed, err = q.DiscardAll(ctx)
	if err != nil {
		t.Fatalf("second DiscardAll() error = %v", err)
	}
	if removed != 0 {
		t.Errorf("second DiscardAll() removed %d, want 0", removed)
	}
}
