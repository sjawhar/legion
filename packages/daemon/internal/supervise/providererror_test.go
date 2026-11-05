package supervise

import (
	"context"
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

// errorTurn ends the turn the claim is working, answers its prompt with the provider error r, and
// posts the turn-outcome reports its plugin sent for that turn. That is the order the daemon
// sees: the agent_end, the prompt's result on the worker stream, and the plugin's report behind
// it, since the report is posted from a handler nothing awaits
// (packages/pi-legion/extensions/legion.ts).
func (h *harness) errorTurn(r StreamPromptResult, reports ...RequestTurnOutcome) {
	h.t.Helper()
	r.Claim, r.AgentInvoked, r.DeliveryID = h.token, true, h.pending().ID
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.must(r)
	for _, report := range reports {
		report.Claim, report.Generation = h.token, h.generation()
		h.must(report)
	}
}

// report posts one turn-outcome report of the claim's current generation.
func (h *harness) report(sequence uint64, outcome claim.TurnOutcome, category string) {
	h.t.Helper()
	h.must(RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: sequence, Outcome: outcome, Category: category})
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
		h.errorTurn(StreamPromptResult{Error: "overloaded_error", Retryable: true})
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

// A rate limit and an overload clear by their status alone, and neither costs the claim a prompt
// failure: chargePrompt retires and then fails a claim past Limits.PromptFailures, which would
// fail every claim of a project at once in a provider outage.
func TestAStatusThatClearsChargesNoPromptBudgetHoweverOftenItHappens(t *testing.T) {
	for _, status := range []int{429, 529} {
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

// A content filter clears whatever the error says about retrying: the plugin has already moved
// the session down its model chain, so the next prompt goes to another model.
func TestANotRetryableErrorARefusedReportNamesClears(t *testing.T) {
	h := workingClaim(t)

	h.errorTurn(StreamPromptResult{Error: "Refusal (reasoning_extraction): I will not help with that."},
		RequestTurnOutcome{Sequence: 1, Outcome: claim.TurnRefused, Category: "reasoning_extraction"})

	if p := h.claim().Pending; p == nil || p.ProviderErrorRetries != 1 {
		t.Fatalf("pending %+v, want the task kept and counted one send", p)
	}
	if _, waiting := h.m.timers[TimerTurnOutcome]; waiting {
		t.Error("the claim still waits for a report; the one it has settled the error")
	}
	h.climbs(1, 30*time.Second)
	if state := h.state(); state != StateWorking {
		t.Fatalf("the claim is %s, want it working the task it was sent again", state)
	}
}

// The one error that holds the issue for its architect: the provider marked it final and the
// claim's own plugin classed the turn a provider error rather than a refusal.
func TestANotRetryableErrorAProviderErrorReportNamesFailsTheClaim(t *testing.T) {
	h := workingClaim(t)

	h.errorTurn(StreamPromptResult{Error: "invalid_request_error", HTTPStatus: 400},
		RequestTurnOutcome{Sequence: 1, Outcome: claim.TurnProviderError})

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
}

// Silence is not a provider saying "this cannot clear". A Claude Code worker, whose bridge sends
// no Retryable and often no status, would otherwise hold its issue at every failed turn.
func TestAProviderErrorNothingSettlesWaitsTwoMinutesAndThenGoesAgain(t *testing.T) {
	h := workingClaim(t)

	h.errorTurn(StreamPromptResult{Error: "the model provider failed"})

	if p := h.pending(); p.ProviderErrorRetries != 0 || p.ProviderErrorNote != "" {
		t.Fatalf("pending %+v, want nothing decided while the report is waited for", p)
	}
	h.advance(2*time.Minute - time.Second)
	h.wantPrompts(1)
	h.advance(time.Second)
	// The wait ends in "this can clear", which is the ladder's first step and not a send of its
	// own: the task goes 30 seconds later.
	h.wantPrompts(1)
	if p := h.pending(); p.ProviderErrorRetries != 1 {
		t.Fatalf("pending %+v, want the wait to have taken the error as one that can clear", p)
	}
	h.climbs(1, 30*time.Second)
}

// A report that lands inside the two minutes decides there and then, either way, rather than
// leaving the claim to wait the rest of it out.
func TestAReportInsideTheWaitDecidesTheProviderErrorAtOnce(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		h := workingClaim(t)
		h.errorTurn(StreamPromptResult{Error: "the model provider failed"})
		h.advance(30 * time.Second)

		h.report(1, claim.TurnRefused, "reasoning_extraction")

		if p := h.pending(); p.ProviderErrorRetries != 1 {
			t.Fatalf("pending %+v, want the report to have decided the error at once", p)
		}
		h.climbs(1, 30*time.Second)
	})
	t.Run("provider error", func(t *testing.T) {
		h := workingClaim(t)
		h.errorTurn(StreamPromptResult{Error: "the model provider failed", HTTPStatus: 400})
		h.advance(30 * time.Second)

		h.report(1, claim.TurnProviderError, "")

		h.wantState(StateFailed)
		if failure := h.claim().Failure; !strings.Contains(failure, "HTTP 400") {
			t.Errorf("the failure is %q, want it to name the status the provider answered", failure)
		}
	})
}

// The turn before's report decides nothing. An overload's own report says provider-error, and it
// reaches the daemon behind the prompt result of the turn that followed it, so reading the
// claim's newest report alone failed this claim and held its issue for an architect over an error
// that clears.
func TestARefusalAfterAnOverloadIsDecidedByTheRefusalsOwnReport(t *testing.T) {
	h := workingClaim(t)

	// The overload clears by its status alone; its own report lands behind that result.
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", HTTPStatus: 529},
		RequestTurnOutcome{Sequence: 1, Outcome: claim.TurnProviderError})
	h.climbs(1, 30*time.Second)

	// The turn the task went again in ends in a content filter, which carries no status and no
	// retryable mark: only this turn's own report can settle it.
	h.errorTurn(StreamPromptResult{Error: "Refusal (reasoning_extraction): I will not help with that."})

	if state := h.state(); state == StateFailed {
		t.Fatalf("the claim failed on the overload's report: %q", h.claim().Failure)
	}
	if p := h.pending(); p.ProviderErrorRetries != 1 {
		t.Fatalf("pending %+v, want nothing decided until the refusal's own report comes", p)
	}

	h.report(2, claim.TurnRefused, "reasoning_extraction")

	if p := h.pending(); p.ProviderErrorRetries != 2 {
		t.Fatalf("pending %+v, want the refusal's own report to have decided the error", p)
	}
	message := h.climbs(2, time.Minute)
	if !strings.Contains(message, "the provider's reasoning_extraction content filter") {
		t.Errorf("the prompt is %q, want it to name the filter this turn's own report gave", message)
	}
}

