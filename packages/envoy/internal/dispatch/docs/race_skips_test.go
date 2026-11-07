package docs

import "testing"

// This file holds the two skips a -race build needs, and it is in every build: the tests that
// read them are, while the room-memory tests that the race build must not compile at all are
// behind upload_memory_test.go's own //go:build !race. raceEnabled is race_disabled_test.go's
// and race_enabled_test.go's.

// skipUnderRace skips a measurement of the heap, or of the allocations a path makes, in a -race
// build: the race detector distorts both figures (its shadow memory, its instrumented allocations
// and the escapes its instrumentation forces are not the measured path's), and these tests
// exercise no concurrency for it to catch, so under -race they cost minutes of the package's time
// budget and prove nothing. The normal build runs them.
func skipUnderRace(t *testing.T) {
	t.Helper()
	if raceEnabled {
		t.Skip("the race detector distorts the heap and the allocations this test measures, and the test checks no concurrency")
	}
}

// skipAtTheBoundsUnderRace skips, in a -race build, a test whose fixture has to stand at one of
// the document's bounds: the 1,048,576 structs ygo decodes in one update (maxUpdateItems), or the
// markdown's byte and element bounds rewritten until the cap refuses a write. Such a fixture is
// built by about a million ygo writes, each of which the race detector instruments and whose
// items it shadows: measured on two processors, the tests this and skipUnderRace leave out took
// 50 of the 121 minutes a -race run of every test in the package took, and the decode-cap ones
// among them carried its resident memory from 0.8 GB to 9.5 GB. Each drives one goroutine over
// its fixture and then checks a figure or a refusal, so what the detector could catch in it - the
// room's settlement, its sweep, its sockets - the tests beside it drive over a small document in
// seconds. The normal build runs them.
func skipAtTheBoundsUnderRace(t *testing.T) {
	t.Helper()
	if raceEnabled {
		t.Skip("the fixture this test stands at the document's bounds costs the race detector minutes and gigabytes, and the test drives one goroutine over it")
	}
}
