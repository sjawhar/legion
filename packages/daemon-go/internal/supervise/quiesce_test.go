package supervise

import (
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A start that takes over the phase of a worker still in its turn waits for that turn: the turn is
// interrupted once, the claim keeps its process and session, and the start goes on only once the
// turn's agent_end arrives. The abort's own answer is not the end: until the turn ends, asking again
// sends no second abort and still waits.
func TestQuiesceInterruptsTheTurnOnceAndWaitsForItsEnd(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	before := h.claim()

	if err := h.m.Quiesce(h.ctx, 7); !errors.Is(err, ErrQuiesceHeld) {
		t.Fatalf("Quiesce mid-turn = %v, want ErrQuiesceHeld", err)
	}
	if err := h.m.Quiesce(h.ctx, 7); !errors.Is(err, ErrQuiesceHeld) {
		t.Fatalf("Quiesce asked again mid-turn = %v, want ErrQuiesceHeld", err)
	}
	if got := h.conn.Aborts(); got != 1 {
		t.Fatalf("aborts = %d, want one for the turn", got)
	}
	h.must(StreamTurnEnd{Claim: h.token})
	if err := h.m.Quiesce(h.ctx, 7); err != nil {
		t.Fatalf("Quiesce after the turn ended = %v, want nil", err)
	}
	after := h.claim()
	if after.State != StateIdle || after.Generation != before.Generation || after.Session != before.Session ||
		after.Locator == nil || *after.Locator != *before.Locator || len(h.calls("Suspend")) != 0 || len(h.calls("Release")) != 0 {
		t.Fatalf("after the interrupt the claim is %+v, want idle on the same launch and session, never stopped", after)
	}
}

// Once a start has found the claim out of its turn, a later turn — a question another role asks
// through Envoy, answered read-only — is never interrupted for it; a newer start is asked afresh.
func TestQuiesceNeverInterruptsALaterTurnForAStartItAnswered(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness)
	}{
		{"a claim found idle", func(h *harness) {
			if err := h.m.Quiesce(h.ctx, 7); err != nil {
				h.t.Fatalf("Quiesce of an idle claim = %v, want nil", err)
			}
		}},
		{"a turn interrupted for it", func(h *harness) {
			h.must(StreamTurnStart{Claim: h.token})
			if err := h.m.Quiesce(h.ctx, 7); !errors.Is(err, ErrQuiesceHeld) {
				h.t.Fatalf("Quiesce mid-turn = %v, want ErrQuiesceHeld", err)
			}
			h.must(StreamTurnEnd{Claim: h.token})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			tc.setup(h)
			aborts := h.conn.Aborts()
			h.must(StreamTurnStart{Claim: h.token})
			if err := h.m.Quiesce(h.ctx, 7); err != nil {
				t.Fatalf("the same start asked during a later turn = %v, want nil", err)
			}
			if err := h.m.Quiesce(h.ctx, 6); err != nil {
				t.Fatalf("an older start asked during a later turn = %v, want nil", err)
			}
			if got := h.conn.Aborts(); got != aborts {
				t.Fatalf("the later turn was interrupted: aborts %d, want %d", got, aborts)
			}
			if err := h.m.Quiesce(h.ctx, 8); !errors.Is(err, ErrQuiesceHeld) || h.conn.Aborts() != aborts+1 {
				t.Fatalf("a newer start = %v with %d aborts, want ErrQuiesceHeld and one more abort", err, h.conn.Aborts())
			}
		})
	}
}

// An abort the transport loses is sent again at the next ask; the start waits meanwhile.
func TestQuiesceSendsAFailedAbortAgain(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	h.conn.FailAbort(errBoom)
	if err := h.m.Quiesce(h.ctx, 7); !errors.Is(err, errBoom) {
		t.Fatalf("Quiesce with the abort lost = %v, want the transport's error", err)
	}
	h.conn.FailAbort(nil)
	if err := h.m.Quiesce(h.ctx, 7); !errors.Is(err, ErrQuiesceHeld) || h.conn.Aborts() != 2 {
		t.Fatalf("Quiesce after the transport recovered = %v with %d aborts, want ErrQuiesceHeld and the abort sent again", err, h.conn.Aborts())
	}
}

