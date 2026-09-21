package daemon

import (
	"sort"
	"sync"

	"github.com/tkoizumi/otter/internal/config"
)

// registered is an integration known to the daemon together with the runtime
// state Otter attaches to it.
type registered struct {
	Integration *config.Integration
	Manifest    *config.Manifest

	// WebhookToken is generated on first start and persisted, so webhook URLs
	// keep working across restarts.
	WebhookToken string
}

// registry holds the discovered integrations. It is rebuilt from disk on every
// daemon start and on every reload; the manifests themselves are the source of
// truth.
type registry struct {
	mu    sync.RWMutex
	byID  map[string]*registered
	order []string
}

func newRegistry() *registry {
	return &registry{byID: map[string]*registered{}}
}

// load replaces the registry contents.
func (r *registry) load(items []*config.Integration, tokens map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.byID = make(map[string]*registered, len(items))
	r.order = make([]string, 0, len(items))

	for _, it := range items {
		entry := &registered{Integration: it, Manifest: it.Manifest}
		if token, ok := tokens[it.ID]; ok {
			entry.WebhookToken = token
		}
		r.byID[it.ID] = entry
		r.order = append(r.order, it.ID)
	}
	sort.Strings(r.order)
}

func (r *registry) get(id string) (*registered, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.byID[id]
	return entry, ok
}

// snapshot returns the current contents keyed by integration id. A reload
// takes one before loading so it can report what actually changed; the map is
// a copy, so the caller can compare it without holding the lock.
func (r *registry) snapshot() map[string]*registered {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[string]*registered, len(r.byID))
	for id, entry := range r.byID {
		out[id] = entry
	}
	return out
}

func (r *registry) all() []*registered {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*registered, 0, len(r.order))
	for _, id := range r.order {
		if entry, ok := r.byID[id]; ok {
			out = append(out, entry)
		}
	}
	return out
}

func (r *registry) len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byID)
}

// counts returns how many registered integrations are valid and invalid.
func (r *registry) counts() (valid, invalid int) {
	for _, entry := range r.all() {
		if entry.Integration.Valid {
			valid++
		} else {
			invalid++
		}
	}
	return valid, invalid
}

// limits returns the per-integration concurrency limits.
func (r *registry) limits() map[string]int {
	limits := map[string]int{}
	for _, entry := range r.all() {
		if entry.Manifest == nil {
			continue
		}
		if entry.Manifest.Concurrency > 0 {
			limits[entry.Integration.ID] = entry.Manifest.Concurrency
		}
	}
	return limits
}
