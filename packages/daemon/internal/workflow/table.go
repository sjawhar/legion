package workflow

import "github.com/sjawhar/legion/daemon/internal/phase"

// TriggerKind names the observation that can advance a workflow phase.
type TriggerKind string

const (
	TriggerGateOpened          TriggerKind = "gate_opened"
	TriggerPlannerCompleted    TriggerKind = "planner_completed"
	TriggerImplementationReady TriggerKind = "implementation_ready"
	TriggerTesterPassed        TriggerKind = "tester_passed"
	TriggerTesterFailed        TriggerKind = "tester_failed"
	TriggerReviewApproved      TriggerKind = "review_approved"
	TriggerReviewRejected      TriggerKind = "review_rejected"
	TriggerRetroCompleted      TriggerKind = "retro_completed"
	TriggerPullRequestMerged   TriggerKind = "pull_request_merged"
	TriggerSignOff             TriggerKind = "sign_off"
	TriggerBackward            TriggerKind = "backward"
	// TriggerConflict is a head GitHub will not merge into its base: the daemon's own reading of
	// its own read, raised by no agent (mergeability). GitHub computes no merge ref for such a
	// head and runs no checks on it, so nothing else would ever arrive for it, and the issue goes
	// back to implementing from whatever later phase it stands in, for the implementer to merge
	// the base forward.
	TriggerConflict TriggerKind = "conflict"
)

// EffectKind identifies the durable outbox consequences selected by a row. EffectSuspend is the
// issue's close alone (Engine.leave): a phase worker stays live from its role's first assignment
// until its issue closes, so no row that only moves the issue between phases suspends anyone.
type EffectKind string

const (
	EffectStatus  EffectKind = "status"
	EffectSuspend EffectKind = "suspend"
	EffectStart   EffectKind = "start"
	EffectNotice  EffectKind = "notice"
	EffectLinger  EffectKind = "linger"
)

// Snapshot is the record state a row guard reads. Guards are pure and contain no side effects.
// HasOpenOrMergedPR is whether the issue's pull request record is open or merged. A merged record
// counts: the work landed, the merge told the architect (pr-merged), and the workflow runs on. A
// completion never moves an issue on a pull request closed without merging, nor with none recorded.
type Snapshot struct {
	Phase             phase.Phase
	HasOpenOrMergedPR bool
	GateOpen          bool
}

// Row is one declarative workflow transition. The engine interprets its effects transactionally.
type Row struct {
	From    phase.Phase
	Trigger TriggerKind
	Guard   func(Snapshot) bool
	To      phase.Phase
	Status  string
	Effects []EffectKind
}

func allow(Snapshot) bool { return true }

// PhaseStatus is the Dispatch status an issue standing in to shows: what a row that moves an issue
// into to writes, and what the boot writes for an issue whose status is not its phase's
// (Engine.correctStatuses). An issue in planning or implementing is in_progress, which is where a
// child first shows that Legion works it (dispatch://INFRA-714/spec R4); merging keeps retro,
// which the row into it writes none over. Phases where an issue waits rather than works — admitted
// before its first phase, held, and awaiting_person, where the root keeps the status of the phase
// it waits in — have no status of their own, and neither has a phase outside the workflow but done.
func PhaseStatus(to phase.Phase) string {
	switch to {
	case phase.Planning, phase.Implementing:
		return "in_progress"
	case phase.Testing:
		return "testing"
	case phase.Reviewing, phase.Accepting:
		return "needs_review"
	case phase.Retro, phase.Merging:
		return "retro"
	case phase.Done:
		return "done"
	}
	return ""
}

