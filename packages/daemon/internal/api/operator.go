package api

import (
	"bytes"
	"context"
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

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/memorygroup"
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
// refused again. It is the one effect the controller's fresh_session runs as well
// (freshSession), and it reaches every claim of the project that is not in a turn, where the
// plain retry reaches only a claim that failed and one an operator stopped.
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
	// PhaseNotRestarted is why nothing restarted the claim's phase, which only the retry with
	// `--fresh-session` sets (freshSession) and only where the claim came back owing no task and
	// the workflow refused that phase's restart; absent everywhere else, that refusal included,
	// since the claim is back in a new session either way and the state records nowhere that the
	// restart did not run.
	PhaseNotRestarted string `json:"phaseNotRestarted,omitempty"`
	// Locator is the claim's process, the runtime's nested shape; none while nothing runs.
	Locator         *runtime.Locator `json:"locator,omitempty"`
	Budgets         BudgetsView      `json:"budgets"`
	UncertainStreak int              `json:"uncertainStreak"`
	// Pending is the task the claim holds, until the turn it started ends.
	Pending *DeliveryView `json:"pending,omitempty"`
	// SuspendedBy is who suspended the claim while it is suspended: operator, daemon, exit,
	// released (its idle pod was released), or unknown (a suspension recorded before migration
	// 0038); absent otherwise. These routes are the one surface that serves it: a session parses
	// every body it reads with the schema of its own contract, so a key an older contract does not
	// know refuses the whole body at a plugin whose answer side predates INFRA-1048, and is dropped
	// at every plugin after it (api.StrandedClaim).
	SuspendedBy string `json:"suspendedBy,omitempty"`
	// SuspensionHeld is a suspension the machine holds for the agent's turn (supervise's
	// holdSuspension); absent when none is.
	SuspensionHeld bool `json:"suspensionHeld,omitempty"`
	// PluginContract is the contract the claim's current registration declared, 0 when none
	// recorded, always emitted.
	PluginContract int `json:"pluginContract"`
	// UpdateRound is the update round the claim is in, absent when none is open: the pod's drift
	// round (`pod:<pod uid>`), shared by every claim of that pod, or this claim's own plugin
	// round (`plugin:<daemon contract>`). Once its worker has answered, the process is waiting to
	// be replaced and the daemon holds the claim's next task. UpdateHead is the commit the worker
	// reported as its issue branch's head at origin when it answered, absent until it answers and
	// for a worker that reported none; it is the worker's word, which nothing verifies.
	UpdateRound string `json:"updateRound,omitempty"`
	UpdateHead  string `json:"updateHead,omitempty"`
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

// OperatorClaims is the list route's answer, in token order, with what the daemon's last
// pod-update sweep decided beside the claims.
type OperatorClaims struct {
	Claims []OperatorClaim `json:"claims"`
	// PodUpdateRounds is the sweep's own last word, which no claim row carries: a claim shows the
	// round it is in, and nothing on it says why the drifted issue beside it was given none. It
	// is a pointer so that a reader can tell an answer that carries no record at all — a daemon
	// of a release before this field — from one whose first sweep has not run, which carries the
	// zero record and says so in `swept` (cmd/legion's podUpdateRoundsLine). The list route
	// fills it on every answer, so only a daemon too old to know the field serves one without it.
	PodUpdateRounds *PodUpdateRounds `json:"pod_update_rounds"`
}

// MarshalJSON keeps `claims` an array on the wire, empty rather than `null`, and
// `pod_update_rounds` an object, the zero record rather than `null`: the field is nil only where
// nobody filled it, and the readers on the other side of this wire take it as required
// (packages/contracts's LegionOperatorClaimsResponse). The sweep's record normalises its own
// lists, which this carries rather than repeats: `type wire` drops the methods of OperatorClaims
// alone, so the field keeps its own (PodUpdateRounds.MarshalJSON).
func (c OperatorClaims) MarshalJSON() ([]byte, error) {
	type wire OperatorClaims
	out := wire(c)
	if out.Claims == nil {
		out.Claims = []OperatorClaim{}
	}
	if out.PodUpdateRounds == nil {
		out.PodUpdateRounds = &PodUpdateRounds{}
	}
	return json.Marshal(out)
}

// PodUpdateRounds is what this daemon's last pod-update sweep decided under the project's two
// limits on how fast one pod template change reaches the fleet (internal/daemon's podUpdateSweep):
// the limits as the configuration holds them, the issues that hold an open round, and the issues
// each limit held back at that sweep. The claims alone cannot say it — a claim carries the round
// it is in, and an issue a limit held back is posted nothing at all — so an operator reading them
// could not tell a fleet the limits are pacing from one the daemon has stopped carrying.
//
// Swept is false for a daemon whose first sweep has not run, where every other field is empty and
// states nothing. Each limit is nil for a key legion.yaml does not set, which is no limit rather
// than a limit of none. Every list is in issue-key order.
type PodUpdateRounds struct {
	Swept bool `json:"swept"`
	// MaxOpenRounds and RoundsPerMinute are `pod_update_max_open_rounds` and
	// `pod_update_rounds_per_minute` as the configuration holds them.
	MaxOpenRounds   *int `json:"max_open_rounds"`
	RoundsPerMinute *int `json:"rounds_per_minute"`
	// InRound is every issue that held one of the project's open-round slots at that sweep, the
	// issues the sweep opened a round for among them.
	InRound []string `json:"in_round"`
	// WaitingOnMaxOpenRounds and WaitingOnRoundsPerMinute are the drifted issues each limit held
	// back at that sweep. An issue both limits held back is under the limit on open rounds alone:
	// that is the one which will still hold it back at the next sweep.
	WaitingOnMaxOpenRounds   []string `json:"waiting_on_max_open_rounds"`
	WaitingOnRoundsPerMinute []string `json:"waiting_on_rounds_per_minute"`
}

// MarshalJSON keeps the three lists arrays on the wire, empty rather than `null`, so a reader
// takes a sweep that held nothing back as the empty list it is rather than as a daemon that said
// nothing of it.
func (r PodUpdateRounds) MarshalJSON() ([]byte, error) {
	type wire PodUpdateRounds
	out := wire(r)
	for _, list := range []*[]string{&out.InRound, &out.WaitingOnMaxOpenRounds, &out.WaitingOnRoundsPerMinute} {
		if *list == nil {
			*list = []string{}
		}
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
		UncertainStreak: c.UncertainStreak,
		SuspendedBy:     string(c.SuspendedBy),
		SuspensionHeld:  c.SuspensionHeld,
		PluginContract:  c.PluginContract,
		UpdateRound:     c.UpdateRound,
		UpdateHead:      c.UpdateHead,
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
		if _, err := s.trees.OpenOperatorTreeLifecycle(ctx, s.project, req.Tree, s.drawMemory(req.Tree)); errors.Is(err, treelifecycle.ErrCleanupReserved) {
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
	// The operator is waiting on this answer, so the wake pacing never holds this launch
	// (supervise's reserveWake): one person's spawn is not the wave the pacing exists for, and a
	// route that answered with a claim parked launching would read as a daemon that did nothing.
	if err := m.Handle(ctx, supervise.RequestSpawn{Claim: token, Exempt: true}); err != nil {
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

// drawMemory is the draw of tree's memory group, for the open that creates the tree's lifecycle
// row to run in its own transaction (store.Store.OpenOperatorTreeLifecycle): the group this
// project's mode gives the tree, recorded on the tree's root (memorygroup.DrawAndRecord, which
// admission's own path draws through too). An operator's tree usually has no issue record at all,
// and then the draw writes nothing (record.Store.DrawMemory) — nothing is refused and no record
// is invented.
func (s *server) drawMemory(tree string) func(ctx context.Context, tx pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		return memorygroup.DrawAndRecord(ctx, tx, s.records, tree, s.memory, s.coin, s.now())
	}
}

// claimRequest is one operator request on an existing claim: event builds the request the claim's
// machine is posted, or answers the caller itself and reports false. A suspension the machine holds
// for the agent's turn (supervise.ErrSuspendHeld) is not a refusal: it answers 202 with the claim as
// it is, still working.
func (s *server) claimRequest(request string, event func(http.ResponseWriter, *http.Request, supervise.Claim) (supervise.Event, bool)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := claim.Token(r.PathValue("token"))
		m, ok := s.supervisor.Machine(token)
		if !ok {
			writeJSON(w, http.StatusNotFound, errorBody("no claim "+string(token)))
			return
		}
		ev, ok := event(w, r, m.Claim())
		if !ok {
			return
		}
		held, ok := s.claimEvent(w, r, m, token, request, ev)
		if !ok {
			return
		}
		status := http.StatusOK
		if held {
			status = http.StatusAccepted
		}
		s.operatorDone(r, token)
		writeJSON(w, status, operatorView(m.Claim()))
	}
}

// claimEvent posts one request to a claim's machine (postClaimEvent), as every operator request
// on a claim does (claimRequest): it answers the caller itself when the daemon is stopping or
// the machine refuses, and reports whether the machine held the request for the agent's turn
// (supervise.ErrSuspendHeld), which is no refusal. The fresh session's resume posts through it
// too.
func (s *server) claimEvent(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token,
	request string, ev supervise.Event) (held, ok bool) {
	posted, err := s.postClaimEvent(w, r, m, token, request, ev)
	switch {
	case !posted:
		return false, false
	case errors.Is(err, supervise.ErrSuspendHeld):
		return true, true
	case err != nil:
		s.operatorFailure(w, request, token, err)
		return false, false
	}
	return false, true
}

// postClaimEvent posts one request to a claim's machine under a decision of the route's, which
// it ends once the machine has answered, and hands that answer back for the caller to read: the
// refusals of one request mean different things to different routes (claimEvent,
// suspendForFreshSession), so each writes its own. The one answer this writes itself is the
// daemon's stop, which no claim's state explains and every route gives in the same words
// (stopped); posted is false there, and nothing was asked of the machine.
func (s *server) postClaimEvent(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token,
	request string, ev supervise.Event) (posted bool, err error) {
	ctx, decided := s.decision(r, token)
	defer decided()
	if s.stopped(w, request) {
		return false, nil
	}
	return true, m.Handle(ctx, ev)
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

// retryClaim is the operator's `legion claims retry`: the claim's own retry (retryStart), or,
// when the body asks for a new session (RetryRequest), the fresh session the controller's
// fresh_session runs too (freshSession). The two differ in what they reach and in what they
// leave behind, and nothing else answers either: a plain retry acts on a claim that failed and
// on one an operator stopped, resuming the session it recorded, while a fresh session reaches
// every claim of the project that is not in a turn and drops that session first. Only the fresh
// session answers the claim with a phase restart that was refused named on it
// (OperatorClaim.PhaseNotRestarted).
func (s *server) retryClaim(w http.ResponseWriter, r *http.Request) {
	token := claim.Token(r.PathValue("token"))
	m, ok := s.supervisor.Machine(token)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody("no claim "+string(token)))
		return
	}
	var req RetryRequest
	if !s.readOptionalBody(w, r, &req) {
		return
	}
	var (
		status       int
		notRestarted string
	)
	if req.FreshSession {
		status, notRestarted, ok = s.freshSession(w, r, m, token)
	} else {
		status, ok = s.retryStart(w, r, m, token)
	}
	if !ok {
		return
	}
	s.operatorDone(r, token)
	view := operatorView(m.Claim())
	view.PhaseNotRestarted = notRestarted
	writeJSON(w, status, view)
}

// retryStart starts a claim that is running nothing: one that failed, and one an operator
// stopped, which retires it. It answers every refusal itself and reports the status its caller's
// success answer takes. A phase worker is retried through its issue's phase, never on its own,
// because a worker relaunched outside its phase is handed no task and does nothing
// (restartWorkedPhase). Every other claim is the machine's to answer (machineRetry): an
// architect is relaunched, from failed and from the retirement an operator's stop leaves, and a
// phase worker whose issue works another role's phase, or that has left the workflow, is
// relaunched on its own with whatever task it still holds. It is how an operator relaunches the
// claims a runtime failed to launch — every failed claim in turn, with
// `legion claims retry --failed` — once the cause is gone.
//
// The session is the claim's own either way: this start resumes the recorded one. A caller that
// wants a new session forgets it before calling here (freshSession), which is also what reaches
// a claim this start refuses.
func (s *server) retryStart(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token) (int, bool) {
	c := m.Claim()
	if failedOrRetired(c.State) {
		if status, refusal, restarted := s.restartWorkedPhase(r, token, c); restarted {
			if refusal != "" {
				writeJSON(w, status, errorBody(refusal))
				return 0, false
			}
			return status, true
		}
	}
	return s.machineRetry(w, r, m, token)
}

// restartWorkedPhase starts again the phase the claim's role works, which is what hands a
// relaunched phase worker that phase's task. Both recoveries run it: the retry of a claim that
// failed or an operator stopped (retryStart), and the fresh session of a phase worker that
// comes back owing no task (freshSession). restarted is false for a claim no phase start
// reaches — an architect, a claim whose issue works another role's phase or has left the
// workflow, and any claim on a daemon that records no issues — and its caller starts such a
// claim another way.
//
//   - an issue held for that worker's own role (Options.RoleFor of the phase it is held from) is
//     retried as its architect's retry_or_escalate retries it (intake.RetryOrEscalate): the phase
//     it was held from comes back, and its role is started on it;
//   - an issue standing in that worker's own phase has that phase started again
//     (intake.RestartPhase). It is where an operator's stop leaves an issue, since a stop is no
//     death and holds nothing, and where a failure the workflow could not hold leaves one.
//
// Either way the start is the workflow's, so a restart that ran reports 202, for its caller to
// answer with the claim as it is until that start runs. A restart that did not run reports the
// status and the sentence it is refused with, and writes neither itself, because the two
// callers answer a refusal differently: the retry gives it as its own answer (retryStart),
// while the fresh session has started the claim before it gets here and answers success with
// the refusal named in it (freshSession). A restart that leaves the issue held relaunched
// nothing (a lingering tree's members stay where they stood), so it is refused naming the issue
// rather than reported as a restart.
func (s *server) restartWorkedPhase(r *http.Request, token claim.Token, c supervise.Claim) (status int, refusal string, restarted bool) {
	if !isPhaseWorker(c.Role) || s.pool == nil || s.roleFor == nil {
		return 0, "", false
	}
	if stopping := s.stopRefusal("retry"); stopping != "" {
		return http.StatusServiceUnavailable, stopping, true
	}
	recorded, err := s.recordedIssue(r.Context(), c.Issue)
	if err != nil {
		s.logFailure("api: read the issue of a phase worker to retry", "claim", token, "error", err)
		return http.StatusInternalServerError, fmt.Sprintf("could not read %s's record: %v", c.Issue, err), true
	}
	if recorded == nil || s.roleFor(workedPhase(*recorded)) != c.Role {
		return 0, "", false
	}
	working := workedPhase(*recorded)
	fact := intake.Fact(intake.RestartPhase{Issue: c.Issue, Role: c.Role})
	if recorded.Phase == phase.Held {
		fact = intake.RetryOrEscalate{Issue: c.Issue, Decision: intake.RetryDecision}
	}
	result, err := intake.ApplyFact(r.Context(), s.pool, "api", requestFactID("operator/retry"), fact, s.handlers...)
	if err != nil {
		s.logFailure("api: apply the operator's retry of a phase", "claim", token, "issue", c.Issue, "error", err)
		return http.StatusInternalServerError, fmt.Sprintf("could not retry %s's phase %s: %v", c.Issue, working, err), true
	}
	if result.Refusal != nil {
		return result.Refusal.Status, result.Refusal.Message, true
	}
	after, err := s.recordedIssue(r.Context(), c.Issue)
	if err != nil {
		s.logFailure("api: read the issue after the operator's retry of its phase", "claim", token, "issue", c.Issue, "error", err)
		return http.StatusInternalServerError, fmt.Sprintf("could not read %s's record after the retry: %v", c.Issue, err), true
	}
	if after != nil && after.Phase == phase.Held {
		return http.StatusConflict, fmt.Sprintf("%s is still held after the retry of its phase %s, so nothing relaunched %s: its tree lingers, and a lingering tree's members stay where they stood", c.Issue, working, token), true
	}
	s.log.Info("api: a retry restarted a phase", "claim", token, "issue", c.Issue, "phase", working)
	return http.StatusAccepted, "", true
}

// machineRetry is the retry an architect or a phase worker that runs nothing gets from its own
// machine: fresh budgets, and the session the claim records relaunched. The machine refuses it
// for a claim in any other state, naming that state, and for the daemon's controller, naming
// who retries it (supervise's operatorRetry). A caller that asked for a new session has
// forgotten the recorded one before it gets here (freshSession), so the relaunch spawns rather
// than resumes. A relaunch the machine holds for the agent's turn answers 202, as every
// operator request on a claim answers one (claimRequest).
func (s *server) machineRetry(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token) (int, bool) {
	held, ok := s.claimEvent(w, r, m, token, "retry", supervise.RequestOperatorRetry{Claim: token})
	if !ok {
		return 0, false
	}
	if held {
		return http.StatusAccepted, true
	}
	return http.StatusOK, true
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

// forgetRecordedSession has the claim drop the session it recorded, for a caller that is
// starting it in a new one (freshSession), and reports whether it did: a machine that refuses
// it — a claim running the session, which no fresh session reaches, or one that recorded
// another session since the route read it — is the caller's answer, given in place of the
// start's own.
func (s *server) forgetRecordedSession(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token, session string) bool {
	ctx, decided := s.decision(r, token)
	defer decided()
	if err := m.Handle(ctx, supervise.RequestForgetSession{Claim: token, Session: session}); err != nil {
		s.operatorFailure(w, "forget-session", token, err)
		return false
	}
	s.log.Info("api: a fresh session left the claim's recorded session behind", "claim", token, "session", session)
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
//
// What the last pod-update sweep decided is answered beside them (Options.PodUpdateRounds), and a
// daemon with no sweep behind it answers the zero record, which says so: an operator who is served
// no record at all cannot tell a daemon that holds no issue back from one too old to say. Every
// answer this route writes carries the record, so an answer without it is an older daemon's and
// nothing else (OperatorClaims.PodUpdateRounds).
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
	rounds := PodUpdateRounds{}
	if s.podUpdateRounds != nil {
		rounds = s.podUpdateRounds()
	}
	writeJSON(w, http.StatusOK, OperatorClaims{Claims: views, PodUpdateRounds: &rounds})
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
