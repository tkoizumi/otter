package daemon

import "sync"

// capacity tracks how many runs of each job are executing right now
// and enforces the per-job `concurrency` limit.
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

// Reserve claims a slot for a job. It returns false when the
// job is at its concurrency limit, when the runtime is draining, or
// when the global worker limit is reached.
func (c *capacity) Reserve(jobID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return false
	}

	limit := c.limits[jobID]
	if limit < 1 {
		limit = 1
	}
	if c.counts[jobID] >= limit {
		return false
	}
	if c.total >= c.maxWorkers {
		return false
	}

	c.counts[jobID]++
	c.total++
	return true
}

// Release gives a slot back.
func (c *capacity) Release(jobID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.counts[jobID] > 0 {
		c.counts[jobID]--
		if c.counts[jobID] == 0 {
			delete(c.counts, jobID)
		}
	}
	if c.total > 0 {
		c.total--
	}
}

func (c *capacity) running(jobID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[jobID]
}

func (c *capacity) totalRunning() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// snapshot returns the running count per job.
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
