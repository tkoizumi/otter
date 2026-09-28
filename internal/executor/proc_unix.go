//go:build unix

package executor

import (
	"errors"
	"os/exec"
	"syscall"
)

// The group helpers below are shared by every Unix target. Launching the child
// in its own process group (`setProcessGroup`) is the one part that is not:
// Linux can additionally ask the kernel to signal the child when the daemon
// dies, and Darwin cannot. That split lives in proc_linux.go, proc_darwin.go
// and proc_unix_other.go.

// terminateGroup sends SIGTERM to the child's process group.
func terminateGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("process not started")
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

// killGroup sends SIGKILL to the child's process group.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("process not started")
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// processExit describes how a process ended: an exit code for a normal exit,
// or a signal name when it was killed.
func processExit(err error) (*int, string) {
	if err == nil {
		code := 0
		return &code, ""
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return nil, status.Signal().String()
			}
			code := status.ExitStatus()
			return &code, ""
		}
		code := exitErr.ExitCode()
		if code < 0 {
			return nil, ""
		}
		return &code, ""
	}

	return nil, ""
}
