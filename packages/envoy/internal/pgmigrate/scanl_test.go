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
// takes seconds on a text pg_stat_activity can hold, which is what this test is here to catch.
//
// It is a ratio rather than a figure, because a figure is a statement about the machine: eight
// times the text costs about eight times the time where the cost is linear and about sixty-four
// where it is squared, so a bound of twenty-four between them separates the two by three times
// in either direction, and the absolute ceiling beside it catches a collapse that keeps its
// shape. Measured 2026-11-06 at this round's head on the issue pod: 11 ms at 8 KiB and 92 ms at
// 64 KiB of "+-", a ratio of 8.4; with opRunTo and opRunAt merged into one reader, 64 KiB takes
// about 5 s, which is sixty times the smaller text and past the ceiling as well.
func TestOperatorRunLexingIsLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("times the lexer over 64 KiB of operator characters")
	}
	for _, piece := range []string{"+-", "/*+*/"} {
		small := timeLexing(strings.Repeat(piece, 8<<10/len(piece)))
		large := timeLexing(strings.Repeat(piece, 64<<10/len(piece)))
		t.Logf("%q: 8 KiB %s, 64 KiB %s, ratio %.1f", piece, small, large, float64(large)/float64(small))
		if ratio := float64(large) / float64(small); ratio > 24 {
			t.Errorf("%q: 64 KiB took %s against 8 KiB's %s, a ratio of %.1f: the eight times the text costs more than eight times the time, so a run is being read more than once", piece, large, small, ratio)
		}
		if large > 2*time.Second {
			t.Errorf("%q: 64 KiB took %s, past the two seconds a linear reading of a text this size cannot reach", piece, large)
		}
	}
}

// timeLexing is the shortest of three readings of text, so that one descheduled run does not
// decide the ratio above.
func timeLexing(text string) time.Duration {
	best := time.Duration(1<<62 - 1)
	for range 3 {
		start := time.Now()
		redactedQuery(text)
		if took := time.Since(start); took < best {
			best = took
		}
	}
	return best
}
