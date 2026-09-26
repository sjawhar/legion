package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

type HandoffCompleteRequest struct {
	GrantID string `json:"grantId"`
	Summary string `json:"summary"`
	Verdict string `json:"verdict"`
	Ready   bool   `json:"ready"`
	Commit  string `json:"commit"`
}

// HandoffCompleteResponse confirms that the completion fact committed.
type HandoffCompleteResponse struct{}

func (s *server) handoffComplete(w http.ResponseWriter, r *http.Request) {
	var req HandoffCompleteRequest
	if !readBody(w, r, &req) || !requireFailureFields(w, field{"grantId", req.GrantID}, field{"summary", req.Summary}, field{"commit", req.Commit}) {
		return
	}
	grant, ok := s.redeem(w, req.GrantID)
	if !ok {
		return
	}
	if grant.Controller {
		writeFailure(w, http.StatusForbidden, "GRANT_INVALID", "grant is unavailable")
		return
	}
	if grant.Role == claim.RoleTester && req.Verdict != "pass" && req.Verdict != "fail" {
		writeFailure(w, http.StatusBadRequest, "TESTER_VERDICT_REQUIRED", "tester verdict must be pass or fail")
		return
	}
	if grant.Role != claim.RoleTester && req.Verdict != "" {
		writeFailure(w, http.StatusBadRequest, "VERDICT_ROLE_FORBIDDEN", "only a tester may report a verdict")
		return
	}
	if req.Ready && grant.Role != claim.RoleMerger {
		writeFailure(w, http.StatusBadRequest, "READY_ROLE_FORBIDDEN", "only a merger may report ready")
		return
	}
	if s.pool == nil || s.records == nil {
		writeFailure(w, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "fact intake is unavailable")
		return
	}
	machine, running := s.supervisor.Machine(grant.Claim)
	if !running {
		writeFailure(w, http.StatusConflict, "HANDOFF_NO_CLAIM", "the daemon supervises no claim for this grant")
		return
	}
	// The run this completion belongs to is the run of the task the worker took: the delivery
	// whose turn is running, or — after that turn ends, which retires the delivery — the run the
	// claim is left serving. The pending delivery alone would not do: a worker told to wait is
	// woken by a notice, whose turn no delivery backs, and its completion still belongs to the run
	// whose task it took. The claim's field alone would not do either: an operator's task carries
	// no run, and a confirmation a busy refusal took back never ran. The pane's own environment
	// cannot say it at all — LEGION_GENERATION is the claim's launch counter, and a live worker is
	// handed the next run's task without being relaunched.
	held := machine.Claim()
	serving := held.ServingGeneration
	if p := held.Pending; p != nil && p.Generation != 0 {
		// A task whose turn is running is the run being worked. A task the agent may have read —
		// delivered, and its turn never seen to start — is one too, when the claim has retired no
		// confirmed task of a run: Oh My Pi's own agent_start names no prompt, so a turn starting
		// after the five-second bound is indistinguishable from a foreign one and confirms
		// nothing, and the worker of a first task would otherwise have its completion refused.
		// A claim serving no run is not a session that has done no work — it may have worked
		// tasks whose turns were never confirmed — but it is one this daemon has seen take no
		// other run's task, so there is no other run this could belong to. A task refused in a
		// turn of the agent's own is not read at all: that take-back drops the delivered mark,
		// and a completion from that foreign turn falls through to the run the claim serves.
		if !p.ConfirmedAt.IsZero() || (serving == 0 && !p.DeliveredAt.IsZero()) {
			serving = p.Generation
		}
	}
	if serving == 0 {
		writeFailure(w, http.StatusConflict, "HANDOFF_NO_RUN",
			"this completion belongs to no task: this claim has taken none, so the run it reports cannot be told")
		return
	}
	issue, round, err := s.handoffPosition(r.Context(), grant.Issue)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the issue record")
		return
	}
	if issue == nil {
		writeFailure(w, http.StatusNotFound, "ISSUE_NOT_FOUND", "issue is not recorded")
		return
	}
	// One phase's completion is identified by where the issue stands — its generation, phase, and
	// review round — with the role and what it reported: the commit, the verdict, and READY. A
	// retried call for the same phase is the same fact; the next phase's completion at the same
	// commit (the implementer's retro, then its production check) is a different one, and so is a
	// corrected report at the same commit (a merger's --ready after READY_REQUIRED), since a
	// refusal is recorded as processed. The position is read before the fact's own transaction:
	// should the issue move in between, the engine re-reads it there and refuses a completion whose
	// role no longer owns the phase.
	//
	// The run the completion is attributed to is part of the key, because a refusal is recorded
	// under it: a stale completion of the run that is over would otherwise take the key of the new
	// run's completion of the same phase at the same commit, and the real one would be answered
	// ALREADY_RECORDED and never reach the workflow.
	eventID := fmt.Sprintf("handoff:%s:%d:%d:%s:%s:%d:%s:%s:%t", grant.Issue, issue.Generation, serving, grant.Role, issue.Phase, round, req.Commit, req.Verdict, req.Ready)
	result, err := intake.ApplyFact(r.Context(), s.pool, "api", eventID, intake.HandoffComplete{Generation: serving, Issue: grant.Issue, Role: grant.Role, Claim: grant.Claim, Summary: req.Summary, Verdict: req.Verdict, Ready: req.Ready, Commit: req.Commit}, s.handlers...)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply handoff fact")
		return
	}
	if result.Duplicate {
		writeFailure(w, http.StatusConflict, "HANDOFF_ALREADY_RECORDED", fmt.Sprintf(
			"the %s completion of phase %s (round %d) at commit %s was already received; this call changed nothing",
			grant.Role, issue.Phase, round, req.Commit))
		return
	}
	if result.Refusal != nil {
		writeFailure(w, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
		return
	}
	writeJSON(w, http.StatusOK, HandoffCompleteResponse{})
}

// handoffPosition reads the issue record and its review round: the implementer's count of returns
// to implementing, which tells each implementing, testing, and reviewing pass from the last.
func (s *server) handoffPosition(ctx context.Context, key string) (*record.Issue, int, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	issue, err := s.records.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return nil, 0, err
	}
	rows, err := s.records.Phases(ctx, tx, key)
	if err != nil {
		return nil, 0, err
	}
	round := 0
	for _, row := range rows {
		if row.Role == claim.RoleImplementer {
			round = row.Rounds
		}
	}
	return issue, round, tx.Commit(ctx)
}
