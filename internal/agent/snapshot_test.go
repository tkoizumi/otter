package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// The per-job desired state, exercised against a runtime that models the thing
// the whole change turns on: a release STORE, keyed by durable job id, from
// which the runtime derives its job list.
//
// The fake is deliberately a store rather than a list of "applied" flags. A
// deleted job disappearing from the runtime's own view is the property that
// stops it being rediscovered, and a fake that remembered an "applied" set would
// test nothing about that.

// digestOf builds a valid sha256 digest that is distinct per job and version, so
// "did this job move" is answerable by inspection.
func poolDigest(job string, version int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", job, version)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func bare(d string) string {
	d = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(d)), "sha256:")
	return d
}

type poolRuntime struct {
	calls []string

	// active is what the release store currently SERVES, keyed by job id. A job
	// absent from it has no active release and is not discoverable.
	active map[string]string
	// managed is the set of released-only jobs: the ones a control plane owns.
	managed map[string]bool
	// workspace marks jobs whose source lives in the jobs directory. The runtime
	// refuses to delete them, exactly as the API handler does.
	workspace map[string]bool

	gated bool

	fetchErrs  map[string]error
	deleteErrs map[string]error
	promoteErr error

	// owner maps a digest to the job its URL named, so Promote can update the
	// right entry without the agent telling it.
	owner map[string]string
}

func newPoolRuntime() *poolRuntime {
	return &poolRuntime{
		active:     map[string]string{},
		managed:    map[string]bool{},
		workspace:  map[string]bool{},
		fetchErrs:  map[string]error{},
		deleteErrs: map[string]error{},
		owner:      map[string]string{},
	}
}

func (f *poolRuntime) Observe(context.Context) (Observed, error) {
	f.calls = append(f.calls, "observe")
	releases := make([]ObservedRelease, 0, len(f.active))
	for job, digest := range f.active {
		releases = append(releases, ObservedRelease{Job: job, Digest: digest})
	}
	sort.Slice(releases, func(i, j int) bool { return releases[i].Job < releases[j].Job })
	managed := make([]string, 0, len(f.managed))
	for job := range f.managed {
		managed = append(managed, job)
	}
	sort.Strings(managed)

	digest := ""
	if len(releases) == 1 {
		digest = releases[0].Digest
	}
	return Observed{
		ReleaseDigest:         digest,
		Releases:              releases,
		ManagedJobs:           managed,
		ManagedReconciliation: true,
		Maintenance:           "serving",
	}, nil
}

func (f *poolRuntime) EnterMaintenance(_ context.Context, reason string, _ time.Duration) error {
	f.calls = append(f.calls, "enter:"+reason)
	f.gated = true
	return nil
}

func (f *poolRuntime) ExitMaintenance(context.Context) error {
	f.calls = append(f.calls, "exit")
	f.gated = false
	return nil
}

func (f *poolRuntime) Promote(_ context.Context, digest string) error {
	f.calls = append(f.calls, "promote:"+digest)
	if f.promoteErr != nil {
		return f.promoteErr
	}
	job, ok := f.owner[bare(digest)]
	if !ok {
		return fmt.Errorf("no job has release %s", digest)
	}
	f.active[job] = digest
	return nil
}

func (f *poolRuntime) Fetch(_ context.Context, rel Release) error {
	f.calls = append(f.calls, "fetch:"+rel.Digest)
	if err := f.fetchErrs[rel.Digest]; err != nil {
		return err
	}
	if job := jobFromURL(rel.URL); job != "" {
		f.owner[bare(rel.Digest)] = job
	}
	return nil
}

func (f *poolRuntime) Validate(_ context.Context, digest string) error {
	f.calls = append(f.calls, "validate:"+digest)
	return nil
}

