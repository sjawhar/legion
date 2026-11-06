package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ClaimRequest is what either of the controller's two claim recoveries names: the claim on Role
// of Issue, under the caller's controller grant. Role is a workflow role or `architect`; the
// project controller is no role here, so the controller's own claim cannot be named at all
// (claim.IsRole, whose six roles the controller is not one of). The fresh session and the
// compaction take the same three members, so they take one type (controllerClaim).
type ClaimRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
	Role    string `json:"role"`
}

// FreshSessionResponse confirms the claim left its session behind and its start ran: either the
// daemon relaunched it here, or the phase's restart it queued will. The controller is told
// nothing else, since what it does next it reads in the state.
type FreshSessionResponse struct{}

// The refusals both recoveries write themselves, which all four doors give word for word
// (freshSession, compact).
const (
	// claimInATurn: cutting a running turn off would lose the turn that may be the one ending
	// the run of provider errors, so both recoveries refuse a claim in one (refuseInATurn).
	claimInATurn = "CLAIM_IN_A_TURN"
	// claimUnsettled: a claim between states — queued, or with a launch in flight — has no agent
	// to stop, no stopped process to start and no session to compact.
	claimUnsettled = "CLAIM_UNSETTLED"
)

// freshSessionReason is what the suspension of a live claim records as its cause.
const freshSessionReason = "a fresh session was asked for"

// controllerOp is one of the two claim recoveries in the words its own refusals name it by,
// which both of its doors give: name reads inside a sentence about the operation,
// notTheController is what the refusal of the role `controller` says the operation does not do
// to the project's own controller (controllerClaim), and cutsTheTurn is what the refusal of a
// claim in a turn says the operation would do to that turn (refuseInATurn).
type controllerOp struct {
	name             string
	notTheController string
	cutsTheTurn      string
}

var (
	compactOp = controllerOp{
		name:             "a compaction",
		notTheController: "the controller does not compact its own session",
		cutsTheTurn:      "a compaction would abort that turn",
	}
	freshSessionOp = controllerOp{
		name:             "a fresh session",
		notTheController: "its own claim takes no fresh session",
		cutsTheTurn:      "a fresh session would cut that turn off",
	}
)

// controllerTarget is the claim a controller's recovery named, read and authenticated: the
// machine to run the effect on, and the issue and role the caller spelled, which the route logs
// as the controller named them.
type controllerTarget struct {
	machine *supervise.Machine
	token   claim.Token
	issue   string
	role    claim.Role
}

// controllerClaim is the door both of the controller's claim recoveries come through
// (claimFreshSession, claimCompact): the claim is named by its issue and role rather than by a
// token the controller never sees, and the caller is its project's controller rather than the
// operator. It answers every refusal of the body, the grant and the lookup itself, and reports
// false when it has; only the effect behind it differs between the two.
//
// The controller is refused its own claim here and nowhere else: the token is built from the
// issue and the role, and no issue's role spells the controller's token (claim.ControllerToken),
// so a role this check takes can never name it.
func (s *server) controllerClaim(w http.ResponseWriter, r *http.Request, op controllerOp) (controllerTarget, bool) {
	var req ClaimRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w,
		field{"grantId", req.GrantID}, field{"issue", req.Issue}, field{"role", req.Role}) {
		return controllerTarget{}, false
	}
	if !claim.IsIssueKey(req.Issue) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ISSUE", "issue must be a Dispatch issue key")
		return controllerTarget{}, false
	}
	role := claim.Role(req.Role)
	if !claim.IsRole(role) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ROLE", fmt.Sprintf(
			"role must be one of %s; the project's controller is none of them, and %s",
			roleList(), op.notTheController))
		return controllerTarget{}, false
	}
	token, err := claim.NewToken(s.project, req.Issue, role)
	if err != nil {
		s.logFailure("api: build the claim token of "+op.name, "issue", req.Issue, "role", req.Role, "error", err)
		writeFailure(w, http.StatusInternalServerError, "CLAIM_TOKEN_FAILED", err.Error())
		return controllerTarget{}, false
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return controllerTarget{}, false
	}
	if !grant.Controller {
		writeFailure(w, http.StatusForbidden, "CONTROLLER_REQUIRED", op.name+" requires a controller grant")
		return controllerTarget{}, false
	}
	m, supervised := s.supervisor.Machine(token)
	if !supervised {
		writeFailure(w, http.StatusNotFound, "NO_CLAIM", "no claim "+string(token))
		return controllerTarget{}, false
	}
	return controllerTarget{machine: m, token: token, issue: req.Issue, role: role}, true
}

