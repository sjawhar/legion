package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// seedIssueAt records key as a root at the given phase, which is where the handoff route reads the
// phase a completion finishes.
func seedIssueAt(t *testing.T, h *harness, key string, at phase.Phase) {
	t.Helper()
	err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(context.Background(), tx, record.Issue{
			Key: key, Tree: key, Project: testProject, Title: key, Phase: at, Generation: 1, Status: "in_progress",
		})
	})
	if err != nil {
		t.Fatalf("seed %s at %s: %v", key, at, err)
	}
}

func TestHandoffCompleteEnforcesRoleSpecificFieldsAndDeduplicatesCommit(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)

	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: tester.grant(t), Summary: "ran the suite", Commit: "aabbcc",
	}, nil), http.StatusBadRequest, "TESTER_VERDICT_REQUIRED")

	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: implementer.grant(t), Summary: "implemented", Verdict: "pass", Commit: "bbccdd",
	}, nil), http.StatusBadRequest, "VERDICT_ROLE_FORBIDDEN")

	worker := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: worker.grant(t), Summary: "reviewed", Ready: true, Commit: "ccddee",
	}, nil), http.StatusBadRequest, "READY_ROLE_FORBIDDEN")

	request := HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: "ddeeff"}
	if recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", request, nil); recorder.Code != http.StatusOK {
		t.Fatalf("handoff = %d: %s", recorder.Code, recorder.Body)
	}
	// A retried completion of the same phase at the same commit changes nothing, and says so.
	request.GrantID = tester.grant(t)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", request, nil), http.StatusConflict, "HANDOFF_ALREADY_RECORDED")
	got := facts.recorded()
	if len(got) != 1 {
		t.Fatalf("handoff facts = %#v, want one deduplicated fact", got)
	}
	fact, ok := got[0].(intake.HandoffComplete)
	if !ok || fact.Role != claim.RoleTester || fact.Verdict != "pass" || fact.Commit != "ddeeff" {
		t.Fatalf("handoff fact = %#v", got[0])
	}
}

// The implementer runs retro and then, after the merge, the production check, and it may report
// both from the one commit carrying its handoff. Each is its own phase's completion.
func TestHandoffCompleteRecordsRetroThenProductionCheckAtOneCommit(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	for _, at := range []phase.Phase{phase.Retro, phase.ProductionCheck} {
		seedIssueAt(t, h, "LEGION-208", at)
		recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
			GrantID: implementer.grant(t), Summary: string(at) + " done", Commit: "c0ffee",
		}, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s handoff = %d: %s", at, recorder.Code, recorder.Body)
		}
	}
	got := facts.recorded()
	if len(got) != 2 {
		t.Fatalf("handoff facts = %#v, want the retro and the production-check completions", got)
	}
	for i, summary := range []string{"retro done", "production_check done"} {
		if fact, ok := got[i].(intake.HandoffComplete); !ok || fact.Summary != summary || fact.Commit != "c0ffee" {
			t.Fatalf("handoff fact %d = %#v, want summary %q at c0ffee", i, got[i], summary)
		}
	}
}

func TestHandoffCompleteRefusesAnUnrecordedIssue(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: implementer.grant(t), Summary: "implemented", Commit: "aabbcc",
	}, nil), http.StatusNotFound, "ISSUE_NOT_FOUND")
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none for an unrecorded issue", got)
	}
}

func TestHandoffCompleteRefusesExpiredAndRevokedGrants(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	h, facts, _ := newArchitectHarness(t, credential.New(func() time.Time { return now }), nil)
	seedIssueAt(t, h, "LEGION-208", phase.Implementing)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	expired := implementer.grant(t)
	now = now.Add(time.Minute)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: expired, Summary: "implemented", Commit: "aabbcc",
	}, nil), http.StatusForbidden, "GRANT_EXPIRED")

	now = now.Add(-time.Minute)
	grant := implementer.grant(t)
	if recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: grant, Summary: "implemented", Commit: "bbccdd",
	}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("first handoff = %d: %s", recorder.Code, recorder.Body)
	}
	// The same command's grant still authenticates a second call; the phase's dedupe answers it.
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: grant, Summary: "implemented", Commit: "bbccdd",
	}, nil), http.StatusConflict, "HANDOFF_ALREADY_RECORDED")

	implementer.replaceRegistration(t)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: grant, Summary: "implemented again", Commit: "ccddee",
	}, nil), http.StatusForbidden, "GRANT_REVOKED")
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("handoff facts = %#v, want only the completion made before the claim was replaced", got)
	}
}

