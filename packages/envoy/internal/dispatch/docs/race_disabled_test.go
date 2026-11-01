//go:build !race

package docs

// raceEnabled reports whether the package was built with the race detector. A test that measures
// the heap a path keeps or the allocations it makes skips itself under -race (skipUnderRace): the
// detector's shadow memory, its instrumentation and the escapes it forces change what the Go heap
// holds and how long each write takes, so the measurement says nothing about the path, and these
// tests check no concurrency the detector could catch.
const raceEnabled = false
