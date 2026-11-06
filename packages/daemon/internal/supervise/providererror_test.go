package supervise

import (
	"errors"
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

// workingClaim is a ready implementer in the turn of its implementing task: where a turn that
// ends in a provider error ends. set, when it is given, changes the dependencies before the
// machine is built, which is the only way a case gives the machine the controller's notice
// path.
func workingClaim(t *testing.T, set ...func(*Deps)) *harness {
	t.Helper()
	return workingClaimOf(t, queuedClaim(), set...)
}

// workingClaimOf is workingClaim over the claim c, for a case the claim's role decides: what a
// retirement and a failure do with a task an update round's question went before turns on
// whether the retry of this claim restarts a phase (Claim.phaseWorker).
//
// The task is the claim's own kind. A phase worker's names the phase and the run it was queued
// for; an architect's names neither, as the workflow queues it (Delivery.Phase).
func workingClaimOf(t *testing.T, c Claim, set ...func(*Deps)) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.token = c.Token
	for _, change := range set {
		change(&h.deps)
	}
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.reach(StateReady)
	task := RequestDeliver{Claim: c.Token, Task: providerErrorTaskText}
	if c.Role != claim.RoleArchitect {
		task.Phase, task.Generation, task.Assignment = phase.Implementing, 7, "A1"
	}
	h.must(task)
	h.must(StreamTurnStart{Claim: c.Token})
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

// A credential error, a rate limit, a timeout and an overload all send the task again, and none
// costs the claim a prompt failure: chargePrompt retires and then fails a claim past
// Limits.PromptFailures, which would fail every claim of a project at once in a provider outage
// — and a credential is the fleet's, so a 401 or a 403 reaches every claim of the project at
// once (holdsTheIssue).
func TestAStatusThatClearsChargesNoPromptBudgetHoweverOftenItHappens(t *testing.T) {
	for _, status := range []int{401, 403, 408, 429, 529} {
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
// that is none of a credential (401, 403), a timeout (408) and a rate limit (429). A malformed
// request is the case this exists for.
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

// The whole rule, status by status. A 400 to 499 other than 401, 403, 408 and 429 holds the
// issue; every other error — 5xx, the four exceptions, and the statusless turn every
// content-filter refusal ends in — keeps the task and sends it again. 401 and 403 are a
// credential, which is the fleet's and not this claim's work: failing every claim whose turn
// hits one is worse than sending the task again, and the controller is told at six such turns
// in a row (dispatch://INFRA-956/comment/838bf35d-69dd-4fb2-a1b3-4b474b4525b3).
func TestOnlyAClientErrorThatIsNoCredentialNoTimeoutAndNoRateLimitHoldsTheIssue(t *testing.T) {
	for _, tc := range []struct {
		status int
		holds  bool
	}{
		{0, false}, {400, true}, {401, false}, {403, false}, {404, true}, {408, false},
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

// The hold is written on the claim, not kept in the memory of the daemon that made it: the
// rollout these rounds exist for replaces the daemon's own pod while they run, the outbox row
// that gave the task is recorded on the claim, and no retry of that row gives it again
// (Claim.DeliveredRows). A daemon restarted while the question is pending gives the task back
// when the question's delivery ends, exactly as the daemon that held it would have: the same
// id, the read mark the worker's turn earned it, and the step of the ladder it stood on. The
// worker has answered the round here, so the claim holds the task it gets back until the round
// ends and nothing re-marks it meanwhile.
func TestTheTaskAQuestionWentBeforeOutlivesADaemonRestart(t *testing.T) {
	h := workingClaim(t)
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
	owed := h.pending()
	if owed.DeliveredAt.IsZero() || owed.MarkedBy == "" {
		t.Fatalf("the owed task is %+v, want the read mark its turn earned it", owed)
	}
	h.openUpdate(testRound, testReasons)
	h.must(StreamTurnStart{Claim: testToken})
	h.signalUpdate("c0ffee")

	h.restart()

	h.must(StreamTurnEnd{Claim: testToken})

	p := h.pending()
	if p.ID != owed.ID || p.Task != owed.Task {
		t.Fatalf("after the restart pending %+v, want the owed task %s back: nothing else ever gives it again", p, owed.ID)
	}
	if !p.DeliveredAt.Equal(owed.DeliveredAt) || p.MarkedBy != owed.MarkedBy {
		t.Errorf("the owed task came back marked %v by %q, want the mark it was held with: %v by %q",
			p.DeliveredAt, p.MarkedBy, owed.DeliveredAt, owed.MarkedBy)
	}
	if p.ProviderErrorRetries != owed.ProviderErrorRetries {
		t.Errorf("the owed task came back with %d sends counted, want the %d it had: its ladder goes on from where it stood",
			p.ProviderErrorRetries, owed.ProviderErrorRetries)
	}
	if d, ok := h.store.delivery(testToken); !ok || d.ID != owed.ID {
		t.Errorf("the store holds %+v (%t) for the claim, want the owed task %s pending again", d, ok, owed.ID)
	}
	h.wantPrompts(2)

	h.must(PodUpdateEnded{Claim: testToken, Round: testRound})

	again := h.wantPrompts(3)[2]
	if again.DeliveryID != owed.ID || !strings.HasSuffix(again.Message, providerErrorTaskText) {
		t.Errorf("the prompt after the round = %+v, want the owed task %s sent again", again, owed.ID)
	}
}

// A phase worker's hold lives as long as the claim it is written on and no longer. An
// operator's stop retires the claim, and the task an update round's question went before goes
// with it: the operator's retry of a phase worker restarts its issue's phase (api/operator.go's
// retryClaim), that restart queues the phase's own task, and a hold left standing would queue
// the work of a life that is over behind it. The question goes too, whether or not its turn
// ran: nothing of this claim will answer a round that is over, and a question left pending is
// the first thing the retry's fresh session would read.
func TestAPhaseWorkersStopDropsTheTaskTheQuestionWentBefore(t *testing.T) {
	for _, tc := range []struct {
		name string
		read bool
	}{{"the worker read the question", true}, {"the stop came first", false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := workingClaim(t)
			h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
			owed := h.pending()
			h.openUpdate(testRound, testReasons)
			if tc.read {
				h.must(StreamTurnStart{Claim: testToken})
			}

			h.must(RequestStop{Claim: testToken})

			h.wantState(StateRetired)
			if p := h.claim().Pending; p != nil {
				t.Errorf("the stopped claim holds %+v, want nothing: the task %s and the round's question both went with the claim", *p, owed.ID)
			}
			if held := h.claim().OwedTask; held != nil {
				t.Errorf("the stopped claim still owes %+v, want nothing", *held)
			}
			if d, ok := h.store.delivery(testToken); ok {
				t.Errorf("the store holds %+v for the stopped claim, want no delivery", d)
			}
		})
	}
}

// R2 has a newer task replace the one a provider error took back, and the task an update round's
// question gave back is still that task: nothing works it, and what holds it is the claim's
// state rather than a wait of its own, so a refusal here would keep the newer task behind it
// until the round ended. The task that replaces it starts its own ladder at the first step.
func TestANewerTaskReplacesTheTaskTheQuestionGaveBack(t *testing.T) {
	h := workingClaim(t)
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
	owed := h.pending()
	h.openUpdate(testRound, testReasons)
	h.must(StreamTurnStart{Claim: testToken})
	h.signalUpdate("c0ffee")
	h.must(StreamTurnEnd{Claim: testToken})
	if p := h.pending(); p.ID != owed.ID {
		t.Fatalf("pending %+v, want the owed task %s back before the newer one is queued", p, owed.ID)
	}

	h.must(RequestDeliver{Claim: testToken, Task: "test the change", Phase: phase.Testing, Generation: 8, Assignment: "A2"})

	p := h.pending()
	if p.Task != "test the change" || p.ID == owed.ID {
		t.Fatalf("pending %+v, want the newer task in the place of the owed task %s", p, owed.ID)
	}
	if p.ProviderErrorRetries != 0 || p.ProviderErrorNote != "" {
		t.Errorf("the task that replaced the owed one is %+v, want a ladder of its own from the first step", p)
	}
	// The round is answered, so every task of this claim waits for its end.
	h.wantPrompts(2)

	h.must(PodUpdateEnded{Claim: testToken, Round: testRound})

	if sent := h.wantPrompts(3)[2]; sent.Message != "test the change" {
		t.Errorf("the prompt after the round = %+v, want the newer task", sent)
	}
}

// R3: the operator stops a claim whose provider keeps refusing its session and retries it with
// a fresh one, and the task the ladder kept is the task that fresh session is sent. The stop
// takes the claim's timers with it (letGo), so no wait names that task any more — and the task
// is still the claim's work, not one the claim owes with nothing left to send it, so the phase
// restart's own task waits behind it (ErrDeliveryPending) until the turn on the kept task
// retires it. Only the two tasks R2 names are replaced (replaceable);
// scripts/e2e/provider-error-retry.sh step 5 is this case live.
func TestThePhaseRestartsTaskWaitsBehindTheTaskTheLadderKept(t *testing.T) {
	h := workingClaim(t)
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
	kept := h.pending()

	h.must(RequestStop{Claim: testToken})
	h.wantState(StateRetired)
	h.must(RequestOperatorRetry{Claim: testToken})

	err := h.handle(RequestDeliver{Claim: testToken, Task: "Continue the issue whose provider is overloaded.",
		Phase: phase.Implementing, Generation: 8, Assignment: "A2"})

	if !errors.Is(err, ErrDeliveryPending) {
		t.Fatalf("the phase restart's task was answered %v, want it to wait behind the task the ladder kept", err)
	}
	p := h.pending()
	if p.ID != kept.ID || p.Task != kept.Task {
		t.Fatalf("pending %+v, want the task the ladder kept, %s: the retried session is sent that task", p, kept.ID)
	}
	if p.ProviderErrorRetries != kept.ProviderErrorRetries {
		t.Errorf("the kept task counts %d sends, want the %d its ladder had", p.ProviderErrorRetries, kept.ProviderErrorRetries)
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

// A claim whose retry restarts no phase keeps the task an update round's question went before:
// that retry relaunches this same claim and sends what the claim kept (table.go's retry), and
// nothing else ever gives the task again, because the outbox row that gave it is recorded on
// the claim (Claim.DeliveredRows). An architect is such a claim. The question is retired all
// the same: the round is over, and a question left pending would be the first thing the
// relaunched session reads.
func TestAnArchitectsStopKeepsTheTaskTheQuestionWentBeforeAndTheRetrySendsIt(t *testing.T) {
	h := workingClaimOf(t, subArchitectClaim())
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
	owed := h.pending()
	h.openUpdate(testRound, testReasons)
	if p := h.pending(); !asksRound(p.ID, testRound) {
		t.Fatalf("pending %+v, want the round's question before the owed task %s", p, owed.ID)
	}

	h.must(RequestStop{Claim: h.token})

	h.wantState(StateRetired)
	p := h.pending()
	if p.ID != owed.ID || p.Task != owed.Task {
		t.Fatalf("the stopped architect holds %+v, want the task %s it owes: no retry of its outbox row gives it again", p, owed.ID)
	}
	if p.ProviderErrorRetries != owed.ProviderErrorRetries || !p.DeliveredAt.Equal(owed.DeliveredAt) {
		t.Errorf("the task came back %+v, want the step of the ladder and the read mark it was held with: %+v", p, owed)
	}
	if held := h.claim().OwedTask; held != nil {
		t.Errorf("the claim still owes %+v, want the task pending instead", *held)
	}
	if d, ok := h.store.delivery(h.token); !ok || d.ID != owed.ID {
		t.Fatalf("the store holds %+v (%t) for the stopped claim, want the owed task %s", d, ok, owed.ID)
	}
	h.wantPrompts(2)

	h.must(RequestOperatorRetry{Claim: h.token})
	h.relaunched()

	again := h.wantPrompts(3)[2]
	if again.DeliveryID != owed.ID || !strings.HasSuffix(again.Message, providerErrorTaskText) {
		t.Errorf("the prompt after the retry = %+v, want the owed task %s", again, owed.ID)
	}
}

// The same rule at the other end of a claim's life. A failure keeps the pending delivery for
// whoever decides what happens next, so a claim whose retry restarts no phase keeps the held
// task there too, and the question it waited behind is retired to free the slot for it. The
// failure here is the one every provider error can end in: the turn that read the round's
// question ended in a malformed request, which holds the issue (holdsTheIssue).
func TestAFailureKeepsTheTaskTheQuestionWentBeforeForAClaimThatRestartsNoPhase(t *testing.T) {
	h := workingClaimOf(t, subArchitectClaim())
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
	owed := h.pending()
	h.openUpdate(testRound, testReasons)
	h.must(StreamTurnStart{Claim: h.token})

	h.errorTurn(StreamPromptResult{Error: "invalid_request_error", HTTPStatus: 400})

	h.wantState(StateFailed)
	if failure := h.claim().Failure; !strings.Contains(failure, "HTTP 400") {
		t.Errorf("the failure is %q, want the status the provider answered", failure)
	}
	p := h.pending()
	if p.ID != owed.ID || p.Task != owed.Task {
		t.Fatalf("the failed architect holds %+v, want the task %s it owes", p, owed.ID)
	}
	if held := h.claim().OwedTask; held != nil {
		t.Errorf("the claim still owes %+v, want the task pending instead", *held)
	}
	if d, ok := h.store.delivery(h.token); !ok || d.ID != owed.ID {
		t.Errorf("the store holds %+v (%t) for the failed claim, want the owed task %s", d, ok, owed.ID)
	}
}

// A phase worker's failure is the other half: its retry restarts its issue's phase, which
// queues the phase's own task, so the held task is dropped — and the question it waited behind
// is retired with it, so the fresh session that phase starts is not first handed a question
// about a pod replacement that already happened.
func TestAPhaseWorkersFailureDropsTheTaskAndTheQuestionWithIt(t *testing.T) {
	h := workingClaim(t)
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
	owed := h.pending()
	h.openUpdate(testRound, testReasons)
	h.must(StreamTurnStart{Claim: testToken})

	h.errorTurn(StreamPromptResult{Error: "invalid_request_error", HTTPStatus: 400})

	h.wantState(StateFailed)
	if p := h.claim().Pending; p != nil {
		t.Errorf("the failed worker holds %+v, want nothing: the task %s and the round's question both went with the claim", *p, owed.ID)
	}
	if held := h.claim().OwedTask; held != nil {
		t.Errorf("the failed worker still owes %+v, want nothing", *held)
	}
	if d, ok := h.store.delivery(testToken); ok {
		t.Errorf("the store holds %+v for the failed claim, want no delivery", d)
	}
}

// The window a given-back task is in once its prompt is acknowledged and before the turn it
// starts: no send of the machine's own is out, the task is not confirmed, and the wait for its
// turn is armed. The task is in the agent's hands there, so a newer task waits behind it
// (ErrDeliveryPending). A task replaced in that window would be lost twice over: the turn the
// given-back task started would confirm the task that replaced it, and that turn's end would
// retire the given-back task unsent, with its outbox row already recorded as delivered.
func TestANewerTaskDoesNotReplaceAGivenBackTaskWhosePromptIsInFlight(t *testing.T) {
	h := workingClaim(t)
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529})
	owed := h.pending()
	h.openUpdate(testRound, testReasons)
	h.must(StreamTurnStart{Claim: testToken})
	h.signalUpdate("c0ffee")
	h.must(StreamTurnEnd{Claim: testToken})
	h.must(PodUpdateEnded{Claim: testToken, Round: testRound})
	sent := h.wantPrompts(3)[2]
	if sent.DeliveryID != owed.ID {
		t.Fatalf("the prompt after the round = %+v, want the owed task %s", sent, owed.ID)
	}
	if p := h.pending(); !p.ConfirmedAt.IsZero() {
		t.Fatalf("the owed task is %+v, want it acknowledged and not yet confirmed", p)
	}

	err := h.handle(RequestDeliver{Claim: testToken, Task: "test the change", Phase: phase.Testing, Generation: 8, Assignment: "A2"})

	if !errors.Is(err, ErrDeliveryPending) {
		t.Fatalf("the newer task was answered %v, want it refused: the task it would replace is in the agent's hands", err)
	}
	if p := h.pending(); p.ID != owed.ID {
		t.Fatalf("pending %+v, want the owed task %s, whose prompt the agent has", p, owed.ID)
	}
	h.wantPrompts(3)

	h.must(StreamTurnStart{Claim: testToken})

	if p := h.pending(); p.ID != owed.ID || p.ConfirmedAt.IsZero() {
		t.Errorf("the turn confirmed %+v, want the owed task %s it was started for", p, owed.ID)
	}
}
