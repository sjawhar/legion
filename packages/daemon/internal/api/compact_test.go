package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// compactHarness is the one daemon both doors are driven against, as freshSessionHarness is: the
// operator's bearer and a controller grant on the same server. backend is the deployment's
// `worker_backends` as the routes see it (Options.Backend); nil runs every role on Oh My Pi.
func compactHarness(t *testing.T, backend func(claim.Role) runtime.Backend) *harness {
	t.Helper()
	h := newHarness(t)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime, Trees: h.store,
		Handlers: []intake.Handler{&phaseRestarter{}}, RoleFor: workflow.RoleFor, Backend: backend,
	}).Handler
	return h
}

// compactDoor is one of the two ways a compaction is asked for: `legion claims compact`, which
// names the claim by its token and carries the operator's bearer, and the controller's compact,
// which names it by its issue and role and carries a controller grant. One effect stands behind
// both (api's compact), so every case runs through both.
type compactDoor struct {
	name string
	ask  func(t *testing.T, h *harness, issue string, role claim.Role) *httptest.ResponseRecorder
}

func compactDoors() []compactDoor {
	return []compactDoor{
		{"the operator's claims compact", func(t *testing.T, h *harness, issue string, role claim.Role) *httptest.ResponseRecorder {
			t.Helper()
			return h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(freshSessionToken(t, issue, role))+"/compact", nil)
		}},
		{"the controller's compact", func(t *testing.T, h *harness, issue string, role claim.Role) *httptest.ResponseRecorder {
			t.Helper()
			grant := controllerGrant(t, h)
			return h.request(http.MethodPost, "/legion/v1/claims/compact",
				ClaimRequest{GrantID: grant.GrantID, Issue: issue, Role: string(role)}, nil)
		}},
	}
}

// connected gives the claim a connection to Oh My Pi, which a compaction and a send both need;
// the harness registers none of its own (operator_test.go's forget-session case).
func connected(h *harness, token claim.Token) *fake.Conn {
	conn := fake.NewConn()
	h.conns.Register(token, conn)
	return conn
}

// settled waits for the send the compaction's end started, so what the connection was asked is
// all of what it was asked.
func settled(h *harness, token claim.Token) {
	m, _ := h.supervisor.Machine(token)
	m.Wait()
}

// decodeCompaction reads a 200 answer as the compaction it reports.
func decodeCompaction(t *testing.T, recorder *httptest.ResponseRecorder) CompactResponse {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("the compaction = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var answer CompactResponse
	decodeInto(t, recorder, &answer)
	return answer
}

// A claim idle on the provider-error ladder keeps its session, has it compacted, and is sent the
// task it owes again — once, and only after the compaction, rather than at the end of a ladder
// step counted against the transcript the compaction just shortened. The run of provider-error
// turns it carried goes back to zero, so the project's controller can be told a second time.
func TestACompactionKeepsTheSessionAndSendsTheOwedTaskAgain(t *testing.T) {
	for _, door := range compactDoors() {
		t.Run(door.name, func(t *testing.T) {
			h := compactHarness(t, nil)
			token := idleOnTheLadder(t, h)
			conn := connected(h, token)
			before := h.stored(token)

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			if answer := decodeCompaction(t, recorder); !answer.Compacted || answer.Reason != "" {
				t.Fatalf("the compaction answered %+v, want it compacted with no reason", answer)
			}
			settled(h, token)
			after := h.stored(token)
			if after.Session != before.Session || after.SessionFile != before.SessionFile {
				t.Errorf("the compaction left session %q (%q), want the session it had, %q (%q)",
					after.Session, after.SessionFile, before.Session, before.SessionFile)
			}
			if after.ProviderErrorRun != 0 {
				t.Errorf("the claim's run of provider errors is %d, want 0: the compacted session has had no turn refused", after.ProviderErrorRun)
			}
			if after.State != supervise.StateIdle {
				t.Errorf("the compacted claim is %s, want idle again", after.State)
			}
			if got := len(conn.Compactions()); got != 1 {
				t.Fatalf("Oh My Pi was asked for %d compactions, want one", got)
			}
			prompts := conn.Prompts()
			if len(prompts) != 1 || prompts[0].DeliveryID != before.Pending.ID {
				t.Fatalf("the connection was sent %#v, want the one task the claim owed (%s)", prompts, before.Pending.ID)
			}
		})
	}
}

// The compaction carries the instructions that make Oh My Pi's summary text: without them it
// archives the history onto snapcompact's images, which a worker resumed on them needs a vision
// model to read back (shimwire.Compact). The bound it is asked within is the operation's own
// ten minutes, not the RPC timeout.
func TestACompactionAsksOhMyPiForATextSummary(t *testing.T) {
	h := compactHarness(t, nil)
	token := readyImplementer(t, h)
	conn := connected(h, token)

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	if answer := decodeCompaction(t, recorder); !answer.Compacted {
		t.Fatalf("the compaction answered %+v, want it compacted", answer)
	}
	asked := conn.Compactions()
	if len(asked) != 1 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want one", len(asked))
	}
	if !strings.Contains(asked[0].CustomInstructions, "Write the summary as text.") {
		t.Errorf("the compaction's instructions are %q, want them to ask for a text summary", asked[0].CustomInstructions)
	}
	if asked[0].Bound != runtime.CompactTimeout {
		t.Errorf("the compaction was bounded at %s, want the operation's own %s", asked[0].Bound, runtime.CompactTimeout)
	}
}

