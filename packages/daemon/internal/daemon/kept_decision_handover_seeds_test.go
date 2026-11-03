package daemon

import (
	"fmt"
	"testing"
)

// A child's decision that an old root's start task already named, and that the task still holds
// unsent when the old root's linger hands the child over, belongs to the child's new tree: the
// hand-over must give it to the child's architect, not leave it in the old architect's task, which
// is sent to the old tree's next run after the child has left. These seeds of the kept-decision
// property model, at 300 steps, each reach that state: the hand-over moves only what is still
// kept, the old root's held start task (row 5, 10, ...) is prompted to the old tree's architect
// after the hand-over, and the child's architect is told the decision 0 times.
func TestADecisionHeldInTheOldArchitectsTaskAtAHandOverReachesTheChild(t *testing.T) {
	t.Parallel()
	// Each seed's run has a database, runtime and state directory of its own (newKeptRun), so the
	// seeds run in parallel, with each other and with the other kept-decision seed tests.
	for _, seed := range []int64{96, 414, 432} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			run := newKeptRun(t, seed, 300)
			run.drive()
			run.quiesce()
			run.held()
			run.report()
			// The test names one state: a hand-over between a held task's queue and the
			// confirmation that tells what it holds. A seed that never reaches it checks nothing
			// this test is for, and passes with every guard of that window broken.
			if run.counts.handedBeforeConfirm == 0 {
				t.Fatalf("seed %d: 0 hand-overs between a held task and its confirmation; the seed does not reach "+
					"the state this test guards", seed)
			}
		})
	}
}
