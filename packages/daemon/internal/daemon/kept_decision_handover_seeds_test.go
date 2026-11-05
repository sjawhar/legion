package daemon

import (
	"fmt"
	"testing"
)

// A child's decision that an old root's start task already named, and that the task still holds
// unsent when the old root's linger hands the child over, belongs to the child's new tree: the
// hand-over must give it to the child's architect, not leave it in the old architect's task, which
// is sent to the old tree's next run after the child has left. These seeds of the kept-decision
// property model, at 300 steps, each reach a hand-over between a held task's queue and the
// confirmation that tells what it holds (keptCounts.handedBeforeConfirm). Left to the generator's
// pick among the enabled steps, a seed reached it on its usual schedule but not on every other: a
// run under load can take another schedule (the daemon's machinery runs on real time), and under
// -race on one CPU seeds 13 and 24 each took one that never reached it about once in thirty runs.
// So the run takes that step the first time it is enabled while the held task names kept decisions
// (seekHeldHandOver), and until then no other step hands a member over, orphans the child or closes
// the tree: on a schedule that handed both members over before the root held such a task, seed 24
// never had the step enabled at all (run 41808140568). Every other step is the seed's own. Seeds
// 96, 414 and 432, which found the fault this test was written for, never reach it at this code;
// they run in keptLongRegressionSeeds.
func TestADecisionHeldInTheOldArchitectsTaskAtAHandOverReachesTheChild(t *testing.T) {
	t.Parallel()
	// Each seed's run has a database, runtime and state directory of its own (newKeptRun), so the
	// seeds run in parallel, with each other and with the other kept-decision seed tests.
	for _, seed := range []int64{13, 20, 24} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			run := newKeptRun(t, seed, 300)
			run.seekHeldHandOver = true
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
