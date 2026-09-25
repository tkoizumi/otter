//go:build race

package timeline

// raceEnabled reports whether this test binary was built with the race detector.
// The Go toolchain defines the `race` build tag for that, which is the only
// reliable signal: the detector has no exported runtime flag.
const raceEnabled = true