// claimFreshSession is the controller's door onto the fresh session (freshSession). The effect
// is the operator's own, and so are its refusals; only the authentication differs
// (controllerClaim).
func (s *server) claimFreshSession(w http.ResponseWriter, r *http.Request) {
	target, ok := s.controllerClaim(w, r, freshSessionOp)
	if !ok {
		return
	}
	if _, ok := s.freshSession(w, r, target.machine, target.token); !ok {
		return
	}
	s.log.Info("api: the controller started a claim in a fresh session",
		"claim", target.token, "issue", target.issue, "role", target.role)
	writeJSON(w, http.StatusOK, FreshSessionResponse{})
}

// roleList names the roles a claim can be on, in the order the workflow reaches them.
func roleList() string {
	names := make([]string, 0, len(claim.Roles))
	for _, role := range claim.Roles {
		names = append(names, string(role))
	}
	return strings.Join(names, ", ")
}

// freshSession is the one fresh-session effect, which `legion claims retry --fresh-session`
// (retryClaim) and the controller's fresh_session (claimFreshSession) both run: the claim leaves
// the session it recorded behind and starts again in a new one. It answers every refusal itself
// and reports the status its caller's success answer takes.
//
// It reaches every claim of the project that is not in a turn — one idle on the provider-error
// ladder, a suspended one, one an operator stopped, a failed one, and a root architect as much
// as a phase worker. The claim a run of provider errors names is live and idle between the
// ladder's sends, not stopped, so a recovery that reached only stopped and failed claims would
// miss the one the notice is sent for.
//
// It is three operator commands in a row, and nothing else: `legion claims suspend` for a claim
// that runs, then `legion claims forget-session`, then the start — `legion claims resume` for a
// suspended claim, and the retry for one that failed or an operator stopped, which a resume is
// refused for (supervise's table). A phase worker is started through its issue's phase whichever
// of those two started it, since that is what hands the relaunched role the phase's task
// (restartWorkedPhase). The new session also puts the claim's run of provider-error turns back
// to zero (supervise.Machine.forgetSession), so a second notice can come for the same claim.
//
// The forget goes before the start, and for a phase worker that means before intake.ApplyFact,
// because the start the fact queues is an outbox row a lane runs once the fact commits:
// ApplyFact runs its commit hooks on the committed fact, those hooks wake the outbox lane, and
// the lane starts the claim on a goroutine of its own (the daemon's outbox lanes). A forget
// sent after the fact races that lane. A lane that reaches the machine first relaunches the
// claim on the very session this request means to drop, and the forget then arrives at a
// launching claim, where the table ignores it (supervise's onForgetSession rows) and the
// machine answers a refusal: the caller reads an error and the phase runs on in the session the
// provider keeps refusing.
//
// What this order costs is the session id of a refused start. Three answers come after the
// forget and relaunch nothing: a fact the store would not apply, the workflow's own refusal of
// it, and an issue the retry leaves held because its tree lingers. The claim each leaves behind
// records no session, so its next accepted start spawns a fresh agent rather than resuming the
// one it had. Only the claim's record of the session goes: the session and its file stand, and
// this logs the id it dropped, so an operator who wanted that session can read it there. A
// forget that loses the race to the lane, by contrast, leaves no way at all to drop the session
// the request was for.
//
// Two states and one claim are refused. A claim in a turn is refused because suspending it would
// cut that turn off, and the turn may be the one that ends the run; the caller is told to ask
// again after it. A claim between states — queued, or with a launch in flight — is refused
// because its suspension would be held for the agent it is still waiting for, and the request
// would answer long after the claim moved on. The project's controller is refused whatever
// state it is in: its keeper starts it, as that keeper alone is told when it must
// (supervise's operatorRetry refuses the controller's retry for the same reason), and the
// controller's own door cannot name it at all, since `controller` is no role a claim token is
// built from (claimFreshSession).
func (s *server) freshSession(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token) (int, bool) {
	c := m.Claim()
	if c.Role == claim.RoleController {
		writeFailure(w, http.StatusConflict, "NOT_THE_CONTROLLER", fmt.Sprintf(
			"%s is the project's controller: the daemon's controller keeper starts it, and a fresh session of it would cut the project's one controller off", token))
		return 0, false
	}
	switch {
	case c.State == supervise.StateWorking:
		return 0, refuseInATurn(w, token, freshSessionOp)
	case startsAgain(c.State):
	case agentRunning(c.State):
		held, ok := s.claimEvent(w, r, m, token, "suspend",
			supervise.RequestSuspend{Claim: token, Reason: freshSessionReason, By: supervise.SuspendedByOperator})
		if !ok {
			return 0, false
		}
		// A turn that started between the read above and the suspension holds it for its end
		// (supervise's holdSuspension): the process still runs, so the claim is in a turn after
		// all and the caller is told what it would have been told a moment earlier.
		if held {
			return 0, refuseInATurn(w, token, freshSessionOp)
		}
	default:
		writeFailure(w, http.StatusConflict, claimUnsettled, fmt.Sprintf(
			"%s is %s: a fresh session reaches a claim whose agent is ready or idle, and one that is suspended, stopped or failed; ask again when %s is one of those",
			token, c.State, token))
		return 0, false
	}
	if session := m.Claim().Session; session != "" && !s.forgetRecordedSession(w, r, m, token, session) {
		return 0, false
	}
	if m.Claim().State != supervise.StateSuspended {
		return s.retryStart(w, r, m, token)
	}
	if _, ok := s.claimEvent(w, r, m, token, "resume", supervise.RequestResume{Claim: token}); !ok {
		return 0, false
	}
	s.log.Info("api: a fresh session was started for a claim that was suspended", "claim", token)
	// A phase worker that owes no task is handed one by its issue's phase, through the same
	// restart the start of a stopped claim runs (restartWorkedPhase): the resume above starts
	// the agent and nothing more, so without it a tester waiting on CI, or a worker an idle
	// release suspended, comes back in a new session with nothing to do. A claim that still
	// owes a task keeps it — the resume sends that one — and an architect is never started
	// through a phase at all.
	if m.Claim().Pending == nil {
		if status, ok, restarted := s.restartWorkedPhase(w, r, token, m.Claim()); restarted {
			return status, ok
		}
	}
	return http.StatusOK, true
}

