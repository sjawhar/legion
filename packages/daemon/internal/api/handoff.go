package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// HandoffCompleteRequest is what a worker reports when its phase is done. Commit is the commit the
// completion is of: for a file-backed phase the pushed commit carrying .legion/<issue>/<phase>.json,
// which the `legion` tool's handoff_complete finds in the pane with its jj (as `legion handoff
// complete` did) before posting; for every other phase the commit the workspace stands on. The
// daemon reads no handoff file and no branch head: what each handoff holds is the role prompt's to
// spell, and the workflow refuses only a commit the role already reported for its previous phase
// (HANDOFF_NOT_NEW).
type HandoffCompleteRequest struct {
	GrantID string `json:"grantId"`
	Summary string `json:"summary"`
	Verdict string `json:"verdict"`
	Ready   bool   `json:"ready"`
	Commit  string `json:"commit"`
}

// HandoffCompleteResponse confirms that the completion fact committed. Note is what READY's checks
// say when READY was published without reading the head's checks - the pull request was already
// merged, or its base requires none (readyChecks) - so the merger's answer does not read like a
// head whose every required check was read and passed; every other completion answers none.
type HandoffCompleteResponse struct {
	Note string `json:"note,omitempty"`
}

func (s *server) handoffComplete(w http.ResponseWriter, r *http.Request) {
	var req HandoffCompleteRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w, field{"grantId", req.GrantID}, field{"summary", req.Summary}, field{"commit", req.Commit}) {
		return
	}
	grant, ok := s.redeem(w, r, req.GrantID)
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
	// The run this completion belongs to is the run of the task the worker took, which the claim
	// answers (supervise.Claim.ServingRun) and nothing on the request says.
	serving := machine.Claim().ServingRun()
	if serving == 0 {
		writeFailure(w, http.StatusConflict, "HANDOFF_NO_RUN",
			"this completion belongs to no task: this claim has taken none, so the run it reports cannot be told")
		return
	}
	issue, treeGeneration, round, err := s.handoffPosition(r.Context(), grant.Issue)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the issue record")
		return
	}
	if issue == nil {
		writeFailure(w, http.StatusNotFound, "ISSUE_NOT_FOUND", "issue is not recorded")
		return
	}
	// READY is held to the pull request's head on GitHub before the fact is applied: a refusal the
	// workflow committed would be recorded as processed under this completion's key, which leaves
	// the summary out, while a refusal here records nothing, and the corrected retry at the same
	// commit is applied. That holds for the packet's length too. The daemon posts the packet it tells
	// (record.ReadyPacket: the merger's packet, with a by-hand READY's note above it) as one Dispatch
	// message, the outbox's marker appended, and Dispatch refuses a longer body on every attempt, so
	// a packet over record.MessagePostLimit would never reach the human; refused only by the
	// workflow, the shortened packet at the same commit would be answered HANDOFF_ALREADY_RECORDED
	// and the issue would stay in merging.
	var answer readyAnswer
	if req.Ready {
		if answer, ok = s.readyNote(w, r, grant, issue, req.Commit); !ok {
			return
		}
		if length := dispatch.MessageBodyLength(record.ReadyPacket(req.Summary, answer.mergeNote)); length > record.MessagePostLimit {
			writeFailure(w, http.StatusBadRequest, "READY_PACKET_TOO_LONG", fmt.Sprintf(
				"the READY packet%s is %d characters over the %d the daemon can post as one Dispatch message (%d/%d): link the pull request body's ## Verification section instead of quoting it, then call handoff_complete again; this completion changed nothing",
				byHandShare(answer.mergeNote), length-record.MessagePostLimit, record.MessagePostLimit, length, record.MessagePostLimit))
			return
		}
	}
	// One phase's completion is identified by where the issue stands — its generation, phase, and
	// review round — with the role and what it reported: the commit, the verdict, and READY. A
	// retried call for the same phase is the same fact; the next phase's completion at the same
	// commit (the implementer's retro, then its production check) is a different one, and so is a
	// corrected report at the same commit (a merger's ready: true after READY_REQUIRED), since a
	// refusal is recorded as processed. The position is read before the fact's own transaction:
	// should the issue move in between, the engine re-reads it there and refuses a completion whose
	// role no longer owns the phase.
	//
	// The run the completion is attributed to is part of the key, because a refusal is recorded
	// under it: a stale completion of the run that is over would otherwise take the key of the new
	// run's completion of the same phase at the same commit, and the real one would be answered
	// ALREADY_RECORDED and never reach the workflow. The tree's generation is part of it for the
	// same reason: re-admission starts a new generation of the tree by bumping only the root's, and
	// restarts a child's worker in the phase it stood in, so a child's completion refused before
	// (while the tree lingered, or while the old gate was closed) would otherwise take the new
	// generation's key.
	eventID := fmt.Sprintf("handoff:%s:%d:%d:%d:%s:%s:%d:%s:%s:%t", grant.Issue, treeGeneration, issue.Generation, serving, grant.Role, issue.Phase, round, req.Commit, req.Verdict, req.Ready)
	result, err := intake.ApplyFact(r.Context(), s.pool, "api", eventID, intake.HandoffComplete{Generation: serving, Issue: grant.Issue, Role: grant.Role, Claim: grant.Claim,
		Summary: req.Summary, Verdict: req.Verdict, Ready: req.Ready, Commit: req.Commit, MergeNote: answer.mergeNote,
		RecordedHead: answer.recordedHead, RecordedHeadAt: answer.recordedHeadAt, HeadAt: answer.headAt}, s.handlers...)
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
		// A refused READY, the design gate's among them, keeps its by-hand note in the refusal: the
		// daemon submits a gate-refused READY's merge when the gate opens, and never a by-hand one's.
		message := result.Refusal.Message
		if answer.mergeNote != "" {
			message += " " + answer.mergeNote
		}
		writeFailure(w, result.Refusal.Status, result.Refusal.Code, message)
		return
	}
	writeJSON(w, http.StatusOK, HandoffCompleteResponse{Note: answer.note})
}

