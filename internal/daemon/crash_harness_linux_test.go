//go:build linux

package daemon

// The Linux half of the P0-01 crash harness. These two assertions depend on
// the parent-death signal (PR_SET_PDEATHSIG, internal/executor/proc_linux.go),
// which is what closes the window in which an orphaned child finishes the same
// work its retry is already doing. Darwin has no equivalent
// (internal/executor/proc_darwin.go), so the same assertions are recorded as
// excluded there by crash_harness_nonlinux_test.go rather than shipped red.

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// crashAssertPlatform proves the two things only Linux can prove:
//
//  1. every child that was executing when the daemon was SIGKILLed is dead
//     shortly afterwards -- the kernel, not the runtime, killed it; and
//  2. no interrupted run's orphan went on to complete the work: each accepted
//     run's chain produced exactly one completion side effect.
//
// The second is the crash-harness form of "none executed twice". A failed
// interrupted row that still wrote a completion line means a child outlived the
// daemon and did the work again, behind its retry's back.
func crashAssertPlatform(t *testing.T, ev *crashPlatformEvidence) {
	t.Helper()

	checked := 0
	deadline := time.Now().Add(15 * time.Second)
	for _, id := range ev.interrupted {
		pid, ok := ev.pids[id]
		if !ok {
			// The run reached `running` but its child had not reported yet.
			// It is still covered by the retry assertions; it just cannot be
			// part of the process-death evidence.
			continue
		}
		checked++
		for crashProcessAlive(pid) {
			if time.Now().After(deadline) {
				t.Errorf("child %d (interrupted run %s) survived the daemon's SIGKILL; Pdeathsig did not fire",
					pid, id)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if checked <= 50 {
		t.Errorf("only %d interrupted children had a recorded pid, want more than one listing page (>50)", checked)
	}
	t.Logf("all %d recorded interrupted children died with the daemon (Pdeathsig)", checked)

	for _, root := range ev.accepted {
		count := 0
		for _, attempt := range ev.chains[root] {
			count += ev.effects[attempt.ID]
		}
		if count != 1 {
			t.Errorf("run chain %s produced %d completion side effects, want exactly 1: an attempt was executed twice (an orphaned child outlived the daemon)",
				root, count)
		}
	}
	t.Logf("every one of %d accepted run chains completed exactly once", len(ev.accepted))
}

// crashProcessAlive reports whether pid is still executing. A zombie has
// finished and is only waiting to be reaped, so it counts as dead: the process
// is no longer the running child the guarantee is about. This mirrors the
// check in internal/executor/proc_linux_test.go.
func crashProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	idx := strings.LastIndexByte(string(data), ')')
	if idx < 0 {
		return false
	}
	fields := strings.Fields(string(data)[idx+1:])
	if len(fields) == 0 {
		return false
	}
	return fields[0] != "Z" && fields[0] != "X"
}