// The same the other way round: a refusal's report is the turn before's when a malformed request
// follows it. Reading the claim's newest report alone sent that request again for ever, behind a
// prompt that named a content filter the turn never met.
func TestAMalformedRequestAfterARefusalFailsTheClaim(t *testing.T) {
	h := workingClaim(t)

	h.errorTurn(StreamPromptResult{Error: "Refusal (reasoning_extraction): I will not help with that."},
		RequestTurnOutcome{Sequence: 1, Outcome: claim.TurnRefused, Category: "reasoning_extraction"})
	h.climbs(1, 30*time.Second)

	h.errorTurn(StreamPromptResult{Error: "invalid_request_error", HTTPStatus: 400},
		RequestTurnOutcome{Sequence: 2, Outcome: claim.TurnProviderError})

	h.wantState(StateFailed)
	failure := h.claim().Failure
	if !strings.Contains(failure, "HTTP 400") || strings.Contains(failure, "content filter") {
		t.Errorf("the failure is %q, want the status the provider answered and no content filter", failure)
	}
	// Two prompts: the task itself and the one send the refusal earned. The malformed request is
	// not sent again at all.
	h.wantPrompts(2)
}

// A report of an earlier turn that arrives while the wait is armed settles nothing: the wait
// stands, and its expiry decides the error from the failed turn's own facts.
func TestAnEarlierTurnsReportInsideTheWaitSettlesNothing(t *testing.T) {
	h := workingClaim(t)
	h.errorTurn(StreamPromptResult{Error: "Refusal (reasoning_extraction): no."},
		RequestTurnOutcome{Sequence: 1, Outcome: claim.TurnRefused, Category: "reasoning_extraction"})
	h.climbs(1, 30*time.Second)

	// The turn the task went again in fails in a way nothing settles, so the daemon waits for the
	// report of that turn.
	h.errorTurn(StreamPromptResult{Error: "the model provider failed"})
	if _, waiting := h.m.timers[TimerTurnOutcome]; !waiting {
		t.Fatal("no wait for this turn's own report is armed")
	}

	// The first turn's report again, as a send of it that overlapped the turn after it would
	// arrive.
	h.report(1, claim.TurnRefused, "reasoning_extraction")

	if p := h.pending(); p.ProviderErrorRetries != 1 {
		t.Fatalf("pending %+v, want the earlier turn's report to settle nothing", p)
	}
	if _, waiting := h.m.timers[TimerTurnOutcome]; !waiting {
		t.Fatal("the earlier turn's report ended the wait for this turn's own")
	}

	// The wait runs out, which takes the error as one that can clear, and the sentence it sends
	// the task behind names no filter: no report of this turn ever came.
	h.advance(turnOutcomeWait)
	if p := h.pending(); p.ProviderErrorRetries != 2 {
		t.Fatalf("pending %+v, want the wait's expiry to have decided the error", p)
	}
	message := h.climbs(2, time.Minute)
	if strings.Contains(message, "content filter") {
		t.Errorf("the prompt is %q, want it built from the failed turn's own facts", message)
	}
}

