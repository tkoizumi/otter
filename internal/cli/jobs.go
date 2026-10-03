package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/identity"
)

// This file is the jobs listing, which answers two halves of one question.
//
// The identity registry is the durable record of what exists: one row per job
// that has ever been registered, with the id that state, history, tokens and
// releases belong to. It is read directly, so the listing works with the
// runtime stopped -- which is exactly when a deployment, a migration or a purge
// needs the destination ids. `--all` adds the identities that are no longer
// active: retired, deleting and deleted.
//
// A reachable daemon adds what only a running runtime has observed: whether the
// current manifest still validates, its triggers, and when it next fires. The
// registry deliberately does not record validity -- an identity can be active
// and invalid at once -- so a job with a broken manifest is only recognizable
// while a daemon is up. `--schedule` is the live view in full.

func (a *App) cmdJobs(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("jobs", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	all := fs.Bool("all", false, "include invalid jobs and retired or deleted identities")
	schedule := fs.Bool("schedule", false, "show each job's cron, next run and last outcome")
	data := fs.String("data", "", "Otter data directory (default: the workspace's)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// A daemon the operator named (--api or OTTER_API_URL) may own a different
	// data directory -- a tunnel to a deployed host is the common case -- so
	// this host's registry must not be mixed into its listing. A runtime
	// discovered in the workspace is a different matter: it serves this
	// registry, so both belong. An explicit --data still wins either way: that
	// is the operator naming a registry on purpose.
	namedDaemon := g.api != "" && (g.apiExplicit || !isLoopbackBase(g.api))
	hasData := false
	var registry []api.JobView
	if !namedDaemon || flagWasSet(fs, "data") {
		if dataDir, ok := jobsDataDir(*data, flagWasSet(fs, "data")); ok {
			hasData = true
			rows, err := readRegistryJobs(ctx, dataDir)
			switch {
			case err == nil:
				registry = rows
			case g.api != "" || *schedule:
				// The registry is a bonus here; the daemon can still answer.
				fmt.Fprintf(a.Stderr, "otter: %v\n", err)
				fmt.Fprintln(a.Stderr, "otter: continuing without the identity registry")
			default:
				fmt.Fprintf(a.Stderr, "otter: %v\n", err)
				return 1
			}
		}
	}

	// A live daemon enriches the listing; it is never required for it, except
	// by --schedule, which is a runtime question.
	var live []api.JobView
	if g.api != "" || *schedule {
		list, err := g.client().ListJobs(ctx)
		switch {
		case err == nil:
			live = list
		case *schedule || len(registry) == 0:
			// Nothing local to fall back on, so the daemon's failure is the
			// answer the operator needs.
			return a.fail(err)
		default:
			fmt.Fprintf(a.Stderr, "otter: %v\n", err)
			fmt.Fprintln(a.Stderr, "otter: showing the identity registry; live status is unavailable")
		}
	}

	// No source at all is the one case that is a usage error rather than an
	// empty listing: outside a workspace, with no daemon to ask.
	if len(registry) == 0 && len(live) == 0 && !hasData && g.api == "" && !*schedule {
		fmt.Fprintln(a.Stderr, "otter: no workspace here (no .otter in this directory or above)")
		fmt.Fprintln(a.Stderr, "otter: cd into a workspace, start one with otter start, or pass --api <url>")
		return 2
	}

	if *schedule {
		return a.printSchedule(ctx, g, live, *all)
	}
	rows := visibleJobs(mergeJobs(registry, live), *all)

	if g.jsonOut {
		// The same versioned envelope the HTTP list endpoint returns: a list
		// response is an object, never a bare array, so it can carry the schema
		// version.
		return a.printJSON(api.JobList{SchemaVersion: api.SchemaVersion, Jobs: rows})
	}

	a.printJobsTable(rows)
	if len(rows) == 0 {
		fmt.Fprintln(a.Stderr, "otter: no jobs found (use --all to include invalid and retired jobs)")
	}
	return 0
}

// jobsDataDir resolves the directory whose registry this listing should read.
//
// It stays silent when there is neither a workspace nor an explicit --data,
// because the caller may still have a daemon to ask and a listing must not fail
// for the lack of a local registry. An explicit --data needs no workspace: it
// is how the command runs on a host that has no checkout.
func jobsDataDir(data string, explicit bool) (string, bool) {
	if explicit && data != "" {
		return data, true
	}
	if _, inProject, err := workspaceRoot(); err != nil || !inProject {
		return "", false
	}
	dir, code := resolveWorkspaceData(io.Discard, "", false)
	if code != 0 {
		return "", false
	}
	return dir, true
}

// readRegistryJobs reads the identity registry in dataDir as job views.
//
// A workspace that has never registered anything has no database yet, and a
// listing is not a reason to create one: a missing file is an empty listing,
// not an error.
func readRegistryJobs(ctx context.Context, dataDir string) ([]api.JobView, error) {
	if _, err := os.Stat(filepath.Join(dataDir, database.FileName)); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read the identity registry: %w", err)
	}

	db, err := database.Open(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	if _, err := database.Migrate(ctx, db); err != nil {
		return nil, err
	}

	instances, err := identity.NewStore(db.DB).Instances(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]api.JobView, 0, len(instances))
	for _, inst := range instances {
		name := inst.Name
		if name == "" {
			name = inst.ID.String()
		}
		rows = append(rows, api.JobView{
			ID:         inst.ID.String(),
			Name:       name,
			Path:       inst.CanonicalPath,
			Status:     string(inst.Status),
			Generation: inst.Generation,
			// Validity is not a registry fact -- status is the ownership
			// lifecycle, deliberately independent of the manifest -- so it is
			// treated as fine until a daemon says otherwise. The alternative
			// would silently empty an offline listing over a field it cannot
			// know.
			Valid: true,
		})
	}
	return rows, nil
}

// mergeJobs combines the registry's rows with the daemon's live views by id.
//
// The live view wins for an id it knows, because the daemon observed the
// current manifest. A live job with no registry row is kept: a remote daemon
// owns identities this host's registry never saw.
func mergeJobs(registry, live []api.JobView) []api.JobView {
	index := make(map[string]int, len(registry)+len(live))
	out := make([]api.JobView, 0, len(registry)+len(live))
	for _, v := range registry {
		index[v.ID] = len(out)
		out = append(out, v)
	}
	for _, v := range live {
		if i, ok := index[v.ID]; ok {
			out[i] = v
			continue
		}
		index[v.ID] = len(out)
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ni, nj := jobViewName(out[i]), jobViewName(out[j])
		if ni != nj {
			return ni < nj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// visibleJobs applies the default filter: a job is listed only when it is both
// registered and usable. An invalid manifest and a non-active identity are
// hidden until --all, which is the flag an operator reaches for when a job they
// expected is missing.
func visibleJobs(rows []api.JobView, all bool) []api.JobView {
	if all {
		return rows
	}
	out := make([]api.JobView, 0, len(rows))
	for _, v := range rows {
		if !v.Valid {
			continue
		}
		if s := identity.Status(v.Status); s != "" && s != identity.StatusActive {
			continue
		}
		out = append(out, v)
	}
	return out
}

// jobViewName is the label to print for a job: the manifest name when there is
// one, the durable id otherwise.
func jobViewName(v api.JobView) string {
	if v.Name != "" {
		return v.Name
	}
	return v.ID
}

// jobViewStatus renders the ownership lifecycle, or "-" when a source did not
// record one.
func jobViewStatus(v api.JobView) string {
	if v.Status == "" {
		return "-"
	}
	return v.Status
}

// printJobsTable renders the listing. Column widths follow the data, so a long
// label never collides with the id or the path beside it, and the error column
// only appears when a row actually has one.
func (a *App) printJobsTable(rows []api.JobView) {
	nameWidth, idWidth, statusWidth, pathWidth := len("NAME"), len("ID"), len("STATUS"), len("PATH")
	withError := false
	for _, v := range rows {
		if n := len(jobViewName(v)); n > nameWidth {
			nameWidth = n
		}
		if n := len(v.ID); n > idWidth {
			idWidth = n
		}
		if n := len(jobViewStatus(v)); n > statusWidth {
			statusWidth = n
		}
		if n := len(jobViewPath(v)); n > pathWidth {
			pathWidth = n
		}
		if !v.Valid {
			withError = true
		}
	}

	if withError {
		fmt.Fprintf(a.Stdout, "%-*s  %-*s  %-*s  %-*s  %s\n",
			nameWidth, "NAME", idWidth, "ID", statusWidth, "STATUS", pathWidth, "PATH", "ERROR")
	} else {
		fmt.Fprintf(a.Stdout, "%-*s  %-*s  %-*s  %s\n",
			nameWidth, "NAME", idWidth, "ID", statusWidth, "STATUS", "PATH")
	}

	for _, v := range rows {
		if withError {
			errText := "-"
			if v.Error != "" {
				errText = oneLine(v.Error)
			}
			fmt.Fprintf(a.Stdout, "%-*s  %-*s  %-*s  %-*s  %s\n",
				nameWidth, jobViewName(v), idWidth, v.ID, statusWidth, jobViewStatus(v),
				pathWidth, jobViewPath(v), errText)
			continue
		}
		fmt.Fprintf(a.Stdout, "%-*s  %-*s  %-*s  %s\n",
			nameWidth, jobViewName(v), idWidth, v.ID, statusWidth, jobViewStatus(v), jobViewPath(v))
	}
}

// jobViewPath renders a source directory, or "-" for an identity whose path is
// no longer owned (a deleted one).
func jobViewPath(v api.JobView) string {
	if v.Path == "" {
		return "-"
	}
	return v.Path
}

// printSchedule answers the question "is this actually running on a schedule?":
// what each job's cron is, when it next fires, and whether it is still
// succeeding.
//
// The last column is the daemon-computed last success that came with the
// listing, so a stale schedule reads as a stale age rather than as an empty
// column -- and rendering it costs no run listing per job. A job that has never
// succeeded is the common case after a first start and says so.
func (a *App) printSchedule(_ context.Context, _ globals, list []api.JobView, includeInvalid bool) int {
	now := time.Now().UTC()

	fmt.Fprintf(a.Stdout, "%-20s %-16s %-21s %-10s %s\n",
		"JOB", "CRON", "NEXT RUN", "IN", "LAST SUCCESS")
	fmt.Fprintln(a.Stdout, strings.Repeat("-", 100))

	shown := 0
	for _, it := range list {
		if !it.Valid {
			if !includeInvalid {
				continue
			}
			fmt.Fprintf(a.Stdout, "%-20s %-16s %-21s %-10s %s\n",
				jobViewName(it), "-", "-", "-", "(invalid: "+oneLine(it.Error)+")")
			shown++
			continue
		}

		cron := it.Triggers.Cron
		if cron == "" {
			continue // not scheduled; this view is about schedules
		}
		shown++

		next, in := "-", "-"
		switch {
		case it.Triggers.Paused:
			// A paused job has no next run. Naming the pause here is
			// what keeps a blank column from reading as "not yet due" or as an
			// job that silently stopped firing.
			next = "paused"
		case it.NextRunAt != nil:
			next = it.NextRunAt.Local().Format("2006-01-02 15:04:05")
			in = it.NextRunAt.Sub(now).Round(time.Second).String()
		}

		last := "never succeeded"
		if it.LastSuccessAt != nil {
			last = fmt.Sprintf("%s (%s ago)",
				it.LastSuccessAt.Local().Format("2006-01-02 15:04:05"),
				now.Sub(*it.LastSuccessAt).Round(time.Second))
		}
		fmt.Fprintf(a.Stdout, "%-20s %-16s %-21s %-10s %s\n",
			jobViewName(it), cron, next, in, last)
	}

	if shown == 0 {
		fmt.Fprintln(a.Stderr, "otter: no jobs with a cron trigger")
		return 0
	}
	return 0
}
