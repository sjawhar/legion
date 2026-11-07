//go:build !race

package docs

// raceEnabled reports whether the package was built with the race detector. The tests that
// measure the heap a room keeps are out of the race build altogether, by upload_memory_test.go's
// own //go:build !race: the detector's shadow memory and instrumentation change what the Go heap
// holds and how long each write takes, so the measurement says nothing about the room, and those
// tests check no concurrency the detector could catch.
//
// What reads this constant is the tests the race build still compiles. The two edit storms run
// fewer seeds and fewer edits under -race, whose instrumentation makes each live-room commit and
// its shadow render several times the cost. A measurement of the heap a path keeps, or of the
// allocations it makes, skips itself (skipUnderRace), the detector's escapes being no part of
// the path. A test whose fixture has to stand at one of the document's bounds skips itself too
// (skipAtTheBoundsUnderRace): a million instrumented ygo writes cost the race run minutes and
// gigabytes, and the test drives one goroutine over them.
const raceEnabled = false