// A merger whose completion was refused READY_REQUIRED corrects it with --ready at the same
// commit. The refusal is recorded as processed, so the corrected call must be a different fact:
// it reaches the workflow, and only a true retry of it is answered "already received". Otherwise
// the issue stays in merging with no way out but a new commit nothing asks for.
func TestHandoffCompleteAppliesTheMergersCorrectedReadyAtTheSameCommit(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, &intake.Refusal{Status: http.StatusConflict, Code: "READY_REQUIRED", Message: "call the legion tool's handoff_complete with ready: true"})
	seedIssueAt(t, h, "LEGION-208", phase.Merging)
	merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: merger.grant(t), Summary: "merge-ready", Commit: "facade",
	}, nil), http.StatusConflict, "READY_REQUIRED")

	facts.mu.Lock()
	facts.refusal = nil
	facts.mu.Unlock()
	corrected := HandoffCompleteRequest{GrantID: merger.grant(t), Summary: "merge-ready", Ready: true, Commit: "facade"}
	if recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", corrected, nil); recorder.Code != http.StatusOK {
		t.Fatalf("corrected READY = %d: %s", recorder.Code, recorder.Body)
	}
	corrected.GrantID = merger.grant(t)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", corrected, nil), http.StatusConflict, "HANDOFF_ALREADY_RECORDED")
	got := facts.recorded()
	if len(got) != 2 {
		t.Fatalf("handoff facts = %#v, want the refused completion and the corrected READY", got)
	}
	if fact, ok := got[1].(intake.HandoffComplete); !ok || !fact.Ready || fact.Commit != "facade" {
		t.Fatalf("corrected fact = %#v, want READY at facade", got[1])
	}
}

// A worker told to wait replies WAITING and its turn ends, which retires the delivery. A notice
// then wakes it — Envoy starts that turn itself — and the completion it reports there is still the
// run's whose task it took: a tester waiting on CI, an implementer waiting on a review, a merger
// waiting on a person. So the claim keeps the run it is serving past the turn that ended, and
// only a claim that has taken no task at all is refused.
func TestHandoffCompleteAcceptsACompletionFromATurnNoDeliveryBacks(t *testing.T) {
	h, _, _ := newArchitectHarness(t, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	// The WAITING turn ends; its delivery is retired with it.
	if err := machine.Handle(context.Background(), supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
		t.Fatalf("end the waiting turn: %v", err)
	}
	if pending := machine.Claim().Pending; pending != nil {
		t.Fatalf("pending after the turn = %+v, want it retired", pending)
	}

	if recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: "aabbcc",
	}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("completion from a notice-started turn = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
}

// A refusal is recorded as processed, under the key the route builds. That key has to name the
// run the completion is attributed to: a completion of the run that is over is refused, and
// without the run in the key the new run's completion of the same phase at the same commit takes
// that key, is answered ALREADY_RECORDED, never reaches the workflow, and leaves the phase
// stalled behind a worker that was told its report was received.
func TestAStaleCompletionDoesNotTakeTheNewRunsKey(t *testing.T) {
	h := newHarness(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, ReviewRoundCap: 3, MaxFixAttempts: 3}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	facts := &factRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine, facts}, Dispatch: &statusRecorder{},
	}).Handler
	h.recordIssue(record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
		Phase: phase.Testing, Generation: 2, Status: "in_progress"})
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	ctx := context.Background()
	complete := func() int {
		t.Helper()
		return h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
			GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: "aabbcc",
		}, nil).Code
	}

	// The worker is still on generation 1's task while the issue has moved to 2: refused.
	if code := complete(); code != http.StatusConflict {
		t.Fatalf("the stale completion = %d, want 409", code)
	}
	// Generation 2's task is delivered and its turn starts; the same report is now the new run's.
	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
		t.Fatalf("end the stale run's turn: %v", err)
	}
	id := "outbox:99"
	if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: tester.token, Task: "the task", ID: id, Generation: 2}); err != nil {
		t.Fatalf("deliver generation 2's task: %v", err)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: tester.token, DeliveryID: id}); err != nil {
		t.Fatalf("start generation 2's turn: %v", err)
	}
	if code := complete(); code != http.StatusOK {
		t.Fatalf("the new run's completion = %d, want 200", code)
	}

	var runs []uint64
	for _, fact := range facts.recorded() {
		if completion, ok := fact.(intake.HandoffComplete); ok {
			runs = append(runs, completion.Generation)
		}
	}
	if len(runs) != 2 || runs[0] != 1 || runs[1] != 2 {
		t.Fatalf("the workflow saw runs %v, want [1 2]", runs)
	}
}

