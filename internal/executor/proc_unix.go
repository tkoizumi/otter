//go:build unix

package executor

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so that signals
// reach any grandchildren it may spawn.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

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