// DeleteJob is the runtime's own delete: it refuses a workspace job outright,
// and otherwise removes the release-store entry. That refusal is the guard that
// keeps a control plane from removing work it never owned.
func (f *poolRuntime) DeleteJob(_ context.Context, job string) (string, error) {
	f.calls = append(f.calls, "delete:"+job)
	if f.workspace[job] {
		return "", fmt.Errorf("job %q has a source in the jobs directory and is not Cloud-managed", job)
	}
	if err := f.deleteErrs[job]; err != nil {
		return "", err
	}
	if _, ok := f.active[job]; !ok && !f.managed[job] {
		return "", fmt.Errorf("job %q not found", job)
	}
	delete(f.active, job)
	delete(f.managed, job)
	return "deleted " + job, nil
}

func jobFromURL(url string) string {
	if url == "" {
		return ""
	}
	parts := strings.Split(strings.TrimRight(url, "/"), "/")
	return parts[len(parts)-1]
}

// desiredJob builds one snapshot entry. The URL's last segment is the job id,
// which is how the fake resolves a promote back to a job.
func desiredJob(job string, version int) DesiredJob {
	return DesiredJob{
		Job:    job,
		Digest: poolDigest(job, version),
		URL:    "https://cloud.invalid/" + job,
	}
}

func snapshot(generation int64, jobs ...DesiredJob) *Desired {
	return &Desired{Generation: generation, Snapshot: true, Jobs: jobs}
}

func observe(t *testing.T, rt *poolRuntime) Observed {
	t.Helper()
	obs, err := rt.Observe(context.Background())
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	// The observe call is bookkeeping, not an assertion.
	if n := len(rt.calls); n > 0 && rt.calls[n-1] == "observe" {
		rt.calls = rt.calls[:n-1]
	}
	return obs
}

func callsContaining(calls []string, prefix string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func drain() Drain { return Drain{Timeout: time.Minute} }

// Two jobs on one runtime: deploying or updating ONE must leave the other
// alone. This is the core Otter Cloud requirement, so it is the first thing
// asserted.
func TestSnapshotUpdatesOneJobAndPreservesTheOther(t *testing.T) {
	rt := newPoolRuntime()
	rt.active["a"] = poolDigest("a", 1)
	rt.active["b"] = poolDigest("b", 1)
	rt.managed["a"], rt.managed["b"] = true, true

	want := snapshot(1, desiredJob("a", 2), desiredJob("b", 1))
	rep, err := Apply(context.Background(), rt, want, 0, drain(), observe(t, rt))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("outcome = %s (%s)", rep.Outcome, rep.Reason)
	}

	if got := callsContaining(rt.calls, "fetch:"); len(got) != 1 || got[0] != "fetch:"+poolDigest("a", 2) {
		t.Errorf("only job a's release should be fetched, got %v", got)
	}
	if got := callsContaining(rt.calls, "promote:"); len(got) != 1 || got[0] != "promote:"+poolDigest("a", 2) {
		t.Errorf("only job a should be promoted, got %v", got)
	}
	if got := callsContaining(rt.calls, "delete:"); len(got) != 0 {
		t.Errorf("an update must not remove anything, got %v", got)
	}
	// The other job is untouched, and the updated one moved.
	if rt.active["b"] != poolDigest("b", 1) {
		t.Errorf("job b's release changed: %s", rt.active["b"])
	}
	if rt.active["a"] != poolDigest("a", 2) {
		t.Errorf("job a was not updated: %s", rt.active["a"])
	}
}

// The normal case: a control plane publishes desired state, so after a
// successful apply every poll asks for exactly what is running. It must cause
// no work at all.
func TestAnAlreadyConvergedSnapshotTouchesNothing(t *testing.T) {
	rt := newPoolRuntime()
	rt.active["a"] = poolDigest("a", 1)
	rt.active["b"] = poolDigest("b", 1)
	rt.managed["a"], rt.managed["b"] = true, true

	want := snapshot(4, desiredJob("a", 1), desiredJob("b", 1))
	rep, err := Apply(context.Background(), rt, want, 4, drain(), observe(t, rt))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("outcome = %s (%s)", rep.Outcome, rep.Reason)
	}
	if len(rt.calls) != 0 {
		t.Fatalf("a converged snapshot must do nothing, calls = %v", rt.calls)
	}
	// The reported set is the evidence Cloud closes on; it must carry both jobs.
	if len(rep.Releases) != 2 {
		t.Errorf("the report must carry the full set, got %v", rep.Releases)
	}
}

