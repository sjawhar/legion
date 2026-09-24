package supervise

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Delivery is a claim's pending task: at most one per claim, persisted, and sent only when the
// claim is ready or idle and its connection is registered.
//
// QueuedAt is when the task was handed to the claim; DeliveredAt is the acknowledgement of the
// latest send; ConfirmedAt is the turn that send started. An acknowledgement is not a delivery —
// Oh My Pi acknowledges before it starts a turn, and can accept a prompt that starts none — so
// only ConfirmedAt says the task arrived. A confirmed delivery stays until the turn it confirmed
// ends, because a refusal that arrives after the acknowledgement takes the confirmation back.
//
// The id is the daemon's and travels on the prompt frame; a shim that sees the same id twice
// answers it without a second turn. The id is kept across a send that failed in transit, so a
// retry is recognised, and rotated after a prompt that was acknowledged and started no turn, so
// the retry is a new prompt rather than an echo of the acknowledged one.
type Delivery struct {
	ID          string
	Task        string
	QueuedAt    time.Time
	DeliveredAt time.Time
	ConfirmedAt time.Time
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

// queue gives the claim a task. An outbox repeats its row id after a crash before FinishOutbox;
// the same task and id are therefore accepted without changing the persisted delivery.
func (m *Machine) queue(ctx context.Context, task, id string) error {
	if pending := m.claim.Pending; pending != nil {
		if id != "" && pending.ID == id && pending.Task == task {
			return nil
		}
		return &RefusedError{State: m.claim.State, Request: "deliver", Reason: "a delivery is already pending"}
	}
	if id == "" {
		id = rand.Text()
	}
	d := Delivery{ID: id, Task: task, QueuedAt: m.deps.Clock.Now()}
	if err := m.deps.Store.PutDelivery(ctx, m.claim.Token, d); err != nil {
		return err
	}
	m.claim.Pending = &d
	return nil
}

// sendPending sends the pending delivery if everything a send needs is true now: the claim is
// ready or idle, the delivery is neither confirmed nor already on its way, and the claim's
// connection is registered. A missing connection is not a failure — the shim's next hello sends
// it. A delivery an earlier daemon may have sent is sent again only after the agent says it is
// not already in a turn; if it is, that turn is taken as the delivery's.
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
	if m.askFirst {
		m.askFirst = false
		if m.streaming(ctx, conn) {
			m.log.Info("supervise: the agent is in a turn; confirming the delivery an earlier daemon sent", "delivery", p.ID)
			m.claim.State = StateWorking
			return m.confirm(ctx)
		}
	}
	m.startSend(conn, *p)
	return nil
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
	m.goroutines++
	go func() {
		err := m.adopt(role, loc)
		if err == nil {
			sending, cancel := context.WithTimeout(m.ctx, m.deps.Timeouts.RPC)
			err = conn.Prompt(sending, d.ID, d.Task)
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

// confirm records that the pending delivery's turn started: the claim first, so that a crash
// between the two writes leaves a working claim with an unconfirmed delivery — which is sent again
// under the same id when the turn ends, and answered by the shim without a second turn — rather
// than an idle claim holding a confirmed one that nothing would retire.
func (m *Machine) confirm(ctx context.Context) error {
	p := m.claim.Pending
	p.ConfirmedAt = m.deps.Clock.Now()
	m.disarm(TimerTurn)
	m.askFirst = false
	m.claim.Budgets.PromptFailures, m.claim.Budgets.PromptRetires = 0, 0
	if err := m.persist(ctx); err != nil {
		return err
	}
	return m.deps.Store.PutDelivery(ctx, m.claim.Token, *p)
}

// settle retires a confirmed delivery once the turn it confirmed is no longer in flight. It runs
// around every decision, so that "a confirmed delivery lives exactly as long as its turn" holds
// however the claim left working — the turn ending, a suspension, a death — and holds again at
// once for a claim restored from a store a crash left in between.
func (m *Machine) settle(ctx context.Context) error {
	p := m.claim.Pending
	if p == nil || p.ConfirmedAt.IsZero() || m.claim.State == StateWorking {
		return nil
	}
	if err := m.deps.Store.RetireDelivery(ctx, m.claim.Token, p.ID); err != nil {
		return err
	}
	m.claim.Pending = nil
	return nil
}

// promptFailed is an acknowledged prompt that did not become the delivery's turn: no turn within
// the bound, or a refusal after the acknowledgement. The delivery is taken back — unconfirmed if a
// turn had confirmed it, under a new id so that the retry is a new prompt rather than an echo the
// shim answers from its record — and one prompt failure is charged. The retry is not made on the
// spot, where a merely slow agent would be prompted into a refusal: the next sweep, hello, turn
// end, or ready sends it.
func (m *Machine) promptFailed(ctx context.Context, why string) error {
	m.disarm(TimerTurn)
	p := m.claim.Pending
	p.ID = rand.Text()
	p.ConfirmedAt = time.Time{}
	if err := m.deps.Store.PutDelivery(ctx, m.claim.Token, *p); err != nil {
		return err
	}
	return m.chargePrompt(ctx, why)
}
