package supervise

import (
	"context"
	"errors"
	"fmt"
)

// ErrQuiesceHeld answers a start that takes over the issue's phase while the claim whose phase it
// was may still be in a turn (Quiesce). It is a wait, not a refusal: the caller keeps the start and
// asks again, and the claim answers nil once that turn is over.
var ErrQuiesceHeld = errors.New("the start waits for the outgoing worker's interrupted turn to end")

// interrupt is the turn Quiesce interrupts: the newest start that asked, the launch generation it
// asked of, and whether the agent has taken the abort.
type interrupt struct {
	row        int64
	generation uint64
	sent       bool
}

// Quiesce is asked by a start, outbox row row, that takes over this claim's issue's phase although
// this claim never completed it: CI settled red while the tester tested, or while the reviewer
// reviewed a round it had not completed, and the issue is back in implementing (workflow's
// TriggerChecksRed, record.SuperviseRequest's Quiesce). A completion ends a worker's writes before
// the next role starts; nothing does here, so the start acts only once this claim is out of its turn,
// and its worker never writes the shared workspace beside the role the start hands it to. The claim
// keeps its process and its session.
//
// It answers nil when the claim is out of a turn: not working, and with no prompt on its way to one
// (a send, an acknowledged prompt awaiting its turn, or a turn a restored claim has yet to ask the
// agent about). Otherwise it answers ErrQuiesceHeld, and:
//
//   - A turn running is interrupted: the agent is asked to abort it (runtime.Conn.Abort), which
//     cancels its tool calls and keeps the process. The abort's answer is not the turn's end; the
//     agent_end that follows it is (turnEnded). An abort that fails is sent again at the next ask;
//     one the agent took is not, and the start waits for that turn's end however long it takes.
//   - A prompt on its way is left to become a turn, interrupted at the next ask, or to fail.
//   - Once a start has found the claim out of a turn, it is answered nil from then on, so a later
//     turn — a question another role asks through Envoy, which the finished role answers read-only —
//     is never interrupted for it. The process ending (letGo) is the interrupted turn over too.
//
// The interrupt and the starts it answered live in memory only. A restarted daemon asks again, and a
// claim that is in a turn then has that turn interrupted.
func (m *Machine) Quiesce(ctx context.Context, row int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row <= m.quiesced {
		return nil
	}
	_, awaiting := m.timers[TimerTurn]
	working := m.claim.State == StateWorking
	if !working && m.send == nil && !awaiting && !m.askFirst {
		m.quiesced, m.interrupt = row, nil
		return nil
	}
	i := m.interrupt
	if i == nil {
		i = &interrupt{generation: m.claim.Generation}
		m.interrupt = i
	}
	i.row = max(i.row, row)
	if !working || i.sent {
		return ErrQuiesceHeld
	}
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		return fmt.Errorf("interrupt the turn of %s: it has no connection: %w", m.claim.Token, ErrQuiesceHeld)
	}
	asking, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
	defer cancel()
	if err := conn.Abort(asking); err != nil {
		return fmt.Errorf("interrupt the turn of %s: %w", m.claim.Token, err)
	}
	i.sent = true
	m.log.Info("supervise: interrupted the agent's turn: a start takes over its issue's phase", "row", i.row, "generation", i.generation)
	return ErrQuiesceHeld
}

// interruptOver is the interrupted turn over — it ended, or the process did — so every start that
// waited for it goes on (Quiesce).
func (m *Machine) interruptOver() {
	i := m.interrupt
	if i == nil {
		return
	}
	m.quiesced, m.interrupt = max(m.quiesced, i.row), nil
	m.log.Info("supervise: the interrupted turn is over", "row", i.row, "generation", i.generation)
}