// Deleting one job leaves the other serving, removes the release-store entry,
// and does NOT gate the runtime: the surviving job never stops.
func TestDeletingAJobRemovesOnlyThatJobAndNeverGatesTheOther(t *testing.T) {
	rt := newPoolRuntime()
	rt.active["a"] = poolDigest("a", 1)
	rt.active["b"] = poolDigest("b", 1)
	rt.managed["a"], rt.managed["b"] = true, true

	want := snapshot(2, desiredJob("b", 1))
	rep, err := Apply(context.Background(), rt, want, 1, drain(), observe(t, rt))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("outcome = %s (%s)", rep.Outcome, rep.Reason)
	}
	if got := callsContaining(rt.calls, "delete:"); len(got) != 1 || got[0] != "delete:a" {
		t.Fatalf("want exactly delete:a, got %v", rt.calls)
	}
	if got := callsContaining(rt.calls, "enter:"); len(got) != 0 {
		t.Errorf("a removal must not gate the runtime, got %v", got)
	}
	if rt.gated {
		t.Error("the runtime was left gated")
	}
	if _, ok := rt.active["a"]; ok {
		t.Error("job a's release store entry survived the delete")
	}
	if rt.active["b"] != poolDigest("b", 1) {
		t.Errorf("job b stopped serving: %s", rt.active["b"])
	}
	if rt.managed["a"] {
		t.Error("job a is still reported as managed after its removal")
	}
}

// Requirement: a deleted job must not come back after an AGENT restart. The
// agent has no memory, so the test restarts it (a fresh run over the same
// runtime) and asserts the job stays gone and is not removed twice.
func TestADeletedJobDoesNotSurviveAnAgentRestart(t *testing.T) {
	rt := newPoolRuntime()
	rt.active["a"] = poolDigest("a", 1)
	rt.active["b"] = poolDigest("b", 1)
	rt.managed["a"], rt.managed["b"] = true, true

	want := snapshot(2, desiredJob("b", 1))
	if _, err := Apply(context.Background(), rt, want, 1, drain(), observe(t, rt)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	rt.calls = nil

	// The agent process restarts: no applied generation, no memory. It observes
	// the SAME runtime and converges the SAME snapshot again.
	observed := observe(t, rt)
	if len(observed.Releases) != 1 || observed.Releases[0].Job != "b" {
		t.Fatalf("the deleted job is still visible to a restarted agent: %+v", observed.Releases)
	}
	rep, err := Apply(context.Background(), rt, want, 0, drain(), observed)
	if err != nil {
		t.Fatalf("re-apply after restart: %v", err)
	}
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("outcome = %s (%s)", rep.Outcome, rep.Reason)
	}
	if got := callsContaining(rt.calls, "delete:"); len(got) != 0 {
		t.Errorf("a removed job must not be removed again, got %v", got)
	}
	if _, ok := rt.active["a"]; ok {
		t.Error("the job came back after an agent restart")
	}
}

