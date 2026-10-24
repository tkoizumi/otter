package release

import (
	"context"
	"database/sql"
)

// Queryer abstracts the sql.QueryContext method so PinnedDigests can run
// against any transaction or connection without depending on a concrete type.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// PinnedDigests returns the distinct release digests that non-terminal runs
// (queued, running, retrying) are still bound to for the given job.
//
// The SQL is:
//
//	SELECT DISTINCT release_digest FROM runs
//	  WHERE job_id = ? AND release_digest <> ''
//	    AND status IN ('queued', 'running', 'retrying')
func PinnedDigests(ctx context.Context, q Queryer, jobID string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT DISTINCT release_digest FROM runs
		  WHERE job_id = ? AND release_digest <> ''
		    AND status IN ('queued', 'running', 'retrying')`,
		jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			return nil, err
		}
		if digest != "" {
			out[digest] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
