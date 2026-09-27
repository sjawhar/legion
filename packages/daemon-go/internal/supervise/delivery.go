package supervise

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Delivery is a claim's pending task: at most one per claim, persisted, and sent only when the
// claim is ready or idle and its connection is registered.
//
// QueuedAt is when the task was handed to the claim; DeliveredAt is the latest acknowledgement of
// its prompt (MarkedBy is the delivery id that prompt carried), and with it the daemon's answer to
// whether the agent may have read this task; ConfirmedAt is the turn that send started. An
// acknowledgement is not a delivery — Oh My Pi acknowledges before it starts a turn, and can
// accept a prompt that starts none — so only ConfirmedAt says the task arrived. A confirmed delivery stays until the turn it confirmed ends,
// because a refusal that arrives after the acknowledgement takes the confirmation back.
//
// DeliveredAt carries that second meaning because a refusal of that prompt clears it: an agent
// that refused it, in a turn of its own or with no turn at all, never read the task, and the claim
// must not attribute a completion to a run whose task nobody has read (Claim.ServingRun). A task
// acknowledged and waiting for a turn keeps the mark — the turn may be this task's, starting late
// — and so does one the wait for its turn re-queued.
//
// The id is the daemon's and travels on the prompt frame; a shim that sees the same id twice
// answers it without a second turn. The id is kept across a send that failed in transit, so a
// retry is recognised, and rotated after a prompt that was acknowledged and started no turn, so
// the retry is a new prompt rather than an echo of the acknowledged one.
//
// Phase is the issue phase the task was queued for, which is what says whether the task is still
// the work to do: it is carried here, persisted, because the id is rewritten by that rotation and
// the task text is the workflow's to write. A delivery of no phase — an operator's own, an
// architect's — is never dropped for the phase its issue reaches, and a suspension does not take
// it either: it waits the suspension out and goes on the resume (settle).
type Delivery struct {
	ID   string
	Task string
	// Generation is the issue generation this task belongs to, from the start row that queued it.
	// A start delivers to whatever process the claim already has — a live worker is not
	// relaunched — so the pane's own environment names the run it was launched for, which may be
	// an earlier one. The work a worker reports is the task it was given, and this is that task's
	// run. Zero for a task of no run: an operator's own, an architect's.
	Generation  uint64
	Phase       phase.Phase
	QueuedAt    time.Time
	DeliveredAt time.Time
	// MarkedBy is the delivery id the marking prompt carried: the prompt whose acknowledgement set
	// DeliveredAt, the read mark. A late refusal is judged against the prompt it names: one naming
	// MarkedBy says the prompt that marked the task never ran, so the mark goes (markUnread), whatever
	// the claim's state and whichever connection carried it; one naming any other prompt says nothing
	// about the mark. Which prompt set the mark is a fact recorded when it is set and kept with the
	// task, so no ordering of sends, acknowledgements, refusals, reconnects and restarts has to be
	// reasoned about: only markRead and clearReadMark write it, each together with DeliveredAt. A task
	// that was replaced may still be named by a refusal, which then has nothing to clear.
	MarkedBy    string
	ConfirmedAt time.Time
	// Interrupted says a turn of the task was running when its process died (interrupted), so the
	// task is sent behind interruptedTask from then on (message). The task text stays as the
	// workflow wrote it.
	Interrupted bool
}

// StillWorked says whether a claim in state will still work this task as the task of generation's
// run in phase p: it is that run's task for that phase, and either not yet confirmed (sent once the
// claim is ready, a task a death took back included) or confirmed in the turn the claim is working.
// A confirmed task on a claim not working is over: the claim's next decision retires it.
func (d Delivery) StillWorked(generation uint64, p phase.Phase, state ClaimState) bool {
	return d.Generation == generation && d.Phase == p && (d.ConfirmedAt.IsZero() || state == StateWorking)
}

// message is the prompt a delivery is sent as: its task, behind interruptedTask once a process
// died in a turn of it.
func (d Delivery) message() string {
	if d.Interrupted {
		return interruptedTask + d.Task
	}
	return d.Task
}