// Crash recovery: the agent dies mid-reconcile (the removal never happened) and
// a fresh agent converges without resurrecting anything and without silently
// dropping the deletion.
func TestCrashMidReconcileConvergesOnTheNextRun(t *testing.T) {
	rt := newPoolRuntime()
	rt.active["a"] = poolDigest("a", 1)
	rt.active["b"] = poolDigest("b", 1)
	rt.managed["a"], rt.managed["b"] = true, true
	// The runtime refuses the removal once, as it would for an in-flight run.
	rt.deleteErrs["a"] = fmt.Errorf("job \"a\" has a running run (run_1)")

	want := snapshot(2, desiredJob("b", 1))
	rep, err := Apply(context.Background(), rt, want, 1, drain(), observe(t, rt))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Outcome != OutcomeFailed {
		t.Fatalf("a refused removal must be reported failed, got %s", rep.Outcome)
	}
	if !strings.Contains(rep.Reason, "remove a") {
		t.Errorf("the failure must name the job: %q", rep.Reason)
	}
	if _, ok := rt.active["a"]; !ok {
		t.Fatal("the fixture is wrong: the refusal removed the job anyway")
	}

	// The in-flight run finishes and the agent comes back.
	delete(rt.deleteErrs, "a")
	rt.calls = nil
	rep, err = Apply(context.Background(), rt, want, 0, drain(), observe(t, rt))
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("outcome = %s (%s)", rep.Outcome, rep.Reason)
	}
	if _, ok := rt.active["a"]; ok {
		t.Error("the deleted job survived the crash and the retry")
	}
	if rt.active["b"] != poolDigest("b", 1) {
		t.Error("the surviving job was disturbed by the retry")
	}
}

// A workspace job must never be removed, however the snapshot changes. The
// runtime only reports released-only jobs as managed, so the agent does not even
// consider it.
func TestAWorkspaceJobIsNeverRemoved(t *testing.T) {
	rt := newPoolRuntime()
	rt.active["a"] = poolDigest("a", 1)
	rt.active["workspace"] = poolDigest("workspace", 1)
	rt.managed["a"] = true // only the released-only job is Cloud-managed
	rt.workspace["workspace"] = true

	// An empty snapshot: every MANAGED job is deleted.
	rep, err := Apply(context.Background(), rt, snapshot(5), 4, drain(), observe(t, rt))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("outcome = %s (%s)", rep.Outcome, rep.Reason)
	}
	if got := callsContaining(rt.calls, "delete:workspace"); len(got) != 0 {
		t.Fatalf("the agent tried to remove a workspace job: %v", rt.calls)
	}
	if rt.active["workspace"] != poolDigest("workspace", 1) {
		t.Error("the workspace job stopped serving")
	}
	if _, ok := rt.active["a"]; ok {
		t.Error("the managed job was not removed from an empty snapshot")
	}
}

// Even if a managed set were wrong, the runtime's refusal is the backstop: a
// workspace job is not removed, and the failure is REPORTED rather than hidden,
// so Cloud cannot mark the operation successful.
func TestARefusedWorkspaceRemovalIsReportedNotSwallowed(t *testing.T) {
	rt := newPoolRuntime()
	rt.active["workspace"] = poolDigest("workspace", 1)
	rt.managed["workspace"] = true // misreported as managed
	rt.workspace["workspace"] = true

	rep, err := Apply(context.Background(), rt, snapshot(5), 4, drain(), observe(t, rt))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Outcome != OutcomeFailed {
		t.Fatalf("a refused removal must not be reported applied, got %s", rep.Outcome)
	}
	if rt.active["workspace"] != poolDigest("workspace", 1) {
		t.Error("the workspace job was removed despite the runtime's refusal")
	}
}

// Changing one job AND deleting another in one snapshot must do both, in the
// order that keeps a failed download from gating a serving runtime.
func TestOneSnapshotCanUpdateOneJobAndDeleteAnother(t *testing.T) {
	rt := newPoolRuntime()
	rt.active["a"] = poolDigest("a", 1)
	rt.active["b"] = poolDigest("b", 1)
	rt.managed["a"], rt.managed["b"] = true, true

	want := snapshot(6, desiredJob("b", 2))
	rep, err := Apply(context.Background(), rt, want, 5, drain(), observe(t, rt))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("outcome = %s (%s)", rep.Outcome, rep.Reason)
	}
	if got := rt.calls; len(got) != 7 ||
		got[0] != "fetch:"+poolDigest("b", 2) ||
		got[1] != "delete:a" ||
		got[2] != "enter:deploy generation 6" ||
		got[3] != "promote:"+poolDigest("b", 2) ||
		got[4] != "validate:"+poolDigest("b", 2) ||
		got[5] != "exit" ||
		got[6] != "observe" {
		t.Errorf("call order = %v", rt.calls)
	}
	if _, ok := rt.active["a"]; ok {
		t.Error("job a was not deleted")
	}
	if rt.active["b"] != poolDigest("b", 2) {
		t.Errorf("job b was not updated: %s", rt.active["b"])
	}
}

