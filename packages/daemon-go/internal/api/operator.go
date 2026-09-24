package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// invalidOperatorToken is every operator route's one refusal, the shipped daemon's sentence
// (packages/daemon/src/daemon/api/routes/controller.ts:87).
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

// OperatorClaim is a claim as the operator sees it: everything the supervisor holds about it but
// its two hashes.
type OperatorClaim struct {
	Token       claim.Token `json:"token"`
	Tree        string      `json:"tree"`
	Issue       string      `json:"issue"`
	Role        claim.Role  `json:"role"`
	Generation  uint64      `json:"generation"`
	State       string      `json:"state"`
	Session     string      `json:"session"`
	SessionFile string      `json:"sessionFile"`
	// Locator is the claim's process, the runtime's nested shape; none while nothing runs.
	Locator         *runtime.Locator `json:"locator,omitempty"`
	Budgets         BudgetsView      `json:"budgets"`
	UncertainStreak int              `json:"uncertainStreak"`
	// Pending is the task the claim holds, until the turn it started ends.
	Pending *DeliveryView `json:"pending,omitempty"`
}

// BudgetsView is the claim's retry counters (`supervise.Budgets`).
type BudgetsView struct {
	LaunchFailures int `json:"launchFailures"`
	PromptFailures int `json:"promptFailures"`
	PromptRetires  int `json:"promptRetires"`
}

// DeliveryView is the claim's pending task (`supervise.Delivery`): DeliveredAt is the latest
// send's acknowledgement, ConfirmedAt the turn it started; each absent until it happens.
type DeliveryView struct {
	ID          string     `json:"id"`
	Task        string     `json:"task"`
	QueuedAt    time.Time  `json:"queuedAt"`
	DeliveredAt *time.Time `json:"deliveredAt,omitempty"`
	ConfirmedAt *time.Time `json:"confirmedAt,omitempty"`
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
			PromptFailures: c.Budgets.PromptFailures,
			PromptRetires:  c.Budgets.PromptRetires,
		},
		UncertainStreak: c.UncertainStreak,
	}
	if p := c.Pending; p != nil {
		view.Pending = &DeliveryView{
			ID: p.ID, Task: p.Task, QueuedAt: p.QueuedAt,
			DeliveredAt: instant(p.DeliveredAt), ConfirmedAt: instant(p.ConfirmedAt),
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
	if !readBody(w, r, &req) || !requireFields(w,
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
	ctx := context.WithoutCancel(r.Context())
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
	writeJSON(w, status, operatorView(m.Claim()))
}

// claimRequest is one operator request on an existing claim: event builds the request the claim's
// machine is posted, or answers the caller itself and reports false.
func (s *server) claimRequest(request string, event func(http.ResponseWriter, *http.Request, claim.Token) (supervise.Event, bool)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := claim.Token(r.PathValue("token"))
		m, ok := s.supervisor.Machine(token)
		if !ok {
			writeJSON(w, http.StatusNotFound, errorBody("no claim "+string(token)))
			return
		}
		ev, ok := event(w, r, token)
		if !ok {
			return
		}
		if err := m.Handle(context.WithoutCancel(r.Context()), ev); err != nil {
			s.operatorFailure(w, request, token, err)
			return
		}
		writeJSON(w, http.StatusOK, operatorView(m.Claim()))
	}
}

func deliverEvent(w http.ResponseWriter, r *http.Request, token claim.Token) (supervise.Event, bool) {
	var req DeliverRequest
	if !readBody(w, r, &req) || !requireFields(w, field{"task", req.Task}) {
		return nil, false
	}
	return supervise.RequestDeliver{Claim: token, Task: req.Task}, true
}

func suspendEvent(_ http.ResponseWriter, _ *http.Request, token claim.Token) (supervise.Event, bool) {
	return supervise.RequestSuspend{Claim: token}, true
}

func resumeEvent(_ http.ResponseWriter, _ *http.Request, token claim.Token) (supervise.Event, bool) {
	return supervise.RequestResume{Claim: token}, true
}

func stopEvent(_ http.ResponseWriter, _ *http.Request, token claim.Token) (supervise.Event, bool) {
	return supervise.RequestStop{Claim: token}, true
}

// list answers every claim the daemon supervises, in token order.
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
	s.log.Error("api: an operator request failed", "request", request, "claim", token, "error", err)
	writeJSON(w, http.StatusInternalServerError, errorBody(fmt.Sprintf("%s %s: %v", request, token, err)))
}