// A prompt on its way to a turn is left to become one: the start waits, nothing is aborted before
// the turn exists, and the turn it starts is interrupted at the next ask.
func TestQuiesceWaitsForAPromptOnItsWay(t *testing.T) {
	h := newHarness(t)
	gated := newGatedConn()
	h.conn = gated.Conn
	h.reach(StateReady)
	h.conns.Register(h.token, gated)
	if err := h.m.Handle(h.ctx, RequestDeliver{Claim: h.token, Task: "test the change"}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	waitFor(t, "the prompt to be on its way", gated.entered)
	if err := h.m.Quiesce(h.ctx, 7); !errors.Is(err, ErrQuiesceHeld) || gated.Aborts() != 0 {
		t.Fatalf("Quiesce with a prompt on its way = %v with %d aborts, want ErrQuiesceHeld and none", err, gated.Aborts())
	}
	gated.release <- struct{}{}
	h.m.Wait()
	h.must(StreamTurnStart{Claim: h.token})
	if err := h.m.Quiesce(h.ctx, 7); !errors.Is(err, ErrQuiesceHeld) || gated.Aborts() != 1 {
		t.Fatalf("Quiesce once the prompt's turn started = %v with %d aborts, want ErrQuiesceHeld and one", err, gated.Aborts())
	}
}

// A turn the interrupt does not end within the stop timeout is cut off as a held suspension is: the
// claim is suspended, its session kept, and the start goes on.
func TestQuiesceSuspendsATurnThatOutlastsTheStopTimeout(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	if err := h.m.Quiesce(h.ctx, 7); !errors.Is(err, ErrQuiesceHeld) {
		t.Fatalf("Quiesce mid-turn = %v, want ErrQuiesceHeld", err)
	}
	h.advance(testStop)
	if err := h.m.Quiesce(h.ctx, 7); !errors.Is(err, ErrQuiesceHeld) || !h.claim().SuspensionHeld {
		t.Fatalf("Quiesce past the stop timeout = %v, held %t; want ErrQuiesceHeld and the suspension held", err, h.claim().SuspensionHeld)
	}
	h.advance(testStop)
	if got := h.claim(); got.State != StateSuspended || got.Session != session || len(h.calls("Suspend")) != 1 {
		t.Fatalf("after the held suspension's timeout the claim is %s on %q with %d suspensions, want suspended once on its session", got.State, got.Session, len(h.calls("Suspend")))
	}
	if err := h.m.Quiesce(h.ctx, 7); err != nil {
		t.Fatalf("Quiesce of the suspended claim = %v, want nil", err)
	}
}

// A restarted daemon forgets the interrupt: it asks again, and a claim still in its turn is
// interrupted again; the process ending is the interrupted turn over.
func TestQuiesceAfterARestartAndAfterTheProcessEnds(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	if err := h.m.Quiesce(h.ctx, 7); !errors.Is(err, ErrQuiesceHeld) {
		t.Fatalf("Quiesce mid-turn = %v, want ErrQuiesceHeld", err)
	}
	h.restart()
	if err := h.m.Quiesce(h.ctx, 7); !errors.Is(err, ErrQuiesceHeld) {
		t.Fatalf("Quiesce of the restored working claim = %v, want ErrQuiesceHeld", err)
	}
	if got := h.conn.Aborts(); got != 2 {
		t.Fatalf("aborts = %d, want the restored machine to interrupt the turn again", got)
	}
	h.observe(runtime.Gone)
	if err := h.m.Quiesce(h.ctx, 7); err != nil {
		t.Fatalf("Quiesce after the interrupted process died = %v, want nil", err)
	}
}