// The version-skew guarantee, from the agent's side: an older control plane's
// single-release answer must never cause a removal. That is what makes a Cloud
// push and an image rebuild safe in either order.
func TestASingleReleaseAnswerRemovesNothing(t *testing.T) {
	rt := newPoolRuntime()
	rt.active["a"] = poolDigest("a", 1)
	rt.active["b"] = poolDigest("b", 1)
	rt.managed["a"], rt.managed["b"] = true, true

	// The legacy shape names one release and no snapshot.
	want := &Desired{
		Generation: 3,
		Release:    Release{Digest: poolDigest("a", 2), URL: "https://cloud.invalid/a"},
	}
	rep, err := Apply(context.Background(), rt, want, 2, drain(), observe(t, rt))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("outcome = %s (%s)", rep.Outcome, rep.Reason)
	}
	if got := callsContaining(rt.calls, "delete:"); len(got) != 0 {
		t.Errorf("a single-release answer must not remove anything, got %v", rt.calls)
	}
	if rt.active["b"] != poolDigest("b", 1) {
		t.Error("a single-release answer disturbed a job it did not name")
	}
}

// A failed download aborts the whole snapshot before anything is gated or
// promoted, so a serving runtime is never taken offline for a deploy that then
// does not happen.
func TestAFailedFetchAbortsBeforeGatingAnything(t *testing.T) {
	rt := newPoolRuntime()
	rt.active["a"] = poolDigest("a", 1)
	rt.managed["a"] = true
	rt.fetchErrs[poolDigest("a", 2)] = fmt.Errorf("the archive changed in transit")

	rep, err := Apply(context.Background(), rt, snapshot(2, desiredJob("a", 2)), 1, drain(), observe(t, rt))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Outcome != OutcomeFailed || !strings.Contains(rep.Reason, "fetch a") {
		t.Fatalf("want a fetch failure naming the job, got %+v", rep)
	}
	for _, c := range rt.calls {
		if strings.HasPrefix(c, "enter:") || strings.HasPrefix(c, "promote:") {
			t.Errorf("a failed fetch must not reach %q", c)
		}
	}
	if rt.gated {
		t.Error("a failed fetch must not gate the runtime")
	}
}

// The wire shape, pinned end to end. The Cloud side sends `snapshot` as an
// explicit marker, and a decoder that dropped it would read a per-job answer as
// the legacy single-release shape -- whose digest is empty -- so nothing would
// deploy and, worse, nothing would be removed.
//
// This is the exact body a control plane sends for two jobs, including the
// empty-snapshot case, decoded by the SAME struct the loop uses.
func TestTheWireSnapshotDecodesIntoThePerJobShape(t *testing.T) {
	var withJobs Desired
	const body = `{
	  "runtime_id": "rt-a",
	  "generation": 12,
	  "snapshot": true,
	  "jobs": [
	    {"job": "a", "digest": "sha256:aa", "url": "https://cloud.invalid/a"},
	    {"job": "b", "digest": "sha256:bb", "url": "https://cloud.invalid/b"}
	  ],
	  "lease_seconds": 60
	}`
	if err := json.Unmarshal([]byte(body), &withJobs); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if !withJobs.Snapshot {
		t.Fatal("the snapshot marker was lost, so a per-job answer reads as legacy")
	}
	if len(withJobs.Jobs) != 2 || withJobs.Jobs[0].Job != "a" || withJobs.Jobs[1].URL != "https://cloud.invalid/b" {
		t.Fatalf("jobs did not decode: %+v", withJobs.Jobs)
	}
	if withJobs.Release.Digest != "" {
		t.Errorf("the legacy release field must be empty on a snapshot answer, got %q", withJobs.Release.Digest)
	}

	// An EMPTY snapshot is a real answer -- every managed job was deleted -- and
	// it must not be mistaken for "no answer".
	var empty Desired
	if err := json.Unmarshal([]byte(`{"runtime_id":"rt-a","generation":13,"snapshot":true,"jobs":[]}`), &empty); err != nil {
		t.Fatalf("decode empty snapshot: %v", err)
	}
	if !empty.Snapshot || empty.Jobs == nil || len(empty.Jobs) != 0 {
		t.Fatalf("an empty snapshot did not decode as an empty set: %+v", empty)
	}

	// The legacy answer carries no marker, and the agent falls back to it.
	var legacy Desired
	if err := json.Unmarshal([]byte(`{"runtime_id":"rt-a","generation":3,"release":{"digest":"sha256:aa","url":"https://cloud.invalid/a"}}`), &legacy); err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if legacy.Snapshot {
		t.Error("a legacy answer must not claim to be a snapshot")
	}
	if legacy.Release.Digest != "sha256:aa" {
		t.Errorf("the legacy release did not decode: %+v", legacy.Release)
	}
}