// A claim that owes no task is sent none: a root architect between turns has its session
// compacted and nothing else.
func TestACompactionSendsNoTaskToAClaimThatOwesNone(t *testing.T) {
	h := compactHarness(t, nil)
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody()); recorder.Code != http.StatusCreated {
		t.Fatalf("spawn the root = %d, want 201; body %s", recorder.Code, recorder.Body)
	}
	token := freshSessionToken(t, "LEGION-208", claim.RoleArchitect)
	conn := connected(h, token)
	h.ready(token, "ses_architect", 1)

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	if answer := decodeCompaction(t, recorder); !answer.Compacted {
		t.Fatalf("the compaction answered %+v, want it compacted", answer)
	}
	settled(h, token)
	if got := len(conn.Compactions()); got != 1 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want one", got)
	}
	if prompts := conn.Prompts(); len(prompts) != 0 {
		t.Fatalf("the root architect was sent %#v, want nothing: it owed no task", prompts)
	}
}

// A compaction Oh My Pi refuses or skips — a session too small, one already compacted — is
// reported to the caller in Oh My Pi's own words, the owed task goes again all the same, and the
// claim's run of provider-error turns stands: the session is as it was.
func TestACompactionThatIsRefusedOrSkippedTellsTheCallerAndStillSendsTheOwedTask(t *testing.T) {
	for _, reason := range []string{"Nothing to compact (session too small)", "Already compacted"} {
		for _, door := range compactDoors() {
			t.Run(reason+": "+door.name, func(t *testing.T) {
				h := compactHarness(t, nil)
				token := idleOnTheLadder(t, h)
				conn := connected(h, token)
				conn.RefuseCompact(reason)
				owed := h.stored(token).Pending.ID

				recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

				answer := decodeCompaction(t, recorder)
				if answer.Compacted || answer.Reason != reason {
					t.Fatalf("the compaction answered %+v, want it not compacted because %q", answer, reason)
				}
				settled(h, token)
				after := h.stored(token)
				if after.ProviderErrorRun != 1 {
					t.Errorf("the claim's run of provider errors is %d, want the 1 it carried: nothing was compacted", after.ProviderErrorRun)
				}
				prompts := conn.Prompts()
				if len(prompts) != 1 || prompts[0].DeliveryID != owed {
					t.Fatalf("the connection was sent %#v, want the one task the claim owed (%s)", prompts, owed)
				}
			})
		}
	}
}

// A compaction that takes its time answers its caller what Oh My Pi really said, however long
// that is, and the claim is busy meanwhile: a fresh session asked for during it is refused, as
// one asked for during a turn is, and the claim keeps its session.
func TestADelayedCompactionAnswersItsRealResultAndAFreshSessionDuringItIsRefused(t *testing.T) {
	h := compactHarness(t, nil)
	token := readyImplementer(t, h)
	conn := connected(h, token)
	release := conn.HoldCompact()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)
	}()
	waitForCompacting(t, h, token)

	refused := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/retry", RetryRequest{FreshSession: true})

	wantFreshSessionRefusal(t, refused, http.StatusConflict, claimInATurn,
		string(token)+" is in a turn: a fresh session would cut that turn off, so ask again after the turn ends")
	if after := h.stored(token); after.Session != "ses_implementer" {
		t.Errorf("the refused claim records session %q, want the session it had", after.Session)
	}
	release()
	if answer := decodeCompaction(t, <-answered); !answer.Compacted {
		t.Fatalf("the delayed compaction answered %+v, want the real result, compacted", answer)
	}
}

