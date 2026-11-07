package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// freshSessionHarness is the one daemon both doors are driven against: the operator's bearer and
// a controller grant on the same server, with the workflow's facts recorded and the role of each
// phase known (workflow.RoleFor), which the start of a phase worker whose process is gone reads.
func freshSessionHarness(t *testing.T, handler intake.Handler) *harness {
	t.Helper()
	return freshSessionHarnessStopping(t, handler, context.Background(), NewRouteDecisions())
}

// freshSessionHarnessStopping is freshSessionHarness with the daemon's stop and the set the
// routes record their decisions in handed to the test (Options.Stopping, Options.Decisions),
// which is how the race below is driven.
func freshSessionHarnessStopping(t *testing.T, handler intake.Handler, stopping context.Context, decisions *RouteDecisions) *harness {
	t.Helper()
	h := newHarness(t)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime, Trees: h.store,
		Handlers: []intake.Handler{handler}, RoleFor: workflow.RoleFor,
		Stopping: stopping, Decisions: decisions,
	}).Handler
	return h
}

// freshSessionDoor is one of the two ways a fresh session is asked for: the operator's
// `legion claims retry --fresh-session`, which names the claim by its token, and the
// controller's fresh_session, which names it by its issue and role and carries a controller
// grant. One effect stands behind both (api's freshSession), so every case runs through both.
type freshSessionDoor struct {
	name string
	ask  func(t *testing.T, h *harness, issue string, role claim.Role) *httptest.ResponseRecorder
}

func freshSessionDoors() []freshSessionDoor {
	return []freshSessionDoor{
		{"the operator's retry --fresh-session", func(t *testing.T, h *harness, issue string, role claim.Role) *httptest.ResponseRecorder {
			t.Helper()
			return h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(freshSessionToken(t, issue, role))+"/retry",
				RetryRequest{FreshSession: true})
		}},
		{"the controller's fresh_session", func(t *testing.T, h *harness, issue string, role claim.Role) *httptest.ResponseRecorder {
			t.Helper()
			grant := controllerGrant(t, h)
			return h.request(http.MethodPost, "/legion/v1/claims/fresh-session",
				ClaimRequest{GrantID: grant.GrantID, Issue: issue, Role: string(role)}, nil)
		}},
	}
}

func freshSessionToken(t *testing.T, issue string, role claim.Role) claim.Token {
	t.Helper()
	token, err := claim.NewToken(testProject, issue, role)
	if err != nil {
		t.Fatalf("token of %s's %s: %v", issue, role, err)
	}
	return token
}

// idleOnTheLadder is the claim the provider-errors notice names: a phase worker that worked its
// task, whose turn ended in a provider error, so the task is kept and the claim waits out the
// ladder with nothing running. It is live and idle, which is why a recovery that reached only
// stopped and failed claims would miss it.
func idleOnTheLadder(t *testing.T, h *harness) claim.Token {
	t.Helper()
	token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
	h.ready(token, "ses_implementer", 1)
	workingIssue(t, h, phase.Implementing)
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver",
		DeliverRequest{Task: "Continue implementing."}); recorder.Code != http.StatusOK {
		t.Fatalf("deliver = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	m, _ := h.supervisor.Machine(token)
	pending := m.Claim().Pending
	if pending == nil {
		t.Fatal("the delivered task is on no claim")
	}
	if err := m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token, DeliveryID: pending.ID}); err != nil {
		t.Fatalf("start the turn: %v", err)
	}
	if err := m.Handle(h.ctx, supervise.StreamPromptResult{
		Claim: token, DeliveryID: pending.ID, AgentInvoked: true, Error: "overloaded_error", HTTPStatus: 529,
	}); err != nil {
		t.Fatalf("the turn's provider error: %v", err)
	}
	if err := m.Handle(h.ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
		t.Fatalf("end the turn: %v", err)
	}
	if c := m.Claim(); c.State != supervise.StateIdle || c.ProviderErrorRun != 1 {
		t.Fatalf("the claim is %s with a run of %d provider errors, want it idle on the ladder after one", c.State, c.ProviderErrorRun)
	}
	return token
}

// readyImplementer is LEGION-209's implementer registered on a session and ready, the state
// every live case below starts from.
func readyImplementer(t *testing.T, h *harness) claim.Token {
	t.Helper()
	token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
	h.ready(token, "ses_implementer", 1)
	workingIssue(t, h, phase.Implementing)
	return token
}

