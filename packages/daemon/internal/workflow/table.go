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
	TriggerReady               TriggerKind = "ready"
	TriggerPullRequestMerged   TriggerKind = "pull_request_merged"
	TriggerSignOff             TriggerKind = "sign_off"
	TriggerBackward            TriggerKind = "backward"
	TriggerChecksRed           TriggerKind = "checks_red"
)

// EffectKind identifies the durable outbox consequences selected by a row. EffectSuspend is the
// issue's close alone (Engine.leave): a phase worker stays live from its role's first assignment
// until its issue closes, so no row that only moves the issue between phases suspends anyone.
type EffectKind string

const (
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
	Effects []EffectKind
}

func allow(Snapshot) bool { return true }

// Table is the complete workflow transition table. Rows, not switch-case scheduling, define every
// phase change. Backward rows exist for every allowed target so coverage detects a deleted edge.
// No row carries a status of its own: every status a row writes is its target phase's, which
// PhaseStatus holds.
var Table = append([]Row{
	{From: phase.Admitted, Trigger: TriggerGateOpened, Guard: allow, To: phase.Planning, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Planning, Trigger: TriggerPlannerCompleted, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Implementing, Trigger: TriggerImplementationReady, Guard: func(s Snapshot) bool { return s.HasOpenOrMergedPR }, To: phase.Testing, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Testing, Trigger: TriggerTesterPassed, Guard: allow, To: phase.Reviewing, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Testing, Trigger: TriggerTesterFailed, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Reviewing, Trigger: TriggerReviewApproved, Guard: allow, To: phase.Accepting, Effects: []EffectKind{EffectNotice}},
	{From: phase.Accepting, Trigger: TriggerSignOff, Guard: allow, To: phase.Retro, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Reviewing, Trigger: TriggerReviewRejected, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Testing, Trigger: TriggerChecksRed, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Reviewing, Trigger: TriggerChecksRed, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Accepting, Trigger: TriggerChecksRed, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Retro, Trigger: TriggerChecksRed, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Merging, Trigger: TriggerChecksRed, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Retro, Trigger: TriggerRetroCompleted, Guard: allow, To: phase.Merging, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.Merging, Trigger: TriggerReady, Guard: allow, To: phase.AwaitingMerge, Effects: []EffectKind{EffectNotice}},
	{From: phase.AwaitingMerge, Trigger: TriggerChecksRed, Guard: allow, To: phase.Implementing, Effects: []EffectKind{EffectStart, EffectNotice}},
	{From: phase.AwaitingMerge, Trigger: TriggerPullRequestMerged, Guard: allow, To: phase.Done, Effects: []EffectKind{EffectSuspend, EffectNotice, EffectLinger}},
}, backwardRows()...)

// backwardRows is the edge from every phase back to each phase before it. Like every other row,
// each writes the status of the phase it enters, so a move back into planning writes in_progress,
// the status an issue shows while Legion works it, rather than leaving Dispatch showing the phase
// the issue left.
func backwardRows() []Row {
	ordered := []phase.Phase{phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing, phase.Accepting, phase.Retro, phase.Merging}
	rows := make([]Row, 0, len(ordered)*(len(ordered)-1)/2)
	for fromIndex, from := range ordered {
		for _, to := range ordered[:fromIndex] {
			rows = append(rows, Row{From: from, Trigger: TriggerBackward, Guard: allow, To: to,
				Effects: []EffectKind{EffectStart, EffectNotice}})
		}
	}
	return rows
}

// PhaseStatus is the Dispatch status an issue standing in to shows: what a row that moves an issue
// into to writes, and what the status repair writes for an issue whose status is not its phase's.
// An issue in planning or implementing is in_progress, which is where a child first shows that
// Legion works it, and merging keeps the retro that the phase before it wrote. The phases where an
// issue waits rather than works have no status of their own: the issue keeps the one it already
// shows, which is what the empty status does, since no write is enqueued for one (Engine.status).
func PhaseStatus(to phase.Phase) string {
	return phaseStatus[to]
}

// phaseStatus holds an entry for every phase of phase.All, which TestEveryPhaseHasAStatusEntry
// holds it to. A phase missing from it would write no status at all, which is a wrong status
// shown for as long as the issue stands there, not a failure anything would report.
var phaseStatus = map[phase.Phase]string{
	// The issue waits before its first phase, and a status written here would take a root waiting
	// in the admission line out of that line.
	phase.Admitted: "",
	// The root keeps the status of the phase it waits in.
	phase.AwaitingPerson: "",
	phase.Planning:       "in_progress",
	phase.Implementing:   "in_progress",
	phase.Testing:        "testing",
	phase.Reviewing:      "needs_review",
	phase.Accepting:      "needs_review",
	phase.Retro:          "retro",
	phase.Merging:        "retro",
	// The issue waits on its merge and keeps the retro it already shows; Dispatch has no status
	// for that wait.
	phase.AwaitingMerge: "",
	phase.Done:          "done",
	// The issue waits on an unfinished Dispatch issue and runs nothing.
	phase.Held: "",
}
