// Package drill holds repository-level tests for the operating drills under
// scripts/drill, the machinery that produces the recorded evidence for the
// Phase 0 tasks.
//
// It ships no runtime code. It exists so that the drills' *hostless* checks run
// wherever the Go suite runs, including CI: a drill that only ever runs when
// someone remembers to type its name is a drill that rots, and the whole point
// of the mechanism is that the evidence stays executable.
package drill
