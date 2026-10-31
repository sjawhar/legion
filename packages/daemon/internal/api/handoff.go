package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// HandoffCompleteRequest is what a worker reports when its phase is done, or what the implementer
// reports on a look of its closed issue's production check. Commit is the commit a phase
// completion is of: for a file-backed phase the pushed commit carrying .legion/<issue>/<phase>.json,
// which the `legion` tool's handoff_complete finds in the pane with its jj (as `legion handoff
// complete` did) before posting; for every other phase the commit the workspace stands on. The
// daemon reads no handoff file and no branch head: what each handoff holds is the role prompt's to
// spell, and the workflow refuses only a commit the role already reported for its previous phase
// (HANDOFF_NOT_NEW). The implementer's completion of implementing is also held to an open pull
// request on its branch, or a merged one recorded (implementationPullRequest:
// HANDOFF_NO_PULL_REQUEST). A check report carries no commit (nothing was pushed) and a Verdict of
// pass, pending or fail; a tester's completion a Verdict of pass or fail.
type HandoffCompleteRequest struct {
	GrantID string `json:"grantId"`
	Summary string `json:"summary"`
	Verdict string `json:"verdict"`
	Commit  string `json:"commit,omitempty"`
}

// HandoffCompleteResponse confirms that the completion fact committed. It carries nothing: the
// daemon merges nothing and reads no head, so no completion has anything to answer with.
type HandoffCompleteResponse struct{}

// handoffComplete records a phase completion or a production check report (checkReport). The grant
// is redeemed and the record read before the two are told apart, so an invalid grant is refused
// before a field only one of them requires: a call by the implementer on its closed issue (phase
// done, which no role works: the check is the work after the close) is a check report when the
// issue's check is open, and is refused CHECK_NOT_OPEN otherwise; every other call is a phase
// completion, held to its commit, its role's verdict rule and the run its claim serves, the
// implementer's in implementing to an open pull request on its branch or a merged one recorded
// (implementationPullRequest), and the engine refuses one of a phase its role does not run
// (HANDOFF_NOT_CURRENT_PHASE).
func (s *server) handoffComplete(w http.ResponseWriter, r *http.Request) {
	var req HandoffCompleteRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w, field{"grantId", req.GrantID}, field{"summary", req.Summary}) {
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
	if s.pool == nil || s.records == nil {
		writeFailure(w, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "fact intake is unavailable")
		return
	}
	machine, running := s.supervisor.Machine(grant.Claim)
	if !running {
		writeFailure(w, http.StatusConflict, "HANDOFF_NO_CLAIM", "the daemon supervises no claim for this grant")
		return
	}
	position, err := s.handoffPosition(r.Context(), grant.Issue)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the issue record")
		return
	}
	issue := position.issue
	if issue == nil {
		writeFailure(w, http.StatusNotFound, "ISSUE_NOT_FOUND", "issue is not recorded")
		return
	}
	if grant.Role == claim.RoleImplementer && issue.Phase == phase.Done {
		s.checkReport(w, r, grant, position.check, req)
		return
	}
	if !requireFailureFields(w, field{"commit", req.Commit}) {
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
	// The run this completion belongs to is the run of the task the worker took, which the claim
	// answers (supervise.Claim.ServingRun) and nothing on the request says.
	serving := machine.Claim().ServingRun()
	if serving == 0 {
		writeFailure(w, http.StatusConflict, "HANDOFF_NO_RUN",
			"this completion belongs to no task: this claim has taken none, so the run it reports cannot be told")
		return
	}
	// A completion of a run that is over is refused here, before the fact is applied, so nothing is
	// recorded under its key: a refusal the workflow committed would be stored as processed, and the
	// worker's retry at the same commit would be answered HANDOFF_ALREADY_RECORDED, "received", while
	// the phase waits. The worker's turn began before the current run's task reached it; that task
	// arrives as its next prompt, and the same completion in its turn is applied.
	if serving != issue.Generation {
		writeFailure(w, http.StatusConflict, "HANDOFF_STALE_GENERATION", staleCompletion(grant.Issue, issue.Generation, serving, machine.Claim().Pending))
		return
	}
	treeGeneration, pass := position.treeGeneration, position.pass
	// One phase's completion is identified by where the issue stands — its generation, phase, and
	// pass through that phase — with the role and what it reported: the commit and the verdict. A
	// retried call for the same phase is the same fact; the next phase's completion at the same
	// commit (the implementer's retro, standing on its implementing pass's last push) is a
	// different one, since the phase and the role are in the key; and so is a second report of the
	// same phase at the same commit that names another verdict — the tester's corrected pass or
	// fail, the only field a role may report differently of one commit — since a refusal the
	// workflow commits is recorded as processed, and a corrected report must not be answered as
	// that refusal's duplicate. The summary is left out on purpose: a call that only rewords it is
	// the same completion, and the first one is what the workflow took. The position is read before
	// the fact's own transaction: should the issue move in between, the engine re-reads
	// it there and refuses a completion whose role no longer owns the phase, or whose run is no
	// longer the issue's (workflow's HANDOFF_STALE_GENERATION guard).
	//
	// The run the completion is attributed to is part of the key for that guard's sake: a completion
	// the route let through and the engine then refused as stale is recorded under it, and without
	// the run it would take the key of the new run's completion of the same phase at the same
	// commit, which would be answered ALREADY_RECORDED and never reach the workflow. With it, that
	// recorded refusal blocks no later call: its key carries the issue generation this route read,
	// the one the issue has since left, so a retry is refused here (recording nothing) and the new
	// run's completion carries the new issue generation and serving run, another key. The tree's
	// generation is part of it for the same reason: re-admission starts a new generation of the tree
	// by bumping only the root's, and restarts a child's worker in the phase it stood in, so a
	// child's completion refused before (while the tree lingered, or while the old gate was closed)
	// would otherwise take the new generation's key.
	eventID := fmt.Sprintf("handoff:%s:%d:%d:%d:%s:%s:%d:%s:%s", grant.Issue, treeGeneration, issue.Generation, serving, grant.Role, issue.Phase, pass, req.Commit, req.Verdict)
	if grant.Role == claim.RoleImplementer && issue.Phase == phase.Implementing && !position.lingers && req.Commit != position.lastHandoff {
		if !s.implementationPullRequest(w, r, issue, position.pullRequest, eventID) {
			return
		}
	}
	result, err := intake.ApplyFact(r.Context(), s.pool, "api", eventID, intake.HandoffComplete{Generation: serving, Issue: grant.Issue, Role: grant.Role, Claim: grant.Claim,
		Summary: req.Summary, Verdict: req.Verdict, Commit: req.Commit}, s.handlers...)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply handoff fact")
		return
	}
	if result.Duplicate {
		writeFailure(w, http.StatusConflict, "HANDOFF_ALREADY_RECORDED", fmt.Sprintf(
			"the %s completion of phase %s (pass %d through it) at commit %s was already received; this call changed nothing",
			grant.Role, issue.Phase, pass, req.Commit))
		return
	}
	if result.Refusal != nil {
		writeFailure(w, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
		return
	}
	// The completion is recorded and its transaction committed, so the daemon's read of the issue's
	// pull request judges what this phase left; the hook only queues that read, which runs on the
	// daemon's own goroutine, so this answer waits for no GitHub call.
	if s.handoffCompleted != nil {
		s.handoffCompleted(grant.Issue)
	}
	writeJSON(w, http.StatusOK, HandoffCompleteResponse{})
}

