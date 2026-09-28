//go:build unix && !linux && !darwin

package executor

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group.
//
// Otter ships Linux and macOS binaries; this file only keeps the remaining
// Unix targets compiling. Like Darwin, they get no parent-death guarantee, so
// nothing here should be read as one.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