// byHandShare is the clause READY_PACKET_TOO_LONG names a by-hand note's share of the packet with,
// empty when READY carries none.
func byHandShare(mergeNote string) string {
	if mergeNote == "" {
		return ""
	}
	return ", with the by-hand note the daemon tells above it,"
}

// readyNote runs READY's checks (readyChecks) against issue's recorded pull request on GitHub for a
// merger's ready: true, and is what they leave (readyAnswer). Every refusal is written before the
// fact is applied, so nothing is recorded under the completion's key and the corrected retry at the
// same commit is applied:
//
//   - NO_REPOSITORY: the issue's project has no repository configured (a guard; configuration
//     requires one).
//   - READY's refusals (readyChecks): NO_PULL_REQUEST, READY_PULL_REQUEST_CLOSED,
//     READY_HEAD_CARRIES_HANDOFFS, READY_HEAD_MOVED, READY_NOT_APPROVED and READY_UNREVIEWED_CHANGE
//     (unreviewedChange: the head is not the review round's approved head plus retro's commits),
//     READY_HEAD_CONFLICTS, READY_FILES_TRUNCATED, READY_DISARM_PENDING, READY_CHECKS_NOT_GREEN.
//   - GITHUB_READ_FAILED: GitHub failed to answer a read; its failure, not the head's.
//
// The reads are made as the implement App, minted for the repository's owner, whose token never
// leaves the daemon; a runtime that boots without Apps publishes no READY (leaseForGrant).
func (s *server) readyNote(w http.ResponseWriter, r *http.Request, grant credential.Grant, issue *record.Issue, readyHead string) (answer readyAnswer, ok bool) {
	var repository ghrepo.Repository
	if s.repository != nil {
		repository, ok = s.repository(issue.Project)
	}
	if !ok || repository.IsZero() {
		writeFailure(w, http.StatusConflict, "NO_REPOSITORY", fmt.Sprintf("the project %s of %s has no repository configured, so its pull request cannot be read", issue.Project, issue.Key))
		return readyAnswer{}, false
	}
	lease, ok := s.leaseForGrant(w, r, grant, appauth.Implement, repository.Owner())
	if !ok {
		return readyAnswer{}, false
	}
	ctx := r.Context()
	pr, disarmPending, approvedHead, err := s.readyRecord(ctx, issue.Key)
	if err != nil {
		s.log.Error("api: read the pull request and approved head of a completing issue", "issue", issue.Key, "error", err)
		writeFailure(w, http.StatusInternalServerError, "RECORD_READ_FAILED", "could not read the issue's pull request and approved head")
		return readyAnswer{}, false
	}
	github := githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(s.githubAPI, repository)}
	var rules config.MergeRules
	if s.mergeRules != nil {
		rules = s.mergeRules(issue.Project)
	}
	answer, refused := readyChecks(ctx, github, repository, rules, issue.Key, readyHead, approvedHead, pr, disarmPending)
	if refused != nil {
		refused.write(w)
		return readyAnswer{}, false
	}
	return answer, true
}

// readyRecord reads, in one repeatable-read snapshot, issue's recorded pull request (nil when it
// has none), whether its unfinished disarm keeps an earlier auto-merge arm standing
// (record.Store.DisarmPending; by-hand READY refuses on that barrier, ordinary READY does not wait
// for it), and the head its review round approved, "" when it recorded none (record.ApprovedHead),
// which READY holds the pull request's head to (unreviewedChange).
func (s *server) readyRecord(ctx context.Context, issue string) (*record.PullRequest, bool, string, error) {
	if s.pool == nil || s.records == nil {
		return nil, false, "", errors.New("record dependencies are unavailable")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, false, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	pr, err := s.records.PullRequest(ctx, tx, issue)
	if err != nil {
		return nil, false, "", err
	}
	pending, err := s.records.DisarmPending(ctx, tx, issue)
	if err != nil {
		return nil, false, "", err
	}
	head, err := record.ApprovedHead(ctx, s.records, tx, issue)
	return pr, pending, head, err
}

// handoffPosition reads the issue record, its tree's generation (the root's), and its review round:
// the implementer's count of returns to implementing, which tells each implementing, testing, and
// reviewing pass from the last.
func (s *server) handoffPosition(ctx context.Context, key string) (*record.Issue, uint64, int, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	issue, err := s.records.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return nil, 0, 0, err
	}
	root := issue
	if !claim.IsTreeRoot(issue.Key, issue.Tree) {
		if root, err = s.records.Issue(ctx, tx, issue.Tree); err != nil {
			return nil, 0, 0, err
		}
		if root == nil {
			return nil, 0, 0, fmt.Errorf("the tree root %s of %s is not recorded", issue.Tree, issue.Key)
		}
	}
	rows, err := s.records.Phases(ctx, tx, key)
	if err != nil {
		return nil, 0, 0, err
	}
	round := 0
	for _, row := range rows {
		if row.Role == claim.RoleImplementer {
			round = row.Rounds
		}
	}
	return issue, root.Generation, round, tx.Commit(ctx)
}
