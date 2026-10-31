package api

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// invalidOperatorToken is every operator route's one refusal.
const invalidOperatorToken = "Invalid operator token"

// SpawnRequest is the operator's spawn: the claim on Role of Issue, in the tree Tree roots.
// Prompt is an optional test override; without it the daemon composes the role prompt itself.
// Task, when set, is its first delivery, queued before the agent is ready and sent once it is.
type SpawnRequest struct {
	Tree   string     `json:"tree"`
	Issue  string     `json:"issue"`
	Role   claim.Role `json:"role"`
	Prompt string     `json:"prompt,omitempty"`
	Task   string     `json:"task,omitempty"`
}

// DeliverRequest gives a claim a task.
type DeliverRequest struct {
	Task string `json:"task"`
}

// RetryRequest is the operator's retry, whose body is optional: a retry that sends none, as every
// caller before this contract did, resumes the claim's recorded session. FreshSession is
// `legion claims retry --fresh-session`, which starts the claim in a new session instead — what a
// claim its provider refused needs, since the session those turns were refused in would be
// refused again.
type RetryRequest struct {
	FreshSession bool `json:"freshSession,omitempty"`
}

// ForgetSessionRequest names the claim's recorded session that the operator is abandoning.
type ForgetSessionRequest struct {
	Session string `json:"session"`
}

// OperatorClaim is a claim as the operator sees it: everything the supervisor holds about it but
// its two hashes.
type OperatorClaim struct {
	Token                claim.Token `json:"token"`
	Tree                 string      `json:"tree"`
	Issue                string      `json:"issue"`
	Role                 claim.Role  `json:"role"`
	Generation           uint64      `json:"generation"`
	State                string      `json:"state"`
	Session              string      `json:"session"`
	SessionFile          string      `json:"sessionFile"`
	ForgottenSession     string      `json:"forgottenSession,omitempty"`
	ForgottenSessionFile string      `json:"forgottenSessionFile,omitempty"`
	// Locator is the claim's process, the runtime's nested shape; none while nothing runs.
	Locator         *runtime.Locator `json:"locator,omitempty"`
	Budgets         BudgetsView      `json:"budgets"`
	UncertainStreak int              `json:"uncertainStreak"`
	// Pending is the task the claim holds, until the turn it started ends.
	Pending *DeliveryView `json:"pending,omitempty"`
	// SuspendedBy is who suspended the claim while it is suspended: operator, daemon, exit, or
	// unknown (a suspension recorded before migration 0038); absent otherwise.
	SuspendedBy string `json:"suspendedBy,omitempty"`
	// SuspensionHeld is a suspension the machine holds for the agent's turn (supervise's
	// holdSuspension); absent when none is.
	SuspensionHeld bool `json:"suspensionHeld,omitempty"`
	// PluginContract is the contract the claim's current registration declared, 0 when none
	// recorded, always emitted.
	PluginContract int `json:"pluginContract"`
	// LastTurnStart and LastTurnEnd are when the claim's agent last began a turn and last ended
	// one, each absent until that happens, and both absent for a claim that last ran before this
	// release recorded them. An operator reads them to tell a worker still making progress from
	// one wedged in a turn or idle since long before now.
	LastTurnStart *time.Time `json:"lastTurnStart,omitempty"`
	LastTurnEnd   *time.Time `json:"lastTurnEnd,omitempty"`
	// LastDeathCause is why the claim's process was last found gone, LastStartFailure why its last
	// start did not produce a running agent, and LastReady when its agent last said it could be
	// prompted. Each is absent until it happens, and the two causes are kept apart because they
	// answer different questions: a claim that cannot start and a claim that keeps dying are
	// different faults. LastReady dates them — a cause older than it is one the running agent has
	// already survived.
	LastDeathCause   string     `json:"lastDeathCause,omitempty"`
	LastStartFailure string     `json:"lastStartFailure,omitempty"`
	LastReady        *time.Time `json:"lastReady,omitempty"`
}

// BudgetsView is the claim's retry counters (`supervise.Budgets`).
type BudgetsView struct {
	LaunchFailures int `json:"launchFailures"`
	Deaths         int `json:"deaths"`
	PromptFailures int `json:"promptFailures"`
	PromptRetires  int `json:"promptRetires"`
}