// HashBootToken is the one hash a boot token is stored and looked up by. A launch mints the token
// and puts its hash on the claim; the shim's hello and the agent's registration present the token,
// and whoever resolves them hashes it here.
func HashBootToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func pendingID(p *Delivery) string {
	if p == nil {
		return ""
	}
	return p.ID
}

// queue gives the claim the task a deliver request carries, with the phase it was queued for. An
// outbox repeats its row id after a crash before FinishOutbox; the same task and id are therefore
// accepted without changing the persisted delivery.
func (m *Machine) queue(ctx context.Context, request RequestDeliver) error {
	id, task := request.ID, request.Task
	if pending := m.claim.Pending; pending != nil {
		if id != "" && pending.ID == id && pending.Task == task {
			return nil
		}
		return &RefusedError{State: m.claim.State, Request: "deliver", Err: ErrDeliveryPending}
	}
	if id == "" {
		id = rand.Text()
	}
	d := Delivery{ID: id, Task: task, Phase: request.Phase, Generation: request.Generation, QueuedAt: m.deps.Clock.Now()}
	if err := m.deps.Store.PutDelivery(ctx, m.claim.Token, d); err != nil {
		return err
	}
	m.claim.Pending, m.unsaved = &d, false
	return nil
}

// sendPending sends the pending delivery if everything a send needs is true now: the claim is
// ready or idle, the delivery is neither confirmed nor already on its way, the issue is still in
// the phase the task was queued for, and the claim's connection is registered. A missing connection is not a
// failure — the shim's next hello sends it. A delivery an earlier daemon may have sent is sent
// again only after the agent says it is not already in a turn; if it is, that turn is taken as the
// delivery's.
func (m *Machine) sendPending(ctx context.Context) error {
	if state := m.claim.State; state != StateReady && state != StateIdle {
		return nil
	}
	p := m.claim.Pending
	if p == nil || !p.ConfirmedAt.IsZero() || m.send != nil {
		return nil
	}
	if _, awaiting := m.timers[TimerTurn]; awaiting {
		return nil
	}
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		m.log.Info("supervise: no connection; the delivery waits for the shim's hello", "delivery", p.ID)
		return nil
	}
	holds, err := m.phaseHolds(ctx, *p)
	if err != nil {
		return err
	}
	if !holds {
		m.log.Info("supervise: the issue has left the phase this task was queued for; it is dropped rather than sent",
			"delivery", p.ID, "issue", m.claim.Issue, "queuedFor", p.Phase)
		if err := m.retirePending(ctx); err != nil {
			return err
		}
	}
	// The question a restart left — whether the turn the agent may be in is the pending
	// delivery's — is asked once, here, whether or not that delivery survived the phase check.
	// Left armed with nothing pending, it would make the next task the running turn's: confirmed,
	// never prompted. Discarded, it leaves the claim ready while its agent works, and the next
	// task is prompted into a busy agent and charged for it. A turn of the agent's own moves the
	// claim to working either way; it confirms the delivery only when there is still one to
	// confirm, and a dropped task's successor waits for that turn to end.
	if m.askFirst {
		m.askFirst = false
		if m.streaming(ctx, conn) {
			m.claim.State = StateWorking
			if !holds {
				m.log.Info("supervise: the agent is in a turn of its own after its task was dropped", "claim", m.claim.Token)
				return m.persist(ctx)
			}
			m.log.Info("supervise: the agent is in a turn; confirming the delivery an earlier daemon sent", "delivery", p.ID)
			return m.confirm(ctx)
		}
	}
	if !holds {
		return nil
	}
	m.startSend(conn, *p)
	return nil
}

// phaseHolds asks whether the issue is still in the phase this task was queued for. A delivery
// that names no phase is nobody's to drop — an operator's own, an architect's — and is never put
// to the answer at all, so the exemption this package documents is this package's own rather
// than a promise every implementation has to keep. A daemon with no workflow configured supplies
// no answer, and every delivery holds.
func (m *Machine) phaseHolds(ctx context.Context, d Delivery) (bool, error) {
	if m.deps.PhaseHolds == nil || d.Phase == "" {
		return true, nil
	}
	holds, err := m.deps.PhaseHolds(ctx, m.claim.Issue, d.Phase)
	if err != nil {
		return false, fmt.Errorf("read the phase of %s: %w", m.claim.Token, err)
	}
	return holds, nil
}

