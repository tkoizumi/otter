//go:build !linux

package daemon

import (
	"runtime"
	"testing"
)

// The non-Linux half of the P0-01 crash harness.
//
// The loss and re-enqueue assertions still run here; only the two assertions
// that depend on the kernel's parent-death signal are excluded. On Darwin an
// in-flight Python child is reparented when the daemon is SIGKILLed and keeps
// running, while the next startup marks its run failed and retries it. That is
// a documented limitation, not a bug the harness should report as red on a
// developer's machine: see internal/executor/proc_darwin.go and
// docs/architecture.md#child-lifetime-is-platform-specific.
//
// Shipping this exclusion rather than a red test is deliberate. On this
// platform the harness cannot distinguish "the runtime duplicated the work"
// from "this operating system has no parent-death signal", and a test that is
// always red is a test nobody reads.
func crashAssertPlatform(t *testing.T, ev *crashPlatformEvidence) {
	t.Helper()
	t.Logf("platform assertions excluded on this OS (%s): no parent-death signal, so an in-flight child is reparented and survives the daemon's SIGKILL. "+
		"Ran everywhere: %d accepted runs terminal, %d interrupted runs re-enqueued, %d distinct completions. "+
		"Excluded here: child-death and one-completion-per-chain (Linux-only, crash_harness_linux_test.go).",
		runtime.GOOS, len(ev.accepted), len(ev.interrupted), len(ev.effects))
}
