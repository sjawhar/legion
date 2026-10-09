package supervise

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A shim whose agent resumed its session in a workspace recreated since the session was last
// written says so in its hello, and the agent's next task is sent behind the notice that names the
// issue's branch: sent behind it again after a refusal, since the agent never read it, and no more
// once a turn of it has started. A later hello without it, the shim's redial after that turn, takes
// nothing back, and a hello that never said it adds no notice.
func TestTheNextTaskAfterAHelloFromARecreatedWorkspaceTellsTheAgent(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.conns.Register(h.token, h.conn)
	h.must(StreamHello{Claim: h.token, Generation: h.generation(), WorkspaceRecreated: true})
	h.register()
	h.ready()
	notice := workspaceRecreatedNotice("LEGION-209")
	if !strings.Contains(notice, "legion/LEGION-209") {
		t.Fatalf("the notice %q does not name the issue's branch", notice)
	}

	h.conn.RefusePrompt("agent busy")
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	if first := h.wantPrompts(1)[0]; first.Message != notice+"implement the plan" {
		t.Fatalf("the first task after the hello was sent as %q, want it behind the notice", first.Message)
	}
	h.conn.FailPrompt(nil)
	h.observe(runtime.Alive)
	if resent := h.wantPrompts(2)[1]; resent.Message != notice+"implement the plan" {
		t.Fatalf("the refused task was re-sent as %q, want it behind the notice still", resent.Message)
	}
	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantState(StateIdle)

	h.connect()
	h.must(RequestDeliver{Claim: testToken, Task: "address the review"})
	if next := h.wantPrompts(3)[2]; next.Message != "address the review" {
		t.Errorf("the task after the notice's turn was sent as %q, want it alone", next.Message)
	}
}

// The notice ends at the agent's first turn whoever sent it, as the shim's hello does: a turn the
// daemon did not send, such as one an Envoy event starts before any task, goes untold, and the task
// sent after it carries no notice that would call that turn's workspace "recreated since your last
// turn".
func TestATurnTheDaemonDidNotSendEndsTheRecreatedNotice(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.conns.Register(h.token, h.conn)
	h.must(StreamHello{Claim: h.token, Generation: h.generation(), WorkspaceRecreated: true})
	h.register()
	h.ready()

	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantState(StateIdle)

	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	if sent := h.wantPrompts(1)[0]; sent.Message != "implement the plan" {
		t.Errorf("the task after the agent's own first turn was sent as %q, want it alone", sent.Message)
	}
}

// A turn that starts before the claim is ready — one an Envoy delivery triggers while the agent is
// registered and has had no task — is the agent's first turn all the same: the table ignores it,
// but the shim stops saying the workspace was recreated there, so the first task sent after it
// carries no notice.
func TestATurnBeforeReadyEndsTheRecreatedNotice(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.conns.Register(h.token, h.conn)
	h.must(StreamHello{Claim: h.token, Generation: h.generation(), WorkspaceRecreated: true})
	h.register()
	h.wantState(StateRegistered)
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateRegistered)
	h.ready()

	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	if sent := h.wantPrompts(1)[0]; sent.Message != "implement the plan" {
		t.Errorf("the task after a turn the agent ran before it was ready was sent as %q, want it alone", sent.Message)
	}
}

// A hello that says nothing of the workspace adds no notice: the ordinary resume, the fresh agent,
// and a tmux pane, whose shim no launcher starts.
func TestATaskAfterAnOrdinaryHelloCarriesNoNotice(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	if sent := h.wantPrompts(1)[0]; sent.Message != "implement the plan" {
		t.Errorf("the task was sent as %q, want it alone", sent.Message)
	}
}
