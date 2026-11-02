package supervise

import (
	"strings"
	"testing"
)

const (
	replacingSession     = "ses_fresh"
	replacingSessionFile = "/state/sessions/fresh.jsonl"
)

// registerFresh brings the agent that replaced an abandoned session up to ready under a session id
// of its own, so a test can tell the session the claim recorded from the one it left behind.
func (h *harness) registerFresh() {
	h.t.Helper()
	h.connect()
	h.must(RequestRegister{
		Claim:          h.token,
		Generation:     h.generation(),
		Session:        replacingSession,
		SessionFile:    replacingSessionFile,
		CapabilityHash: []byte("capability"),
		PluginContract: h.deps.PluginContract,
	})
	h.must(RequestReady{Claim: h.token, Generation: h.generation(), Session: replacingSession})
}

// abandonSession walks the claim to where the provider refused every turn of it out of its launch
// budget and an operator retried it, which leaves that session behind: from here the claim records
// an abandoned session nothing has told its replacement of yet.
func (h *harness) abandonSession(refusal string) {
	h.t.Helper()
	h.reach(StateIdle)
	for turn := 1; turn <= testLimits().LaunchFailures; turn++ {
		h.report(uint64(turn), true, refusal)
	}
	h.wantState(StateFailed)
	h.must(RequestRetry{Claim: h.token})
}

// A claim the provider refused out of its budget fails, and the retry of it leaves that session
// behind. The claim records which session it abandoned and why, since nothing else does — the
// session itself stays in its store — and the first task the agent that replaces it is given names
// that session, the cause in the provider's own words (the context size it refused at among them),
// the issue's branch and the handoffs committed on it. Nothing in a new session says that an older
// one existed, so a worker given the bare task would read an empty transcript as a fresh start and
// do again what the abandoned session already pushed (dispatch://INFRA-714, item 4 of the refusal
// section).
func TestTheFreshSessionsFirstTaskNamesTheSessionItReplacedAndWhatOutlivedIt(t *testing.T) {
	h := newHarness(t)
	refusal := "the provider ended the turn with an error at 226k tokens of context"

	h.abandonSession(refusal)

	if got := h.claim().Abandoned; got.Session != session || got.SessionFile != sessionFile || !strings.Contains(got.Why, refusal) {
		t.Fatalf("the claim records %+v, want the session %q it left behind and the provider's own words for why", got, session)
	}
	h.registerFresh()
	base := len(h.prompts())

	h.must(RequestDeliver{Claim: h.token, Task: "Continue Workflow. Issue: LEGION-209. Phase: implementing."})

	first := h.wantPrompts(base + 1)[base].Message
	for _, want := range []string{
		session, "abandoned", refusal,
		"legion/LEGION-209", ".legion/LEGION-209/", "plan.json", "implement.json", "test.json", "review.json",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("the first task of the fresh session does not name %q:\n%s", want, first)
		}
	}
	if !strings.HasSuffix(first, "Continue Workflow. Issue: LEGION-209. Phase: implementing.") {
		t.Errorf("the first task of the fresh session is %q, want it to end with the task itself", first)
	}

	// The fresh agent has ended a turn, so it has read that first task: the next one is the task
	// alone, not the abandonment told again.
	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamTurnEnd{Claim: h.token})

	h.must(RequestDeliver{Claim: h.token, Task: "the next task"})

	if second := h.wantPrompts(base + 2)[base+1].Message; second != "the next task" {
		t.Errorf("the next task of the fresh session is %q, want the task alone", second)
	}
}

// Whoever abandons a session, the claim records which one and why in that one's own words: an
// operator's `legion claims forget-session`, and an operator's retry in a new session, which fails
// nothing and so has no failure to name.
func TestEveryAbandonmentIsRecordedWithItsOwnReason(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event Event
		why   string
	}{
		{"the operator forgetting the session", RequestForgetSession{Claim: testToken, Session: session}, "forget-session"},
		{"a retry asking for a new session", RequestRetry{Claim: testToken, FreshSession: true}, "the retry asked for a new session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			h.must(RequestStop{Claim: h.token})
			h.wantState(StateRetired)

			h.must(tc.event)

			got := h.claim()
			if got.Session != "" || got.SessionFile != "" {
				t.Fatalf("the claim still records session %q (%s), want it abandoned", got.Session, got.SessionFile)
			}
			if got.Abandoned.Session != session || !strings.Contains(got.Abandoned.Why, tc.why) {
				t.Fatalf("the claim records %+v, want the abandoned %q and a reason naming %q", got.Abandoned, session, tc.why)
			}
		})
	}
}

// A prompt the agent refused reached nobody, so the abandoned session is still owed to the fresh
// agent: the task goes back in the queue and the claim and the store both still record the
// sentence as untold. The prompt the agent does acknowledge carries it again, and that
// acknowledgement is what marks it told, so the task after it is the task alone. Marking it where
// the send started instead left the first task the fresh session actually read without the
// branch, the handoffs and the sentence that says an older session existed at all
// (dispatch://INFRA-714).
func TestARefusedPromptLeavesTheAbandonedSessionStillToBeTold(t *testing.T) {
	h := newHarness(t)
	refusal := "the provider ended the turn with an error at 226k tokens of context"
	h.abandonSession(refusal)
	h.registerFresh()
	base := len(h.prompts())
	h.conn.RefusePrompt("the agent will not take it")

	h.must(RequestDeliver{Claim: h.token, Task: "Continue Workflow. Issue: LEGION-209. Phase: implementing."})

	if first := h.wantPrompts(base + 1)[base].Message; !strings.Contains(first, session) {
		t.Fatalf("the refused prompt is %q, want it to name the abandoned session %q", first, session)
	}
	if told, stored := h.claim().Abandoned.Told, h.store.load(h.token).Abandoned.Told; told || stored {
		t.Fatalf("after the refusal the claim records the abandonment told = %v and the store %v, want it untold in both: the agent the prompt was refused by never read it", told, stored)
	}

	// The shim's next hello sends the task again, and this time the agent takes it.
	h.conn.FailPrompt(nil)
	h.must(StreamHello{Claim: h.token, Generation: h.generation()})

	taken := h.wantPrompts(base + 2)[base+1].Message
	for _, want := range []string{session, "abandoned", refusal, "legion/LEGION-209", ".legion/LEGION-209/"} {
		if !strings.Contains(taken, want) {
			t.Errorf("the task the agent acknowledged does not name %q:\n%s", want, taken)
		}
	}
	if told, stored := h.claim().Abandoned.Told, h.store.load(h.token).Abandoned.Told; !told || !stored {
		t.Fatalf("after the acknowledgement the claim records the abandonment told = %v and the store %v, want it told in both", told, stored)
	}

	// That task's turn is over, so the one after it is the task alone.
	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamTurnEnd{Claim: h.token})

	h.must(RequestDeliver{Claim: h.token, Task: "the next task"})

	if next := h.wantPrompts(base + 3)[base+2].Message; next != "the next task" {
		t.Errorf("the task after the one the agent acknowledged is %q, want the task alone", next)
	}
}
