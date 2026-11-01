package supervise

import (
	"context"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/wait"
)

// ErrSuspendHeld answers a suspension that arrived while the claim's agent is in a turn: the
// machine holds it and suspends the claim itself (holdSuspension). It is not a refusal. A caller
// that keeps the request durably asks again, and the claim answers nil once it is suspended.
var ErrSuspendHeld = wait.New("the suspension is held for the agent's turn to end")

// suspend stops the process and keeps the session. A suspension ends the claim's phase (the
// workflow suspends a worker only when its issue closes or its run is over), so a task
// queued for a phase and still pending unconfirmed (acknowledged and then refused, or lost to the
// transport) is retired with it (settle): the next resume is started with its new phase's task,
// never handed the finished one's. A task of no phase — an operator's own, an architect's — is
// not the workflow's to end, and goes on that resume.
func suspend(m *Machine, ctx context.Context, ev Event) error {
	request, err := suspendRequest(m, ev)
	if err != nil {
		return err
	}
	return m.suspendNow(ctx, request)
}

// holdSuspension is a suspension that arrives while the agent is in a turn, and this is the one
// statement of what a held suspension (Machine.held) does.
//
// A worker's issue can close, or an operator suspend it, while it is in a turn: the production
// check's sign-off, say, arriving while the implementer's report of the check is still a tool call
// inside that turn, or an architect's own sign-off moving its tree into linger. Stopping the
// process at once would cut that call off before Oh My Pi writes its result, and a session resumed
// from that transcript holds a report with no answer. So the suspension is held:
//
//   - It runs when the turn ends (turnEnded), or at its deadline if the turn has not ended by then
//     (TimerSuspend, suspendAtTimeout). The operator's suspension has one at the stop timeout from
//     its arrival: an operator stopping the process — to drain the daemon, say — needs the stop
//     bounded. The daemon's own suspension (its issue left the workflow, or its tree lingers) has
//     none unless it names one (RequestSuspend.Deadline), and waits for the turn however long it
//     takes: what bounds it is what ends the process anyway, the tree's close when its linger expires
//     or the child's close as done. No close reaches a carried child's role still running in its old
//     tree, since the child has left that tree, so the daemon names that suspension the deadline the
//     old tree's close would have had, the old root's linger expiry. Asked again meanwhile, it is the
//     same hold, under one rule: a request can only move the deadline earlier, and the operator's
//     also takes the hold over, from then on the operator's. Until the deadline the agent keeps its
//     process and its capability.
//   - A stop the runtime refuses keeps it held and tries again every probe interval (suspendHeld),
//     so the deadline bounds the wait only while the runtime can stop the process. A claim
//     whose turn ended stays idle meanwhile and is handed nothing (sendPending); a turn it starts
//     anyway is cut off at the next try.
//   - A process that dies first, or is found at a stale address (repoint), leaves the claim
//     suspended, charged nothing (endHeld). A prompt retirement meanwhile is the held stop itself
//     (suspendHeld), charged nothing.
//   - A start run against the claim drops a daemon's hold (StartedBy); one the operator's
//     suspension holds waits instead. Every other end of the process drops it with the process
//     (letGo).
//   - It lives in memory only. A restart forgets it, so whoever holds the request asks again: the
//     outbox retries its suspend row until the claim answers nil. A restart starts the operator's
//     stop timeout over; a deadline the daemon names is a time, so it holds as named.
//   - It is the one thing that lets a row end in suspended without naming that state (Handle).
//
// It answers ErrSuspendHeld.
func holdSuspension(m *Machine, _ context.Context, ev Event) error {
	request, err := suspendRequest(m, ev)
	if err != nil {
		return err
	}
	// The deadline is the asker's own: suspendRequest hands a daemon's request under the operator's
	// hold the operator's source, which must not give it the operator's stop timeout too.
	deadline := ev.(RequestSuspend).Deadline
	if ev.(RequestSuspend).By == SuspendedByOperator {
		deadline = m.deps.Clock.Now().Add(m.deps.Timeouts.Stop)
	}
	switch {
	case m.held == nil:
		m.held = &RequestSuspend{Claim: request.Claim, Reason: request.Reason, By: request.By}
		m.log.Info("supervise: the suspension is held for the agent's turn to end", "reason", request.Reason, "by", request.By)
	case request.By == SuspendedByOperator && m.held.By != SuspendedByOperator:
		m.log.Info("supervise: the operator's suspension takes the held suspension over", "reason", request.Reason, "held", m.held.Reason)
		m.held.By, m.held.Reason = request.By, request.Reason
	}
	// The held deadline only moves earlier, and the timer is re-armed only when that deadline comes
	// before the one already armed: a stop the runtime refused is retried at the probe interval
	// (suspendHeld), and a deadline later than that retry must not push it back.
	if !deadline.IsZero() && (m.held.Deadline.IsZero() || deadline.Before(m.held.Deadline)) {
		m.held.Deadline = deadline
		if a, ok := m.timers[TimerSuspend]; !ok || deadline.Before(a.due) {
			m.arm(TimerSuspend, max(0, deadline.Sub(m.deps.Clock.Now())), "")
			m.log.Info("supervise: the held suspension stops the agent at its deadline if its turn has not ended", "deadline", deadline, "by", m.held.By)
		}
	}
	return ErrSuspendHeld
}