// DeliveryView is the claim's pending task (`supervise.Delivery`): DeliveredAt is the latest
// acknowledgement of this task's prompt, cleared when that prompt is refused, and ConfirmedAt the
// turn a send started; each absent until it happens.
type DeliveryView struct {
	ID   string `json:"id"`
	Task string `json:"task"`
	// Phase is the issue phase the task was queued for, and empty for a task of no phase — an
	// operator's own, an architect's. It is what says whether a pending task is still the work to
	// do, so an operator reading a claim can see it rather than infer it from the task's prose.
	Phase       phase.Phase `json:"phase,omitempty"`
	QueuedAt    time.Time   `json:"queuedAt"`
	DeliveredAt *time.Time  `json:"deliveredAt,omitempty"`
	ConfirmedAt *time.Time  `json:"confirmedAt,omitempty"`
	// Interrupted is a task a process died in a turn of: it is sent behind the sentence saying so
	// (`supervise.Delivery.Interrupted`). Absent for every other task.
	Interrupted bool `json:"interrupted,omitempty"`
}

// OperatorClaims is the list route's answer, in token order.
type OperatorClaims struct {
	Claims []OperatorClaim `json:"claims"`
}

// MarshalJSON keeps `claims` an array on the wire, empty rather than `null`.
func (c OperatorClaims) MarshalJSON() ([]byte, error) {
	type wire OperatorClaims
	out := wire(c)
	if out.Claims == nil {
		out.Claims = []OperatorClaim{}
	}
	return json.Marshal(out)
}

func operatorView(c supervise.Claim) OperatorClaim {
	view := OperatorClaim{
		Token:       c.Token,
		Tree:        c.Tree,
		Issue:       c.Issue,
		Role:        c.Role,
		Generation:  c.Generation,
		State:       string(c.State),
		Session:     c.Session,
		SessionFile: c.SessionFile,
		Locator:     c.Locator,
		Budgets: BudgetsView{
			LaunchFailures: c.Budgets.LaunchFailures,
			Deaths:         c.Budgets.Deaths,
			PromptFailures: c.Budgets.PromptFailures,
			PromptRetires:  c.Budgets.PromptRetires,
		},
		UncertainStreak:  c.UncertainStreak,
		SuspendedBy:      string(c.SuspendedBy),
		SuspensionHeld:   c.SuspensionHeld,
		PluginContract:   c.PluginContract,
		LastTurnStart:    instant(c.LastTurnStart),
		LastTurnEnd:      instant(c.LastTurnEnd),
		LastDeathCause:   c.LastDeathCause,
		LastStartFailure: c.LastStartFailure,
		LastReady:        instant(c.LastReady),
	}
	if p := c.Pending; p != nil {
		view.Pending = &DeliveryView{
			ID: p.ID, Task: p.Task, Phase: p.Phase, QueuedAt: p.QueuedAt,
			DeliveredAt: instant(p.DeliveredAt), ConfirmedAt: instant(p.ConfirmedAt), Interrupted: p.Interrupted,
		}
	}
	return view
}

// instant is a time that may not have happened yet: the zero time is absent on the wire.
func instant(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// operator admits a request that carries the operator's bearer, compared in constant time over
// both sides' hashes so neither the token nor its length leaks through the comparison. A daemon
// holding no operator token admits nothing.
func (s *server) operator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		given, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !s.operatorSet || !bearer || !secureBearerEqual(s.operatorHash, given) {
			writeJSON(w, http.StatusForbidden, errorBody(invalidOperatorToken))
			return
		}
		next(w, r)
	}
}

func secureBearerEqual(expected [sha256.Size]byte, given string) bool {
	presented := sha256.Sum256([]byte(given))
	return subtle.ConstantTimeCompare(presented[:], expected[:]) == 1
}