// waitForCompacting waits until the machine reports the claim being compacted, which is what
// makes it busy for every other request.
func waitForCompacting(t *testing.T, h *harness, token claim.Token) {
	t.Helper()
	m, _ := h.supervisor.Machine(token)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c := m.View().Claim; c.Compacting && c.State == supervise.StateWorking {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s never reported itself being compacted", token)
}

// A second compaction of a claim already being compacted is refused: the compaction in flight is
// what it waits for.
func TestASecondCompactionOfAClaimBeingCompactedIsRefused(t *testing.T) {
	h := compactHarness(t, nil)
	token := readyImplementer(t, h)
	conn := connected(h, token)
	release := conn.HoldCompact()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)
	}()
	waitForCompacting(t, h, token)

	second := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	wantFreshSessionRefusal(t, second, http.StatusConflict, claimInATurn,
		string(token)+" is being compacted already: wait for that compaction to end")
	release()
	<-answered
}

// A claim in a turn is refused through either door, and the refusal names the compaction: a
// compaction aborts whatever turn its session is running, and that turn may be the one that
// ends the run of provider errors. A controller whose skill has it compact first must not read
// a refusal telling it to ask for a fresh session, which is the operation it is to try second
// (api's refuseInATurn).
func TestACompactionRefusedForAClaimInATurnNamesTheCompaction(t *testing.T) {
	for _, door := range compactDoors() {
		t.Run(door.name, func(t *testing.T) {
			h := compactHarness(t, nil)
			token := readyImplementer(t, h)
			connected(h, token)
			m, _ := h.supervisor.Machine(token)
			if err := m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
				t.Fatalf("start the turn: %v", err)
			}

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimInATurn,
				string(token)+" is in a turn: a compaction would abort that turn, so ask again after the turn ends")
		})
	}
}

// A claim whose agent is not running has no session to compact, and the refusal names the
// operation that does reach it: a suspended claim, one an operator stopped, and a failed one.
func TestACompactionIsRefusedForASuspendedAStoppedAndAFailedClaimNamingFreshSession(t *testing.T) {
	for _, tc := range freshSessionCases() {
		if tc.name == "a phase worker idle on the provider-error ladder" || tc.name == "an idle root architect" {
			continue
		}
		for _, door := range compactDoors() {
			t.Run(tc.name+": "+door.name, func(t *testing.T) {
				h := compactHarness(t, nil)
				token := tc.start(t, h)
				connected(h, token)
				state := h.stored(token).State

				recorder := door.ask(t, h, tc.issue, tc.role)

				wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimNotRunning,
					string(token)+" is "+string(state)+", so it runs no Oh My Pi session to compact; "+
						freshSessionName+" starts it again instead")
			})
		}
	}
}

// A claim whose launch has not registered yet has no agent to ask at all.
func TestACompactionIsRefusedForAClaimWhoseLaunchIsInFlight(t *testing.T) {
	h := compactHarness(t, nil)
	token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
	if got := h.stored(token).State; got != supervise.StateLaunching {
		t.Fatalf("the launched worker is %s, want launching", got)
	}

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	assertFailure(t, recorder, http.StatusConflict, claimUnsettled)
}

// A role the deployment runs on Claude Code has no Oh My Pi session to compact, and the daemon
// knows which role runs on which harness from its own configuration (Options.Backend).
func TestACompactionIsRefusedForAClaudeCodeClaim(t *testing.T) {
	for _, door := range compactDoors() {
		t.Run(door.name, func(t *testing.T) {
			h := compactHarness(t, func(role claim.Role) runtime.Backend {
				if role == claim.RoleImplementer {
					return runtime.BackendClaude
				}
				return runtime.BackendOmp
			})
			token := readyImplementer(t, h)
			conn := connected(h, token)

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimNotOnOmp,
				string(token)+" runs on Claude Code, which has no Oh My Pi session to compact; "+
					freshSessionName+" is what starts it again")
			if got := len(conn.Compactions()); got != 0 {
				t.Fatalf("the refused claim was asked for %d compactions, want none", got)
			}
		})
	}
}

