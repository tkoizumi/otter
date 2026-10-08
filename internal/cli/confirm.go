package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// The destructive-delete gate lives here, in one place, because `otter delete`
// has two paths -- the workspace purge through the local daemon and the new
// `--cloud` delete through the control plane -- and a confirmation that drifted
// between them would be a safety bug, not a cosmetic one.
//
// Both callers invoke confirmDelete BEFORE their first request. That ordering
// is the whole point: a declined or unanswerable prompt must not read the
// organization, resolve a job, queue a command or touch the store. The function
// therefore takes the reference the operator typed, not a resolved view, so it
// never has to call the API to know what to name.

// confirmDelete asks whether an irreversible delete of job may proceed.
//
// assumeYes is --yes/-y: the one spelling that skips the prompt, and the only
// way a non-interactive caller may delete. It returns (true, 0) when the delete
// may proceed. When it may not, the reason has already been written to stderr
// and the caller returns the returned code, which is 1: a declined prompt is a
// failed operation, not a usage error (2).
func (a *App) confirmDelete(job string, assumeYes bool) (bool, int) {
	if assumeYes {
		return true, 0
	}

	fmt.Fprintf(a.Stderr, "otter: deleting %s destroys, irreversibly:\n", job)
	fmt.Fprintln(a.Stderr, "  - every run of this job, and the logs those runs produced")
	fmt.Fprintln(a.Stderr, "  - every captured request and response payload")
	fmt.Fprintln(a.Stderr, "  - the job's stored state")
	fmt.Fprintln(a.Stderr, "  - every schedule it owns")
	fmt.Fprintln(a.Stderr, "  - its configuration")
	fmt.Fprintln(a.Stderr, "  - every release, including the active one")

	in := a.stdin()
	if !interactiveInput(in) {
		fmt.Fprintf(a.Stderr, "otter: refusing to delete %s: stdin is not a terminal, so the prompt cannot be answered\n", job)
		fmt.Fprintln(a.Stderr, "otter: re-run with --yes to delete without a prompt")
		fmt.Fprintln(a.Stderr, "otter: aborted; nothing was deleted")
		return false, 1
	}

	fmt.Fprintf(a.Stderr, "Delete %s? [y/N] ", job)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		// A terminal that closed between the prompt and the answer reads as no
		// answer, which aborts. It must never be read as consent.
		fmt.Fprintln(a.Stderr, "otter: aborted; nothing was deleted")
		return false, 1
	}
	answer := strings.TrimSpace(line)
	if strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes") {
		return true, 0
	}

	fmt.Fprintln(a.Stderr, "otter: aborted; nothing was deleted")
	return false, 1
}

// stdin is the reader a prompt answers from. Nil means the process's stdin,
// matching how `otter login` prompts for a token.
func (a *App) stdin() io.Reader {
	if a.Stdin != nil {
		return a.Stdin
	}
	return os.Stdin
}

// interactiveInput reports whether r is a terminal a prompt can be answered on.
//
// It mirrors interactiveOutput and is a variable so a test can stand in for a
// terminal without a pty. A pipe, a file or a nil/closed reader is not
// interactive: the caller refuses instead of blocking on input nobody will
// type.
var interactiveInput = func(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return isTerminal(f)
}
