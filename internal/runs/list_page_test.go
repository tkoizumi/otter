package runs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// seedRuns creates n runs whose created_at ascends with their index, so the
// newest-first listing has a predictable order (run-000 last).
func seedRuns(t *testing.T, store *Store, n int) []*Run {
	t.Helper()
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	out := make([]*Run, 0, n)
	for i := 0; i < n; i++ {
		r := sampleRun(fmt.Sprintf("run-%04d", i), StatusSucceeded, 1)
		r.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		if err := store.Create(context.Background(), r); err != nil {
			t.Fatalf("create run-%04d: %v", i, err)
		}
		out = append(out, r)
	}
	return out
}

// TestListPageWalksEveryRunExactlyOnce is the point of the change: a caller
// paging at 100 must reach every row, in order, with no repeats and no gaps.
func TestListPageWalksEveryRunExactlyOnce(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	seedRuns(t, store, 250)

	var (
		got      []string
		cursor   string
		pages    int
		lastSeen string
	)
	for {
		page, err := store.ListPage(ctx, Filter{Limit: 100, Cursor: cursor})
		if err != nil {
			t.Fatalf("list page %d: %v", pages+1, err)
		}
		pages++
		if len(page.Runs) == 0 {
			t.Fatal("a page before the end should not be empty")
		}
		for _, r := range page.Runs {
			// Newest first: every id must sort below the previous one.
			if lastSeen != "" && r.ID >= lastSeen {
				t.Fatalf("order broke: %s came after %s", r.ID, lastSeen)
			}
			lastSeen = r.ID
			got = append(got, r.ID)
		}
		if !page.HasMore {
			if page.NextCursor != "" {
				t.Errorf("last page returned a cursor %q", page.NextCursor)
			}
			break
		}
		if page.NextCursor == "" {
			t.Fatal("has_more with an empty cursor leaves no way to continue")
		}
		cursor = page.NextCursor
		if pages > 5 {
			t.Fatal("paging did not terminate")
		}
	}

	if len(got) != 250 {
		t.Fatalf("walked %d runs, want 250", len(got))
	}
	if pages != 3 {
		t.Errorf("walked 250 runs at 100 per page in %d pages, want 3", pages)
	}
	seen := make(map[string]bool, len(got))
	for _, id := range got {
		if seen[id] {
			t.Fatalf("%s returned twice", id)
		}
		seen[id] = true
	}
}

// TestListPageIsStableWhenARunArrivesBetweenPages pins the reason the cursor is
// a keyset over (created_at, rowid) rather than an offset: a row inserted at the
// top of the listing while an operator is paging must not shift the window, or
// page two repeats a row it already showed (or skips the one it pushed down).
func TestListPageIsStableWhenARunArrivesBetweenPages(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	seedRuns(t, store, 250)

	first, err := store.ListPage(ctx, Filter{Limit: 100})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if !first.HasMore {
		t.Fatal("first page of 250 at 100 should report more")
	}

	// A newer run lands while the operator is still on page one.
	newest := sampleRun("run-newest", StatusSucceeded, 1)
	newest.CreatedAt = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Create(ctx, newest); err != nil {
		t.Fatalf("create late run: %v", err)
	}

	second, err := store.ListPage(ctx, Filter{Limit: 100, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}

	onFirst := make(map[string]bool, len(first.Runs))
	for _, r := range first.Runs {
		onFirst[r.ID] = true
	}
	for _, r := range second.Runs {
		if onFirst[r.ID] {
			t.Fatalf("%s appeared on both page one and page two", r.ID)
		}
		if r.ID == newest.ID {
			t.Fatal("the run inserted after page one appeared on page two, which means the window shifted")
		}
	}
	if len(second.Runs) != 100 {
		t.Fatalf("second page has %d runs, want 100 (a shifted window would have dropped one)", len(second.Runs))
	}
}

// TestListPageFiltersByRunIDPrefix covers the search the UI needs: a full id,
// a unique prefix, and a prefix matching nothing (an empty list, not an error).
func TestListPageFiltersByRunIDPrefix(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	seedRuns(t, store, 20)

	cases := []struct {
		name      string
		prefix    string
		wantCount int
		wantFirst string
	}{
		{name: "full id", prefix: "run-0007", wantCount: 1, wantFirst: "run-0007"},
		{name: "unique prefix", prefix: "run-001", wantCount: 10, wantFirst: "run-0019"},
		{name: "matches everything", prefix: "run-", wantCount: 20, wantFirst: "run-0019"},
		{name: "matches nothing", prefix: "nope-", wantCount: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := store.ListPage(ctx, Filter{Limit: 100, RunIDPrefix: tc.prefix})
			if err != nil {
				t.Fatalf("list page: %v", err)
			}
			if len(page.Runs) != tc.wantCount {
				t.Fatalf("got %d runs for prefix %q, want %d", len(page.Runs), tc.prefix, tc.wantCount)
			}
			if tc.wantCount > 0 && page.Runs[0].ID != tc.wantFirst {
				t.Fatalf("first run is %s, want %s", page.Runs[0].ID, tc.wantFirst)
			}
		})
	}
}

// TestListPagePrefixAndJobFilterCombine keeps the two predicates from
// accidentally replacing one another.
func TestListPagePrefixAndJobFilterCombine(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	seedRuns(t, store, 5)

	other := sampleRun("run-0001-other", StatusSucceeded, 1)
	other.JobID = "other-job"
	other.CreatedAt = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	if err := store.Create(ctx, other); err != nil {
		t.Fatalf("create other job run: %v", err)
	}

	page, err := store.ListPage(ctx, Filter{Limit: 100, RunIDPrefix: "run-0001", JobID: "other-job"})
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if len(page.Runs) != 1 || page.Runs[0].ID != "run-0001-other" {
		t.Fatalf("got %d runs, want only run-0001-other", len(page.Runs))
	}
}

// TestListPageRejectsBadInput keeps a mangled cursor from silently returning
// page one, and an oversized page from silently shrinking to the default.
func TestListPageRejectsBadInput(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	seedRuns(t, store, 3)

	if _, err := store.ListPage(ctx, Filter{Cursor: "not-a-cursor"}); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("mangled cursor: got %v, want ErrInvalidCursor", err)
	}
	if _, err := store.ListPage(ctx, Filter{Limit: maxListLimit + 1}); !errors.Is(err, ErrLimitTooLarge) {
		t.Errorf("oversized limit: got %v, want ErrLimitTooLarge", err)
	}
}

// TestListPageStillHonoursOffset keeps the pre-existing offset callers working:
// a cursor was added, not swapped in for what was already there.
func TestListPageStillHonoursOffset(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	seedRuns(t, store, 10)

	first, err := store.ListPage(ctx, Filter{Limit: 5})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	second, err := store.ListPage(ctx, Filter{Limit: 5, Offset: 5})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Runs) != 5 {
		t.Fatalf("offset page has %d runs, want 5", len(second.Runs))
	}
	if first.Runs[0].ID == second.Runs[0].ID {
		t.Fatal("offset and no-offset returned the same first row")
	}
	if second.HasMore {
		t.Error("the last five of ten should report no more")
	}
}
