package supervise

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// providerErrorTaskText is the work the cases below give the claim, so a prompt's own sentence is
// told from the task behind it.
const providerErrorTaskText = "implement the plan"

// workingClaim is a ready claim in the turn of its implementing task: where a turn that ends in a
// provider error ends. set, when it is given, changes the dependencies before the machine is
// built, which is the only way a case gives the machine the controller's notice path.
func workingClaim(t *testing.T, set ...func(*Deps)) *harness {
	t.Helper()
	h := newBareHarness(t)
	for _, change := range set {
		change(&h.deps)
	}
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: providerErrorTaskText, Phase: phase.Implementing, Generation: 7, Assignment: "A1"})
	h.must(StreamTurnStart{Claim: testToken})
	return h
}

// errorTurn ends the turn the claim is working and answers its prompt with the provider error r.
// That is the order the daemon sees: the agent_end, then the prompt's result on the worker
// stream, which is the whole of what the decision reads.
func (h *harness) errorTurn(r StreamPromptResult) {
	h.t.Helper()
	r.Claim, r.AgentInvoked, r.DeliveryID = h.token, true, h.pending().ID
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.must(r)
}

// climbs walks the ladder's next step: nothing goes before the wait is over, the task goes when
// it is, and the agent starts its turn on it. It returns the prompt that step sent.
func (h *harness) climbs(sentSoFar int, wait time.Duration) string {
	h.t.Helper()
	h.advance(wait - time.Second)
	h.wantPrompts(sentSoFar)
	h.advance(time.Second)
	sent := h.wantPrompts(sentSoFar + 1)
	h.must(StreamTurnStart{Claim: h.token})
	return sent[sentSoFar].Message
}

// The ladder is exact and has no top step that ends it: 30 s, 60 s, 120 s, doubling to 10 minutes
// and never further apart than that, for as many turns as the provider keeps failing.
func TestAProviderErrorTurnKeepsItsTaskAndSendsItAgainOnAGrowingLadder(t *testing.T) {
	h := workingClaim(t)
	sent := 1
	for attempt, wait := range []time.Duration{
		30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute,
		8 * time.Minute, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute,
	} {
		h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
		p := h.pending()
		if !p.ConfirmedAt.IsZero() || p.ProviderErrorRetries != attempt+1 {
			t.Fatalf("attempt %d: the task is %+v, want it unconfirmed and counted %d sends", attempt+1, p, attempt+1)
		}
		if message := h.climbs(sent, wait); !strings.HasSuffix(message, providerErrorTaskText) {
			t.Fatalf("attempt %d sent %q, want the task again", attempt+1, message)
		}
		sent++
	}
	if state := h.state(); state != StateWorking {
		t.Fatalf("the claim is %s after eight provider errors, want it still working its task", state)
	}
}

