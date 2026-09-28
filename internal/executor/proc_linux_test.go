//go:build linux

package executor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The parent-death guarantee cannot be exercised in-process: the process that
// has to die is the one running the test. So the test re-executes this binary
// as a small stand-in for otterd. The stand-in starts a child through the same
// setProcessGroup the executor uses, reports the child's pid, and then blocks.
// The test SIGKILLs the stand-in -- the abrupt daemon death this guarantee is
// about -- and asserts the child is gone.
//
// This test fails before the fix: with Setpgid alone the child is reparented
// and keeps running. It is the Linux half of the guarantee; Darwin has no
// equivalent and is documented instead (see docs/architecture.md).
const (
	pdeathsigHelperEnv = "OTTER_TEST_PDEATHSIG_HELPER"
	pdeathsigPidFile   = "OTTER_TEST_PDEATHSIG_PIDFILE"
)

func TestMain(m *testing.M) {
	if os.Getenv(pdeathsigHelperEnv) == "1" {
		pdeathsigHelper()
		return
	}
	os.Exit(m.Run())
}

// pdeathsigHelper runs inside the re-executed test binary. It must not return
// on its own: an ordinary exit would fire the very parent-death signal under
// test and turn a failure into a false pass.
func pdeathsigHelper() {
	child := exec.Command("sleep", "300")
	setProcessGroup(child)
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "pdeathsig helper: start child:", err)
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv(pdeathsigPidFile), []byte(strconv.Itoa(child.Process.Pid)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "pdeathsig helper: write pid:", err)
		os.Exit(2)
	}
	// Sleep in a loop rather than select{} so the Go deadlock detector cannot
	// decide the process is stuck and exit it on its own.
	for {
		time.Sleep(time.Hour)
	}
}

func TestChildDiesWhenDaemonIsKilled(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep is not available")
	}

	pidPath := filepath.Join(t.TempDir(), "child.pid")

	helper := exec.Command(os.Args[0])
	helper.Env = append(os.Environ(),
		pdeathsigHelperEnv+"=1",
		pdeathsigPidFile+"="+pidPath,
	)
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}

	// Reap the stand-in however the test ends, so a failed assertion does not
	// leave it behind.
	defer func() {
		_ = helper.Process.Kill()
		_ = helper.Wait()
	}()

	pid := waitForChildPID(t, pidPath)
	// If the assertion below fails, do not leak a 300-second sleep.
	defer func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}()

	if !waitForOwnGroup(pid, 5*time.Second) {
		t.Fatalf("child %d never became its own process group leader; Setpgid did not take effect", pid)
	}
	if !processAlive(pid) {
		t.Fatalf("helper's child %d was not running before the kill", pid)
	}

	// kill -9 the stand-in: the abrupt death that gives recovery no chance to
	// terminate the group, and the case Pdeathsig exists for.
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_ = helper.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d survived its parent's SIGKILL (state %q); Pdeathsig did not fire",
				pid, processState(pid))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForChildPID polls the file the helper writes until it names a pid.
func waitForChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not report a child pid in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForOwnGroup waits for the child to be in a process group of its own,
// which is how Setpgid shows up in /proc. It is a wait rather than a single
// check because Start returns after fork while the child may not have called
// setpgid yet.
func waitForOwnGroup(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if processGroup(pid) == pid {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processAlive reports whether pid is still executing. A zombie has finished
// and is only waiting to be reaped, so it counts as dead: /proc is still
// readable but the process is no longer the running child the guarantee is
// about.
func processAlive(pid int) bool {
	state := processState(pid)
	return state != "" && state != "Z"
}

// processState returns the single-letter /proc/<pid>/stat state, or "" when the
// process no longer exists.
func processState(pid int) string {
	fields := procStatFields(pid)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// processGroup returns the process group id from /proc/<pid>/stat, or 0 when it
// cannot be read.
func processGroup(pid int) int {
	fields := procStatFields(pid)
	// After the parenthesised comm field the columns are state, ppid, pgrp.
	if len(fields) < 3 {
		return 0
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0
	}
	return pgid
}

// procStatFields splits /proc/<pid>/stat after the comm field. comm is
// parenthesised and may itself contain spaces and parentheses, so splitting on
// the last ')' is the only safe parse.
func procStatFields(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil
	}
	idx := strings.LastIndexByte(string(data), ')')
	if idx < 0 {
		return nil
	}
	return strings.Fields(string(data)[idx+1:])
}
