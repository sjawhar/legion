package daemon

import (
	"fmt"
	"testing"
)

// keptLongRegressionSeeds are the seeds of the kept-decision property model whose 300-step runs
// found a fault: eleven (11 through 475) reached a hand-over at which the old root architect's
// held start task was the only carrier of a decision made on the handed-over child, which the
// child's tree must be told and the held task must stop naming (supervise's Deps.RootTask, the
// daemon's rootTask); four (363 through 479) reached one where the old architect's held task had
// already been prompted before the hand-over, and the row that carried it was long deleted by
// then, which the oracle's own reading of "a row composed before the hand-over" mistook for a
// telling to another tree; 48 adopted a queued root into a tree whose architect already ran, and
// nothing told that architect what the root had kept while it waited (workflow's Adopt now writes
// it a catch-up); 69 told one decision twice to its architect in the same sweep; 427 dropped a
// re-held copy of a catch-up while the row it copies was publishing, so the decisions the copy
// claimed were named again (DropCatchUps now keeps every row of a telling one of whose rows is
// leased). They run at 300 steps because that is where each reaches it.
var keptLongRegressionSeeds = []int64{11, 48, 69, 73, 96, 125, 311, 363, 365, 366, 367, 388, 414, 427, 432, 459, 475, 479}

// keptShortRegressionSeeds are the seeds whose runs at the default length found faults earlier in
// this work — a decision told twice, one told to an architect owed nothing, one left kept at
// quiesce. They run at the default bounds, which is where each was found.
var keptShortRegressionSeeds = []int64{1, 4, 7, 8, 14, 19, 21, 22, 25, 27, 29, 30, 56, 66, 69, 110, 199, 260}

// Every seed of the property model that has ever failed is run again here, each at the length that
// found it, so a regression in the kept-decision machinery is caught by a named test rather than
// by whichever sweep happens to draw that seed again. The invariant and the oracle are the
// property test's own (TestKeptDecisionsReachTheCurrentArchitectOnce): a person's decision reaches
// the architect of the tree its issue stands in exactly once, at most one telling more goes to the
// tree it left, carried by a row composed before the hand-over, and nothing is left kept.
//
// The default property test keeps the bounds it has, so it stays a minute's work; these seeds are
// the ones worth paying for every time.
func TestKeptDecisionsRegressionSeeds(t *testing.T) {
	for _, seed := range keptLongRegressionSeeds {
		t.Run(fmt.Sprintf("seed-%d-at-300-steps", seed), func(t *testing.T) {
			run := newKeptRun(t, seed, 300)
			run.drive()
			run.quiesce()
			run.held()
			run.report()
		})
	}
	for _, seed := range keptShortRegressionSeeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			run := newKeptRun(t, seed, keptPropertySteps)
			run.drive()
			run.quiesce()
			run.held()
			run.report()
		})
	}
}