// The report the agent sends must carry the release SET, because that is what a
// per-job operation closes on, and the field names are what the control plane
// reads. A rename on either side passes its own suite and breaks the closure.
func TestTheReportCarriesTheObservedSetOnTheWire(t *testing.T) {
	rep := Reported{
		RuntimeID:   "rt-a",
		Generation:  12,
		Outcome:     OutcomeApplied,
		Releases:    []ObservedRelease{{Job: "a", Digest: "sha256:aa"}, {Job: "b", Digest: "sha256:bb"}},
		ManagedJobs: []string{"a", "b"},
	}
	body, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if _, ok := decoded["releases"]; !ok {
		t.Fatalf("the report carries no release set: %s", body)
	}
	if _, ok := decoded["managed_jobs"]; !ok {
		t.Fatalf("the report carries no managed set: %s", body)
	}
}

// The desired REQUEST declares the capability that selects the answer shape.
func TestTheDesiredRequestDeclaresTheSnapshotCapability(t *testing.T) {
	body, err := json.Marshal(DesiredRequest{
		RuntimeID:    "rt-a",
		Observed:     Observed{ManagedReconciliation: true, Releases: []ObservedRelease{{Job: "a", Digest: "sha256:aa"}}},
		Capabilities: []string{CapabilityDesiredSnapshot},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	caps, ok := decoded["capabilities"].([]any)
	if !ok || len(caps) != 1 || caps[0] != CapabilityDesiredSnapshot {
		t.Fatalf("capabilities not sent: %s", body)
	}
	observed, ok := decoded["observed"].(map[string]any)
	if !ok {
		t.Fatalf("observed not sent: %s", body)
	}
	if _, ok := observed["managed_reconciliation"]; !ok {
		t.Errorf("the runtime's reconciliation capability must travel with the observation: %s", body)
	}
}

// A stale agent applies nothing, for either shape. Allowing it would let an
// agent that missed a newer generation roll the runtime backwards.
func TestASnapshotFromBehindTheGenerationIsFenced(t *testing.T) {
	rt := newPoolRuntime()
	rt.active["a"] = poolDigest("a", 3)
	rt.managed["a"] = true

	rep, err := Apply(context.Background(), rt, snapshot(2, desiredJob("a", 1)), 5, drain(), observe(t, rt))
	if err == nil {
		t.Fatal("a fenced snapshot must return ErrFenced")
	}
	if rep.Outcome != OutcomeRefusedFenced {
		t.Errorf("outcome = %q, want refused_fenced", rep.Outcome)
	}
	if len(rt.calls) != 0 {
		t.Errorf("a fenced snapshot must cause no runtime calls, got %v", rt.calls)
	}
}
