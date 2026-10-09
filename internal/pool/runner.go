package pool

import (
	"context"
	"os/exec"
	"strings"
)

// Runner runs one local command and returns its combined output.
//
// Injectable because everything this package does to the machine is a command, and
// a test that cannot substitute one has to own a Docker daemon to assert anything.
// A test double records the argv, which is also how the launch contract stays
// checkable from Go without reimplementing it.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner runs real commands.
type ExecRunner struct{}

// Run executes the command and returns stdout+stderr combined.
//
// Combined because the provisioner writes its reason to stderr and its progress to
// stdout, and an operator reading a failure wants both in the order they happened.
// The output is returned even on error, because the reason for a failure is almost
// always in it.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// RecordingRunner is a test double: it answers from a scripted table and records
// every call.
//
// It lives in the package rather than in a `_test.go` file because the CLI's own
// tests use it too, and duplicating a fake in two packages is how the two start
// disagreeing about what the real command looks like.
type RecordingRunner struct {
	// Respond maps "name arg1 arg2" to a canned answer. A key that is a prefix of the
	// command matches, so a test can answer every `docker inspect` without repeating
	// the container name.
	Respond func(name string, args []string) (string, error)
	Calls   [][]string
}

// Run records the call and answers from Respond, defaulting to empty success.
func (r *RecordingRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.Calls = append(r.Calls, append([]string{name}, args...))
	if r.Respond == nil {
		return "", nil
	}
	return r.Respond(name, args)
}

// Command builds the display form of a recorded call, for an assertion message.
func Command(call []string) string { return strings.Join(call, " ") }