// The wait is the task's own, fenced on the delivery id the arming recorded. A task that replaced
// it is held by nothing — and the task the provider error took back is never sent again, because
// its issue left the phase it was queued for.
func TestANewerTaskQueuedWhileTheLadderWaitsGoesAtOnce(t *testing.T) {
	at := phase.Implementing
	h := workingClaim(t, func(d *Deps) {
		d.PhaseHolds = func(_ context.Context, _ string, p phase.Phase, _ uint64) (bool, error) { return p == at, nil }
	})
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", Retryable: true})
	owed := h.pending().ID

	// The issue moves to testing while the ladder waits: the owed task is the implementer's, and
	// the ladder's own firing drops it rather than sending it.
	at = phase.Testing
	h.advance(30 * time.Second)
	h.wantPrompts(1)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending %+v, want the task of a phase the issue has left dropped", *p)
	}

	h.must(RequestDeliver{Claim: testToken, Task: "test the change", Phase: phase.Testing, Generation: 7, Assignment: "A1"})

	sent := h.wantPrompts(2)[1]
	if sent.Message != "test the change" || sent.DeliveryID == owed {
		t.Fatalf("sent %+v, want the newer task at once under an id of its own", sent)
	}
}

// A wait left armed names the task it was armed for. The guard reads that id rather than the
// kind alone, so a task that replaced the one the wait is for is held by nothing.
func TestAWaitLeftArmedForAnOlderTaskHoldsNothing(t *testing.T) {
	h := workingClaim(t)

	h.errorTurn(StreamPromptResult{Error: "overloaded_error", Retryable: true})

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
func TestTheResentPromptNamesTheCategoryOrTheStatusAndNoProviderText(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  StreamPromptResult
		reports []RequestTurnOutcome
		// contains is what the sentence says, and written is the provider's own text, which no
		// prompt may carry.
		contains string
		written  string
	}{
		{
			name:     "a refusal names its category",
			result:   StreamPromptResult{Error: "Refusal (reasoning_extraction): I will not help you extract that."},
			reports:  []RequestTurnOutcome{{Sequence: 1, Outcome: claim.TurnRefused, Category: "reasoning_extraction"}},
			contains: "the provider's reasoning_extraction content filter",
			written:  "I will not help you extract that.",
		},
		{
			name:     "an error with no category names its status",
			result:   StreamPromptResult{Error: "Overloaded: upstream capacity is gone", HTTPStatus: 529},
			contains: "a provider error (HTTP 529)",
			written:  "upstream capacity is gone",
		},
		{
			name:     "an error with neither names only that the provider failed",
			result:   StreamPromptResult{Error: "the upstream connection dropped", Retryable: true},
			contains: "a provider error.",
			written:  "the upstream connection dropped",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := workingClaim(t)

			h.errorTurn(tc.result, tc.reports...)
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
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", Retryable: true})
	h.climbs(1, 30*time.Second)
	h.errorTurn(StreamPromptResult{Error: "overloaded_error", Retryable: true})
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
		h.errorTurn(StreamPromptResult{Error: "Refusal (reasoning_extraction): no.", Model: "claude-opus-4-8"},
			RequestTurnOutcome{Sequence: uint64(turn + 1), Outcome: claim.TurnRefused, Category: "reasoning_extraction"})
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
	// The notice goes when the error arrives, which is before the report of the turn it ended
	// (providererror.go, ownReport), so its cause is what the error's own fields give: a refusal
	// carries no status, and the category the report names is not there to be read. The turn
	// before's category is not read for it either.
	for _, want := range []string{"6 turns in a row", "claude-opus-4-8", "ended in a provider error"} {
		if !strings.Contains(notice.reason, want) {
			t.Errorf("the reason is %q, want it to name %q", notice.reason, want)
		}
	}
	if strings.Contains(notice.reason, "content filter") {
		t.Errorf("the reason is %q, want no category: no report of the turn it is about had come", notice.reason)
	}
	if got := h.claim().ProviderErrorRun; got != 7 {
		t.Errorf("the run of provider errors is %d, want 7: the notice bounds nothing", got)
	}
}