// Table is the complete workflow transition table. Rows, not switch-case scheduling, define every
// phase change. Backward rows exist for every allowed target so coverage detects a deleted edge.
// Every status a row writes is its target phase's (PhaseStatus).
var Table = append([]Row{
	{From: phase.Admitted, Trigger: TriggerGateOpened, Guard: allow, To: phase.Planning, Status: "in_progress", Effects: []EffectKind{EffectStatus, EffectStart, EffectNotice}},
	{From: phase.Planning, Trigger: TriggerPlannerCompleted, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Implementing, Trigger: TriggerImplementationReady, Guard: func(s Snapshot) bool { return s.HasOpenOrMergedPR }, To: phase.Testing, Status: "testing", Effects: []EffectKind{EffectStatus, EffectStart, EffectNotice}},
	{From: phase.Testing, Trigger: TriggerTesterPassed, Guard: allow, To: phase.Reviewing, Status: "needs_review", Effects: []EffectKind{EffectStatus, EffectStart, EffectNotice}},
	{From: phase.Testing, Trigger: TriggerTesterFailed, Guard: allow, To: phase.Implementing, Status: "in_progress", Effects: []EffectKind{EffectStatus, EffectStart, EffectNotice}},
	{From: phase.Reviewing, Trigger: TriggerReviewApproved, Guard: allow, To: phase.Accepting, Effects: []EffectKind{EffectNotice}},
	{From: phase.Accepting, Trigger: TriggerSignOff, Guard: allow, To: phase.Retro, Status: "retro", Effects: []EffectKind{EffectStatus, EffectStart, EffectNotice}},
	{From: phase.Reviewing, Trigger: TriggerReviewRejected, Guard: allow, To: phase.Implementing, Status: "in_progress", Effects: []EffectKind{EffectStatus, EffectStart, EffectNotice}},
	{From: phase.Retro, Trigger: TriggerRetroCompleted, Guard: allow, To: phase.Merging, Effects: []EffectKind{EffectStart, EffectNotice}},
	// A head that conflicts with its base goes back to implementing from every phase after the
	// implementer's own (transition). It is one trigger for all of them so that the quiesce
	// (takenOver) and the architect's notice (noticeFor) have a single name to key on, and so that
	// the architect's own backward move stays the separate thing it is.
	{From: phase.Testing, Trigger: TriggerConflict, Guard: allow, To: phase.Implementing, Status: "in_progress", Effects: []EffectKind{EffectStatus, EffectStart, EffectNotice}},
	{From: phase.Reviewing, Trigger: TriggerConflict, Guard: allow, To: phase.Implementing, Status: "in_progress", Effects: []EffectKind{EffectStatus, EffectStart, EffectNotice}},
	{From: phase.Accepting, Trigger: TriggerConflict, Guard: allow, To: phase.Implementing, Status: "in_progress", Effects: []EffectKind{EffectStatus, EffectStart, EffectNotice}},
	{From: phase.Retro, Trigger: TriggerConflict, Guard: allow, To: phase.Implementing, Status: "in_progress", Effects: []EffectKind{EffectStatus, EffectStart, EffectNotice}},
	{From: phase.Merging, Trigger: TriggerConflict, Guard: allow, To: phase.Implementing, Status: "in_progress", Effects: []EffectKind{EffectStatus, EffectStart, EffectNotice}},
	// The merger merges, and the merge is what ends merging: the daemon submits none and reads
	// GitHub's merge of the pull request as the phase's own terminating fact.
	{From: phase.Merging, Trigger: TriggerPullRequestMerged, Guard: allow, To: phase.Done, Status: "done", Effects: []EffectKind{EffectStatus, EffectSuspend, EffectNotice, EffectLinger}},
}, backwardRows()...)

func backwardRows() []Row {
	ordered := []phase.Phase{phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing, phase.Accepting, phase.Retro, phase.Merging}
	rows := make([]Row, 0, len(ordered)*(len(ordered)-1)/2)
	for fromIndex, from := range ordered {
		for _, to := range ordered[:fromIndex] {
			rows = append(rows, Row{From: from, Trigger: TriggerBackward, Guard: allow, To: to, Status: statusForBackward(to),
				Effects: []EffectKind{EffectStatus, EffectStart, EffectNotice}})
		}
	}
	return rows
}

func statusForBackward(to phase.Phase) string {
	switch to {
	case phase.Testing:
		return "testing"
	case phase.Reviewing, phase.Accepting:
		return "needs_review"
	case phase.Retro:
		return "retro"
	case phase.Implementing:
		return "in_progress"
	default:
		return ""
	}
}