// The wait that follows a task's turn can be hours long — a tester on CI, a merger on a person —
// and a daemon restarted inside it rebuilds the machine from the store. The run the claim serves
// has to be there, or the completion the worker reports when a notice finally wakes it is refused
// by a daemon that has forgotten which run it was working.
func TestTheRunAClaimServesSurvivesARestart(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	if err := machine.Handle(context.Background(), supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
		t.Fatalf("end the task's turn: %v", err)
	}

	if restarted := h.supervisor.restart(t, tester.token); restarted.Claim().ServingGeneration != 1 {
		t.Fatalf("the rebuilt claim serves generation %d, want 1", restarted.Claim().ServingGeneration)
	}
	if recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: "aabbcc",
	}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("completion after the restart = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	recorded := facts.recorded()
	completion, ok := recorded[len(recorded)-1].(intake.HandoffComplete)
	if !ok {
		t.Fatalf("the fact recorded is %T, want a completion", recorded[len(recorded)-1])
	}
	if completion.Generation != 1 {
		t.Fatalf("completion generation = %d, want the run's 1", completion.Generation)
	}
}

// Oh My Pi's own agent_start names no prompt, so a turn that starts after the daemon's
// five-second bound confirms nothing: the task is taken back and waits for the sweep to resend it
// while the worker is already doing it. A worker on its first task has no earlier run to fall
// back on, and its completion would be refused; the task it holds is the only run it can be
// working, so that is what the completion is attributed to.
func TestACompletionFromATurnThatStartedLateNamesTheTaskTheWorkerHolds(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	token, boot := h.launch("LEGION-208", claim.RoleTester)
	session := "ses_tester_late"
	registration := h.registered(boot, session)
	tester := liveClaim{h: h, token: token, session: session, secret: registration.Secret, tree: "LEGION-208", issue: "LEGION-208"}
	machine, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	ctx := context.Background()
	if err := machine.Handle(ctx, supervise.RequestReady{Claim: token, Generation: machine.Claim().Generation, Session: session}); err != nil {
		t.Fatalf("ready: %v", err)
	}
	id := "outbox:42"
	if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: token, Task: "the task", ID: id, Generation: 1}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	// The prompt is acknowledged and no turn starts within the bound, so the task is taken back —
	// keeping the mark, because the turn that has not started may still be this task's. The turn
	// the worker is in then starts late, naming no prompt, and confirms nothing. That leaves the
	// task delivered and unconfirmed, which is the state this completion arrives in.
	if err := machine.Handle(ctx, supervise.PromptAcked{Claim: token, Generation: machine.Claim().Generation, DeliveryID: id}); err != nil {
		t.Fatalf("acknowledge the prompt: %v", err)
	}
	// The bound's take-back is driven in internal/supervise, where the clock moves
	// (TestOnlyTheTurnBoundLeavesTheTaskMarkedAsRead); this route test starts from the state it
	// leaves.
	if p := machine.Claim().Pending; p == nil || p.DeliveredAt.IsZero() || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the acknowledgement = %+v, want it delivered and unconfirmed", p)
	}

	if recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: "aabbcc",
	}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("completion from the late turn = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	recorded := facts.recorded()
	completion, ok := recorded[len(recorded)-1].(intake.HandoffComplete)
	if !ok {
		t.Fatalf("the fact recorded is %T, want a completion", recorded[len(recorded)-1])
	}
	if completion.Generation != 1 {
		t.Fatalf("completion generation = %d, want the task's 1", completion.Generation)
	}
}