// staleCompletion is the refusal of a completion attributed to a run the issue has left. It names
// the current run's task when the claim holds one, which the worker's turn began before: that task
// is sent as the worker's next prompt, and the same completion in its turn is applied.
func staleCompletion(issue string, current, serving uint64, pending *supervise.Delivery) string {
	stale := fmt.Sprintf("%s is on generation %d and this completion is generation %d's; nothing was recorded", issue, current, serving)
	if pending != nil && pending.Generation == current {
		return fmt.Sprintf("%s: your turn began before the daemon's generation-%d task (%s) reached this session. End this turn without completing; that task arrives as your next prompt, and handoff_complete at the same commit in its turn is applied",
			stale, current, pending.ID)
	}
	return fmt.Sprintf("%s: the daemon has queued no task of generation %d for this claim, and may queue none (the new run's phase may be another claim's). End this turn without completing and tell the architect; if that task arrives, call handoff_complete at the same commit in its turn", stale, current)
}

// implementationPullRequest is the implementer's completion's read of GitHub: in implementing, a
// completion moves its issue to testing only on a pull request recorded open or merged for it
// (workflow's TriggerImplementationReady), and only the opened webhook, or the daemon's per-issue
// read standing in for one that webhook lost (readIssuePullRequest), records one open. So before
// the completion is applied, the open pull request on the issue's branch (claim.IssueBranch) is
// read as the implement App, minted for the owner of the issue's repository in GitHub's spelling
// (record.Issue.GitHubRepo, which the webhook's facts name it by), and it says whether the
// completion goes on:
//
//   - one open: when the record does not already hold it open, the intake.PullRequestOpened its
//     webhook would have carried is applied first, a reopen when the record holds the same pull
//     request closed, under the read's stable id (intake.OpenedRead). The completion then
//     moves the issue to testing. A failure to apply it is FACT_APPLY_FAILED, with the completion
//     not applied.
//   - none, with the record holding the pull request merged: the completion is applied and moves
//     the issue to testing. The work landed, its merge told the architect (pr-merged), and the
//     workflow runs on.
//   - none otherwise: HANDOFF_NO_PULL_REQUEST, 409, and nothing is applied, so the completion at
//     the same commit is applied once the pull request is open. When the record holds the pull
//     request closed without merging, the refusal names it: the close told the architect, which
//     decides the phase.
//   - unreadable (a repository the record cannot name, a failed token mint or a failed read,
//     GitHub's rate limit among them): logged WARN, and the completion is applied as it was before
//     this read, so GitHub's trouble never blocks it; the per-issue read records the pull request
//     when it finds one.
//
// A completion the workflow refuses whatever GitHub shows reads nothing: the route calls this only
// for one that is not already applied (eventID, its key, read with intake.Applied), whose tree does
// not linger and whose commit is not the one the implementer reported for its previous phase
// (workflow's HANDOFF_NOT_NEW). It returns false when it wrote the answer.
func (s *server) implementationPullRequest(w http.ResponseWriter, r *http.Request, issue *record.Issue, recorded *record.PullRequest, eventID string) bool {
	ctx := r.Context()
	applied, err := intake.Applied(ctx, s.pool, "api", []string{eventID})
	if err != nil {
		s.log.Error("api: read whether a completion is already applied", "issue", issue.Key, "error", err)
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read whether this completion was already received")
		return false
	}
	if applied[eventID] {
		return true
	}
	if s.tokens == nil {
		writeFailure(w, http.StatusInternalServerError, "GITHUB_TOKEN_SOURCE_UNAVAILABLE", "GitHub token source is unavailable")
		return false
	}
	branch := claim.IssueBranch(issue.Key)
	logUnread := func(err error) {
		attributes := []any{"issue", issue.Key, "repo", issue.GitHubRepo, "branch", branch, "error", err}
		var answer *githubrest.Answer
		if errors.As(err, &answer) {
			attributes = append(attributes, "status", answer.Status)
		}
		s.log.Warn("api: read the open pull request on a completing implementer's branch; the completion is applied unread, and the per-issue read records the pull request it finds", attributes...)
	}
	repository, err := ghrepo.Parse("the GitHub repository of "+issue.Key, issue.GitHubRepo)
	if err != nil {
		logUnread(err)
		return true
	}
	lease, err := s.tokens.Token(ctx, appauth.Implement, repository.Owner())
	if err != nil {
		logUnread(fmt.Errorf("mint the implement App token for %s: %w", repository.Owner(), err))
		return true
	}
	github := githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(s.githubAPI, repository)}
	pull, found, err := github.OpenPullRequestOn(ctx, repository.Owner(), branch)
	if err != nil {
		logUnread(err)
		return true
	}
	if !found {
		// This read is GitHub's own answer on an open pull request, so of the states that let the
		// implementation move on (record.LetsImplementationMove) only merged still does here: a
		// record holding the pull request open is one GitHub closed or merged since the daemon last
		// heard, and the completion waits for whichever of the two the daemon records next.
		if record.LetsImplementationMove(recorded) && recorded.State != record.PullRequestOpen {
			return true
		}
		message := fmt.Sprintf("no open pull request on %s %s; nothing was recorded. Open the pull request and complete again.", issue.GitHubRepo, branch)
		if recorded != nil && recorded.State == record.PullRequestClosed {
			message += fmt.Sprintf(" pull request #%d is closed; your architect was told and decides this issue's phase.", recorded.Number)
		}
		writeFailure(w, http.StatusConflict, "HANDOFF_NO_PULL_REQUEST", message)
		return false
	}
	if recorded != nil && recorded.State == record.PullRequestOpen && recorded.Number == pull.Number {
		return true
	}
	fact, read := intake.OpenedRead(issue.Project, issue.GitHubRepo, branch, pull, issue.Generation, recorded)
	if _, err := intake.ApplyFact(ctx, s.pool, "github", read, fact, s.handlers...); err != nil {
		s.log.Error("api: apply the open pull request a completing implementer's branch shows", "issue", issue.Key, "pullRequest", pull.Number, "error", err)
		writeFailure(w, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply the open pull request read from GitHub; this completion changed nothing")
		return false
	}
	return true
}

