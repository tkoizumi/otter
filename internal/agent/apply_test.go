package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeRuntime records the order of operations, because the order is the security
// property: a swap before the drain would take the tenant's in-flight work.
type fakeRuntime struct {
	calls []string

	fetchErr    error
	enterErr    error
	promoteErr  error
	validateErr error
	// gated tracks whether the runtime is currently in maintenance, so a test can
	// assert it is never left gated by accident.
	gated bool
}

func (f *fakeRuntime) Observe(context.Context) (Observed, error) {
	f.calls = append(f.calls, "observe")
	return Observed{ReleaseDigest: "sha256:new", Maintenance: "serving"}, nil
}

func (f *fakeRuntime) EnterMaintenance(_ context.Context, reason string, _ time.Duration) error {
	f.calls = append(f.calls, "enter:"+reason)
	if f.enterErr != nil {
		return f.enterErr
	}
	f.gated = true
	return nil
}

func (f *fakeRuntime) ExitMaintenance(context.Context) error {
	f.calls = append(f.calls, "exit")
	f.gated = false
	return nil
}

func (f *fakeRuntime) Promote(_ context.Context, digest string) error {
	f.calls = append(f.calls, "promote:"+digest)
	return f.promoteErr
}

func (f *fakeRuntime) Fetch(_ context.Context, r Release) error {
	f.calls = append(f.calls, "fetch:"+r.Digest)
	return f.fetchErr
}

func (f *fakeRuntime) Validate(_ context.Context, digest string) error {
	f.calls = append(f.calls, "validate:"+digest)
	return f.validateErr
}

func want(gen int64) *Desired {
	return &Desired{Generation: gen, Release: Release{Digest: "sha256:new", URL: "https://example.invalid/r"}}
}

func TestApplyFollowsTheProtocolOrder(t *testing.T) {
	rt := &fakeRuntime{}
	rep, err := Apply(context.Background(), rt, want(42), 41, Drain{Timeout: time.Minute}, Observed{})
	if err != nil {
		t.Fatalf("a clean apply must not error: %v", err)
	}
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("outcome = %q, want %q", rep.Outcome, OutcomeApplied)
	}
	// Verify, then prepare, then drain, THEN swap, validate, ACTIVATE, and only
	// then observe and report -- a state observed before activation describes a
	// gated runtime and would be recorded wrongly.
	got := strings.Join(rt.calls, " ")
	want := "fetch:sha256:new enter:deploy generation 42 promote:sha256:new validate:sha256:new exit observe"
	if got != want {
		t.Errorf("call order:\n got %q\nwant %q", got, want)
	}
	if rt.gated {
		t.Error("the runtime was left gated after a successful apply")
	}
}

// A digest mismatch must stop before anything is promoted. This is the property
// that makes content-addressing worth having: a compromised control plane cannot
// substitute release content.
func TestApplyStopsWhenTheReleaseCannotBeFetched(t *testing.T) {
	rt := &fakeRuntime{fetchErr: errors.New("digest mismatch")}
	rep, err := Apply(context.Background(), rt, want(7), 6, Drain{Timeout: time.Minute}, Observed{})
	if err != nil {
		t.Fatalf("a refused fetch is an outcome, not an error: %v", err)
	}
	if rep.Outcome != OutcomeFailed {
		t.Errorf("outcome = %q, want %q", rep.Outcome, OutcomeFailed)
	}
	for _, c := range rt.calls {
		if strings.HasPrefix(c, "promote") || strings.HasPrefix(c, "enter") {
			t.Errorf("a failed fetch must not reach %q", c)
		}
	}
	if rt.gated {
		t.Error("the runtime must never be gated when the fetch failed")
	}
}

// The protocol is explicit: an expired drain ABORTS. It must not kill work, and
// it must not leave the runtime gated on the agent's own authority.
func TestExpiredDrainAbortsAndReturnsToServing(t *testing.T) {
	rt := &fakeRuntime{enterErr: ErrDrainTimeout}
	rep, err := Apply(context.Background(), rt, want(9), 8, Drain{Timeout: time.Second}, Observed{})
	if !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("expected ErrDrainTimeout, got %v", err)
	}
	if rep.Outcome != OutcomeFailed {
		t.Errorf("outcome = %q, want %q", rep.Outcome, OutcomeFailed)
	}
	if !strings.Contains(rep.Reason, "abandoned") {
		t.Errorf("reason should say the upgrade was abandoned: %q", rep.Reason)
	}
	for _, c := range rt.calls {
		if strings.HasPrefix(c, "promote") {
			t.Errorf("an aborted drain must not promote: %q", c)
		}
	}
	if rt.gated {
		t.Error("an aborted drain must return the runtime to serving, not leave it gated")
	}
}

// A failure after the gate is entered must restore serving, or the tenant is
// offline for a deploy that did not happen.
func TestAFailureAfterGatingRestoresServing(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   *fakeRuntime
	}{
		{"promote fails", &fakeRuntime{promoteErr: errors.New("no such release")}},
		{"validate fails", &fakeRuntime{validateErr: errors.New("manifest unreadable")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep, err := Apply(context.Background(), tc.rt, want(3), 2, Drain{Timeout: time.Minute}, Observed{})
			if err != nil {
				t.Fatalf("a failed step is an outcome, not an error: %v", err)
			}
			if rep.Outcome != OutcomeFailed {
				t.Errorf("outcome = %q, want %q", rep.Outcome, OutcomeFailed)
			}
			sawExit := false
			for _, c := range tc.rt.calls {
				if c == "exit" {
					sawExit = true
				}
			}
			if !sawExit {
				t.Error("the runtime must be returned to serving after a failure past the gate")
			}
			if tc.rt.gated {
				t.Error("the runtime was left gated")
			}
		})
	}
}