// A task the agent refused is not a task it read, whichever way it refused. In a turn of its own
// ("Agent is already processing", the d9194522 sequence) it never saw this prompt; with no turn
// running (a provider answering "No API key found" before any turn began) the prompt did not run
// at all. Either way a completion arriving in whatever turn follows is not that task's run, and a
// claim that has confirmed nothing has no run to attribute it to.
func TestACompletionInAForeignTurnIsNotTheRefusedTasksRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		refusal   string
		turnFirst bool
	}{
		{
			name:      "refused in a turn of the agent's own",
			refusal:   "Agent is already processing. Use steer() or followUp() to queue messages",
			turnFirst: true,
		},
		{name: "refused with no turn running", refusal: "No API key found for provider anthropic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newArchitectHarness(t, nil, nil)
			h.recordIssue(record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
				Phase: phase.Testing, Generation: 2, Status: "in_progress"})
			token, boot := h.launch("LEGION-208", claim.RoleTester)
			session := "ses_tester_foreign"
			registration := h.registered(boot, session)
			tester := liveClaim{h: h, token: token, session: session, secret: registration.Secret, tree: "LEGION-208", issue: "LEGION-208"}
			machine, ok := h.supervisor.Machine(token)
			if !ok {
				t.Fatal("no machine for the tester")
			}
			ctx := context.Background()
			if err := machine.Handle(ctx, supervise.RequestReady{Claim: token, Generation: machine.Claim().Generation, Session: session}); err != nil {
				t.Fatalf("ready: %v", err)
			}
			// The claim has confirmed no task: run 1's prompt was acknowledged and its turn was
			// never seen, and the re-entry's stop retired that task on its way through. The child
			// re-entered at run 2, whose task is delivered and acknowledged.
			second := "outbox:42"
			if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: token, Task: "run 2's task", ID: second, Generation: 2}); err != nil {
				t.Fatalf("deliver run 2's task: %v", err)
			}
			if err := machine.Handle(ctx, supervise.PromptAcked{Claim: token, Generation: machine.Claim().Generation, DeliveryID: second}); err != nil {
				t.Fatalf("acknowledge run 2's prompt: %v", err)
			}
			// The busy refusal is the agent already being in a turn, so that turn starts first;
			// the provider's refusal arrives with no turn running at all, and the notice's turn —
			// the one the completion is reported from — starts after it.
			notice := func() {
				t.Helper()
				if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
					t.Fatalf("the notice's turn: %v", err)
				}
			}
			if tc.turnFirst {
				notice()
			}
			if err := machine.Handle(ctx, supervise.StreamLateRefusal{Claim: token, DeliveryID: second, Error: tc.refusal}); err != nil {
				t.Fatalf("refuse run 2's prompt: %v", err)
			}
			if !tc.turnFirst {
				notice()
			}

			assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
				GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: "aabbcc",
			}, nil), http.StatusConflict, "HANDOFF_NO_RUN")
		})
	}
}

// An operator's task belongs to no run: it carries no generation, and the operator asked for it
// whatever phase the issue is in. Running one must not cost the worker the run it is serving —
// before, the operator's task became the claim's run and the phase stayed open behind a worker
// whose every completion was refused.
func TestAnOperatorsTaskLeavesTheRunTheClaimIsServing(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	ctx := context.Background()
	// The run's task is worked and its turn ends; the run the claim serves is that one.
	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
		t.Fatalf("end the run's turn: %v", err)
	}
	// An operator's task runs and ends in between.
	operator := "outbox:operator"
	if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: tester.token, Task: "say it again", ID: operator}); err != nil {
		t.Fatalf("deliver the operator's task: %v", err)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: tester.token, DeliveryID: operator}); err != nil {
		t.Fatalf("start the operator's turn: %v", err)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
		t.Fatalf("end the operator's turn: %v", err)
	}

	if recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: "aabbcc",
	}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("completion after the operator's task = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	recorded := facts.recorded()
	completion, ok := recorded[len(recorded)-1].(intake.HandoffComplete)
	if !ok {
		t.Fatalf("the fact recorded is %T, want a completion", recorded[len(recorded)-1])
	}
	if completion.Generation != 1 {
		t.Fatalf("completion generation = %d, want the run's 1", completion.Generation)
	}
}

// A confirmation a busy refusal takes back names a turn the delivery never had: the pane was
// working something else, which is the sequence this branch saw live at `d9194522`. The next
// run's task has not been taken, so a completion arriving in that foreign turn is still the old
// run's — and the workflow refuses it, rather than crediting the new run with a completion of a
// task nobody has read.
func TestATakenBackConfirmationDoesNotMoveTheRunTheClaimIsServing(t *testing.T) {
	h, facts, _ := newArchitectHarness(t, nil, nil)
	h.recordIssue(record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
		Phase: phase.Testing, Generation: 2, Status: "in_progress"})
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	ctx := context.Background()
	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
		t.Fatalf("end the first run's turn: %v", err)
	}
	// The next run's task is sent, a foreign turn confirms it, and the agent refuses it as busy.
	id := "outbox:99"
	if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: tester.token, Task: "the next run's task", ID: id, Generation: 2}); err != nil {
		t.Fatalf("deliver the next run's task: %v", err)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: tester.token, DeliveryID: id}); err != nil {
		t.Fatalf("start the foreign turn: %v", err)
	}
	if err := machine.Handle(ctx, supervise.StreamLateRefusal{Claim: tester.token, DeliveryID: id,
		Error: "Agent is already processing. Use steer() or followUp() to queue messages"}); err != nil {
		t.Fatalf("refuse the task: %v", err)
	}
	if p := machine.Claim().Pending; p == nil || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the refusal = %+v, want it waiting unconfirmed", p)
	}

	if recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: "aabbcc",
	}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("handoff = %d; body %s", recorder.Code, recorder.Body)
	}
	recorded := facts.recorded()
	completion, ok := recorded[len(recorded)-1].(intake.HandoffComplete)
	if !ok {
		t.Fatalf("the fact recorded is %T, want a completion", recorded[len(recorded)-1])
	}
	// The workflow refuses generation 1 against an issue on 2; crediting the new run would accept
	// a completion of a task the worker has not read.
	if completion.Generation != 1 {
		t.Fatalf("completion generation = %d, want the old run's 1", completion.Generation)
	}
}

