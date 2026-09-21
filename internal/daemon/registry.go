package daemon

import (
	"sort"
	"sync"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/identity"
)

// registered is an integration instance known to the daemon together with the
// runtime state Otter attaches to it.
//
// Integration.ID is the durable registry identity; Integration.Name is the
// mutable manifest label. The two are deliberately different values: state,
// runs, tokens, releases and environments key off the identity, while the
// label is what an operator types and reads.
type registered struct {
	Integration *config.Integration
	Manifest    *config.Manifest
	Instance    identity.Instance

	// WebhookToken is generated on first start and persisted, so webhook URLs
	// keep working across restarts.
	WebhookToken string
}

// registry holds the discovered integrations. It is rebuilt from disk on every
// daemon start and on every reload; the registry database and the manifests
// themselves are the source of truth.
type registry struct {
	mu     sync.RWMutex
	byID   map[string]*registered
	byName map[string][]string
	order  []string
}

func newRegistry() *registry {
	return &registry{byID: map[string]*registered{}, byName: map[string][]string{}}
}

// discovered is the result of one observation-and-reconciliation pass.
type discovered struct {
	items     []*config.Integration
	tokens    map[string]string
	instances map[string]identity.Instance
}

// load replaces the registry contents.
func (r *registry) load(set discovered) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.byID = make(map[string]*registered, len(set.items))
	r.byName = make(map[string][]string, len(set.items))
	r.order = make([]string, 0, len(set.items))

	for _, it := range set.items {
		// An unregistered-but-present directory is surfaced under its label.
		// It must never shadow a real identity that happens to share the key.
		if _, exists := r.byID[it.ID]; exists {
			continue
		}
		entry := &registered{Integration: it, Manifest: it.Manifest}
		if inst, ok := set.instances[it.ID]; ok {
			entry.Instance = inst
		}
		if token, ok := set.tokens[it.ID]; ok {
			entry.WebhookToken = token
		}
		r.byID[it.ID] = entry
		r.byName[it.Name] = append(r.byName[it.Name], it.ID)
		r.order = append(r.order, it.ID)
	}
	sort.Strings(r.order)
	for name := range r.byName {
		sort.Strings(r.byName[name])
	}
}

func (r *registry) get(id string) (*registered, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.byID[id]
	return entry, ok
}

// byLabel returns every registered instance carrying a label, in a stable
// order. Zero matches means the label is unknown; more than one means a bare
// label reference is ambiguous.
func (r *registry) byLabel(label string) []*registered {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := r.byName[label]
	out := make([]*registered, 0, len(ids))
	for _, id := range ids {
		if entry, ok := r.byID[id]; ok {
			out = append(out, entry)
		}
	}
	return out
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
