//go:build !unix

package executor

import (
	"errors"
	"os/exec"
)

// Otter targets Linux and macOS. These stubs keep the package buildable on
// other platforms without pretending to provide process-group semantics.

func setProcessGroup(_ *exec.Cmd) {}

func terminateGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("process not started")
	}
	return cmd.Process.Kill()
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("process not started")
	}
	return cmd.Process.Kill()
}

func processExit(err error) (*int, string) {
	if err == nil {
		code := 0
		return &code, ""
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return &code, ""
		}
	}
	return nil, ""
}
