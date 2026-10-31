package supervise

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// restoredRootTask is the tree root architect's claim as a restart reads it: ready, its agent
// registered, holding the task of a run that nobody has sent — a start row gave it, and the root
// had no architect to hear it then (Deps.RootHears). The task names no phase and a run, and the
// claim is its tree's root, so it is the one delivery supervise holds to that answer.
func restoredRootTask() Claim {
	c := rootClaim()
	c.State = StateReady
	c.Generation = 3
	c.Session, c.SessionFile = session, sessionFile
	c.Locator = &runtime.Locator{
		Runtime:     runtime.RuntimeTmux,
		Claim:       rootToken,
		Incarnation: "4242:77",
		Tmux:        &runtime.TmuxLocator{Window: "@1", Pane: "%1"},
	}
	c.DeliveredRows = []int64{7}
	c.Pending = &Delivery{ID: "delivery-root", Task: "a person decided on the tree", Generation: 2, QueuedAt: epoch}
	return c
}

// A root architect's held task is not confirmed by the question a restart left (askFirst) while
// its root has nobody to hear it: the agent is in a turn, but that turn is its own — nothing sent
// this task, and the decisions it names were forgotten when the claim queued it, so a confirmation
// would retire it unsent and lose them. The task stays pending and unconfirmed through that turn
// and is prompted once the root has an architect to hear it again.
func TestAStreamingAgentDoesNotConfirmARootArchitectsHeldTask(t *testing.T) {
	restored := restoredRootTask()
	h := newBareHarness(t)
	h.token = rootToken
	hears := false
	h.deps.RootHears = func(_ context.Context, c Claim, d Delivery) (bool, error) {
		if c.Token != rootToken || d.Generation == 0 || d.Phase != "" {
			t.Errorf("RootHears asked about claim %s and delivery %+v, want only the root architect's task of a run", c.Token, d)
		}
		return hears, nil
	}
	if err := h.store.PutClaim(h.ctx, restored); err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutDelivery(h.ctx, rootToken, *restored.Pending); err != nil {
		t.Fatalf("seed the restored delivery: %v", err)
	}
	h.conns.Register(rootToken, h.conn)
	h.conn.SetStreaming(true) // the agent is in a turn an earlier daemon may have seen start
	h.start(restored)

	h.must(RequestReady{Claim: rootToken, Generation: restored.Generation, Session: session})

	h.wantPrompts(0)
	if p := h.pending(); !p.ConfirmedAt.IsZero() || !p.DeliveredAt.IsZero() {
		t.Fatalf("pending delivery = %+v, want the held task unsent and unconfirmed: the agent's turn is its own", p)
	}

	// The agent's own turn starts and ends. Neither confirms the task: nothing sent it, so no turn
	// of the agent's own is its.
	h.must(StreamTurnStart{Claim: rootToken})
	if p := h.pending(); !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending delivery = %+v after a turn the agent started itself, want it unconfirmed", p)
	}
	h.conn.SetStreaming(false)
	h.must(StreamTurnEnd{Claim: rootToken})
	if p := h.m.Claim().Pending; p == nil || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending delivery after the turn ended = %+v, want the held task still waiting: a task nobody sent is never retired", p)
	}
	h.wantPrompts(0)

	// The root runs again, and the task it held is prompted.
	hears = true
	h.must(RequestReady{Claim: rootToken, Generation: restored.Generation, Session: session})
	if sent := h.wantPrompts(1)[0]; sent.Message != "a person decided on the tree" {
		t.Fatalf("prompt = %+v, want the task held while the root had nobody to hear it", sent)
	}
}

// A turn the agent starts on its own — an Envoy notice's, a person's message in its session —
// confirms no root architect's held task, however the machine came by it: a restored claim whose
// agent it has still to ask about (askFirst) is not a task in flight. Only a send of this machine's
// own, or a prompt it is waiting for a turn on, makes an agent_start naming no delivery that
// delivery's.
func TestAForeignTurnDoesNotConfirmARootArchitectsHeldTask(t *testing.T) {
	restored := restoredRootTask()
	h := newBareHarness(t)
	h.token = rootToken
	h.deps.RootHears = func(context.Context, Claim, Delivery) (bool, error) { return false, nil }
	if err := h.store.PutClaim(h.ctx, restored); err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutDelivery(h.ctx, rootToken, *restored.Pending); err != nil {
		t.Fatalf("seed the restored delivery: %v", err)
	}
	h.start(restored)

	// No connection yet, so nothing has asked the agent anything: the question the restart left is
	// still armed when the agent starts a turn of its own.
	h.must(StreamTurnStart{Claim: rootToken})

	h.wantState(StateWorking)
	if p := h.pending(); !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending delivery = %+v, want the held task unconfirmed: the turn is the agent's own", p)
	}
	h.must(StreamTurnEnd{Claim: rootToken})
	if p := h.m.Claim().Pending; p == nil || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending delivery after the foreign turn = %+v, want the held task still waiting", p)
	}
	if stored := h.store.load(rootToken); stored.Pending == nil || !stored.Pending.ConfirmedAt.IsZero() {
		t.Fatalf("the store holds %+v, want the held task unconfirmed there too: a restart must not read it as served", stored.Pending)
	}
}
