package supervise

import (
	"context"
	"errors"
	"fmt"
)

// ErrSuspendHeld answers a suspension that arrived while the claim's agent is in a turn: the
// machine holds it and suspends the claim itself (holdSuspension). It is not a refusal. A caller
// that keeps the request durably asks again, and the claim answers nil once it is suspended.
var ErrSuspendHeld = errors.New("the suspension is held for the agent's turn to end")

// suspend stops the process and keeps the session. A suspension ends the claim's phase, so a task
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
// The workflow suspends a worker as it records the phase completion the worker reports from a tool
// call inside its turn. Stopping the process at once would cut that call off before Oh My Pi writes
// its result, and a session resumed from that transcript holds a report with no answer. So the
// suspension is held:
//
//   - It runs when the turn ends (turnEnded), or when the stop timeout runs out first
//     (TimerSuspend, suspendAtTimeout). Asked again meanwhile, it is the same hold, its timeout
//     unchanged. Until it runs the agent keeps its process and its capability.
//   - A stop the runtime refuses keeps it held and tries again every probe interval (suspendHeld),
//     so the stop timeout bounds the wait only while the runtime can stop the process. A claim
//     whose turn ended stays idle meanwhile and is handed nothing (sendPending); a turn it starts
//     anyway is cut off at the next try.
//   - A process that dies first leaves the claim suspended, charged nothing (endHeld). A prompt
//     retirement meanwhile is the held stop itself (suspendHeld), charged nothing.
//   - A start run against the claim drops it (StartedBy), and every other end of the process drops
//     it with the process (letGo).
//   - It lives in memory only. A restart forgets it, so whoever holds the request asks again: the
//     outbox retries its suspend row until the claim answers nil, and a restart starts the stop
//     timeout over.
//   - It is the one thing that lets a row end in suspended without naming that state (Handle).
//
// It answers ErrSuspendHeld.
func holdSuspension(m *Machine, _ context.Context, ev Event) error {
	request, err := suspendRequest(m, ev)
	if err != nil {
		return err
	}
	if m.held == nil {
		m.held = &request
		m.arm(TimerSuspend, m.deps.Timeouts.Stop, "")
		m.log.Info("supervise: the suspension is held for the agent's turn to end", "reason", request.Reason,
			"stopTimeout", m.deps.Timeouts.Stop)
	}
	return ErrSuspendHeld
}

// suspendAtTimeout is the held suspension's timer: the turn did not end within the stop timeout,
// so the process is stopped mid-turn; or an earlier stop failed, and it is tried again.
func suspendAtTimeout(m *Machine, ctx context.Context, _ Event) error {
	if m.claim.State == StateWorking {
		m.log.Warn("supervise: the agent's turn has not ended; suspending it mid-turn", "stopTimeout", m.deps.Timeouts.Stop)
	} else {
		m.log.Info("supervise: trying the held suspension's stop again")
	}
	return m.suspendHeld(ctx)
}

// suspendRequest is the event a suspension's row is handed. The table routes only RequestSuspend
// there (kindOf's onSuspend); another event is a wiring error, refused before anything is stopped,
// rather than a panic or a journal line with no reason.
func suspendRequest(m *Machine, ev Event) (RequestSuspend, error) {
	request, ok := ev.(RequestSuspend)
	if !ok {
		return RequestSuspend{}, fmt.Errorf("suspend %s: the suspend action was handed %T, not a RequestSuspend", m.claim.Token, ev)
	}
	return request, nil
}

// suspendNow stops the claim's process for request and keeps its session.
func (m *Machine) suspendNow(ctx context.Context, request RequestSuspend) error {
	if err := m.suspendProcess(ctx); err != nil {
		return fmt.Errorf("suspend %s: %w", m.claim.Token, err)
	}
	return m.suspendedFor(ctx, request)
}

// suspendedFor moves the claim to suspended once request's stop has ended its process, and says
// why.
func (m *Machine) suspendedFor(ctx context.Context, request RequestSuspend) error {
	if err := m.suspended(ctx); err != nil {
		return err
	}
	m.log.Info("supervise: suspended", "reason", request.Reason)
	return nil
}

// suspended moves the claim to suspended: its session kept, and the process it stopped let go for
// the resume to wait out. It only persists — revoking the stopped agent's capability, in memory
// even when the write fails. The finished phase's unconfirmed task is retired after it by settle,
// which Handle runs after every row; a retirement that fails is reported, and the claim's next
// decision retires it before anything else.
func (m *Machine) suspended(ctx context.Context) error {
	m.letGo()
	m.claim.State = StateSuspended
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

// endHeld is the claim's process found dead while a suspension is held. Nothing is left to hold the
// suspension for, so the claim is suspended rather than relaunched, and nothing is charged: died
// asks before it charges anything. A runtime that cannot stop the dead process is logged and the
// claim is suspended all the same, as a failed claim is (fail).
func (m *Machine) endHeld(ctx context.Context) error {
	request := *m.held
	m.dropHeld()
	incarnation := m.claim.Locator.Incarnation
	if err := m.suspendProcess(ctx); err != nil {
		m.log.Error("supervise: could not stop the process with the runtime; suspending its claim anyway",
			"incarnation", incarnation, "error", err)
	}
	return m.suspendedFor(ctx, request)
}