// checkReport records the implementer's report on a look of its closed issue's production check
// (intake.CheckReport): the check must be open (CHECK_NOT_OPEN: a done issue whose check closed, or
// never opened, since nothing merged), and the verdict one of pass, pending or fail
// (CHECK_VERDICT_REQUIRED). No commit is read: the look pushed nothing. No run is read either: the
// check fences itself on the issue and the look the route read as the current one, which the
// engine holds the report to (CHECK_LOOK_NOT_OPEN), rather than on the claim's run, since the look's
// task is no phase's run and a woken session may hold an older one. However that look ended, its
// report counts while the check is open (dispatch://INFRA-714/spec R12). The event is the issue, the
// check (record.ProductionCheck.ID: a later merge of the same issue replaces the check and counts
// its looks from one again, and the rerun's report on look 1 is a new fact, not the first check's),
// the look and the verdict: a retried report of the same look is the same fact, and one the engine
// refuses is left unmarked (intake's ApplyFact), so the implementer's next send of it applies.
func (s *server) checkReport(w http.ResponseWriter, r *http.Request, grant credential.Grant, check *record.ProductionCheck, req HandoffCompleteRequest) {
	if check == nil || !check.Open() {
		writeFailure(w, http.StatusConflict, "CHECK_NOT_OPEN", fmt.Sprintf("%s has no open production check; this completion changed nothing", grant.Issue))
		return
	}
	if req.Verdict != "pass" && req.Verdict != "pending" && req.Verdict != "fail" {
		writeFailure(w, http.StatusBadRequest, "CHECK_VERDICT_REQUIRED", "a production check report names a verdict of pass, pending or fail")
		return
	}
	eventID := fmt.Sprintf("check:%s:%s:%d:%s", grant.Issue, check.ID(), check.Looks, req.Verdict)
	result, err := intake.ApplyFact(r.Context(), s.pool, "api", eventID, intake.CheckReport{Issue: grant.Issue, Claim: grant.Claim, Look: check.Looks,
		Verdict: req.Verdict, Summary: req.Summary}, s.handlers...)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply handoff fact")
		return
	}
	if result.Duplicate {
		writeFailure(w, http.StatusConflict, "HANDOFF_ALREADY_RECORDED", fmt.Sprintf(
			"the %s report on look %d of %s's production check was already received; this call changed nothing", req.Verdict, check.Looks, grant.Issue))
		return
	}
	if result.Refusal != nil {
		writeFailure(w, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
		return
	}
	writeJSON(w, http.StatusOK, HandoffCompleteResponse{})
}