// promptRetryDelay is read by nothing else, and the step it is asked for comes from a persisted
// count: an attempt a restart reads back past the ladder's top is still the top step. Attempts 29
// to 31 are the ladder past the point where a shift of promptRetryFirst leaves int64: a wait the
// claim would have taken as "send it again now".
func TestThePromptRetryLadderStopsGrowingAtTenMinutes(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{{0, 30 * time.Second}, {1, 30 * time.Second}, {2, time.Minute}, {5, 8 * time.Minute},
		{6, 10 * time.Minute}, {29, 10 * time.Minute}, {30, 10 * time.Minute},
		{31, 10 * time.Minute}, {40, 10 * time.Minute}, {1 << 20, 10 * time.Minute},
		{math.MaxInt, 10 * time.Minute}} {
		if got := promptRetryDelay(tc.attempt); got != tc.want {
			t.Errorf("promptRetryDelay(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}
}

// A rate limit, a timeout and an overload all send the task again, and none costs the claim a
// prompt failure: chargePrompt retires and then fails a claim past Limits.PromptFailures, which
// would fail every claim of a project at once in a provider outage.
func TestAStatusThatClearsChargesNoPromptBudgetHoweverOftenItHappens(t *testing.T) {
	for _, status := range []int{408, 429, 529} {
		t.Run(fmt.Sprintf("HTTP %d", status), func(t *testing.T) {
			h := workingClaim(t)
			sent := 1
			// More turns than the claim's prompt-failure limit, which is what used to retire and
			// then fail it.
			for turn := range h.deps.Limits.PromptFailures + 2 {
				h.errorTurn(StreamPromptResult{Error: "the provider answered an error", HTTPStatus: status})
				h.climbs(sent, promptRetryDelay(turn+1))
				sent++
			}
			h.wantBudgets(Budgets{})
			if state := h.state(); state == StateFailed {
				t.Fatalf("the claim failed after %d provider errors: %q", sent-1, h.claim().Failure)
			}
			if h.claim().Pending == nil {
				t.Fatal("the claim holds no task; a provider error keeps the work it was given")
			}
		})
	}
}

// A content filter's refusal carries no HTTP status at all, and it is sent to the same session
// again: the plugin has already moved the session down its model chain, so the next prompt goes
// to another model. Nothing is waited for first — the prompt's own result is the whole of what
// the decision reads — so the ladder's first step is armed in the same step the error arrives.
func TestAStatuslessRefusalIsSentAgainOnTheLadder(t *testing.T) {
	h := workingClaim(t)

	h.errorTurn(StreamPromptResult{Error: "Refusal (reasoning_extraction): I will not help with that."})

	p := h.claim().Pending
	if p == nil || p.ProviderErrorRetries != 1 || p.ProviderErrorNote == "" {
		t.Fatalf("pending %+v, want the task kept, counted one send, and carrying the sentence it goes behind", p)
	}
	h.climbs(1, 30*time.Second)
	if state := h.state(); state != StateWorking {
		t.Fatalf("the claim is %s, want it working the task it was sent again", state)
	}
}

// The one error that holds the issue for its architect: a client error the provider answered
// that is neither a timeout nor a rate limit. A malformed request is the case this exists for.
func TestAMalformedRequestFailsTheClaim(t *testing.T) {
	h := workingClaim(t)

	h.errorTurn(StreamPromptResult{Error: "invalid_request_error", HTTPStatus: 400})

	h.wantState(StateFailed)
	if failure := h.claim().Failure; !strings.Contains(failure, "HTTP 400") {
		t.Errorf("the failure is %q, want it to name the status the provider answered", failure)
	}
	if h.claim().Pending == nil {
		t.Error("the failed claim holds no task; the work is still owed and the architect decides it")
	}
	if got := h.store.load(testToken).ProviderErrorRun; got != 1 {
		t.Errorf("the stored claim's run of provider errors is %d, want 1", got)
	}
	// One prompt: the task itself. A malformed request is not sent again at all.
	h.wantPrompts(1)
}

// The whole rule, status by status. A 400 to 499 other than 408 and 429 holds the issue; every
// other error — 5xx, 408, 429, and the statusless turn every content-filter refusal ends in —
// keeps the task and sends it again.
func TestOnlyAClientErrorThatIsNoTimeoutAndNoRateLimitHoldsTheIssue(t *testing.T) {
	for _, tc := range []struct {
		status int
		holds  bool
	}{
		{0, false}, {400, true}, {401, true}, {403, true}, {404, true}, {408, false},
		{422, true}, {429, false}, {499, true}, {500, false}, {503, false}, {529, false},
	} {
		t.Run(fmt.Sprintf("HTTP %d", tc.status), func(t *testing.T) {
			h := workingClaim(t)

			h.errorTurn(StreamPromptResult{Error: "the provider answered an error", HTTPStatus: tc.status})

			if held := h.state() == StateFailed; held != tc.holds {
				t.Fatalf("HTTP %d left the claim %s, want holds=%v", tc.status, h.state(), tc.holds)
			}
			if tc.holds {
				return
			}
			if p := h.pending(); p.ProviderErrorRetries != 1 {
				t.Fatalf("pending %+v, want the task kept and counted one send", p)
			}
			h.climbs(1, 30*time.Second)
		})
	}
}

// One turn's error decides that turn and nothing else. A refusal with no status keeps the task
// and sends it again; the turn that re-sent task runs ends in a malformed request, and that
// error holds the issue, whatever the turn before it did.
func TestARefusalThenAMalformedRequestHoldsTheIssue(t *testing.T) {
	h := workingClaim(t)

	h.errorTurn(StreamPromptResult{Error: "Refusal (reasoning_extraction): I will not help with that."})
	h.climbs(1, 30*time.Second)

	h.errorTurn(StreamPromptResult{Error: "invalid_request_error", HTTPStatus: 400})

	h.wantState(StateFailed)
	if failure := h.claim().Failure; !strings.Contains(failure, "HTTP 400") {
		t.Errorf("the failure is %q, want the status the provider answered", failure)
	}
	// Two prompts: the task itself and the one send the refusal earned. The malformed request is
	// not sent again at all.
	h.wantPrompts(2)
}

// R2: a newer daemon task for the claim replaces the task a provider error took back, and goes at
// once. Nothing works the owed task, the ladder's next step can be ten minutes away and a run of
// errors has no end of its own, so a task that waited behind it would wait that long. What the
// ladder counted is the owed delivery's own and goes with it, so the task that replaces it starts
// its own ladder at the first step; the claim's run of provider-error turns is the claim's and
// stays.
func TestANewerTaskQueuedWhileTheLadderWaitsReplacesTheOwedTask(t *testing.T) {
	h := workingClaim(t)
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
	owed := h.pending()
	if owed.ProviderErrorRetries != 1 || owed.ProviderErrorNote == "" {
		t.Fatalf("the owed task is %+v, want one send counted against it and the sentence it would go behind", owed)
	}

	h.must(RequestDeliver{Claim: testToken, Task: "test the change", Phase: phase.Testing, Generation: 8, Assignment: "A2"})

	sent := h.wantPrompts(2)[1]
	if sent.Message != "test the change" || sent.DeliveryID == owed.ID {
		t.Fatalf("sent %+v, want the newer task at once under an id of its own", sent)
	}
	if p := h.pending(); p.ProviderErrorRetries != 0 || p.ProviderErrorNote != "" {
		t.Errorf("the task that replaced the owed one is %+v, want a ladder of its own from the first step", p)
	}
	if _, armed := h.m.timers[TimerPromptRetry]; armed {
		t.Error("the ladder's wait is still armed for a task the claim no longer holds")
	}
	if run := h.claim().ProviderErrorRun; run != 1 {
		t.Errorf("the claim's run of provider-error turns is %d, want 1: the run is the claim's, not the task's", run)
	}
}

// R2's other half: an update round's question goes before the task a provider error took back,
// and goes before it rather than instead of it. The round is the daemon replacing this claim's
// pod, and a question refused behind the owed task (ErrDeliveryPending) would wait out a ladder
// whose steps reach ten minutes, so in a run of errors the round would never get in; a question
// that retired the owed task would lose it for good, since the outbox row that gave it is
// recorded on the claim and no retry of that row gives it again, and the claim would come back
// in its new pod with work owed and nothing pending.
func TestAnUpdateRoundsQuestionGoesBeforeTheOwedTask(t *testing.T) {
	h := workingClaim(t)
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
	owed := h.pending()

	h.openUpdate(testRound, testReasons)

	sent := h.wantPrompts(2)[1]
	if !asksRound(sent.DeliveryID, testRound) {
		t.Fatalf("sent %+v, want the round's question before the owed task", sent)
	}
	if p := h.pending(); !asksRound(p.ID, testRound) {
		t.Fatalf("pending %+v, want the round's question, not the task %s the provider error took back", p, owed.ID)
	}
	if _, armed := h.m.timers[TimerPromptRetry]; armed {
		t.Error("the ladder's wait is still armed for a task the question went before")
	}

	// The worker reads the question, answers it and ends its turn. The task it owes comes back
	// to the claim there, and the answered round holds it until the round is over.
	h.must(StreamTurnStart{Claim: testToken})
	h.signalUpdate("c0ffee")
	h.must(StreamTurnEnd{Claim: testToken})

	p := h.pending()
	if p.ID != owed.ID || p.Task != owed.Task {
		t.Fatalf("after the question's turn pending %+v, want the owed task %s back: nothing else ever gives it again", p, owed.ID)
	}
	if p.ProviderErrorRetries != owed.ProviderErrorRetries {
		t.Errorf("the owed task came back with %d sends counted, want the %d it had: its ladder goes on from where it stood",
			p.ProviderErrorRetries, owed.ProviderErrorRetries)
	}
	h.wantPrompts(2)

	h.must(PodUpdateEnded{Claim: testToken, Round: testRound})

	again := h.wantPrompts(3)[2]
	if again.DeliveryID != owed.ID || !strings.HasSuffix(again.Message, providerErrorTaskText) {
		t.Errorf("the prompt after the round = %+v, want the owed task %s sent again", again, owed.ID)
	}
}

// A wait left armed names the task it was armed for. The guard reads that id rather than the
// kind alone, so a task that replaced the one the wait is for is held by nothing.
func TestAWaitLeftArmedForAnOlderTaskHoldsNothing(t *testing.T) {
	h := workingClaim(t)

	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})

	owed := h.pending().ID
	if !h.m.waitsForProviderError(owed) {
		t.Fatalf("no wait is armed for %s, the task the provider error took back", owed)
	}
	if h.m.waitsForProviderError("a task this claim does not hold") {
		t.Error("a wait armed for one task holds another")
	}
}

// The sentence the re-sent prompt carries is built from structured fields alone. A refusal's own
// explanation is itself refused when it is sent back
// (dispatch://INFRA-956/artifact/cause-evidence-md), so none of the provider's words are in it.
func TestTheResentPromptNamesTheStatusAndNoProviderText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result StreamPromptResult
		// contains is what the sentence says, and written is the provider's own text, which no
		// prompt may carry.
		contains string
		written  string
	}{
		{
			name:     "an error with a status names it",
			result:   StreamPromptResult{Error: "Overloaded: upstream capacity is gone", HTTPStatus: 529},
			contains: "a provider error (HTTP 529)",
			written:  "upstream capacity is gone",
		},
		{
			name:     "an error with none names only that the provider failed",
			result:   StreamPromptResult{Error: "Refusal (reasoning_extraction): I will not help you extract that."},
			contains: "a provider error.",
			written:  "I will not help you extract that.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := workingClaim(t)

			h.errorTurn(tc.result)
			message := h.climbs(1, 30*time.Second)

			if !strings.Contains(message, tc.contains) {
				t.Errorf("the prompt is %q, want it to name %q", message, tc.contains)
			}
			if !strings.HasSuffix(message, "\n\n"+providerErrorTaskText) {
				t.Errorf("the prompt is %q, want the task itself behind the sentence", message)
			}
			if strings.Contains(message, tc.written) {
				t.Errorf("the prompt carries the provider's own text %q: %q", tc.written, message)
			}
		})
	}
}

