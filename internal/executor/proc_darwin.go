//go:build darwin

package executor

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group.
//
// Darwin is the one shipped target without a parent-death signal: its
// syscall.SysProcAttr has no Pdeathsig field, and XNU offers no equivalent of
// Linux's PR_SET_PDEATHSIG or FreeBSD's procctl(PROC_PDEATHSIG_CTL). So when
// the daemon is killed outright, an in-flight child is reparented and keeps
// running while the next startup marks its run failed and retries it. An
// integration whose external effects are not idempotent can therefore be
// duplicated.
//
// Only abrupt daemon death is exposed, and only on this platform. Timeout,
// cancellation and graceful shutdown all signal the whole process group. The
// limitation is stated in docs/architecture.md; closing it needs a supervisor
// or a death-watch pipe in the child, not a persisted pgid.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