// retirePending drops the pending delivery, and is the one place a delivery ends: settle calls it
// for a delivery whose life is over, and sendPending for a task the issue's phase has left behind.
// The claim keeps its state — the task is stale, not the agent.
//
// A confirmed task of a run leaves the run behind it: the worker goes on working it in whatever
// turn comes next — one an Envoy notice starts, with no delivery of its own — and its completion
// belongs to that run. Only a delivery whose turn actually ran moves it, so a confirmation taken
// back by a busy refusal never does, and only a task of a run: an operator's task carries no
// generation and leaves the run the claim is serving alone. Every retire of a confirmed task of a
// run writes the row, the second task of one run included: the write is unconditional on the
// value, not a diff.
//
// The claim goes with the delete, in the store's one transaction, and is durable there rather
// than left for the claim's next write: the wait that follows can be hours long — a tester on CI,
// a merger on a person — and a daemon that crashed between the two writes, or was restarted in
// that wait, rebuilds the machine from the store, where a run left unwritten would refuse the
// completion the worker's next turn reports.
func (m *Machine) retirePending(ctx context.Context) error {
	p := m.claim.Pending
	retired := m.stored()
	if !p.ConfirmedAt.IsZero() && p.Generation != 0 {
		retired.ServingGeneration = p.Generation
	}
	// The deaths were charged against this task, so they end with it, in the same write.
	retired.Budgets.Deaths = 0
	if err := m.deps.Store.RetireDelivery(ctx, retired, p.ID); err != nil {
		return err
	}
	m.claim, m.unsaved = retired, false
	return nil
}

// saved records the outcome of a write of the pending delivery that memory changed first, since
// the decision it records has been made whether or not the store takes it. A write that failed
// leaves the delivery unsaved, and settle writes it again, with the claim, before the next decision
// - confirm's write covers both, and a delivery written confirmed beside a claim still ready is a
// turn a restart would find over. So once the store takes writes again, a restart reads what the
// machine decided rather than what it replaced; one before that reads the older rows, as a restart
// after a crash between a decision and its write does.
func (m *Machine) saved(err error) error {
	m.unsaved = err != nil
	return err
}

// putPending writes the pending delivery as memory holds it (saved).
func (m *Machine) putPending(ctx context.Context) error {
	return m.saved(m.deps.Store.PutDelivery(ctx, m.claim.Token, *m.claim.Pending))
}

// streaming asks the agent whether a turn is in flight. An agent that cannot say is taken as not
// streaming: what the caller does next — send under the same id, or end a turn — is what the
// shim's dedupe and the next agent_start make safe.
func (m *Machine) streaming(ctx context.Context, conn runtime.Conn) bool {
	asking, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
	defer cancel()
	state, err := conn.GetState(asking)
	if err != nil {
		m.log.Warn("supervise: the agent could not say whether it is in a turn", "error", err)
		return false
	}
	return state.IsStreaming
}

// startSend runs one prompt on its own goroutine and posts its outcome back to the machine.
func (m *Machine) startSend(conn runtime.Conn, d Delivery) {
	token, generation, role, loc := m.claim.Token, m.claim.Generation, m.claim.Role, m.claim.Locator
	m.send, m.helloDuringSend = &sending{id: d.ID, generation: generation}, false
	if seq := conn.Sequence(); seq > m.sentThrough {
		m.sentThrough = seq
	}
	m.goroutines++
	go func() {
		err := m.adopt(role, loc)
		if err == nil {
			sending, cancel := context.WithTimeout(m.ctx, m.deps.Timeouts.RPC)
			err = conn.Prompt(sending, d.ID, d.message())
			cancel()
		}
		var ev Event = PromptAcked{Claim: token, Generation: generation, DeliveryID: d.ID}
		if err != nil {
			ev = PromptRefused{Claim: token, Generation: generation, DeliveryID: d.ID, Err: err}
		}
		if err := m.Handle(m.ctx, ev); err != nil {
			m.log.Error("supervise: prompt outcome", "delivery", d.ID, "error", err)
		}
		m.mu.Lock()
		m.goroutines--
		m.idle.Broadcast()
		m.mu.Unlock()
	}()
}

