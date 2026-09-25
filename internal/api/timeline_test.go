package api

import (
	"context"

	"github.com/tkoizumi/otter/internal/timeline"
)

// TimelinePage implements Backend for the API tests.
//
// The merge, the cursor and the revision live in internal/timeline and are
// exercised directly there and end to end through the daemon. What the API layer
// owns is the query contract, the authorization and the error mapping, so this
// fake exposes a seam for those: a test sets timelinePage or timelineErr and
// asserts the HTTP result, and the default is an empty page for requests that
// only need the route to exist.
func (b *fakeBackend) TimelinePage(ctx context.Context, req timeline.Request) (*timeline.Page, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.timelineRequests = append(b.timelineRequests, req)
	if b.timelineErr != nil {
		return nil, b.timelineErr
	}
	if b.timelinePage != nil {
		return b.timelinePage, nil
	}
	return &timeline.Page{
		Context: timeline.Context{
			SchemaVersion: timeline.SchemaVersion,
			RunID:         req.RunID,
			IncludeHTTP:   req.IncludeHTTP,
		},
		Events: []timeline.Event{},
	}, nil
}
