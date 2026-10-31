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
	for _, seed := range []int64{96, 414, 432} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			run := newKeptRun(t, seed, 300)
			run.drive()
			run.quiesce()
			run.held()
			run.report()
		})
	}
}
