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
type Snapshot struct {
	Phase    phase.Phase
	HasPR    bool
	GateOpen bool
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

// Table is the complete workflow transition table. Rows, not switch-case scheduling, define every
// phase change. Backward rows exist for every allowed target so coverage detects a deleted edge.
var Table = append([]Row{
	{From: phase.Admitted, Trigger: TriggerGateOpened, Guard: allow, To: phase.Planning, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Planning, Trigger: TriggerPlannerCompleted, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Implementing, Trigger: TriggerImplementationReady, Guard: func(s Snapshot) bool { return s.HasPR }, To: phase.Testing, Status: "testing", Effects: []EffectKind{EffectStatus, EffectStart, EffectNotice}},
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