// suspendedImplementer is readyImplementer suspended by source: what the daemon's own suspend
// leaves, and what the release of an idle issue pod leaves. Neither suspension holds against a
// rule, so a start the phase's restart queues can reach such a claim while this route works.
func suspendedImplementer(t *testing.T, h *harness, by supervise.SuspendSource) claim.Token {
	t.Helper()
	token := readyImplementer(t, h)
	m, _ := h.supervisor.Machine(token)
	if err := m.Handle(h.ctx, supervise.RequestSuspend{Claim: token, Reason: string(by) + " suspended the claim", By: by}); err != nil {
		t.Fatalf("suspend %s by %s: %v", token, by, err)
	}
	if got := h.stored(token); got.State != supervise.StateSuspended || got.SuspendedBy != by {
		t.Fatalf("the worker is %s suspended by %q, want it suspended by %q", got.State, got.SuspendedBy, by)
	}
	return token
}

// freshSessionCase is one claim the operation reaches, in the state `c-r3-operator` names.
type freshSessionCase struct {
	name  string
	issue string
	role  claim.Role
	// start leaves the claim in that state and answers its token.
	start func(t *testing.T, h *harness) claim.Token
	// facts is how many workflow facts the start applies: a phase worker that comes back owing
	// no task is started through its issue's phase, which is what hands it that phase's task,
	// and a claim that owes one keeps it and needs no phase.
	facts int
	// launches is whether the request itself asks the runtime to launch the claim: the daemon
	// resumes a claim it suspended, where a claim the workflow's phase start relaunches is
	// launched by the outbox lane long after this answer.
	launches bool
}

func freshSessionCases() []freshSessionCase {
	return []freshSessionCase{
		{
			name: "a phase worker idle on the provider-error ladder", issue: "LEGION-209", role: claim.RoleImplementer, launches: true,
			start: idleOnTheLadder,
		},
		{
			name: "a suspended phase worker", issue: "LEGION-209", role: claim.RoleImplementer, facts: 1, launches: true,
			start: func(t *testing.T, h *harness) claim.Token {
				t.Helper()
				token := readyImplementer(t, h)
				if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil); recorder.Code != http.StatusOK {
					t.Fatalf("suspend = %d, want 200; body %s", recorder.Code, recorder.Body)
				}
				return token
			},
		},
		{
			name: "a phase worker an operator stopped", issue: "LEGION-209", role: claim.RoleImplementer, facts: 1,
			start: func(t *testing.T, h *harness) claim.Token {
				t.Helper()
				token := readyImplementer(t, h)
				if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/stop", nil); recorder.Code != http.StatusOK {
					t.Fatalf("stop = %d, want 200; body %s", recorder.Code, recorder.Body)
				}
				return token
			},
		},
		{
			name: "a failed phase worker", issue: "LEGION-209", role: claim.RoleImplementer, facts: 1,
			start: func(t *testing.T, h *harness) claim.Token {
				t.Helper()
				token := readyImplementer(t, h)
				refused := fake.SpawnResult{Err: errors.New("the runtime refused the launch")}
				h.runtime.ScriptResume(refused, refused, refused)
				m, _ := h.supervisor.Machine(token)
				loc := m.Claim().Locator
				if loc == nil {
					t.Fatal("the ready worker runs no process")
				}
				if err := m.Handle(h.ctx, supervise.RuntimeObservation{
					Observation: runtime.Observation{Locator: *loc, Kind: runtime.Gone},
				}); err == nil {
					t.Fatal("the relaunches the runtime refused answered no error")
				}
				if got := m.Claim().State; got != supervise.StateFailed {
					t.Fatalf("the worker is %s, want failed", got)
				}
				return token
			},
		},
		{
			name: "an idle root architect", issue: "LEGION-208", role: claim.RoleArchitect, launches: true,
			start: func(t *testing.T, h *harness) claim.Token {
				t.Helper()
				if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody()); recorder.Code != http.StatusCreated {
					t.Fatalf("spawn the root = %d, want 201; body %s", recorder.Code, recorder.Body)
				}
				token := freshSessionToken(t, "LEGION-208", claim.RoleArchitect)
				h.ready(token, "ses_architect", 1)
				m, _ := h.supervisor.Machine(token)
				if err := m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
					t.Fatalf("start the architect's turn: %v", err)
				}
				if err := m.Handle(h.ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
					t.Fatalf("end the architect's turn: %v", err)
				}
				if got := m.Claim().State; got != supervise.StateIdle {
					t.Fatalf("the root architect is %s, want idle", got)
				}
				return token
			},
		},
	}
}

