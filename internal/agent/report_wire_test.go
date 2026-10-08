package agent

// The report's observed RELEASE SET, pinned at the BYTES.
//
// A decoded struct cannot tell `"releases":[]` from `"releases"` being absent:
// both decode to a nil or empty slice, and Go's `reflect.DeepEqual` on a decoded
// value treats them the same. The control plane is the one that can tell them
// apart -- `Array.isArray(body.releases)` is true for `[]` and false for an
// absent field or `null` -- and it has to, because a removal is confirmed by the
// job's ABSENCE and "I never mentioned the set" is not evidence of absence.
//
// This is the bug being fixed: `omitempty` on the report's Releases dropped the
// field when a runtime served nothing, so the last removal on a runtime could
// never be confirmed. Its operation stayed pending, held the runtime lock, and
// every later deploy was refused with `operation_open`. `omitempty` alone was not
// the whole defect and removing it alone does not fix it: a nil slice marshals as
// `null`, which is ALSO not an array. The slice must be a NON-NIL EMPTY slice, and
// the tag must not omit it. So these tests assert on the serialised bytes.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A runtime that successfully answers the release question and serves NOTHING:
// the exact state left behind by the last removal on a pooled tenant.
func emptyRuntime(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": "v1"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runtime/maintenance":
			_ = json.NewEncoder(w).Encode(map[string]any{"mode": "serving", "running": 0})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runtime/releases/active":
			// An EMPTY active set, explicitly reported. This is what the runtime's
			// own /v1/runtime/releases/active handler sends once the release
			// directory is gone.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"active":                 []any{},
				"managed_jobs":           []string{},
				"managed_reconciliation": true,
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
}

// The report of a runtime serving NO releases must carry `releases: []` -- a
// present, EMPTY ARRAY. "Reported, and empty" is the only shape that can confirm
// the last removal; an absent field or `null` is "not reported" and must stay
// distinguishable from it.
func TestAnEmptyReleaseSetIsReportedAsAnEmptyArray(t *testing.T) {
	srv := emptyRuntime(t)
	defer srv.Close()

	rt := &RuntimeHTTP{BaseURL: srv.URL, Token: "t"}
	observed, err := rt.Observe(context.Background())
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	// The producer must give a NON-NIL empty slice. If it gave nil, the tag alone
	// could only ever produce `null`, which is not evidence.
	if observed.Releases == nil {
		t.Fatal("a successful read of an empty release set must produce a non-nil empty slice")
	}
	if len(observed.Releases) != 0 {
		t.Fatalf("the runtime serves nothing, got %#v", observed.Releases)
	}
	if !observed.ManagedReconciliation {
		t.Fatal("the runtime declared it can reconcile, so the empty set is a fact")
	}
	if observed.ManagedJobs == nil {
		t.Fatal("a reported managed set must be non-nil even when empty")
	}

	rep := convergedReport(&Desired{Generation: 7}, observed)
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, `"releases":[]`) {
		t.Errorf("an empty observed set must serialise as `\"releases\":[]`; got %s", body)
	}
	if strings.Contains(body, `"releases":null`) {
		t.Errorf("`null` reads as not-reported in Cloud, which cannot confirm a removal; got %s", body)
	}
	if strings.Contains(body, `"releases":`) == false {
		t.Errorf("the releases field must be PRESENT, not omitted; got %s", body)
	}
	if !strings.Contains(body, `"managed_jobs":[]`) {
		t.Errorf("an empty managed set must serialise as `\"managed_jobs\":[]`; got %s", body)
	}
}

// The apply path, not just the type: converging a runtime whose last job was
// removed produces the empty-set report that closes the operation.
func TestAConvergedEmptySnapshotReportsAnEmptyArray(t *testing.T) {
	rt := newPoolRuntime() // serves nothing, manages nothing
	want := snapshot(9)    // the desired snapshot is empty: the removal fully landed

	rep, err := Apply(context.Background(), rt, want, 9, drain(), observe(t, rt))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("outcome = %s (%s)", rep.Outcome, rep.Reason)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"releases":[]`) {
		t.Errorf("the empty-set report must carry `\"releases\":[]`; got %s", raw)
	}
}

// Regression: a report that DOES carry releases serialises them unchanged. The
// fix must not turn a real set into an empty one or drop it.
func TestANonEmptyReportStillSerialisesTheFullSet(t *testing.T) {
	rt := newPoolRuntime()
	rt.active["a"] = poolDigest("a", 1)
	rt.active["b"] = poolDigest("b", 1)
	rt.managed["a"], rt.managed["b"] = true, true

	want := snapshot(5, desiredJob("a", 1), desiredJob("b", 1))
	rep, err := Apply(context.Background(), rt, want, 5, drain(), observe(t, rt))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	// The fake reports in a deterministic (job-sorted) order, so the exact array
	// is assertable -- and an exact assertion catches a reordered or truncated set
	// that a length check would miss.
	wantSet := `"releases":[{"job":"a","digest":"` + rt.active["a"] +
		`"},{"job":"b","digest":"` + rt.active["b"] + `"}]`
	if !strings.Contains(string(raw), wantSet) {
		t.Errorf("a non-empty report must serialise its full set unchanged;\nwant %s\ngot  %s", wantSet, raw)
	}
	if strings.Contains(string(raw), `"releases":[]`) {
		t.Errorf("a non-empty report must not serialise an empty set; got %s", raw)
	}
}

// The other side of the distinction: a report with NO set at all (an agent that
// only speaks the single-release shape) must serialise as `null`, not `[]`. If it
// serialised as `[]`, Cloud would read silence as "serves nothing" and confirm a
// removal the agent never confirmed -- the exact bug the guard exists to stop.
func TestAReportWithNoSetSerialisesAsNullNotAnEmptyArray(t *testing.T) {
	rep := Reported{
		RuntimeID:  "rt-1",
		Generation: 3,
		Outcome:    OutcomeApplied,
		Observed:   map[string]string{"release_digest": "sha256:aa"},
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, `"releases":null`) {
		t.Errorf("a report with no set must serialise `\"releases\":null`; got %s", body)
	}
	if strings.Contains(body, `"releases":[]`) {
		t.Errorf("a report with no set must NOT read as an empty set; got %s", body)
	}
}

// A FAILED read of the active set is "not reported", not "reported empty". A
// runtime that could not answer must never be able to confirm a removal, so the
// producer must leave the slice nil rather than pre-initialising it, and the
// report must carry `null`, never `[]`.
func TestAFailedReleaseReadIsReportedAsNotReported(t *testing.T) {
	srv := idleRuntime(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/runtime/releases/active" {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	})
	defer srv.Close()

	rt := &RuntimeHTTP{BaseURL: srv.URL, Token: "t"}
	observed, err := rt.Observe(context.Background())
	if err != nil {
		t.Fatalf("a failed release read is not fatal to observing health and maintenance: %v", err)
	}
	if observed.Releases != nil {
		t.Fatalf("a failed read must be NOT REPORTED (nil), got %#v", observed.Releases)
	}
	if observed.ManagedReconciliation {
		t.Fatal("a failed read cannot claim the runtime reconciles")
	}

	raw, err := json.Marshal(convergedReport(&Desired{Generation: 4}, observed))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"releases":null`) {
		t.Errorf("a failed read must serialise `\"releases\":null`; got %s", raw)
	}
	if strings.Contains(string(raw), `"releases":[]`) {
		t.Errorf("a failed read must not read as an empty set -- that would confirm a removal by silence; got %s", raw)
	}
}