// spawn creates the claim — or finds it, when it exists — and posts the launch to its machine,
// then the task, so a claim the machine will not launch is given no task either.
func (s *server) spawn(w http.ResponseWriter, r *http.Request) {
	var req SpawnRequest
	if !s.readBody(w, r, &req, nil) || !requireFields(w,
		field{"tree", req.Tree}, field{"issue", req.Issue}, field{"role", string(req.Role)},
	) {
		return
	}
	for _, key := range []field{{"tree", req.Tree}, {"issue", req.Issue}} {
		if !claim.IsIssueKey(key.value) {
			writeJSON(w, http.StatusBadRequest, errorBody(fmt.Sprintf("%s %q is not an issue key", key.name, key.value)))
			return
		}
	}
	if !claim.IsRole(req.Role) {
		writeJSON(w, http.StatusBadRequest, errorBody(fmt.Sprintf("role %q is not a role", req.Role)))
		return
	}
	token, err := claim.NewToken(s.project, req.Issue, req.Role)
	if err != nil {
		s.operatorFailure(w, "spawn", token, err)
		return
	}
	ctx, decided := s.decision(r, token)
	defer decided()
	if s.stopped(w, "spawn") {
		return
	}
	if req.Tree == req.Issue && req.Role == claim.RoleArchitect {
		if _, err := s.trees.OpenTreeLifecycle(ctx, s.project, req.Tree, treelifecycle.AuthorityOperator); errors.Is(err, treelifecycle.ErrCleanupReserved) {
			writeJSON(w, http.StatusConflict, errorBody(fmt.Sprintf("operator tree %s is waiting for durable cleanup: %v", req.Tree, err)))
			return
		} else if err != nil {
			s.operatorFailure(w, "spawn", token, err)
			return
		}
	}
	m, created, err := s.supervisor.Create(ctx, supervise.Claim{
		Token: token, Project: s.project, Tree: req.Tree, Issue: req.Issue, Role: req.Role, State: supervise.StateQueued,
	}, req.Prompt)
	if err != nil {
		s.operatorFailure(w, "spawn", token, err)
		return
	}
	if err := m.Handle(ctx, supervise.RequestSpawn{Claim: token}); err != nil {
		s.operatorFailure(w, "spawn", token, err)
		return
	}
	if req.Task != "" {
		if err := m.Handle(ctx, supervise.RequestDeliver{Claim: token, Task: req.Task}); err != nil {
			s.operatorFailure(w, "deliver", token, err)
			return
		}
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.operatorDone(r, token)
	writeJSON(w, status, operatorView(m.Claim()))
}

// claimRequest is one operator request on an existing claim: event builds the request the claim's
// machine is posted, or answers the caller itself and reports false. A suspension the machine holds
// for the agent's turn (supervise.ErrSuspendHeld) is not a refusal: it answers 202 with the claim as
// it is, still working.
//
// Both reads of the claim are the machine's published view (supervise.Machine.View), never
// Machine.Claim: a claim whose launch is running holds its machine's lock for as long as the
// runtime waits on its pod, and a route that read through that lock would not even reach the
// machine with the suspension that stops that start (dispatch://INFRA-714 R10). The view is what
// the claim's latest decision left, which is what the answer reports.
func (s *server) claimRequest(request string, event func(http.ResponseWriter, *http.Request, supervise.Claim) (supervise.Event, bool)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := claim.Token(r.PathValue("token"))
		m, ok := s.supervisor.Machine(token)
		if !ok {
			writeJSON(w, http.StatusNotFound, errorBody("no claim "+string(token)))
			return
		}
		ev, ok := event(w, r, m.View().Claim)
		if !ok {
			return
		}
		status := http.StatusOK
		ctx, decided := s.decision(r, token)
		defer decided()
		if s.stopped(w, request) {
			return
		}
		if err := m.Handle(ctx, ev); errors.Is(err, supervise.ErrSuspendHeld) {
			status = http.StatusAccepted
		} else if err != nil {
			s.operatorFailure(w, request, token, err)
			return
		}
		s.operatorDone(r, token)
		writeJSON(w, status, operatorView(m.View().Claim))
	}
}

func (s *server) deliverEvent(w http.ResponseWriter, r *http.Request, c supervise.Claim) (supervise.Event, bool) {
	var req DeliverRequest
	if !s.readBody(w, r, &req, nil) || !requireFields(w, field{"task", req.Task}) {
		return nil, false
	}
	return supervise.RequestDeliver{Claim: c.Token, Task: req.Task}, true
}

func suspendEvent(_ http.ResponseWriter, _ *http.Request, c supervise.Claim) (supervise.Event, bool) {
	return supervise.RequestSuspend{Claim: c.Token, Reason: "the operator suspended it", By: supervise.SuspendedByOperator}, true
}

func resumeEvent(_ http.ResponseWriter, _ *http.Request, c supervise.Claim) (supervise.Event, bool) {
	return supervise.RequestResume{Claim: c.Token}, true
}

func stopEvent(_ http.ResponseWriter, _ *http.Request, c supervise.Claim) (supervise.Event, bool) {
	return supervise.RequestStop{Claim: c.Token}, true
}