// A stale agent must apply nothing. Allowing it would let an agent that missed a
// newer generation roll the runtime backwards.
func TestAStaleGenerationIsFenced(t *testing.T) {
	rt := &fakeRuntime{}
	rep, err := Apply(context.Background(), rt, want(10), 12, Drain{Timeout: time.Minute}, Observed{})
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("expected ErrFenced, got %v", err)
	}
	if rep.Outcome != OutcomeRefusedFenced {
		t.Errorf("outcome = %q, want %q", rep.Outcome, OutcomeRefusedFenced)
	}
	if len(rt.calls) != 0 {
		t.Errorf("a fenced generation must cause no runtime calls, got %v", rt.calls)
	}
}

// A desired state with no release is a control-plane bug, and must not be
// interpreted as "leave things alone" or "remove the release".
func TestApplyRefusesADesiredStateWithoutARelease(t *testing.T) {
	rt := &fakeRuntime{}
	rep, err := Apply(context.Background(), rt, &Desired{Generation: 5}, 4, Drain{Timeout: time.Minute}, Observed{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.Outcome != OutcomeFailed || !strings.Contains(rep.Reason, "no release") {
		t.Errorf("want a failure naming the missing release, got %+v", rep)
	}
	if len(rt.calls) != 0 {
		t.Errorf("nothing should touch the runtime, got %v", rt.calls)
	}
}

// The reason recorded against the gate is what an operator sees during a drain,
// so it has to name the deploy rather than being empty.
func TestTheGateReasonNamesTheGeneration(t *testing.T) {
	rt := &fakeRuntime{}
	if _, err := Apply(context.Background(), rt, want(77), 76, Drain{Timeout: time.Minute}, Observed{}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range rt.calls {
		if strings.HasPrefix(c, "enter:") && strings.Contains(c, fmt.Sprint(77)) {
			found = true
		}
	}
	if !found {
		t.Errorf("the maintenance reason should name generation 77: %v", rt.calls)
	}
	// An explicit control-plane reason wins over the generated one.
	rt2 := &fakeRuntime{}
	w := want(78)
	w.Maintenance = &MaintenanceWant{Desired: "maintenance", Reason: "operator window"}
	if _, err := Apply(context.Background(), rt2, w, 77, Drain{Timeout: time.Minute}, Observed{}); err != nil {
		t.Fatal(err)
	}
	if rt2.calls[1] != "enter:operator window" {
		t.Errorf("operator reason not used for the gate: %v", rt2.calls)
	}
}

// A control plane publishes DESIRED STATE, not events: it keeps returning the
// same generation until a deploy replaces it. After a successful apply, every
// subsequent cycle therefore asks for the release the runtime is already serving,
// and re-applying it re-downloaded the release, gated the tenant, re-activated
// identical content and leaked a staging directory -- once every few seconds.
func TestApplyDoesNothingWhenTheRuntimeAlreadyServesTheRelease(t *testing.T) {
	rt := &fakeRuntime{}
	hex := strings.Repeat("a", 64)
	d := &Desired{Generation: 4, Release: Release{Digest: "sha256:" + hex, URL: "https://example.invalid/r"}}

	// The runtime's store names releases by bare hex while Cloud stores
	// sha256:<hex>; both spellings are the same release and both must be caught.
	rep, err := Apply(context.Background(), rt, d, 4, Drain{Timeout: time.Minute}, Observed{ReleaseDigest: hex, Maintenance: "serving"})
	if err != nil {
		t.Fatalf("apply returned an unexpected error: %v", err)
	}
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("outcome = %s, want applied (%s)", rep.Outcome, rep.Reason)
	}
	if len(rt.calls) != 0 {
		t.Fatalf("an already-serving release must not be touched; calls = %v", rt.calls)
	}
	if rep.Observed["release_digest"] != hex {
		t.Fatalf("reported digest = %q, want the observed one", rep.Observed["release_digest"])
	}
}

// The check is CONTENT, not generation: a runtime serving something else must
// still be brought to the desired release, even at the same generation. That is
// what keeps a runtime changed behind Cloud's back from staying that way.
func TestApplyStillAppliesWhenTheRuntimeServesSomethingElse(t *testing.T) {
	rt := &fakeRuntime{}
	hex := strings.Repeat("a", 64)
	d := &Desired{Generation: 4, Release: Release{Digest: "sha256:" + hex, URL: "https://example.invalid/r"}}

	rep, err := Apply(context.Background(), rt, d, 4, Drain{Timeout: time.Minute}, Observed{ReleaseDigest: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatalf("apply returned an unexpected error: %v", err)
	}
	if rep.Outcome != OutcomeApplied {
		t.Fatalf("outcome = %s, want applied (%s)", rep.Outcome, rep.Reason)
	}
	if len(rt.calls) == 0 {
		t.Fatal("different content must be applied")
	}
}