// Every claim of the project that is not in a turn gets a new session through either door: the
// claim forgets the session it recorded and starts again, and the run of provider errors it
// carried starts again with it, so the project's controller can be told a second time. A phase
// worker that comes back owing no task is started through its issue's phase, which is what
// hands it that phase's task: without it a tester waiting on CI, or a worker an idle release
// suspended, would come back in a new session with nothing to do. A claim that still owes a
// task keeps it, and a root architect works no phase at all.
func TestAFreshSessionStartsEveryClaimThatIsNotInATurn(t *testing.T) {
	for _, door := range freshSessionDoors() {
		for _, tc := range freshSessionCases() {
			t.Run(door.name+": "+tc.name, func(t *testing.T) {
				facts := &phaseRestarter{}
				h := freshSessionHarness(t, facts)
				token := tc.start(t, h)
				before := h.stored(token)
				if before.Session == "" {
					t.Fatalf("the %s records no session to leave behind", tc.name)
				}
				calls := len(h.runtime.Calls())

				recorder := door.ask(t, h, tc.issue, tc.role)

				if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
					t.Fatalf("the fresh session = %d, want 200 or 202; body %s", recorder.Code, recorder.Body)
				}
				after := h.stored(token)
				if after.Session != "" || after.SessionFile != "" {
					t.Errorf("the fresh session left session %q (%q) on the claim, want both forgotten", after.Session, after.SessionFile)
				}
				if after.ProviderErrorRun != 0 {
					t.Errorf("the claim's run of provider errors is %d, want 0: the new session has had no turn refused", after.ProviderErrorRun)
				}
				if applied := facts.recorded(); len(applied) != tc.facts {
					t.Fatalf("the fresh session applied %#v, want %d facts", applied, tc.facts)
				}
				if tc.facts > 0 {
					if fact, ok := facts.recorded()[0].(intake.RestartPhase); !ok || fact.Issue != tc.issue || fact.Role != tc.role {
						t.Errorf("the fresh session applied %#v, want RestartPhase{%s, %s}", facts.recorded()[0], tc.issue, tc.role)
					}
				}
				if !tc.launches {
					return
				}
				// The daemon started the claim itself, and with no recorded session the launch
				// is a spawn rather than a resume of the session being dropped.
				launches := h.runtime.Calls()[calls:]
				if len(launches) == 0 {
					t.Fatalf("the fresh session asked the runtime nothing, want the claim launched again")
				}
				for _, call := range launches {
					if call.Method == "Resume" {
						t.Errorf("the fresh session resumed %q, want a new session spawned", call.Spec.ResumeSessionFile)
					}
				}
			})
		}
	}
}

// refusedRestart is the phase's restart the workflow refuses, in the words it refuses it with.
func refusedRestart() *phaseRestarter {
	facts := &phaseRestarter{}
	facts.refusal = &intake.Refusal{
		Status: http.StatusConflict, Code: "PHASE_REFUSED",
		Message: "LEGION-209 works no phase the implementer can be restarted on",
	}
	return facts
}

