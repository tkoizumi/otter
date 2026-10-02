package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tkoizumi/otter/internal/api"
)

// The renderings below turn the health response's operational signals into the
// one-line answers `otter status` prints. They are separate functions so the
// wording is testable without an HTTP server.

// freshnessLine names each job's last success and its age, stalest first, so
// the answer to "did something quietly stop succeeding?" is the first thing
// read. A job that has never succeeded leads, because that is the loudest
// version of the same signal.
func freshnessLine(entries []api.HealthFreshness, now time.Time) string {
	if len(entries) == 0 {
		return "no jobs"
	}

	sorted := append([]api.HealthFreshness(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].LastSuccessAt, sorted[j].LastSuccessAt
		switch {
		case a == nil:
			return b != nil
		case b == nil:
			return false
		default:
			return a.Before(*b)
		}
	})

	const limit = 4
	parts := make([]string, 0, limit+1)
	for i, entry := range sorted {
		if i == limit {
			parts = append(parts, fmt.Sprintf("+%d more", len(sorted)-limit))
			break
		}
		label := entry.Name
		if label == "" {
			label = entry.JobID
		}
		if entry.LastSuccessAt == nil {
			parts = append(parts, label+" never")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s ago", label, now.Sub(*entry.LastSuccessAt).Round(time.Second)))
	}
	return strings.Join(parts, ", ")
}

// backlogLine reports queue depth per job, deepest first, capped so a status
// line stays a status line.
func backlogLine(byJob map[string]int) string {
	type depth struct {
		jobID string
		n     int
	}
	depths := make([]depth, 0, len(byJob))
	for jobID, n := range byJob {
		depths = append(depths, depth{jobID: jobID, n: n})
	}
	sort.Slice(depths, func(i, j int) bool {
		if depths[i].n != depths[j].n {
			return depths[i].n > depths[j].n
		}
		return depths[i].jobID < depths[j].jobID
	})

	const limit = 4
	parts := make([]string, 0, limit+1)
	for i, d := range depths {
		if i == limit {
			parts = append(parts, fmt.Sprintf("+%d more", len(depths)-limit))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d", d.jobID, d.n))
	}
	return strings.Join(parts, ", ")
}

// formatBytes renders a byte count for a human reader. Binary units, because
// that is what `df`, `du` and the database's own page arithmetic use.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f EiB", value/unit)
}