// `controller` is no role the controller's own tool may name, as it is none for a fresh session:
// the roles are the six a claim can be on, and the project's controller is none of them.
func TestTheControllersCompactRefusesTheRoleController(t *testing.T) {
	h := compactHarness(t, nil)
	grant := controllerGrant(t, h)

	recorder := h.request(http.MethodPost, "/legion/v1/claims/compact",
		ClaimRequest{GrantID: grant.GrantID, Issue: "LEGION-209", Role: "controller"}, nil)

	wantFreshSessionRefusal(t, recorder, http.StatusBadRequest, "INVALID_ROLE",
		"role must be one of architect, planner, implementer, tester, reviewer, merger; "+
			"the project's controller is none of them, and the controller does not compact its own session")
}

// The controller's door refuses a grant that is not the controller's, as every controller route
// does, and names a claim the daemon does not supervise with a 404 of its own.
func TestTheControllersCompactNeedsAControllerGrantAndAClaim(t *testing.T) {
	h := compactHarness(t, nil)
	token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
	registered := h.registered(h.bootToken(token), "ses_implementer")
	var worker GrantResponse
	minted := h.request(http.MethodPost, "/legion/v1/grants", GrantRequest{
		SessionID: "ses_implementer", Secret: registered.Secret, Tree: "LEGION-208", Issue: "LEGION-209",
	}, nil)
	if minted.Code != http.StatusOK {
		t.Fatalf("mint the worker's grant = %d; body %s", minted.Code, minted.Body)
	}
	decodeInto(t, minted, &worker)

	assertFailure(t, h.request(http.MethodPost, "/legion/v1/claims/compact",
		ClaimRequest{GrantID: worker.GrantID, Issue: "LEGION-209", Role: string(claim.RoleImplementer)}, nil),
		http.StatusForbidden, "CONTROLLER_REQUIRED")

	assertFailure(t, h.request(http.MethodPost, "/legion/v1/claims/compact",
		ClaimRequest{GrantID: controllerGrant(t, h).GrantID, Issue: "LEGION-777", Role: string(claim.RoleTester)}, nil),
		http.StatusNotFound, "NO_CLAIM")
}

// A compaction the daemon could not get an answer to — the wait ran out, or the transport failed
// — is reported as a compaction that did not happen, and the owed task goes again: the claim is
// never left working on a compaction nobody will answer for.
func TestACompactionWhoseAnswerNeverCameGivesTheClaimBack(t *testing.T) {
	h := compactHarness(t, nil)
	token := idleOnTheLadder(t, h)
	conn := connected(h, token)
	conn.FailCompact(errors.New("worker-stream: compact: no answer came"))
	owed := h.stored(token).Pending.ID

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	answer := decodeCompaction(t, recorder)
	if answer.Compacted || !strings.Contains(answer.Reason, "no answer came") {
		t.Fatalf("the compaction answered %+v, want it not compacted, naming why", answer)
	}
	settled(h, token)
	if after := h.stored(token); after.State != supervise.StateIdle {
		t.Errorf("the claim is %s, want idle again", after.State)
	}
	if prompts := conn.Prompts(); len(prompts) != 1 || prompts[0].DeliveryID != owed {
		t.Fatalf("the connection was sent %#v, want the one task the claim owed (%s)", prompts, owed)
	}
}

// A claim the machine takes no compaction in is refused in the words of its own state, not as a
// claim in a turn: a claim whose agent registered and is not ready yet has no session to ask,
// and the machine is what says so.
func TestACompactionRefusedForAClaimThatIsNotReadyNamesWhatIsWrong(t *testing.T) {
	h := compactHarness(t, nil)
	token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
	h.registered(h.bootToken(token), "ses_implementer")
	connected(h, token)
	if got := h.stored(token).State; got != supervise.StateRegistered {
		t.Fatalf("the launched worker is %s, want registered", got)
	}

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimUnsettled,
		string(token)+" is registered: a compaction reaches a claim whose agent is ready or idle; ask again when "+
			string(token)+" is one of those")
}

