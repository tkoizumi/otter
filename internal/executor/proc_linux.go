//go:build linux

package executor

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group and asks the kernel
// to kill it if the daemon dies first.
//
// The daemon is the child's parent, so `kill -9`, an OOM kill or a panic would
// otherwise leave the job running behind a restart that has already
// marked its run failed and retried it: the same work running twice, with its
// external effects duplicated. Pdeathsig closes that window on the platform
// Otter is deployed on. SIGKILL is the signal because the daemon is gone --
// there is nothing left to escalate a graceful SIGTERM after a grace period.
//
// Two limits are deliberate:
//
//   - It covers the direct child only. A grandchild the job spawns is
//     reparented, not signalled. The graceful paths -- timeout, cancellation
//     and shutdown -- signal the whole group, so this only concerns abrupt
//     daemon death.
//   - It is Linux-specific. Darwin has no kernel equivalent; see
//     proc_darwin.go and the platform note in docs/architecture.md.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
}