// refuseInATurn is the refusal of a claim whose turn is running, which both recoveries give:
// the operation names itself and what it would do to the turn, so a controller told to compact
// first is not told to ask for a fresh session instead (controllerOp). It always reports false,
// so a caller returns it as its own answer.
func refuseInATurn(w http.ResponseWriter, token claim.Token, op controllerOp) bool {
	writeFailure(w, http.StatusConflict, claimInATurn, fmt.Sprintf(
		"%s is in a turn: %s, so ask again after the turn ends", token, op.cutsTheTurn))
	return false
}

// startsAgain is whether a claim's process is already gone, so the fresh session starts it rather
// than suspending it first: the states forgetSession and the retry both act on.
func startsAgain(state supervise.ClaimState) bool {
	return state == supervise.StateSuspended || failedOrRetired(state)
}

// agentRunning is whether a claim runs an agent that is in no turn: one registered, ready or
// idle. It is the set the fresh session suspends at once, where a claim whose launch has not
// registered yet holds its suspension instead (supervise's holdSuspendUntilReady) and is
// refused rather than waited for. A compaction takes a narrower set, the two states the
// machine takes one in (compactable).
func agentRunning(state supervise.ClaimState) bool {
	return state == supervise.StateRegistered || state == supervise.StateReady || state == supervise.StateIdle
}

// claimEvent posts one request to a claim's machine under a decision of the route's, as every
// operator request on a claim does (claimRequest): it answers the caller itself when the daemon
// is stopping or the machine refuses, and reports whether the machine held the request for the
// agent's turn (supervise.ErrSuspendHeld), which is no refusal. Both recoveries post through
// it.
func (s *server) claimEvent(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token,
	request string, ev supervise.Event) (held, ok bool) {
	ctx, decided := s.decision(r, token)
	defer decided()
	if s.stopped(w, request) {
		return false, false
	}
	switch err := m.Handle(ctx, ev); {
	case errors.Is(err, supervise.ErrSuspendHeld):
		return true, true
	case err != nil:
		s.operatorFailure(w, request, token, err)
		return false, false
	}
	return false, true
}