// holdSuspendUntilReady retains a suspend that reaches a rule-resuming launch after the rule
// changed the claim to launching. The old table ignored it, returning a conflict to an operator and
// dropping a daemon's linger or leave row. The hold survives launch and is run as soon as the agent
// says ready (ready), before the claim is published ready or takes a task. It answers
// ErrSuspendHeld so the operator sees 202 and a daemon row retries across a restart.
func holdSuspendUntilReady(m *Machine, _ context.Context, ev Event) error {
	request, err := suspendRequest(m, ev)
	if err != nil {
		return err
	}
	m.held = &request
	m.log.Info("supervise: the suspension waits for the rule-resuming agent to become ready", "reason", request.Reason, "by", request.By)
	return ErrSuspendHeld
}

// suspendedAgain is a suspension of a claim already suspended. An operator's suspend of a claim the
// daemon suspended takes the suspension over (Claim.SuspendedBy), so the rule resumes it no more
// until the operator does; any other changes nothing, and an operator's suspension stays the
// operator's.
func suspendedAgain(m *Machine, ctx context.Context, ev Event) error {
	request, err := suspendRequest(m, ev)
	if err != nil {
		return err
	}
	if request.By != SuspendedByOperator || m.claim.SuspendedBy == SuspendedByOperator {
		return nil
	}
	m.log.Info("supervise: the operator's suspension takes the suspended claim over", "reason", request.Reason, "suspendedBy", m.claim.SuspendedBy)
	m.claim.SuspendedBy = SuspendedByOperator
	return m.persist(ctx)
}

// suspendAtTimeout is the held suspension's timer: the turn did not end by the hold's deadline, so
// the process is stopped mid-turn; or an earlier stop failed, and it is tried again.
//
// A stop forced mid-turn is not the turn's end: the confirmed task did not finish, so it goes back
// interrupted, as a death in a turn takes it back. The task is taken back before the stop, as died
// does: a take-back that fails stops nothing and is tried again at the probe interval, and a crash
// between the two leaves a running claim holding an interrupted task, never a suspended claim holding
// a confirmed one, which settle would retire as a finished turn. What happens to it then is the
// suspension's: the operator's resume re-sends it to the same owner, and a daemon's suspension retires
// a phase's task with the phase (settle), so the next start hands the session its phase's resume task;
// the workspace, the issue's branch and the session are kept either way.
func suspendAtTimeout(m *Machine, ctx context.Context, _ Event) error {
	if m.held != nil && m.claim.State == StateWorking {
		m.log.Warn("supervise: the agent's turn has not ended; suspending it mid-turn", "by", m.held.By, "deadline", m.held.Deadline)
		if err := m.interrupted(ctx, "the held suspension's deadline ended the task's turn"); err != nil {
			m.arm(TimerSuspend, m.deps.Timeouts.Probe, "")
			return fmt.Errorf("take back the task of %s before its forced stop: %w", m.claim.Token, err)
		}
	} else {
		m.log.Info("supervise: trying the held suspension's stop again")
	}
	return m.suspendHeld(ctx)
}

