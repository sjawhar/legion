package pgmigrate

import (
	"strings"
	"testing"
	"time"
)

// Reading a run of {op_chars} costs the run's length once, not once per byte of it. Flex asks
// {operator} and {xcstart} about every byte of such a run while their actions take one or two
// bytes of it at a time (yyless), so a reading that measures the run on every byte measures it
// once per byte of itself, and the cost is the text's length squared. opRun's two readers hold
// the run last measured, which is what keeps it linear: opRunTo answers from it for every
// position inside the run, and opRunAt for every position inside it the run's stop has not
// passed. Merging the two leaves a reading that is green on every other test in this package and
// takes seconds on a text pg_stat_activity can hold.
//
// What this test reads is the work itself - the bytes measureOpRun examines, which opRunScanned
// hands it - and not the time that work took. A time is a statement about the machine: a bound on
// the ratio between two sizes that linear code keeps near 10 on this pod was passed by linear
// code 4 times in 80 readings under three busy loops, worst 41.5, and envoy's packages are tested
// in parallel, so such a test sends work back for the load on a runner. A count cannot do that.
//
// The multiple below is what the two shapes cost here, with room above it: measured on this pod,
// 655,358 bytes examined over 65,536 bytes of "+-", exactly 10.00 times its length, and 524,282
// over 65,535 of "/*+*/", exactly 8.00 - each a count of this reading's own passes over a run
// (its extent, the two searches that stop at its first "/*" or "--", the trailing [+-] run and
// the search back for a qualifying character), so each is the same on every machine and on every
// run. A reading that measures a run once per byte of itself examines a multiple of the text's
// own length instead, which at 64 KiB is thousands of times this bound. The ceiling on the whole
// reading is kept beside it, since a collapse that keeps its shape would pass a ratio and not a
// wall of two seconds.
func TestOperatorRunLexingIsLinear(t *testing.T) {
	for _, c := range []struct {
		piece    string
		multiple int
	}{
		{"+-", 16},
		{"/*+*/", 16},
	} {
		text := strings.Repeat(c.piece, 64<<10/len(c.piece))
		scanned, took := lexingWork(text)
		t.Logf("%q: %d bytes examined over %d bytes of text, %.2f times its length, in %s",
			c.piece, scanned, len(text), float64(scanned)/float64(len(text)), took)
		if scanned > c.multiple*len(text) {
			t.Errorf("%q: measureOpRun examined %d bytes over a text of %d, %.1f times its length and past the %d this reading needs: a run is being measured more than once",
				c.piece, scanned, len(text), float64(scanned)/float64(len(text)), c.multiple)
		}
		if took > 2*time.Second {
			t.Errorf("%q: 64 KiB took %s, past the two seconds a linear reading of a text this size cannot reach", c.piece, took)
		}
	}
}

// lexingWork is the bytes measureOpRun examined while redactedQuery read text, and how long that
// took. The hook is set for this one reading and put back after it, so no other test of this
// package counts anything.
func lexingWork(text string) (int, time.Duration) {
	scanned := 0
	opRunScanned = func(bytes int) { scanned += bytes }
	defer func() { opRunScanned = nil }()
	start := time.Now()
	redactedQuery(text)
	return scanned, time.Since(start)
}