// adopt hands the working copy the role's App identity before a task reaches the agent, as the
// shipped daemon does before every assignment (packages/daemon/src/daemon/processes.ts:1126), so
// the task's commits are authored by the bot. A failure keeps the task from being sent.
func (m *Machine) adopt(role claim.Role, loc *runtime.Locator) error {
	if m.deps.Identity == nil {
		return nil
	}
	if loc == nil {
		return fmt.Errorf("adopt the working copy of %s: the claim has no locator", m.claim.Token)
	}
	id, err := m.deps.Identity(m.ctx, role)
	if err != nil {
		return fmt.Errorf("the git identity of %s: %w", m.claim.Token, err)
	}
	return m.deps.Runtime.AdoptWorkingCopy(m.ctx, *loc, id)
}

// confirm records that the pending delivery's turn started: the claim and the delivery together,
// because either half alone is a claim a restart reads wrongly — a working claim whose delivery
// is unconfirmed is re-sent under the same id and answered by the shim without a second turn, so
// its worker is never prompted again, and an idle claim holding a confirmed delivery holds one
// nothing retires.
func (m *Machine) confirm(ctx context.Context) error {
	p := m.claim.Pending
	p.ConfirmedAt = m.deps.Clock.Now()
	m.disarm(TimerTurn)
	m.askFirst = false
	m.claim.Budgets.PromptFailures, m.claim.Budgets.PromptRetires = 0, 0
	return m.saved(m.deps.Store.PutClaimAndDelivery(ctx, m.stored(), *p))
}

// settle retires a delivery whose life is over, and runs around every decision, so that two things
// hold however the claim got where it is — the turn ending, a suspension — and hold again at once
// for a claim restored from a store a crash left in between: a confirmed delivery lives exactly as
// long as its turn, and a suspended claim holds no task the suspension finished. A death is not
// one of them: it takes the task whose turn it ended back first (interrupted), so settle finds
// nothing of it to retire.
//
// A suspension ends the claim's phase, so a task queued for a phase is the finished phase's and
// goes with it; the next resume is handed its new phase's task, never that one. A task of no
// phase is not the workflow's to end: an operator asked for it and was answered 200, and a phase
// change they had no part in must not take it. It waits out the suspension and goes on the resume.
func (m *Machine) settle(ctx context.Context) error {
	p := m.claim.Pending
	if p == nil {
		return nil
	}
	if m.unsaved {
		if err := m.saved(m.deps.Store.PutClaimAndDelivery(ctx, m.stored(), *p)); err != nil {
			m.log.Warn("supervise: the pending delivery is still not written", "delivery", p.ID, "error", err)
		}
	}
	turnOver := !p.ConfirmedAt.IsZero() && m.claim.State != StateWorking
	suspended := m.claim.State == StateSuspended && p.Phase != ""
	if !turnOver && !suspended {
		return nil
	}
	return m.retirePending(ctx)
}

// promptFailed is a prompt that was acknowledged and then came to nothing: the wait for its turn
// ran out, or Oh My Pi answered it a failure with no turn of its own running. The task goes back
// to waiting and the prompt is charged, so an agent that never takes one walks its budget to the
// relaunch and the retirement. read says whether the agent may have read it: the bound's caller
// passes true, since the turn that has not started may still be this task's, and a refusal passes
// false, since a prompt the agent answered a failure never ran.
func (m *Machine) promptFailed(ctx context.Context, why string, read bool) error {
	if err := m.takeBackPending(ctx, read); err != nil {
		return err
	}
	return m.chargePrompt(ctx, why)
}