// retryEvent is the operator's retry of a failed architect: its session relaunched with fresh
// budgets, or a new one when the body asked for it (RetryRequest, read once by retryClaim). The
// machine refuses it for a claim in any other state, naming that state, and for a failed claim
// that is not an architect's, naming who retries it (supervise's operatorRetry). A claim the
// provider refused out of its budget takes a new session either way (supervise's freshSession).
func retryEvent(req RetryRequest) func(http.ResponseWriter, *http.Request, supervise.Claim) (supervise.Event, bool) {
	return func(_ http.ResponseWriter, _ *http.Request, c supervise.Claim) (supervise.Event, bool) {
		return supervise.RequestOperatorRetry{Claim: c.Token, FreshSession: req.FreshSession}, true
	}
}

// readOptionalBody decodes a request body the route reads when there is one: an operator command
// that sends none — every `legion claims` subcommand with no flags of its own, and every caller
// from before the body existed — leaves into as it was. A body that is there is read as strictly
// as any other.
func (s *server) readOptionalBody(w http.ResponseWriter, r *http.Request, into any) bool {
	body, err := decodeJSON(w, r, into, true)
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) && len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	s.refuseBadBody(w, r, err)
	return false
}

// forgetSession clears a recorded session only when the operator names that exact session and the
// claim has no live process. The session store is deliberately untouched.
func (s *server) forgetSession(w http.ResponseWriter, r *http.Request) {
	var req ForgetSessionRequest
	if !s.readBody(w, r, &req, nil) {
		return
	}
	if !requireFields(w, field{"session", req.Session}) {
		return
	}
	token := claim.Token(r.PathValue("token"))
	m, ok := s.supervisor.Machine(token)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody("no claim "+string(token)))
		return
	}
	c := m.Claim()
	if req.Session != c.Session {
		writeJSON(w, http.StatusConflict, errorBody(fmt.Sprintf(
			"forget-session refused: %s records session %s; pass --session %s to start fresh", c.Token, c.Session, c.Session)))
		return
	}
	if !forgettable(c.State) {
		writeJSON(w, http.StatusConflict, errorBody(fmt.Sprintf(
			"forget-session refused: %s is %s; suspend, fail, or retire it before starting fresh", c.Token, c.State)))
		return
	}
	if s.beforeForgetSessionHandle != nil {
		s.beforeForgetSessionHandle()
	}
	ctx, decided := s.decision(r, token)
	defer decided()
	if s.stopped(w, "forget-session") {
		return
	}
	if err := m.Handle(ctx, supervise.RequestForgetSession{Claim: token, Session: req.Session}); err != nil {
		s.operatorFailure(w, "forget-session", token, err)
		return
	}
	s.log.Info("api: operator forgot session", "claim", token, "session", c.Session, "sessionFile", c.SessionFile)
	s.operatorDone(r, token)
	view := operatorView(m.Claim())
	view.ForgottenSession, view.ForgottenSessionFile = c.Session, c.SessionFile
	writeJSON(w, http.StatusOK, view)
}

func forgettable(state supervise.ClaimState) bool {
	return state == supervise.StateQueued || state == supervise.StateSuspended || state == supervise.StateFailed || state == supervise.StateRetired
}