// suspendRequest is the event a suspension's row is handed. The table routes only RequestSuspend
// there (kindOf's onSuspend); another event is a wiring error, and so is a suspension that names
// neither the operator nor the daemon (a root's exit suspends it through RequestExit, never this),
// since the source is what decides whether the daemon may resume the claim. Either is refused
// before anything is stopped, rather than a panic, a journal line with no reason, or a claim
// suspended by nobody.
func suspendRequest(m *Machine, ev Event) (RequestSuspend, error) {
	request, ok := ev.(RequestSuspend)
	if !ok {
		return RequestSuspend{}, fmt.Errorf("suspend %s: the suspend action was handed %T, not a RequestSuspend", m.claim.Token, ev)
	}
	if request.By != SuspendedByOperator && request.By != SuspendedByDaemon {
		return RequestSuspend{}, fmt.Errorf("suspend %s: the suspension names source %q, not operator or daemon", m.claim.Token, request.By)
	}
	// An operator's held suspension wins everywhere — launching, registered, ready while its
	// stop retries, and working — until it completes. A later daemon linger/leave request must
	// use that same source rather than changing a deliberate operator hold into a rule-resumable
	// daemon suspension.
	if m.held != nil && m.held.By == SuspendedByOperator && request.By != SuspendedByOperator {
		return *m.held, nil
	}
	return request, nil
}

// suspendNow stops the claim's process for request and keeps its session.
func (m *Machine) suspendNow(ctx context.Context, request RequestSuspend) error {
	if err := m.endProcess(ctx); err != nil {
		return fmt.Errorf("suspend %s: %w", m.claim.Token, err)
	}
	return m.suspendedFor(ctx, request)
}

// suspendedFor moves the claim to suspended once request's stop has ended its process, and says
// why.
func (m *Machine) suspendedFor(ctx context.Context, request RequestSuspend) error {
	if err := m.suspended(ctx, request.By); err != nil {
		return err
	}
	m.log.Info("supervise: suspended", "reason", request.Reason, "by", request.By)
	return nil
}

// suspended moves the claim to suspended by by: its session kept, and the process it stopped let
// go for the resume to wait out. It only persists — revoking the stopped agent's capability, in
// memory even when the write fails. The finished phase's unconfirmed task is retired after it by
// settle, which Handle runs after every row; a retirement that fails is reported, and the claim's
// next decision retires it before anything else.
func (m *Machine) suspended(ctx context.Context, by SuspendSource) error {
	m.letGo()
	m.claim.State, m.claim.SuspendedBy = StateSuspended, by
	return m.persist(ctx)
}

// suspendHeld runs the held suspension's stop. One the runtime refuses stays held and is tried
// again at the probe interval, as the registration deadline retries its own: the operator's
// suspend, answered 202, has nothing else to retry it. A stop that landed has let the process go
// and the hold with it (letGo), whether or not its write then failed; nothing is retried then, and
// m.held is nil — which is how a caller tells the two apart.
func (m *Machine) suspendHeld(ctx context.Context) error {
	request := m.held
	if request == nil {
		return fmt.Errorf("supervise: no suspension of %s is held", m.claim.Token)
	}
	if err := m.suspendNow(ctx, *request); err != nil {
		if m.held != nil {
			m.arm(TimerSuspend, m.deps.Timeouts.Probe, "")
		}
		return err
	}
	return nil
}

// dropHeld ends the held suspension without running it.
func (m *Machine) dropHeld() {
	m.held = nil
	m.disarm(TimerSuspend)
}

// endHeld is the claim's process found dead, or alive holding an address a process launched now is
// not handed (repoint), while a suspension is held. Nothing is left to hold the suspension for, so
// the claim is suspended rather than relaunched, and nothing is charged: died asks before it charges
// anything. A runtime that cannot stop the process is logged and the claim is suspended all the
// same, as a failed claim is (fail).
func (m *Machine) endHeld(ctx context.Context) error {
	request := *m.held
	m.dropHeld()
	incarnation := m.claim.Locator.Incarnation
	if err := m.endProcess(ctx); err != nil {
		m.log.Error("supervise: could not stop the process with the runtime; suspending its claim anyway",
			"incarnation", incarnation, "error", err)
	}
	return m.suspendedFor(ctx, request)
}