// interruptedTask begins a task re-sent because the process running its turn died. Oh My Pi does
// not continue an interrupted turn when its session is resumed, and the resumed session already
// holds the task, so the agent is told to carry on from where the turn stopped, not start over.
const interruptedTask = "Your previous turn on this task was interrupted when your process died. " +
	"Before repeating anything, check what that turn already did in your workspace and on the " +
	"issue's branch, then continue the task.\n\n"

// interrupted takes back the pending task whose turn the claim's dead process was running, so the
// relaunched agent's ready sends it again: Oh My Pi does not resume the turn itself. The turn ran,
// so the agent read the task and it keeps its read mark; it goes back unconfirmed under a new id
// the new process's shim has no record of, marked Interrupted, so it is sent behind
// interruptedTask. It is neither retired nor its run marked served, since the turn never
// finished. A task with no turn running is left as it is: the relaunch sends it as it was.
//
// The taken-back task becomes the claim's only once its write has landed. A write that fails
// leaves the task confirmed in memory as in the store, and the process is still recorded, so the
// sweep finds it gone again and this takes the task back then — where a task taken back in memory
// alone would be skipped by that death, relaunched from a store still holding it confirmed, and
// retired as served by a restart.
func (m *Machine) interrupted(ctx context.Context) error {
	p := m.claim.Pending
	if p == nil || p.ConfirmedAt.IsZero() {
		return nil
	}
	m.log.Warn("supervise: the process died in the task's turn; the task waits for the relaunch", "delivery", p.ID)
	next := *p
	next.ID, next.ConfirmedAt, next.Interrupted = rand.Text(), time.Time{}, true
	if err := m.deps.Store.PutDelivery(ctx, m.claim.Token, next); err != nil {
		return err
	}
	*p = next
	return nil
}

// taskRead and taskUnread say whether the agent may have read a task being taken back: the task
// keeps its delivered mark when it may have been read, which is what lets a completion reported
// from a turn the daemon did not deliver be attributed to it.
const (
	taskRead   = true
	taskUnread = false
)

// markRead and clearReadMark are the only writers of the pending task's read mark: its
// acknowledgement time (DeliveredAt) and the delivery id the acknowledged prompt carried
// (MarkedBy) are set together and cleared together, so the prompt a late refusal is judged against
// is always the one whose acknowledgement set the mark standing now.
func (m *Machine) markRead(p *Delivery) {
	p.DeliveredAt, p.MarkedBy = m.deps.Clock.Now(), p.ID
}

func (m *Machine) clearReadMark(p *Delivery) {
	p.DeliveredAt, p.MarkedBy = time.Time{}, ""
}

// markUnread drops the pending task's read mark without touching its id or its budget: an
// outcome that arrived for a send this machine had already given up on says only that the agent
// never read the task.
func (m *Machine) markUnread(ctx context.Context) error {
	p := m.claim.Pending
	marked := !p.DeliveredAt.IsZero()
	m.clearReadMark(p)
	if !marked {
		return nil
	}
	return m.putPending(ctx)
}

// takeBackPending returns the pending delivery to waiting after a prompt came to nothing:
// unconfirmed if a turn had confirmed it, under a new id so the retry is a new prompt rather than
// an echo the shim answers from its record, and with the wait for its turn disarmed. A task taken
// back unread loses the mark, so nothing the worker reports from whatever turn follows belongs to
// it.
//
// The refusal or timeout behind it arrives once, and settle and ServingRun read the claim as memory
// holds it, so what it established — not confirmed, and not read when taken back unread — holds at
// once, whether or not the write recording it lands. Only the new id waits for the write, so the
// claim's id is always one the store holds; a write that fails leaves the delivery unsaved, and
// settle writes it again under the id it kept (saved).
func (m *Machine) takeBackPending(ctx context.Context, read bool) error {
	m.disarm(TimerTurn)
	p := m.claim.Pending
	p.ConfirmedAt = time.Time{}
	if !read {
		m.clearReadMark(p)
	}
	next := *p
	next.ID = rand.Text()
	if err := m.saved(m.deps.Store.PutDelivery(ctx, m.claim.Token, next)); err != nil {
		return err
	}
	p.ID = next.ID
	return nil
}
