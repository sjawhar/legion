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
// reads thousands of times the length of a text pg_stat_activity can hold, which the counts below
// are what bound.
//
// What this test reads is the work itself - every byte a scan asked isOpChar about, which is
// where <xc>'s own matchOpChar probes enter the count too, plus what each of measureOpRun's two
// searches and two backward loops read - and not the time that work took. A time is a statement
// about the machine: a bound on the ratio between two sizes that linear code keeps near 10 was
// passed by linear code under load, and a wall-clock ceiling beside it failed every run under
// -race while the code was right. A count cannot do either, and the counts below are the same on
// any machine.
//
// What the count does not see, said here and nowhere else, since the two hooks point at this
// header for it: a scan that asks isOpChar nothing. An inline byte test over the run's extent, or
// a strings call that answers the same question, is work this test charges nobody for - measured,
// a re-scan written as an inline loop READS about 13,110 times the text's length, as the same
// work written through isOpChar does, and this test COUNTS it at 4.40, the figure the run below
// reads without it. The hooks are where a scan written the way this file writes them is counted,
// not a proof that no other scan exists.
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
// the reuse branch taken out, and with a re-scan planted inside it that asks isOpChar, "+/**/"
// reads 13,110 times its length - the same work either way, since the branch is the only thing
// between them.
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
// asked isOpChar about, plus what each of measureOpRun's two searches and two backward loops
// read. The scan is counted at isOpChar rather than reported by measureOpRun because a function's
// own report of its work is a claim about which of its branches ran: a re-scan planted inside the
// reuse branch read 858,901,710 bytes of a 64 KiB text while the count built on measureOpRun's
// own report came to 262,140 - its extent loop and the searches together, the extent loop's own
// share of that being 131,070 - and the test passed on it. What that moves is where the claim
// sits, not whether one is made. Both hooks are set for this one reading and put back after it,
// so no other test of this package counts anything.
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
