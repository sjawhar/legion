//go:build !race

package docs

// raceEnabled reports whether the package was built with the race detector. A test that measures
// the heap a path keeps or the allocations it makes skips itself under -race (skipUnderRace): the
// detector's shadow memory, its instrumentation and the escapes it forces change what the Go heap
// holds and how long each write takes, so the measurement says nothing about the path, and these
// tests check no concurrency the detector could catch. A test whose fixture has to stand at one of
// the document's bounds skips itself too (skipAtTheBoundsUnderRace): a million instrumented ygo
// writes cost the race run minutes and gigabytes, and the test drives one goroutine over them.
const raceEnabled = false