// A ready claim whose shim is not connected is the one refusal the take writes itself: the
// daemon has nothing to ask through, so Oh My Pi was asked nothing and the claim was sent
// nothing. The caller is told the compaction failed rather than told a compaction that did not
// run, whose answer carries the claim's task with it.
func TestACompactionOfAClaimWhoseShimIsNotConnectedSaysNothingWasAsked(t *testing.T) {
	h := compactHarness(t, nil)
	token := readyImplementer(t, h)

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", recorder.Code, recorder.Body)
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	if failure.Code != compactionFailed || !strings.Contains(failure.Error, "shim is not connected") {
		t.Fatalf("the refusal is %+v, want %s naming the shim that is not connected", failure, compactionFailed)
	}
	if after := h.stored(token); after.State != supervise.StateReady {
		t.Errorf("the refused claim is %s, want the ready it was", after.State)
	}
}

// A compaction whose end the store would not write is answered as the store failure it is. Oh My
// Pi compacted the session and the write that gives the claim back did not happen, so the claim
// is left working and the task it owes is unsent: answered 200 it would tell the caller a
// compaction that did not run, whose answer promises the claim is back and its task has gone
// again.
func TestACompactionWhoseEndTheStoreWouldNotWriteAnswersTheStoreFailure(t *testing.T) {
	h := compactHarness(t, nil)
	token := idleOnTheLadder(t, h)
	conn := connected(h, token)
	release := conn.HoldCompact()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)
	}()
	waitForCompacting(t, h, token)

	// The store is gone from here on, so the write the compaction's end makes fails.
	h.store.Pool().Close()
	release()

	recorder := <-answered
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", recorder.Code, recorder.Body)
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	if failure.Code != compactionFailed {
		t.Fatalf("the failure is %+v, want %s: the compaction ended and the claim could not be given back", failure, compactionFailed)
	}
	if got := len(conn.Compactions()); got != 1 {
		t.Fatalf("Oh My Pi was asked for %d compactions, want the one that ran", got)
	}
	if prompts := conn.Prompts(); len(prompts) != 0 {
		t.Fatalf("the connection was sent %#v, want nothing: the claim was never given back", prompts)
	}
}

// A claim that loses its process while its session is being compacted is sent nothing by the
// compaction's end: that end reaches a claim the relaunch has moved past, so it decides nothing.
// The answer says the claim was not given back and leaves the owed task to the moment its agent
// runs again, where a compaction reported as run would have the CLI say the task had gone again
// when nothing had sent it.
func TestACompactionOfAClaimThatLosesItsProcessSaysItWasNotGivenBack(t *testing.T) {
	h := compactHarness(t, nil)
	token := idleOnTheLadder(t, h)
	conn := connected(h, token)
	owed := h.stored(token).Pending.ID
	release := conn.HoldCompact()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)
	}()
	waitForCompacting(t, h, token)

	h.relaunch(token)
	release()

	wantFreshSessionRefusal(t, <-answered, http.StatusConflict, compactionFailed,
		string(token)+" lost its process while its session was being compacted; it was not given back, and the task it owes goes when its agent runs again")
	settled(h, token)
	if prompts := conn.Prompts(); len(prompts) != 0 {
		t.Fatalf("the connection was sent %#v, want nothing: the compaction's end reached a claim its relaunch had moved past", prompts)
	}

	h.ready(token, "ses_implementer", 2)

	settled(h, token)
	if prompts := conn.Prompts(); len(prompts) != 1 || prompts[0].DeliveryID != owed {
		t.Fatalf("the relaunched claim was sent %#v, want the one task it owed (%s)", prompts, owed)
	}
}

// The reason a compaction did not run is words the daemon did not write — Oh My Pi's, and
// through it a summarizer model's own explanation — and it reaches the controller's tool result
// and so its next prompt. The answer carries what the operation needs of it and no more.
func TestACompactionsReasonIsBoundedBeforeItReachesTheCaller(t *testing.T) {
	h := compactHarness(t, nil)
	token := idleOnTheLadder(t, h)
	conn := connected(h, token)
	conn.RefuseCompact("Already compacted. " + strings.Repeat("and here is why at length. ", 200))

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil)

	answer := decodeCompaction(t, recorder)
	if len(answer.Reason) > reasonBound+len("…") {
		t.Fatalf("the answer carried %d bytes of reason, want at most %d", len(answer.Reason), reasonBound+len("…"))
	}
	if !strings.HasPrefix(answer.Reason, "Already compacted.") {
		t.Fatalf("the answer's reason is %q, want it to start with what Oh My Pi said", answer.Reason)
	}
	if !strings.HasSuffix(answer.Reason, "…") {
		t.Fatalf("the answer's reason is %q, want the cut marked so a reader sees the sentence did not end", answer.Reason)
	}
}
