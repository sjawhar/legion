package classify

import (
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// maxRunSteps bounds a head run: a run longer than this is cut to its newest maxRunSteps steps,
// its anchor moving to the judgement before the first head among them still waiting, and the run
// closing when none of them waits. A run stays open only while a head in it waits for a push of
// its move, and GitHub sends every push, so a run reaches the bound only when a push webhook is
// lost for good, or each of more than this many facts arrives before the push it waits for; the
// heads cut from it keep the judgement the last replay gave them, and no later push replays them.
// The run lets a push go only when the record's own bound does (ApplyPush), so a head whose push
// arrived first is never cut loose from it, however many heads follow one lost push.
const maxRunSteps = 32

// A head run (record.PullRequest.Run) makes the fix-attempt count, its mark, the planned mark and
// the CI settlement the same in every order GitHub delivers a pull request's pushes, heads and
// settlements in. A head that arrives before every push of its move is recorded opens one: its
// anchor is the judgement before that head, and every fact applied while it is open is a step -
// a head, a settlement, a required read, the clearing of the head the required workflows were
// read at, and a reopen. Each step and each push recorded meanwhile replays the run (replayRun):
// the steps again, from the anchor, with every push recorded - which is the order in which every
// push arrived before every head. The run closes once every head in it was judged with a chain of
// pushes, and its front - the steps before the first head still waiting - folds into the anchor.
// The replay decides the judgement only; the pull request's own pushes stay as the facts left
// them (AdvancePullRequestHead).

// JudgementChanged says whether the part of a pull request a head run replays differs between
// before and after: a replay that changed it changes what the CI rule decides at the head. A set
// never read and a settlement never made (Required, CheckRuns nil) differ from empty ones; the
// other lists are compared by what they hold, which the store and the run keep in either form.
func JudgementChanged(before, after record.PullRequest) bool {
	b, a := judgementOf(before), judgementOf(after)
	return b.HeadSHA != a.HeadSHA || b.FixAttempts != a.FixAttempts || b.HeadCounted != a.HeadCounted || b.PlannedRed != a.PlannedRed ||
		b.CheckedHead != a.CheckedHead || !sameItems(b.Failing, a.Failing) || !sameItems(b.Cancelled, a.Cancelled) || !same(b.CheckRuns, a.CheckRuns) ||
		b.Generation != a.Generation || b.Snapshot != a.Snapshot || !same(b.Required, a.Required) || !sameItems(b.Workflows, a.Workflows) ||
		b.WorkflowsHead != a.WorkflowsHead
}

// runStep applies step to pr's open run and replays it.
func runStep(pr record.PullRequest, step record.RunStep) record.PullRequest {
	run := *pr.Run
	run.Steps = append(clone(run.Steps), step)
	pr.Run = &run
	return replayRun(pr)
}

// replayRun replays pr's open run: its steps from its anchor, with the run's pushes - every code
// push recorded since the anchor's, in delivery order, two pushes of the same two heads kept
// apart - and every push the pull request holds that carries an approval across, which no fact
// drops (unspentPushes), so the run does not keep a copy of them; less every push whose move the
// anchor had already made: a push delivered after its move was judged before the run opened is
// spent there, as it is when it arrives first, and its move keeps it (pushedMoves). A head step
// arrives by the move its synchronize named (record.RunStep.Before, Sender), with the moves late
// synchronizes named (record.HeadRun.Names) whenever they were recorded, each spent by the arrival
// that took it. The run stays open from its first head judged with no chain of pushes, or still
// waiting for what its synchronize named, its anchor the judgement before that head, and closes
// when there is none; past maxRunSteps its oldest steps fold into the anchor too. The pull request
// keeps its own pushes. BlockedAttempts is what was reported, so it stays, unless the count it
// reported is no longer reached, as when a count is taken back.
func replayRun(pr record.PullRequest) record.PullRequest {
	run, live := pr.Run, pr.Pushes
	blocked := pr.BlockedAttempts
	at := withJudgement(pr, run.Anchor)
	at.Pushes, at.Run = clone(run.Pushes), nil
	for _, p := range live {
		if carriesApproval(p) {
			at.Pushes = withPush(at.Pushes, p)
		}
	}
	for _, p := range at.Pushes {
		at.Moves, _ = pushedMoves(at.Moves, p)
	}
	at.Pushes = unspentPushes(at.Pushes, at.Moves, false)
	type state struct {
		judgement record.Judgement
		pushes    []record.ClassifiedPush
		names     []record.Named
		since     time.Time
	}
	names, since := clone(run.Names), run.Since
	before := make([]state, len(run.Steps))
	var unchained []int
	for i, step := range run.Steps {
		before[i] = state{judgementOf(at), withoutApprovalCarriers(at.Pushes), names, since}
		var chained bool
		at, chained, names = applyStep(at, step, names, since)
		if step.Head != "" {
			since = step.SyncedAt
			if !chained {
				unchained = append(unchained, i)
			}
		}
	}
	waiting := len(run.Steps)
	for _, i := range unchained {
		if i >= len(run.Steps)-maxRunSteps {
			waiting = i
			break
		}
	}
	if waiting < len(run.Steps) {
		at.Run = &record.HeadRun{Anchor: before[waiting].judgement, Pushes: before[waiting].pushes, Names: clone(before[waiting].names),
			Since: before[waiting].since, Steps: clone(run.Steps[waiting:]), Decided: run.Decided}
	}
	at.Pushes, at.BlockedAttempts = unspentPushes(live, at.Moves, false), blocked
	if at.FixAttempts < blocked {
		at.BlockedAttempts = 0
	}
	return at
}

// withoutApprovalCarriers is pushes without those that carry an approval across.
func withoutApprovalCarriers(pushes []record.ClassifiedPush) []record.ClassifiedPush {
	kept := make([]record.ClassifiedPush, 0, len(pushes))
	for _, p := range pushes {
		if !carriesApproval(p) {
			kept = append(kept, p)
		}
	}
	return kept
}

// applyStep applies one step of a head run with the run's named moves and GitHub's clock of the
// head before it (since), says, for a head, whether a chain of pushes led to it with nothing left to
// wait for (advance) - any other step is chained - and returns the named moves the step left.
func applyStep(pr record.PullRequest, step record.RunStep, names []record.Named, since time.Time) (record.PullRequest, bool, []record.Named) {
	switch {
	case step.Head != "":
		return advance(pr, Arrival{Before: step.Before, Sender: step.Sender, At: step.SyncedAt, Since: since}, step.Head, names)
	case step.Settlement != nil:
		pr, _ = settle(pr, candidateOf(*step.Settlement))
	case step.Required != nil:
		pr.Required = clone(step.Required.Names)
		pr.Workflows = clone(step.Required.Workflows)
		pr.WorkflowsHead = step.Required.WorkflowsHead
	case step.WorkflowsCleared:
		pr.WorkflowsHead = ""
	case step.Reopened:
		pr.CheckedHead, pr.Failing, pr.Cancelled, pr.CheckRuns, pr.Generation, pr.Snapshot = "", []string{}, nil, nil, 0, ""
	}
	return pr, true, names
}

// withStep applies s to pr: as a step of its head run while one is open, and on its own otherwise.
func withStep(pr record.PullRequest, s record.RunStep) record.PullRequest {
	if pr.Run != nil {
		return runStep(pr, s)
	}
	pr, _, _ = applyStep(pr, s, nil, time.Time{})
	return pr
}

// Reopen is the pull request a reopen at head leaves: moved to head as a synchronize would move it
// (AdvancePullRequestHead), its CI settlement started afresh. While a head run is open the reopen
// is a step of it.
func Reopen(pr record.PullRequest, head string) record.PullRequest {
	if head != pr.HeadSHA {
		pr = AdvancePullRequestHead(pr, Arrival{}, head)
	}
	return withStep(pr, record.RunStep{Reopened: true})
}

// ApplyCheckSettlement applies a CI settlement to the pull request when it stands for the head
// (SettlementFor) and is newer than the one recorded (ApplySettlement), and says whether the
// judgement changed. While a head run is open the settlement is a step of it, recorded whether it
// stands or not: a push the run is waiting for can make it stand. The pull request then changed
// either way, and the judgement changed when the replay with it differs. A redelivery is no new
// step (redundantSettlement), so the listener's redeliveries do not use up the run's bound
// (maxRunSteps).
func ApplyCheckSettlement(pr record.PullRequest, candidate SettlementCandidate) (record.PullRequest, bool) {
	if pr.Run == nil {
		return settle(pr, candidate)
	}
	step := record.Settlement{Head: candidate.Head, CheckRuns: clone(candidate.CheckRuns), Generation: candidate.Generation,
		Snapshot: candidate.Snapshot, Failing: clone(candidate.Failing), Cancelled: clone(candidate.Cancelled)}
	if redundantSettlement(pr.Run.Steps, step) {
		return pr, false
	}
	replayed := runStep(pr, record.RunStep{Settlement: &step})
	return replayed, JudgementChanged(pr, replayed)
}

// redundantSettlement is whether settlement, appended to steps, would replay as nothing in every
// order of the pushes still to come: an exact copy of it is a step, and no head arrived and no
// reopen started the settlement afresh after that copy. The copy then meets the same head and the
// same pushes as the redelivery would: where the copy stands and is newer, it applies and the
// redelivery is not newer; where it does not, neither does the redelivery. A copy before a head's
// arrival is no such copy - a settlement of a head that had not arrived stands once it has - so the
// redelivery is a step of its own.
func redundantSettlement(steps []record.RunStep, settlement record.Settlement) bool {
	for i := len(steps) - 1; i >= 0; i-- {
		switch held := steps[i]; {
		case held.Head != "" || held.Reopened:
			return false
		case held.Settlement != nil && sameSettlement(*held.Settlement, settlement):
			return true
		}
	}
	return false
}

// sameSettlement is whether a and b are the same report of the same settlement.
func sameSettlement(a, b record.Settlement) bool {
	return a.Head == b.Head && a.Generation == b.Generation && a.Snapshot == b.Snapshot && sameItems(a.CheckRuns, b.CheckRuns) &&
		sameItems(a.Failing, b.Failing) && sameItems(a.Cancelled, b.Cancelled)
}

// ApplyRequiredRead records a read of what the base branch requires (intake.RequiredChecks): its
// required checks, and its required workflows with their runs' results at head. While a head run
// is open the read is a step of it.
func ApplyRequiredRead(pr record.PullRequest, names []string, workflows []record.RequiredWorkflow, head string) record.PullRequest {
	read := record.RequiredRead{Names: append([]string{}, names...), Workflows: append([]record.RequiredWorkflow{}, workflows...), WorkflowsHead: head}
	return withStep(pr, record.RunStep{Required: &read})
}

// ClearWorkflowsHead clears the head the required workflows' runs were read at, so the next read
// of the runs is a change (record.RequiredReadUnchanged). While a head run is open it is a step of
// it.
func ClearWorkflowsHead(pr record.PullRequest) record.PullRequest {
	return withStep(pr, record.RunStep{WorkflowsCleared: true})
}

// settle applies candidate when it stands for the head and is newer than the recorded settlement,
// and otherwise leaves pr as it was.
func settle(pr record.PullRequest, candidate SettlementCandidate) (record.PullRequest, bool) {
	stood, stands := SettlementFor(pr, candidate)
	if !stands {
		return pr, false
	}
	applied, ok := ApplySettlement(stood, candidate)
	if !ok {
		return pr, false
	}
	return applied, true
}

func candidateOf(s record.Settlement) SettlementCandidate {
	return SettlementCandidate{Head: s.Head, CheckRuns: clone(s.CheckRuns), Generation: s.Generation, Snapshot: s.Snapshot,
		Failing: clone(s.Failing), Cancelled: clone(s.Cancelled)}
}

// judgementOf is the part of pr a head run replays.
func judgementOf(pr record.PullRequest) record.Judgement {
	return record.Judgement{HeadSHA: pr.HeadSHA, FixAttempts: pr.FixAttempts, HeadCounted: pr.HeadCounted, PlannedRed: pr.PlannedRed,
		CheckedHead: pr.CheckedHead, Failing: clone(pr.Failing), Cancelled: clone(pr.Cancelled), CheckRuns: clone(pr.CheckRuns),
		Generation: pr.Generation, Snapshot: pr.Snapshot, Required: clone(pr.Required), Workflows: clone(pr.Workflows), WorkflowsHead: pr.WorkflowsHead,
		Moves: clone(pr.Moves)}
}

// withJudgement is pr with j as its judgement.
func withJudgement(pr record.PullRequest, j record.Judgement) record.PullRequest {
	pr.HeadSHA, pr.FixAttempts, pr.HeadCounted, pr.PlannedRed = j.HeadSHA, j.FixAttempts, j.HeadCounted, j.PlannedRed
	pr.CheckedHead, pr.Failing, pr.Cancelled, pr.CheckRuns = j.CheckedHead, clone(j.Failing), clone(j.Cancelled), clone(j.CheckRuns)
	pr.Generation, pr.Snapshot = j.Generation, j.Snapshot
	pr.Required, pr.Workflows, pr.WorkflowsHead = clone(j.Required), clone(j.Workflows), j.WorkflowsHead
	pr.Moves = clone(j.Moves)
	return pr
}

// clone is a copy of s, nil when s is: a nil required set is one never read, and a nil set of
// check runs one never settled, both apart from an empty one.
func clone[T any](s []T) []T {
	if s == nil {
		return nil
	}
	return append(make([]T, 0, len(s)), s...)
}

// same is whether a and b hold the same elements in the same order, a nil slice apart from an
// empty one (clone).
func same[T comparable](a, b []T) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameItems is whether a and b hold the same elements in the same order, a nil slice the same as
// an empty one.
func sameItems[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