// retryClaim is the operator's retry of a claim that is running nothing: one that failed, and one
// an operator stopped, which retires it. A phase worker is retried through its issue's phase,
// never on its own, because a worker relaunched outside its phase is handed no task and does
// nothing:
//
//   - an issue held for that worker's own role (Options.RoleFor of the phase it is held from) is
//     retried as its architect's retry_or_escalate retries it (intake.RetryOrEscalate): the phase
//     it was held from comes back, and its role is started on it;
//   - an issue standing in that worker's own phase has that phase started again
//     (intake.RestartPhase). It is where an operator's stop leaves an issue, since a stop is no
//     death and holds nothing, and where a failure the workflow could not hold leaves one.
//
// Either way the start is the workflow's, so the route answers 202 with the claim as it is until
// that start runs, and a refusal the workflow gives is the route's answer. A retry that leaves the
// issue held relaunched nothing (a lingering tree's members stay where they stood), so the route
// answers 409 naming the issue rather than report a retry. Every other claim is the machine's to
// answer (retryEvent): an architect is relaunched, and a phase worker whose issue works another
// role's phase, or that has left the workflow, is refused. It is how an operator relaunches the
// claims a runtime failed to launch — every failed claim in turn, with `legion claims retry
// --failed` — once the cause is gone.
//
// --fresh-session (RetryRequest) is the other half of that retry, and it is the claim's own: the
// claim forgets the session it recorded (supervise's RequestForgetSession) before the phase is
// started, so the start that relaunches it spawns a fresh agent and gives it the phase's task.
// That is what a session the provider refuses needs — resuming it is refused again, turn after
// turn — and the one thing no route could do before: a claim the provider refused out of its
// budget forgets its session whoever retried it (supervise's freshSession), but a worker stuck
// any other way had its session cleared by hand or not at all.
func (s *server) retryClaim(w http.ResponseWriter, r *http.Request) {
	token := claim.Token(r.PathValue("token"))
	m, ok := s.supervisor.Machine(token)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody("no claim "+string(token)))
		return
	}
	// The body is read once here: both halves of the retry read it, and the request's body can be
	// read only once.
	var req RetryRequest
	if !s.readOptionalBody(w, r, &req) {
		return
	}
	c := m.Claim()
	if !failedOrRetired(c.State) || !isPhaseWorker(c.Role) || s.pool == nil || s.records == nil || s.roleFor == nil {
		s.claimRequest("retry", retryEvent(req))(w, r)
		return
	}
	if s.stopped(w, "retry") {
		return
	}
	recorded, err := s.recordedIssue(r.Context(), c.Issue)
	if err != nil {
		s.logFailure("api: read the issue of a phase worker to retry", "claim", token, "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody(fmt.Sprintf("could not read %s's record: %v", c.Issue, err)))
		return
	}
	if recorded == nil || s.roleFor(workedPhase(*recorded)) != c.Role {
		s.claimRequest("retry", retryEvent(req))(w, r)
		return
	}
	working := workedPhase(*recorded)
	if req.FreshSession && !s.forgetSessionForRetry(w, r, m, token, c.Session) {
		return
	}
	fact := intake.Fact(intake.RestartPhase{Issue: c.Issue, Role: c.Role})
	if recorded.Phase == phase.Held {
		fact = intake.RetryOrEscalate{Issue: c.Issue, Decision: intake.RetryDecision}
	}
	result, err := intake.ApplyFact(r.Context(), s.pool, "api", requestFactID("operator/retry"), fact, s.handlers...)
	if err != nil {
		s.logFailure("api: apply the operator's retry of a phase", "claim", token, "issue", c.Issue, "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody(fmt.Sprintf("could not retry %s's phase %s: %v", c.Issue, working, err)))
		return
	}
	if result.Refusal != nil {
		writeJSON(w, result.Refusal.Status, errorBody(result.Refusal.Message))
		return
	}
	after, err := s.recordedIssue(r.Context(), c.Issue)
	if err != nil {
		s.logFailure("api: read the issue after the operator's retry of its phase", "claim", token, "issue", c.Issue, "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody(fmt.Sprintf("could not read %s's record after the retry: %v", c.Issue, err)))
		return
	}
	if after != nil && after.Phase == phase.Held {
		writeJSON(w, http.StatusConflict, errorBody(fmt.Sprintf("%s is still held after the retry of its phase %s, so nothing relaunched %s: its tree lingers, and a lingering tree's members stay where they stood", c.Issue, working, token)))
		return
	}
	s.log.Info("api: the operator retried a phase", "claim", token, "issue", c.Issue, "phase", working, "freshSession", req.FreshSession)
	s.operatorDone(r, token)
	writeJSON(w, http.StatusAccepted, operatorView(m.Claim()))
}

// failedOrRetired is whether a claim's state is one the operator's retry acts on through its
// issue's phase: the claim's process is gone and no rule of the daemon's own starts it again. A
// claim that failed is one, and so is one an operator stopped, which retires it.
func failedOrRetired(state supervise.ClaimState) bool {
	return state == supervise.StateFailed || state == supervise.StateRetired
}

// workedPhase is the phase a recorded issue's worker works: the phase it is held from while it is
// held, and the phase it stands in otherwise.
func workedPhase(issue record.Issue) phase.Phase {
	if issue.Phase == phase.Held && issue.Hold != nil {
		return issue.Hold.From
	}
	return issue.Phase
}

// forgetSessionForRetry has the claim drop the session it recorded, for a retry that asked for a
// new one, and reports whether it did: a machine that refuses it — a claim running the session,
// which no retry of this route reaches, or one that recorded another session since this request
// read it — is the operator's answer, and nothing is started after it. The session is passed back
// as the claim reported it, so the machine clears the one this retry saw and never a later one
// (supervise's forgetSession).
func (s *server) forgetSessionForRetry(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token, session string) bool {
	ctx, decided := s.decision(r, token)
	defer decided()
	if err := m.Handle(ctx, supervise.RequestForgetSession{Claim: token, Session: session}); err != nil {
		s.operatorFailure(w, "retry", token, err)
		return false
	}
	return true
}

// closeTree is the operator closing a tree no workflow issue backs, through its root claim: the
// tree's close, as the workflow's tree_close is for a tree whose linger expired. Without it a
// root the operator spawned could never end — under a sandbox, its Sandbox and the tree volume
// would stay for good. A workflow issue's tree is the workflow's to close, and a worker's claim is
// stopped, not closed.
//
// The tree's claims are read first and held as this route's decisions (RouteDecisions), so a
// daemon's stop that begins during the close keeps their worker stream connections; a read that
// fails there closes nothing, and a close the daemon's stop has already begun is refused. Then the
// root goes first, since its close is where the supervisor answers both of the questions that
// decide it — whether a workflow issue backs the tree, and whether this claim is its tree's root
// at all — where the answers and the close they decide sit together; a refused close stops
// nothing, and the refusal an operator sees is the machine's. A close of a tree already retired
// answers 200: the operator asked for an outcome that holds, and a retry after a timeout is not a
// failure. Then every other claim of the tree that has not retired is stopped, as tree_close stops
// each, so no worker is left on a tree volume that is being deleted. The close reserves the tree's
// cleanup first, so no claim of the tree can bind or launch meanwhile; after every claim stopped,
// the store's reserved cleanup has the runtime release the tree (Options.Releaser) and confirms
// it. A release failure leaves the reservation durable for the same close to resume; a claim the
// daemon supervises no machine for cannot be stopped that way, since the stop route answers 404 for
// it, until the daemon restarts and builds a machine for every stored claim; the 500 says so for
// that claim. Nothing retries either for the operator.
func (s *server) closeTree(w http.ResponseWriter, r *http.Request) {
	token := claim.Token(r.PathValue("token"))
	root, ok := s.supervisor.Machine(token)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody("no claim "+string(token)))
		return
	}
	c := root.Claim()
	ctx, decided := s.decision(r, token)
	defer decided()
	// The tree's other claims are this route's decisions from before the root's close, which waits
	// out the root's exit: a daemon's stop that begins meanwhile keeps their worker stream
	// connections open, and each one's stop below sends its shutdown frame over its own
	// (RouteDecisions). A claim the store gains later is recorded with the read the stops use.
	claims, err := s.supervisor.Claims(ctx)
	if err != nil {
		s.logFailure("api: read the claims of a tree the operator closes", "tree", c.Tree, "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody(fmt.Sprintf("closed nothing: the daemon could not read the claims of tree %s", c.Tree)))
		return
	}
	defer s.decisions.begin(treeOthers(claims, c.Tree, token))()
	if s.stopped(w, "close") {
		return
	}
	if err := root.Handle(ctx, supervise.RequestOperatorClose{Claim: token}); err != nil {
		s.operatorFailure(w, "close", token, err)
		return
	}
	lifecycle, cleanup, err := s.trees.ReserveOperatorTreeCleanup(ctx, s.project, c.Tree)
	if err != nil {
		s.logFailure("api: reserve cleanup of an operator-closed tree", "tree", c.Tree, "error", err)
		writeJSON(w, http.StatusConflict, errorBody(fmt.Sprintf("closed %s's root claim, but its durable tree cleanup could not be reserved: %v. Retry legion claims close once that is resolved", c.Tree, err)))
		return
	}
	claims, err = s.supervisor.Claims(ctx)
	if err != nil {
		s.logFailure("api: read the claims of a tree the operator closed", "tree", c.Tree, "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody(fmt.Sprintf("closed %s's root claim %s, but the daemon could not read the tree's other claims to stop them", c.Tree, token)))
		return
	}
	others := treeOthers(claims, c.Tree, token)
	// The one record that no stop check follows (stopped): a claim the tree gained while its root
	// exited, by an operator's spawn into the tree being closed, is recorded only here, so a daemon's
	// stop that begins in that same moment may already have closed its connection, and its stop then
	// ends its agent without the shutdown frame. Its stored state is still right (retired); a check
	// here would instead leave the root closed and the tree's other claims unstopped.
	defer s.decisions.begin(others)()
	var unstopped []string
	for _, other := range others {
		m, ok := s.supervisor.Machine(other)
		if !ok {
			s.log.Error("api: a claim of a tree the operator closed has no machine to stop", "claim", other)
			unstopped = append(unstopped, fmt.Sprintf("%s (the daemon supervises no machine for it, so legion claims stop cannot reach it until the daemon restarts)", other))
			continue
		}
		if err := m.Handle(ctx, supervise.RequestStop{Claim: other}); err != nil {
			s.logFailure("api: stop a claim of a tree the operator closed", "claim", other, "error", err)
			unstopped = append(unstopped, fmt.Sprintf("%s (%v)", other, err))
		}
	}
	if len(unstopped) > 0 {
		writeJSON(w, http.StatusInternalServerError, errorBody(fmt.Sprintf("closed %s's root claim %s, but these claims of the tree were not stopped: %s. Stop each now with legion claims stop, then close again: the tree's durable cleanup waits for every claim of it",
			c.Tree, token, strings.Join(unstopped, "; "))))
		return
	}
	if cleanup {
		if err := s.trees.CleanupReservedTree(ctx, s.project, c.Tree, lifecycle.Epoch, s.releaser); err != nil {
			s.logFailure("api: cleanup an operator-closed tree", "tree", c.Tree, "error", err)
			writeJSON(w, http.StatusInternalServerError, errorBody(fmt.Sprintf("closed %s's claims, but durable resource cleanup is pending: %v. Retry legion claims close after the reported cleanup error is resolved", c.Tree, err)))
			return
		}
	}
	s.operatorDone(r, token)
	writeJSON(w, http.StatusOK, operatorView(root.Claim()))
}

// operatorDone logs the one Info line every operator request on a claim that succeeded writes: its
// route, the claim and the caller's address. The operator's bearer names nobody, so the address is
// what the line can say of the caller; with it, a day's operator requests are counted from the log
// alone. A request that fails logs its failure instead (operatorFailure).
func (s *server) operatorDone(r *http.Request, token claim.Token) {
	s.log.Info("api: operator request", "route", r.Pattern, "claim", token, "caller", r.RemoteAddr)
}

// treeOthers is every claim of tree in claims, but root and those already retired.
func treeOthers(claims []supervise.Claim, tree string, root claim.Token) []claim.Token {
	var others []claim.Token
	for _, c := range claims {
		if c.Tree == tree && c.Token != root && c.State != supervise.StateRetired {
			others = append(others, c.Token)
		}
	}
	return others
}

// list answers every claim the daemon supervises, in token order: as its machine last published it
// (supervise.Machine.View), or, for a claim no machine supervises, as the store holds it. The
// machine's is what knows whether a suspension is held. It never waits on a decision in flight: a
// relaunch holds its machine for as long as the runtime waits out its pods, and with many claims
// one is nearly always relaunching.
func (s *server) list(w http.ResponseWriter, r *http.Request) {
	claims, err := s.supervisor.Claims(r.Context())
	if err != nil {
		s.log.Error("api: read the claims", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("the daemon could not read its claims"))
		return
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].Token < claims[j].Token })
	views := make([]OperatorClaim, len(claims))
	for i, c := range claims {
		if m, ok := s.supervisor.Machine(c.Token); ok {
			c = m.View().Claim
		}
		views[i] = operatorView(c)
	}
	writeJSON(w, http.StatusOK, OperatorClaims{Claims: views})
}

// operatorFailure answers an operator request its machine did not take: 409 with the machine's
// reason when the claim's state does not allow it, and otherwise a 500 that says what failed — the
// operator is the one who has to act on a launch the runtime refused.
func (s *server) operatorFailure(w http.ResponseWriter, request string, token claim.Token, err error) {
	var refused *supervise.RefusedError
	if errors.As(err, &refused) {
		writeJSON(w, http.StatusConflict, errorBody(refused.Error()))
		return
	}
	s.logFailure("api: an operator request failed", "request", request, "claim", token, "error", err)
	writeJSON(w, http.StatusInternalServerError, errorBody(fmt.Sprintf("%s %s: %v", request, token, err)))
}
