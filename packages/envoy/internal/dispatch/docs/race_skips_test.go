package docs

import "testing"

// This file holds the two skips a -race build needs, and it is in every build: the tests that
// read them are, while the room-memory tests that the race build must not compile at all are
// behind upload_memory_test.go's own //go:build !race.
//
// They read raceSkipsEnabled rather than raceEnabled, and the two say the same thing
// (race_skips_disabled_test.go, race_skips_enabled_test.go). The reason is the step envoy-go runs
// for this lane: `.github/scripts/ci_job_reads.py` derives, from every raceEnabled a test reads,
// the list of tests that run smaller under the detector and so need a plain full-size run, and it
// refuses a read that no one top-level test encloses, since it cannot say which test that read
// makes smaller. A helper's read is exactly that, and the two helpers below are the shape the
// package wants: one sentence of reasoning, written once, for the thirty-odd tests that skip for
// it. The full-size plain run those tests need is their own package's job, envoy-go-docs, which
// runs every test of this package plain on every change envoy_go selects and again in
// envoy-go-docs-finalizers; naming them in envoy-go's ten-minute step as well would run this
// package's heaviest rows a second time under a budget they do not fit (INFRA-179).

// skipUnderRace skips a measurement of the heap, or of the allocations a path makes, in a -race
// build: the race detector distorts both figures (its shadow memory, its instrumented allocations
// and the escapes its instrumentation forces are not the measured path's), and these tests
// exercise no concurrency for it to catch, so under -race they cost minutes of the package's time
// budget and prove nothing. The normal build runs them.
func skipUnderRace(t *testing.T) {
	t.Helper()
	if raceSkipsEnabled {
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
	if raceSkipsEnabled {
		t.Skip("the fixture this test stands at the document's bounds costs the race detector minutes and gigabytes, and the test drives one goroutine over it")
	}
}
