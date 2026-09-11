package daemon

import "sync"

// capacity tracks how many runs of each integration are executing right now
// and enforces the per-integration `concurrency` limit.
//
// It implements queue.Capacity so the durable queue can reserve a slot in the
// same transaction that claims a run. That is what makes "claim" and "start"
// atomic: two workers can never both decide they have room.
type capacity struct {
	mu         sync.Mutex
	limits     map[string]int
	counts     map[string]int
	total      int
	maxWorkers int
	closed     bool
}

func newCapacity(maxWorkers int) *capacity {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	return &capacity{
		limits:     map[string]int{},
		counts:     map[string]int{},
		maxWorkers: maxWorkers,
	}
}

func (c *capacity) setLimits(limits map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.limits = limits
}

// Reserve claims a slot for an integration. It returns false when the
// integration is at its concurrency limit, when the runtime is draining, or
// when the global worker limit is reached.
func (c *capacity) Reserve(integrationID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return false
	}

	limit := c.limits[integrationID]
	if limit < 1 {
		limit = 1
	}
	if c.counts[integrationID] >= limit {
		return false
	}
	if c.total >= c.maxWorkers {
		return false
	}

	c.counts[integrationID]++
	c.total++
	return true
}

// Release gives a slot back.
func (c *capacity) Release(integrationID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.counts[integrationID] > 0 {
		c.counts[integrationID]--
		if c.counts[integrationID] == 0 {
			delete(c.counts, integrationID)
		}
	}
	if c.total > 0 {
		c.total--
	}
}

func (c *capacity) running(integrationID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[integrationID]
}

func (c *capacity) totalRunning() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// snapshot returns the running count per integration.
func (c *capacity) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.counts))
	for id, n := range c.counts {
		out[id] = n
	}
	return out
}

// close stops granting new slots.
func (c *capacity) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}