// completionPosition is where issue key stands as the completion route reads it, in one repeatable-read
// snapshot: the issue record, its tree's generation (the root's) and whether the tree lingers, its
// pass — the implementer's count of returns to an earlier phase, which tells each implementing,
// testing, and reviewing pass from the last — and the commit the implementer last reported for a
// file-backed phase (record.PhaseRow.LastHandoff), its pull request record while it is implementing
// (the one phase the route reads it in), nil in any other phase or while none is recorded, and its
// production check, nil while none was ever opened.
type completionPosition struct {
	issue          *record.Issue
	treeGeneration uint64
	lingers        bool
	pass           int
	lastHandoff    string
	pullRequest    *record.PullRequest
	check          *record.ProductionCheck
}

// handoffPosition reads key's position (completionPosition); an issue the workflow does not record is
// a position with no issue and no error.
func (s *server) handoffPosition(ctx context.Context, key string) (completionPosition, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return completionPosition{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	issue, err := s.records.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return completionPosition{}, err
	}
	root := issue
	if !claim.IsTreeRoot(issue.Key, issue.Tree) {
		if root, err = s.records.Issue(ctx, tx, issue.Tree); err != nil {
			return completionPosition{}, err
		}
		if root == nil {
			return completionPosition{}, fmt.Errorf("the tree root %s of %s is not recorded", issue.Tree, issue.Key)
		}
	}
	rows, err := s.records.Phases(ctx, tx, key)
	if err != nil {
		return completionPosition{}, err
	}
	pass, lastHandoff := 0, ""
	for _, row := range rows {
		if row.Role == claim.RoleImplementer {
			pass, lastHandoff = row.Pass, row.LastHandoff
		}
	}
	var pr *record.PullRequest
	if issue.Phase == phase.Implementing {
		if pr, err = s.records.PullRequest(ctx, tx, key); err != nil {
			return completionPosition{}, err
		}
	}
	check, err := s.records.ProductionCheck(ctx, tx, key)
	if err != nil {
		return completionPosition{}, err
	}
	return completionPosition{issue: issue, treeGeneration: root.Generation, lingers: root.Lingers(), pass: pass, lastHandoff: lastHandoff,
		pullRequest: pr, check: check}, tx.Commit(ctx)
}
