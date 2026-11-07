package supervise

import (
	"context"
	"time"
)

// LastStep is the moment the claim's agent was last known to be moving: the last step the daemon
// received, or the turn's own start where the agent has sent no step since that turn began. It is
// the zero time for a claim whose record holds neither, which is a claim nothing may be judged
// from — the daemon reads this record and guesses nothing (INFRA-1274's backstop).
//
// The two are compared rather than the call's own times read, because the record is never
// cleared: after a recovery the call it names is the one that was open when the agent stopped, so
// a turn begun since is younger than the step before it, and the later of the two is what says
// when this claim last moved.
func (s StepRecord) LastStep() time.Time {
	if s.At.Before(s.TurnStartedAt) {
		return s.TurnStartedAt
	}
	return s.At
}

// backstopStalledTurn is the daemon's stall backstop reaching a claim whose recorded last step is
// older than its role's bound (StallBackstop, the daemon's backstopStalledTurns).
//
// It re-reads the claim's own record first and acts only while that record is still older than
// the deadline the request carries, because the sweep read the claims before this decision was
// taken: a frame that arrived in between is the agent moving, and its turn is left alone. It
// reads two things and takes the later: the record the claim carries, which is the store's as
// this machine was built, and the steps this machine has read since (steppedAt, putStep), since
// a frame the stream delivered after the sweep's read is already in this machine's inbox, ahead
// of this event. That re-reading is the fence refreshPlugin makes for the same reason, and the
// one thing that decides it is the record, never a guess of the daemon's.
//
// What it does then is exactly what a daemon-made replacement does (replaceProcess): the turn in
// flight is interrupted, so the task it was serving goes back to waiting and the relaunched agent
// is sent it again; a held suspension ends in a suspension (endHeld); and otherwise the recorded
// session is resumed on a new process. Nothing is charged — not a launch failure, and not a death
// with work outstanding (chargeDeath) — which is the whole reason this path is replaceProcess and
// not the death path: the worker's own ladder already ends its process twice before the daemon is
// reached (the plugin's stall watch), so a stall that repeats would spend the claim's death
// budget and fail the claim at launch_failure_limit, where the requirement is that the claim come
// back with its task still owed.
//
// It never waits for the turn to end. A suspension that arrives mid-turn is held for that turn
// and bounded by a deadline (holdSuspension, whose deadline only moves earlier); this request
// carries its deadline already passed — the recorded last step plus the role's bound — because
// the turn it exists for is one that never ends, and a request held for it would wait for ever.
func (m *Machine) backstopStalledTurn(ctx context.Context, ev StallBackstop) error {
	step := m.claim.Step
	last := step.LastStep()
	if m.steppedAt.After(last) {
		last = m.steppedAt
	}
	if last.IsZero() || !last.Add(ev.Bound).Before(m.deps.Clock.Now()) {
		m.log.Info("supervise: the stall backstop found the claim's record inside its bound; the turn is left alone",
			"bound", ev.Bound, "deadline", ev.Deadline, "lastStep", last, "tool", step.Tool, "callId", step.CallID)
		return nil
	}
	m.log.Warn("supervise: the daemon's stall backstop ends a turn whose agent stopped stepping; the task stays owed and the session comes back",
		"incarnation", m.claim.Locator.Incarnation, "bound", ev.Bound, "deadline", ev.Deadline,
		"lastStep", last, "turnStartedAt", step.TurnStartedAt, "tool", step.Tool, "callId", step.CallID,
		"callStartedAt", step.StartedAt)
	return m.replaceProcess(ctx, "the daemon's stall backstop ended a turn whose agent stopped stepping", ordinaryEnd)
}

// backstopStall is the StallBackstop event's row, for the one state a claim is in a turn in.
func backstopStall(m *Machine, ctx context.Context, ev Event) error {
	return m.backstopStalledTurn(ctx, ev.(StallBackstop))
}