// freshSessionNote is the sentence both doors carry when the claim is back in a new session and
// nothing restarted its phase (api.FreshSessionResponse, api.OperatorClaim), and empty when the
// answer carries none. The operator's door answers the whole claim, so this reads the one member
// rather than the body.
func freshSessionNote(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var answer struct {
		PhaseNotRestarted string `json:"phaseNotRestarted"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", recorder.Body, err)
	}
	return answer.PhaseNotRestarted
}

// A phase worker that owes no task is started through its issue's phase, and a restart the
// workflow refuses is no refusal of the fresh session: the resume runs first, so the claim is
// back in a new session whatever the workflow answers, and that is what the caller is told. The
// answer names the refused restart, because nothing in the claim's state records that its phase
// was not restarted. A refusal here instead would tell the caller nothing happened for a worker
// this route had just relaunched, and, for the worker it suspended itself, leave it stopped
// behind an operator's hold no rule of the daemon's can lift.
func TestAFreshSessionStartsTheClaimWhenThePhaseRestartIsRefused(t *testing.T) {
	for _, door := range freshSessionDoors() {
		for _, tc := range []struct {
			name  string
			start func(t *testing.T, h *harness) claim.Token
		}{
			{"a claim the operator suspended", func(t *testing.T, h *harness) claim.Token {
				t.Helper()
				token := readyImplementer(t, h)
				if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil); recorder.Code != http.StatusOK {
					t.Fatalf("suspend = %d, want 200; body %s", recorder.Code, recorder.Body)
				}
				return token
			}},
			{"a claim the daemon suspended", func(t *testing.T, h *harness) claim.Token {
				t.Helper()
				return suspendedImplementer(t, h, supervise.SuspendedByDaemon)
			}},
			{"a claim an idle release suspended", func(t *testing.T, h *harness) claim.Token {
				t.Helper()
				return suspendedImplementer(t, h, supervise.SuspendedByReleased)
			}},
			{"a ready worker owing no task", readyImplementer},
		} {
			t.Run(door.name+": "+tc.name, func(t *testing.T) {
				facts := refusedRestart()
				h := freshSessionHarness(t, facts)
				token := tc.start(t, h)
				spawns, resumes := len(h.runtime.CallsOf("Spawn")), len(h.runtime.CallsOf("Resume"))

				recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

				if recorder.Code != http.StatusOK {
					t.Fatalf("the fresh session = %d, want 200: the claim started, and only its phase did not; body %s", recorder.Code, recorder.Body)
				}
				want := string(token) + " is back in a new session owing no task, and nothing restarted its phase: " +
					"LEGION-209 works no phase the implementer can be restarted on"
				if got := freshSessionNote(t, recorder); got != want {
					t.Errorf("the answer says %q, want %q", got, want)
				}
				if applied := facts.recorded(); len(applied) != 1 {
					t.Fatalf("the fresh session applied %#v, want the phase's restart alone", applied)
				}
				after := h.stored(token)
				if after.State != supervise.StateLaunching || after.Session != "" || after.SessionFile != "" {
					t.Errorf("the claim is %s recording session %q (%q), want it launching in a new session", after.State, after.Session, after.SessionFile)
				}
				if got := len(h.runtime.CallsOf("Spawn")); got != spawns+1 {
					t.Errorf("the fresh session made %d spawns, want one more than the %d before it", got, spawns)
				}
				if got := len(h.runtime.CallsOf("Resume")); got != resumes {
					t.Errorf("the fresh session resumed a session %d times, want the %d before it: the claim records none", got, resumes)
				}
			})
		}
	}
}

// ruleResumingRestart is the phase's restart with the outbox lane behind it: the fact's own
// commit hook posts the rule's resume that lane posts once the fact commits (intake.OnCommit,
// which is how the lane is woken), and that resume is what relaunches a claim no operator
// suspended. The hook runs after the commit, as the lane does, because a machine event posted
// inside the fact's transaction would wait on the fact lock that transaction holds.
type ruleResumingRestart struct {
	phaseRestarter
	resume func() error
	posted bool
	failed error
}

func (r *ruleResumingRestart) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	intake.OnCommit(ctx, func() {
		r.posted = true
		r.failed = r.resume()
	})
	return r.phaseRestarter.Apply(ctx, tx, fact)
}

// The resume runs before the phase's restart, so the start that restart queues finds the claim
// already relaunched and does nothing. Run the other way round, the lane's rule resume reached
// the suspended claim first — neither the daemon's suspension nor an idle release's holds
// against a rule — and this route's own resume then met a launching claim, which the machine
// refuses: the caller read a 409 for a claim that was relaunching.
func TestAFreshSessionOutrunsTheRuleResumeThePhaseFactWakes(t *testing.T) {
	for _, door := range freshSessionDoors() {
		for _, by := range []supervise.SuspendSource{supervise.SuspendedByDaemon, supervise.SuspendedByReleased} {
			t.Run(door.name+": suspended by "+string(by), func(t *testing.T) {
				facts := &ruleResumingRestart{}
				h := freshSessionHarness(t, facts)
				token := suspendedImplementer(t, h, by)
				m, _ := h.supervisor.Machine(token)
				facts.resume = func() error {
					return m.Handle(h.ctx, supervise.RequestRuleResume{Claim: token, Reason: "it owns its issue's phase"})
				}
				spawns := len(h.runtime.CallsOf("Spawn"))

				recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

				if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
					t.Fatalf("the fresh session = %d, want 200 or 202; body %s", recorder.Code, recorder.Body)
				}
				if !facts.posted {
					t.Fatal("the fresh session applied no fact, so no lane's resume raced it")
				}
				if facts.failed != nil {
					t.Fatalf("the lane's resume under the fact: %v", facts.failed)
				}
				if note := freshSessionNote(t, recorder); note != "" {
					t.Errorf("the answer says %q, want nothing: the phase was restarted", note)
				}
				after := h.stored(token)
				if after.State != supervise.StateLaunching || after.Session != "" {
					t.Errorf("the claim is %s recording session %q, want it launching in a new session", after.State, after.Session)
				}
				if got := len(h.runtime.CallsOf("Spawn")); got != spawns+1 {
					t.Errorf("the fresh session made %d spawns, want one more than the %d before it: the lane's resume finds the claim launching", got, spawns)
				}
			})
		}
	}
}

// A claim in a turn is refused through either door, word for word: suspending it would cut the
// turn off, and that turn may be the one that ends the run of provider errors. The claim is left
// exactly as it was, its session included.
func TestAFreshSessionIsRefusedForAClaimInATurn(t *testing.T) {
	for _, door := range freshSessionDoors() {
		t.Run(door.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			token := readyImplementer(t, h)
			m, _ := h.supervisor.Machine(token)
			if err := m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
				t.Fatalf("start the turn: %v", err)
			}

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimInATurn,
				string(token)+" is in a turn: a fresh session would cut that turn off, so ask again after the turn ends")
			if after := h.stored(token); after.Session != "ses_implementer" || after.State != supervise.StateWorking {
				t.Errorf("the refused claim is %s recording session %q, want it working on the session it had", after.State, after.Session)
			}
		})
	}
}

// turnStartingStop is a daemon that is not stopping, and the one seam a test has for starting a
// turn in the instant the fresh session's suspend races: the suspend asks Options.Stopping
// whether the daemon is stopping (api's stopped) on the statement between freshSession's read of
// the claim's state and the machine's lock, and nothing else either door runs asks it at all. The
// first question starts the turn and then answers that the daemon runs on, so the suspension
// reaches a machine whose claim is working. Production code knows nothing of it, and the check on
// the route's own decision keeps it honest: a question asked before the suspend began its
// decision is one from somewhere else, and the test fails rather than proving the wrong gap.
type turnStartingStop struct {
	context.Context
	decisions *RouteDecisions
	token     claim.Token
	// startTurn runs once, inside the gap; failed is what it reported, which the test reads
	// after the request is answered.
	startTurn func() error
	once      sync.Once
	failed    error
}

func (s *turnStartingStop) Err() error {
	s.once.Do(func() {
		if !s.decisions.Holds(s.token) {
			s.failed = fmt.Errorf("the daemon's stop was read outside the suspend's own decision of %s, so this test no longer rides the gap between the route's read and the machine's lock", s.token)
			return
		}
		s.failed = s.startTurn()
	})
	return s.Context.Err()
}

// A turn that starts after the fresh session read the claim in none is refused through either
// door, in the words the read a moment earlier would have given, and the claim is left exactly as
// that read found it: working on the session it had, no phase fact applied, and no suspension
// held. The suspension the route posts may not be held (supervise.RequestSuspend.NeverHold),
// because the answer the caller gets says nothing happened: held, it would stop the worker when
// its turn ended, behind that answer, with the session it was told was kept.
func TestAFreshSessionRefusedForATurnThatStartedUnderItHoldsNoSuspension(t *testing.T) {
	for _, door := range freshSessionDoors() {
		t.Run(door.name, func(t *testing.T) {
			facts := &phaseRestarter{}
			decisions := NewRouteDecisions()
			race := &turnStartingStop{Context: context.Background(), decisions: decisions}
			h := freshSessionHarnessStopping(t, facts, race, decisions)
			token := readyImplementer(t, h)
			m, _ := h.supervisor.Machine(token)
			race.token = token
			race.startTurn = func() error { return m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}) }

			recorder := door.ask(t, h, "LEGION-209", claim.RoleImplementer)

			if race.failed != nil {
				t.Fatalf("start the turn under the route: %v", race.failed)
			}
			wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimInATurn,
				string(token)+" is in a turn: a fresh session would cut that turn off, so ask again after the turn ends")
			if c := m.Claim(); c.State != supervise.StateWorking || c.SuspensionHeld || c.Session != "ses_implementer" {
				t.Fatalf("the refused claim is %s recording session %q, suspension held %t; want it working on the session it had with none held",
					c.State, c.Session, c.SuspensionHeld)
			}
			if applied := facts.recorded(); len(applied) != 0 {
				t.Errorf("the refused fresh session applied %#v, want no fact", applied)
			}

			if err := m.Handle(h.ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
				t.Fatalf("end the turn: %v", err)
			}

			if c := m.Claim(); c.State != supervise.StateIdle {
				t.Fatalf("the claim is %s once its turn ended, want it idle: the refused suspension stopped nothing", c.State)
			}
		})
	}
}

// `controller` is no role the controller's own tool may name: the roles are the six a claim can
// be on, and the project's controller is none of them. The refusal comes before the grant is
// redeemed, as every check of this route's body does.
func TestTheControllersFreshSessionRefusesTheRoleController(t *testing.T) {
	h := freshSessionHarness(t, &phaseRestarter{})
	grant := controllerGrant(t, h)

	recorder := h.request(http.MethodPost, "/legion/v1/claims/fresh-session",
		ClaimRequest{GrantID: grant.GrantID, Issue: "LEGION-209", Role: "controller"}, nil)

	wantFreshSessionRefusal(t, recorder, http.StatusBadRequest, "INVALID_ROLE",
		"role must be one of architect, planner, implementer, tester, reviewer, merger; "+
			"the project's controller is none of them, and its own claim takes no fresh session")
}

// The controller's own claim is refused through the operator's door too, which names a claim by
// its token and so can name the controller's: the controller is started by its keeper, and a
// fresh session of it would cut the project's one controller off.
func TestAFreshSessionRefusesTheControllersOwnClaim(t *testing.T) {
	h := freshSessionHarness(t, &phaseRestarter{})
	// The refusal comes before any state is read, so the launched claim needs no agent: the
	// controller is refused whatever it is doing.
	token, _ := h.launchController()

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/retry", RetryRequest{FreshSession: true})

	assertFailure(t, recorder, http.StatusConflict, "NOT_THE_CONTROLLER")
}

// A fresh session is refused for a claim no launch has registered yet: its suspension would be
// held for the agent it is still waiting for, and the request would answer long after the claim
// moved on.
func TestAFreshSessionIsRefusedForAClaimWhoseLaunchIsInFlight(t *testing.T) {
	h := freshSessionHarness(t, &phaseRestarter{})
	token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
	if got := h.stored(token).State; got != supervise.StateLaunching {
		t.Fatalf("the launched worker is %s, want launching", got)
	}

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/retry", RetryRequest{FreshSession: true})

	assertFailure(t, recorder, http.StatusConflict, claimUnsettled)
}

// The controller's door refuses a grant that is not the controller's, as every controller route
// does, and names a claim the daemon does not supervise with a 404 of its own.
func TestTheControllersFreshSessionNeedsAControllerGrantAndAClaim(t *testing.T) {
	h := freshSessionHarness(t, &phaseRestarter{})
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

	assertFailure(t, h.request(http.MethodPost, "/legion/v1/claims/fresh-session",
		ClaimRequest{GrantID: worker.GrantID, Issue: "LEGION-209", Role: string(claim.RoleImplementer)}, nil),
		http.StatusForbidden, "CONTROLLER_REQUIRED")

	assertFailure(t, h.request(http.MethodPost, "/legion/v1/claims/fresh-session",
		ClaimRequest{GrantID: controllerGrant(t, h).GrantID, Issue: "LEGION-777", Role: string(claim.RoleTester)}, nil),
		http.StatusNotFound, "NO_CLAIM")
}

// wantFreshSessionRefusal holds a refusal to its status, its code and its sentence: the shape
// both doors give the refusals the fresh session writes itself.
func wantFreshSessionRefusal(t *testing.T, recorder *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, status, recorder.Body)
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	if failure.Code != code || failure.Error != message {
		t.Fatalf("refusal = %+v, want %q and %q", failure, code, message)
	}
}