// A claim that has taken no task cannot say which run a completion belongs to: an operator
// running the command by hand, or a worker that never confirmed a delivery.
func TestHandoffCompleteRefusesACompletionFromAClaimThatTookNoTask(t *testing.T) {
	h, _, _ := newArchitectHarness(t, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	token, boot := h.launch("LEGION-208", claim.RoleTester)
	session := "ses_tester_untasked"
	registration := h.registered(boot, session)
	untasked := liveClaim{h: h, token: token, session: session, secret: registration.Secret, tree: "LEGION-208", issue: "LEGION-208"}

	assertFailure(t, h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
		GrantID: untasked.grant(t), Summary: "tests pass", Verdict: "pass", Commit: "aabbcc",
	}, nil), http.StatusConflict, "HANDOFF_NO_RUN")
}

// The run a completion belongs to is the run of the task the worker is working, and the daemon
// reads it from the delivery that turn confirmed rather than from the pane: a live worker is
// handed the next run's task without being relaunched, so its own environment still names the run
// it was launched for. The engine refuses a completion of a run the issue has left
// (TestACompletionFromAnInterruptedRunIsRefused); what this route owes is the attribution.
func TestHandoffCompleteCarriesTheRunOfTheTaskBeingWorked(t *testing.T) {
	for _, tc := range []struct {
		name      string
		delivered uint64
		relaunch  bool
	}{
		{name: "the task of the run the issue is on", delivered: 2},
		{name: "the task of the run that is over", delivered: 1},
		{name: "a worker relaunched inside one run", delivered: 2, relaunch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delivered := tc.delivered
			h, facts, _ := newArchitectHarness(t, nil, nil)
			h.recordIssue(record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: testProject, Title: "LEGION-208",
				Phase: phase.Testing, Generation: 2, Status: "in_progress"})
			tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
			machine, ok := h.supervisor.Machine(tester.token)
			if !ok {
				t.Fatal("no machine for the tester")
			}
			// The pane keeps working; only the task it holds changes.
			if err := machine.Handle(context.Background(), supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
				t.Fatalf("end the first turn: %v", err)
			}
			id := "outbox:99"
			if err := machine.Handle(context.Background(), supervise.RequestDeliver{Claim: tester.token, Task: "the task", ID: id, Generation: delivered}); err != nil {
				t.Fatalf("deliver: %v", err)
			}
			if err := machine.Handle(context.Background(), supervise.StreamTurnStart{Claim: tester.token, DeliveryID: id}); err != nil {
				t.Fatalf("start the turn: %v", err)
			}
			if tc.relaunch {
				// The claim's own launch counter moves — a crash relaunch, a resume — while the
				// run it is serving does not.
				if err := machine.Handle(context.Background(), supervise.RequestSuspend{Claim: tester.token}); err != nil {
					t.Fatalf("suspend: %v", err)
				}
				if err := machine.Handle(context.Background(), supervise.RequestResume{Claim: tester.token}); err != nil {
					t.Fatalf("resume: %v", err)
				}
				// The relaunched pane registers its session again, which is what revokes the old
				// grants; the run the claim is serving is not a property of the process.
				tester.replaceRegistration(t)
			}

			if recorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", HandoffCompleteRequest{
				GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: "aabbcc",
			}, nil); recorder.Code != http.StatusOK {
				t.Fatalf("handoff = %d; body %s", recorder.Code, recorder.Body)
			}

			recordedFacts := facts.recorded()
			if len(recordedFacts) == 0 {
				t.Fatal("no fact reached the intake")
			}
			completion, ok := recordedFacts[len(recordedFacts)-1].(intake.HandoffComplete)
			if !ok {
				t.Fatalf("the fact recorded is %T, want a completion", recordedFacts[len(recordedFacts)-1])
			}
			if completion.Generation != delivered {
				t.Fatalf("the completion names generation %d, want the delivery's %d", completion.Generation, delivered)
			}
		})
	}
}
