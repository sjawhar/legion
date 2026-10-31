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
	h.reach(StateIdle)
	refusal := "the provider ended the turn with an error at 226k tokens of context"
	for turn := 1; turn <= testLimits().LaunchFailures; turn++ {
		h.report(uint64(turn), true, refusal)
	}
	h.wantState(StateFailed)

	h.must(RequestRetry{Claim: h.token})

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
