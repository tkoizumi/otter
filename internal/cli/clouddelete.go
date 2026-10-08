package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/tkoizumi/otter/internal/cloud"
)

// cmdDeleteCloud is `otter delete --cloud <job>`: the self-serve half of a
// pooled-runtime delete.
//
// It is deliberately shaped like `otter deploy --cloud`: the credential comes
// from `otter login` (or OTTER_CLOUD_TOKEN), the organization and its runtimes
// are read through the same call, and `--runtime` is resolved by the same rule.
// What differs is the ordering of the delete itself, and it matters:
//
//  1. the credential is checked, which is a local file read;
//  2. the shared confirmation runs, which makes NO request -- a declined prompt
//     must leave the fleet untouched;
//  3. only then is the job resolved and the delete sent.
//
// The job is resolved by name or id through the Cloud job list, because the
// DELETE route takes the durable id and an operator has a label. A reference
// Cloud cannot find is reported as a not-found and nothing is queued; it is
// never allowed to surface as a server error.
func (a *App) cmdDeleteCloud(ctx context.Context, g globals, ref, runtimeFlag string, assumeYes bool) int {
	ident, err := cloud.Resolve("", "")
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 1
	}
	if ident.Token == "" {
		fmt.Fprintln(a.Stderr, "otter: not logged in to Otter Cloud")
		fmt.Fprintf(a.Stderr, "otter: run otter login, or set %s\n", cloud.TokenEnv)
		return 1
	}

	// The gate is before the first request: nothing below runs on a no.
	if proceed, code := a.confirmDelete(ref, assumeYes); !proceed {
		return code
	}

	client := cloud.NewClient(ident.BaseURL, ident.Token)
	me, err := client.Me(ctx)
	if err != nil {
		return a.cloudFail(err)
	}
	runtimeID, code := deleteRuntimeScope(a, me, runtimeFlag)
	if code != 0 {
		return code
	}

	jobs, err := client.Jobs(ctx, runtimeID)
	if err != nil {
		return a.cloudFail(err)
	}
	matches := matchCloudJobs(jobs, ref)
	switch len(matches) {
	case 0:
		fmt.Fprintf(a.Stderr, "otter: no job %q in Otter Cloud", ref)
		if runtimeID != "" {
			fmt.Fprintf(a.Stderr, " on runtime %s", runtimeID)
		}
		fmt.Fprintln(a.Stderr)
		fmt.Fprintln(a.Stderr, "otter: nothing was deleted; check the job's id or label, or narrow the search with --runtime <id>")
		return 1
	case 1:
		// exactly one job, below
	default:
		fmt.Fprintf(a.Stderr, "otter: %q matches %d jobs in this organization; choose one with --runtime <id>\n", ref, len(matches))
		for _, job := range matches {
			fmt.Fprintf(a.Stderr, "  %s  %s (runtime %s)\n", job.ID, job.Name, job.RuntimeID)
		}
		fmt.Fprintln(a.Stderr, "otter: nothing was deleted")
		return 2
	}
	job := matches[0]

	result, err := client.DeleteJob(ctx, job.ID)
	if err != nil {
		// The route resolves the job before queueing anything, so a missing id
		// is a clean 404. Reporting it as such keeps a wrong name from looking
		// like a control-plane fault.
		if cloud.IsNotFound(err) {
			fmt.Fprintf(a.Stderr, "otter: no job %s in Otter Cloud\n", job.ID)
			fmt.Fprintln(a.Stderr, "otter: nothing was deleted")
			return 1
		}
		return a.cloudFail(err)
	}
	if g.jsonOut {
		return a.printJSON(result)
	}

	name := strings.TrimSpace(job.Name)
	if name == "" {
		name = job.ID
	}
	// THREE OUTCOMES, kept apart because they need different actions. A delete
	// that removed nothing, and a delete the runtime has not confirmed, are both
	// failures to report as success -- the release stayed in Cloud's desired
	// state, or the agent has not converged yet, and each has its own fix.
	if !result.Applied || !result.RemovedFromDesired() {
		fmt.Fprintf(a.Stderr, "otter: %s (%s) is still in Otter Cloud's desired state; nothing was removed\n", name, job.ID)
		if note := result.Note(); note != "" {
			fmt.Fprintf(a.Stderr, "otter: %s\n", note)
		}
		return 1
	}
	if !result.Deleted() {
		fmt.Fprintf(a.Stdout, "pending     %s (%s)\n", name, job.ID)
		// The control plane's note is already the honest account -- what left
		// desired state and why the runtime has not confirmed it -- so it is
		// printed once rather than beside an invented summary.
		if note := result.Note(); note != "" {
			fmt.Fprintf(a.Stdout, "note        %s\n", note)
		} else {
			fmt.Fprintln(a.Stdout, "note        removed from Cloud's desired state, but the runtime has not confirmed the removal")
		}
		return 1
	}
	fmt.Fprintf(a.Stdout, "deleted     %s (%s)\n", name, job.ID)
	// The runtime's note states what the delete actually removed and what it
	// could not. Swallowing it would hide exactly the scope a caller must not
	// have to infer.
	if note := result.Note(); note != "" {
		fmt.Fprintf(a.Stdout, "note        %s\n", note)
	}
	return 0
}

// deleteRuntimeScope decides which runtime's job list a cloud delete searches.
//
// With --runtime it applies the deploy rule exactly: the id must be assigned to
// the organization, and an unknown one is refused with the candidate list.
// Without it, one runtime is used as deploy does and none is a refusal, but
// several are searched together rather than refused: the delete route resolves
// the runtime from the job id itself, so --runtime is only needed to
// disambiguate a name. An empty id with a zero code means "search them all".
func deleteRuntimeScope(a *App, me *cloud.Me, selected string) (string, int) {
	if strings.TrimSpace(selected) != "" {
		runtime, code := resolveCloudRuntime(a, me, selected)
		if code != 0 {
			return "", code
		}
		return runtime.ID, 0
	}
	switch len(me.Runtimes) {
	case 0:
		fmt.Fprintln(a.Stderr, "otter: no runtime is assigned to this organization yet")
		fmt.Fprintln(a.Stderr, "otter: attach a runtime in Otter Cloud, then re-run this delete")
		return "", 1
	case 1:
		return me.Runtimes[0].ID, 0
	default:
		return "", 0
	}
}

// matchCloudJobs resolves a name-or-id reference against the jobs Cloud lists.
//
// The id is matched exactly and wins outright: it is opaque and unambiguous,
// and a job whose label happens to equal another job's id must not shadow it.
// The label is matched case-insensitively, because a label is typed by hand.
// More than one label match is ambiguity for the caller to report, never a
// guess about which job the operator meant.
func matchCloudJobs(jobs []cloud.Job, ref string) []cloud.Job {
	ref = strings.TrimSpace(ref)
	var byName []cloud.Job
	for _, job := range jobs {
		if ref != "" && job.ID == ref {
			return []cloud.Job{job}
		}
		if job.Name != "" && strings.EqualFold(strings.TrimSpace(job.Name), ref) {
			byName = append(byName, job)
		}
	}
	return byName
}