// Only a run of provider errors is counted, and a turn that ends with none ends the run.
func TestATurnThatEndsWithNoProviderErrorEndsTheRun(t *testing.T) {
	h := workingClaim(t)
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
	h.climbs(1, 30*time.Second)
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
	h.climbs(2, time.Minute)
	if got := h.claim().ProviderErrorRun; got != 2 {
		t.Fatalf("the run of provider errors is %d, want 2", got)
	}

	h.mustQuiet(StreamTurnEnd{Claim: testToken})
	h.must(StreamPromptResult{Claim: testToken, DeliveryID: h.pending().ID, AgentInvoked: true})

	if got := h.claim().ProviderErrorRun; got != 0 {
		t.Errorf("the run of provider errors is %d after a clean turn, want 0", got)
	}
	if got := h.store.load(testToken).ProviderErrorRun; got != 0 {
		t.Errorf("the stored run of provider errors is %d after a clean turn, want 0", got)
	}
	if p := h.claim().Pending; p != nil {
		t.Errorf("pending %+v, want the task the clean turn worked retired", *p)
	}
}

// The one exit from a run of provider errors is the project's controller, told once. There is no
// hold and no attempt limit behind it.
func TestTheSixthProviderErrorTurnInARowTellsTheControllerOnce(t *testing.T) {
	type told struct {
		claim  Claim
		reason string
	}
	var notices []told
	h := workingClaim(t, func(d *Deps) {
		d.ProviderErrorRunReached = func(c Claim, reason string) { notices = append(notices, told{c, reason}) }
	})

	sent := 1
	for turn := range 7 {
		h.errorTurn(StreamPromptResult{Error: "Refusal (reasoning_extraction): no.", Model: "claude-opus-4-8"})
		if turn == 5 && len(notices) != 1 {
			t.Fatalf("the controller was told %d times at the sixth turn, want once", len(notices))
		}
		h.climbs(sent, promptRetryDelay(turn+1))
		sent++
	}

	if len(notices) != 1 {
		t.Fatalf("the controller was told %d times over seven turns, want once", len(notices))
	}
	notice := notices[0]
	if notice.claim.Issue != "LEGION-209" || notice.claim.Role != claim.RoleImplementer {
		t.Errorf("the notice names %s of %s, want the implementer of LEGION-209", notice.claim.Role, notice.claim.Issue)
	}
	// The cause is what the error's own fields give, and a content filter's refusal carries no
	// status at all, so the notice names neither a status nor the filter's own words.
	for _, want := range []string{"6 turns in a row", "claude-opus-4-8", "ended in a provider error"} {
		if !strings.Contains(notice.reason, want) {
			t.Errorf("the reason is %q, want it to name %q", notice.reason, want)
		}
	}
	if strings.Contains(notice.reason, "reasoning_extraction") {
		t.Errorf("the reason is %q, want none of the provider's own words", notice.reason)
	}
	if got := h.claim().ProviderErrorRun; got != 7 {
		t.Errorf("the run of provider errors is %d, want 7: the notice bounds nothing", got)
	}
}
