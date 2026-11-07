package pgmigrate

import (
	"strings"
	"testing"
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
// What this test reads is the work itself - the bytes each search of measureOpRun read, worked
// out here from what opRunScanned is told - and not the time that work took. A time is a
// statement about the machine: a bound on the ratio between two sizes that linear code keeps near
// 10 on this pod was passed by linear code 4 times in 80 readings under three busy loops, and a
// wall-clock ceiling beside it failed every run under -race while the code was right. A count
// cannot do either.
//
// The three texts are the three shapes the reading has. "+-" is one run measured once and then
// answered from; "/*+*/" opens a comment at the start of each run, so each is measured afresh;
// and "+/**/" opens one inside a run, which is the shape that takes measureOpRun's reuse branch,
// where the extent is taken from the run before it and only the searches run again. Each count
// below is this reading's own passes over those bytes, so it is the same on every machine and
// every run, and each bound is the measurement with room above it:
//
//	+-      655,358 bytes over 65,536  10.00 times its length
//	/*+*/   314,588 bytes over 65,535   4.80
//	+/**/   288,354 bytes over 65,535   4.40
//
// A reading that measures a run once per byte of itself examines a multiple of the text's own
// length instead: with the two readers merged, "/*+*/" reads thousands of times its length; with
// the reuse branch taken out, and with a re-scan planted inside it, "+/**/" reads 13,110 times
// its length - the same work either way, since the branch is the only thing between them.
func TestOperatorRunLexingIsLinear(t *testing.T) {
	for _, c := range []struct {
		piece    string
		multiple int
	}{
		{"+-", 16},
		{"/*+*/", 8},
		{"+/**/", 8},
	} {
		text := strings.Repeat(c.piece, 64<<10/len(c.piece))
		scanned := lexingWork(text)
		t.Logf("%q: %d bytes read over %d bytes of text, %.2f times its length",
			c.piece, scanned, len(text), float64(scanned)/float64(len(text)))
		if scanned > c.multiple*len(text) {
			t.Errorf("%q: this reading examined %d bytes over a text of %d, %.1f times its length and past the %d it needs: a run is being read more than once",
				c.piece, scanned, len(text), float64(scanned)/float64(len(text)), c.multiple)
		}
	}
}

// lexingWork is the bytes this reading examined while redactedQuery read text: every byte it
// asked isOpChar about, wherever that scan is written, plus what each of measureOpRun's two
// searches and two backward loops read. The scan is counted at isOpChar rather than reported by
// measureOpRun because a function's own report of its work is a claim about which of its branches
// ran, and a claim is what this test exists to replace: a re-scan planted inside the reuse branch
// read 858,901,710 bytes of a 64 KiB text while measureOpRun's own extent count reported 262,140,
// and the test passed at 4.00 times the text's length while taking 20 s. Both hooks are set for
// this one reading and put back after it, so no other test of this package counts anything.
func lexingWork(text string) int {
	scanned := 0
	opCharRead = func() { scanned++ }
	opRunScanned = func(run opRun, slash, dash int) {
		at := run.from
		if slash >= 0 {
			scanned += slash + 2
		} else {
			scanned += run.to - at
		}
		dashEnd := run.to
		if slash >= 0 {
			dashEnd = at + slash
		}
		if dash >= 0 {
			scanned += dash + 2
		} else {
			scanned += dashEnd - at
		}
		scanned += run.stop - run.plus
		switch {
		case run.qualify >= 0:
			scanned += run.stop - 1 - run.qualify
		case run.stop-1 > at:
			scanned += run.stop - 1 - at
		}
	}
	defer func() {
		opCharRead = nil
		opRunScanned = nil
	}()
	redactedQuery(text)
	return scanned
}
