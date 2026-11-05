//go:build !race

package docs

// raceEnabled reports whether the package was built with the race detector. A test that measures
// the heap a room keeps skips itself under -race (skipUnderRace): the detector's shadow memory and
// instrumentation change what the Go heap holds and how long each write takes, so the measurement
// says nothing about the room, and the test checks no concurrency the detector could catch. The
// edit storms run fewer seeds and fewer edits under -race, whose instrumentation makes each
// live-room commit and its shadow render several times the cost.
const raceEnabled = false
