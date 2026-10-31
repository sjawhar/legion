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
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// HandoffCompleteRequest is what a worker reports when its phase is done, or what the implementer
// reports on a look of its closed issue's production check. A phase with a handoff (planning,
// implementing, testing, reviewing; phase.Handoff) ends with a handoff record on the Dispatch
// issue, which the worker writes with `dispatch handoff-write` under its own session, and its
// completion names it: HandoffID and HandoffVersion, which the `legion` tool's handoff_complete
// reads as the newest record of the phase before posting. The daemon reads that record back from
// Dispatch and holds it to the issue, the phase's word, the version and the claim's session
// (handoffRecord); it reads no body, which is the next role's to read. Commit is the commit the
// implementer's completion is of — its pushed tip for an implementing round, the commit it stands
// on for retro — and the head the merger's READY names; the planner, the tester and the reviewer
// report none. The workflow refuses a record the role already named for its previous phase
// (HANDOFF_NOT_NEW). The implementer's completion of implementing is also held to an open pull
// request on its branch, or a merged one recorded (implementationPullRequest:
// HANDOFF_NO_PULL_REQUEST). A check report carries no commit and no record (nothing was pushed or
// written) and a Verdict of pass, pending or fail; a tester's completion a Verdict of pass or fail.
type HandoffCompleteRequest struct {
	GrantID        string `json:"grantId"`
	Summary        string `json:"summary"`
	Verdict        string `json:"verdict"`
	Ready          bool   `json:"ready"`
	Commit         string `json:"commit,omitempty"`
	HandoffID      string `json:"handoffId,omitempty"`
	HandoffVersion int    `json:"handoffVersion,omitempty"`
}

// HandoffCompleteResponse confirms that the completion fact committed. Note is what READY answers
// when it has something to say about the merge it is publishing: that the pull request was already
// merged, so its head was published unread (readyChecks), or that a person merges this one
// (mergeReadyPolicy's by-hand note); every other completion answers none.
type HandoffCompleteResponse struct {
	Note string `json:"note,omitempty"`
}

// handoffComplete records a phase completion or a production check report (checkReport). The grant
// is redeemed and the record read before the two are told apart, so an invalid grant is refused
// before a field only one of them requires: a call by the implementer on its closed issue (phase
// done, which no role works: the check is the work after the close) is a check report when the
// issue's check is open, and is refused CHECK_NOT_OPEN otherwise; every other call is a phase
// completion, held to its commit (the implementer's and the merger's), the Dispatch handoff record
// a file-backed phase ends with (handoffRecord), its role's verdict rule and the run its claim
// serves, the implementer's in implementing to an open pull request on its branch or a merged one
// recorded (implementationPullRequest), and the engine refuses one of a phase its role does not
// run (HANDOFF_NOT_CURRENT_PHASE).
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
	if (grant.Role == claim.RoleImplementer || grant.Role == claim.RoleMerger) && !requireFailureFields(w, field{"commit", req.Commit}) {
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
	held := machine.Claim()
	serving := held.ServingRun()
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
	// A phase with a handoff ends with a Dispatch handoff record, which its writer's completion
	// names and the route reads back and holds to the completion before anything is recorded; a
	// phase that writes none takes no record. A completion of such a phase by a role other than
	// its writer is the engine's to refuse (HANDOFF_NOT_CURRENT_PHASE), so its record is not read.
	word, writer, handoffPhase := phase.Handoff(issue.Phase)
	recordBacked := handoffPhase && writer == grant.Role
	switch {
	case recordBacked:
		if !s.handoffRecord(w, r, held.Session, issue, word, req) {
			return
		}
	case !handoffPhase && (req.HandoffID != "" || req.HandoffVersion != 0):
		writeFailure(w, http.StatusConflict, "HANDOFF_RECORD_MISMATCH", fmt.Sprintf(
			"phase %s of %s ends with no handoff record, and this completion names one: complete without handoffId and handoffVersion; this completion changed nothing", issue.Phase, issue.Key))
		return
	}
	treeGeneration, round := position.treeGeneration, position.round
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
	// review round — with the role and what it reported: the handoff record a file-backed phase
	// names, or the commit every other phase reports, the verdict, and READY. A retried call for the
	// same phase is the same fact, and so is one naming the same record at another commit (the
	// record is the phase's handoff); the next phase's completion at the same commit (the
	// implementer's retro, standing on its implementing round's last push) is a different one, and
	// so is a corrected report at the same commit (a merger's ready: true after READY_REQUIRED),
	// since a refusal the workflow commits is recorded as processed. The position is read before the
	// fact's own transaction: should the issue move in between, the engine re-reads it there and
	// refuses a completion whose role no longer owns the phase, or whose run is no longer the
	// issue's (workflow's HANDOFF_STALE_GENERATION guard).
	//
	// The run the completion is attributed to is part of the key for that guard's sake: a completion
	// the route let through and the engine then refused as stale is recorded under it, and without
	// the run it would take the key of the new run's completion of the same phase with the same
	// report, which would be answered ALREADY_RECORDED and never reach the workflow. With it, that
	// recorded refusal blocks no later call: its key carries the issue generation this route read,
	// the one the issue has since left, so a retry is refused here (recording nothing) and the new
	// run's completion carries the new issue generation and serving run, another key. The tree's
	// generation is part of it for the same reason: re-admission starts a new generation of the tree
	// by bumping only the root's, and restarts a child's worker in the phase it stood in, so a
	// child's completion refused before (while the tree lingered, or while the old gate was closed)
	// would otherwise take the new generation's key.
	reported, received := req.Commit, "at commit "+req.Commit
	if recordBacked {
		reported, received = req.HandoffID, fmt.Sprintf("naming handoff record %s (%s version %d)", req.HandoffID, word, req.HandoffVersion)
	}
	eventID := fmt.Sprintf("handoff:%s:%d:%d:%d:%s:%s:%d:%s:%s:%t", grant.Issue, treeGeneration, issue.Generation, serving, grant.Role, issue.Phase, round, reported, req.Verdict, req.Ready)
	if grant.Role == claim.RoleImplementer && issue.Phase == phase.Implementing && !position.lingers && reported != position.lastHandoff {
		if !s.implementationPullRequest(w, r, issue, position.pullRequest, eventID) {
			return
		}
	}
	result, err := intake.ApplyFact(r.Context(), s.pool, "api", eventID, intake.HandoffComplete{Generation: serving, Issue: grant.Issue, Role: grant.Role, Claim: grant.Claim,
		Summary: req.Summary, Verdict: req.Verdict, Ready: req.Ready, Commit: req.Commit, HandoffRecord: req.HandoffID, HandoffVersion: req.HandoffVersion,
		MergeNote: answer.mergeNote, RecordedHead: answer.recordedHead, RecordedHeadAt: answer.recordedHeadAt, HeadAt: answer.headAt}, s.handlers...)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply handoff fact")
		return
	}
	if result.Duplicate {
		writeFailure(w, http.StatusConflict, "HANDOFF_ALREADY_RECORDED", fmt.Sprintf(
			"the %s completion of phase %s (round %d) %s was already received; this call changed nothing",
			grant.Role, issue.Phase, round, received))
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

// handoffRecord reads the Dispatch handoff record a completion of issue's file-backed phase, whose
// handoff word is word, names, and holds it to the completion: the record must be the issue's, of
// the phase's word, at the version reported, and written by session, the session the claim
// registered. Every refusal is written before the fact is applied, so nothing is recorded and the
// corrected or retried call is applied:
//
//   - HANDOFF_RECORD_REQUIRED: the completion names no record (handoffId) or no positive version.
//   - HANDOFF_RECORD_NOT_FOUND: Dispatch has no such record (its 404, or another refusal of the id).
//   - DISPATCH_READ_FAILED: Dispatch did not answer the read, or answered it with a refusal that
//     says nothing of the id (a 5xx, a credential answer; dispatch.PermanentRefusal); the worker
//     completes again once Dispatch answers.
//   - HANDOFF_RECORD_MISMATCH: the record is another issue's, another phase's, or another version.
//   - HANDOFF_SESSION_MISMATCH: another session wrote it, or no session did. The import of the
//     handoff files committed before handoffs moved to Dispatch writes under the daemon's own
//     session (dispatch.DaemonSession), so its record is refused this way too, and the refusal says
//     to read it and write it again from the worker's session.
func (s *server) handoffRecord(w http.ResponseWriter, r *http.Request, session string, issue *record.Issue, word string, req HandoffCompleteRequest) bool {
	write := fmt.Sprintf("`dispatch handoff-write --issue %s --phase %s --body-file <file>`", issue.Key, word)
	read := fmt.Sprintf("`dispatch handoff-read --issue %s --phase %s`", issue.Key, word)
	if req.HandoffID == "" || req.HandoffVersion < 1 {
		writeFailure(w, http.StatusBadRequest, "HANDOFF_RECORD_REQUIRED", fmt.Sprintf(
			"phase %s of %s ends with its %s handoff recorded on the Dispatch issue, and this completion names no record (handoffId and a positive handoffVersion): write it with %s, then complete again; this completion changed nothing",
			issue.Phase, issue.Key, word, write))
		return false
	}
	if s.dispatch == nil {
		writeFailure(w, http.StatusInternalServerError, "DISPATCH_UNAVAILABLE", "Dispatch is unavailable")
		return false
	}
	handoff, err := s.dispatch.Handoff(r.Context(), req.HandoffID)
	if err != nil {
		if _, permanent := dispatch.PermanentRefusal(err); permanent || dispatch.Missing(err) {
			writeFailure(w, http.StatusNotFound, "HANDOFF_RECORD_NOT_FOUND", fmt.Sprintf(
				"Dispatch has no handoff record %s (%v): name the record %s shows, or write one with %s, then complete again; this completion changed nothing",
				req.HandoffID, err, read, write))
			return false
		}
		s.log.Warn("api: read the handoff record a completion names", "issue", issue.Key, "handoff", req.HandoffID, "error", err)
		writeFailure(w, http.StatusBadGateway, "DISPATCH_READ_FAILED", fmt.Sprintf(
			"Dispatch did not answer the read of handoff record %s (%v): complete again once Dispatch answers; this completion changed nothing",
			req.HandoffID, err))
		return false
	}
	if handoff.IssueKey != issue.Key || handoff.Phase != word || handoff.Version != req.HandoffVersion {
		writeFailure(w, http.StatusConflict, "HANDOFF_RECORD_MISMATCH", fmt.Sprintf(
			"handoff record %s is version %d of %s's %s handoff, and this completion is of %s's %s handoff at version %d: name the record %s shows; this completion changed nothing",
			req.HandoffID, handoff.Version, handoff.IssueKey, handoff.Phase, issue.Key, word, req.HandoffVersion, read))
		return false
	}
	if handoff.Author.IsSession(session) {
		return true
	}
	if handoff.Author.IsSession(dispatch.DaemonSession(issue.Key)) {
		writeFailure(w, http.StatusForbidden, "HANDOFF_SESSION_MISMATCH", fmt.Sprintf(
			"handoff record %s (%s version %d) was written by the daemon's own session %s, which imported the handoff file committed before handoffs moved to Dispatch, not by this claim's session %s: read it with %s, write it again with %s from this session, then complete naming the new record; this completion changed nothing",
			req.HandoffID, word, handoff.Version, handoff.Author.ID, session, read, write))
		return false
	}
	writeFailure(w, http.StatusForbidden, "HANDOFF_SESSION_MISMATCH", fmt.Sprintf(
		"handoff record %s (%s version %d) was written by %s %s, not by this claim's session %s: write the handoff again with %s from this session, then complete naming the new record; this completion changed nothing",
		req.HandoffID, word, handoff.Version, handoff.Author.Kind, handoff.Author.ID, session, write))
	return false
}

// staleCompletion is the refusal of a completion attributed to a run the issue has left. It names
// the current run's task when the claim holds one, which the worker's turn began before: that task
// is sent as the worker's next prompt, and the same completion in its turn is applied.
func staleCompletion(issue string, current, serving uint64, pending *supervise.Delivery) string {
	stale := fmt.Sprintf("%s is on generation %d and this completion is generation %d's; nothing was recorded", issue, current, serving)
	if pending != nil && pending.Generation == current {
		return fmt.Sprintf("%s: your turn began before the daemon's generation-%d task (%s) reached this session. End this turn without completing; that task arrives as your next prompt, and the same handoff_complete in its turn is applied",
			stale, current, pending.ID)
	}
	return fmt.Sprintf("%s: the daemon has queued no task of generation %d for this claim, and may queue none (the new run's phase may be another claim's). End this turn without completing and tell the architect; if that task arrives, call the same handoff_complete in its turn", stale, current)
}

// implementationPullRequest is the implementer's completion's read of GitHub: in implementing, a
// completion moves its issue to testing only on a pull request recorded open or merged for it
// (workflow's TriggerImplementationReady), and only the opened webhook, or the required-checks
// pass reading for one that webhook lost, records one open. So before the completion is applied,
// the open pull request on the issue's branch (claim.IssueBranch) is read as the implement App,
// minted for the owner of the issue's repository in GitHub's spelling (record.Issue.GitHubRepo,
// which the webhook's facts name it by), and it says whether the completion goes on:
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
//     this read, so GitHub's trouble never blocks it; the required-checks pass records the pull
//     request when it finds one.
//
// A completion the workflow refuses whatever GitHub shows reads nothing: the route calls this only
// for one that is not already applied (eventID, its key, read with intake.Applied), whose tree does
// not linger and whose handoff record is not the one the implementer named for its previous phase
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
		s.log.Warn("api: read the open pull request on a completing implementer's branch; the completion is applied unread, and the required-checks pass records the pull request it finds", attributes...)
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
// task is no phase's run and a woken session may hold an older one. The event is the issue, the
// check (record.ProductionCheck.ID: a later merge of the same issue replaces the check and counts
// its looks from one again, and the rerun's report on look 1 is a new fact, not the first check's),
// the look and the verdict: a retried report of the same look is the same fact, and a refusal — the
// look ended by its bound before the report landed — is recorded as processed under it, so the
// report of the next look is a new fact.
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

// byHandShare is the clause READY_PACKET_TOO_LONG names a by-hand note's share of the packet with,
// empty when READY carries none.
func byHandShare(mergeNote string) string {
	if mergeNote == "" {
		return ""
	}
	return ", with the by-hand note the daemon tells above it,"
}

// readyNote runs READY's checks (readyChecks) against issue's recorded pull request on GitHub, in
// the repository issue's record names, for a merger's ready: true, and is what they leave
// (readyAnswer). Every refusal is written before the fact is applied, so nothing is recorded under
// the completion's key and the corrected retry at the same commit is applied:
//
//   - READY's refusals (readyChecks): NO_PULL_REQUEST, READY_PULL_REQUEST_CLOSED,
//     READY_HEAD_MOVED, READY_NOT_APPROVED and READY_UNREVIEWED_CHANGE
//     (unreviewedChange: the head is not the review round's approved head plus retro's commits),
//     READY_HEAD_CONFLICTS, READY_FILES_TRUNCATED, READY_DISARM_PENDING.
//   - GITHUB_READ_FAILED: GitHub failed to answer a read; its failure, not the head's.
//
// The reads are made as the implement App, minted for the repository's owner, whose token never
// leaves the daemon; a runtime that boots without Apps publishes no READY (leaseForGrant).
func (s *server) readyNote(w http.ResponseWriter, r *http.Request, grant credential.Grant, issue *record.Issue, readyHead string) (answer readyAnswer, ok bool) {
	repository, err := issue.Repository()
	if err != nil {
		s.log.Error("api: read the repository of a completing issue", "issue", issue.Key, "error", err)
		writeFailure(w, http.StatusInternalServerError, "RECORD_READ_FAILED", fmt.Sprintf("the record of %s names no repository its pull request can be read in", issue.Key))
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
	answer, refused := readyChecks(ctx, github, rules, issue.Key, readyHead, approvedHead, pr, disarmPending)
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

// completionPosition is where issue key stands as the completion route reads it, in one repeatable-read
// snapshot: the issue record, its tree's generation (the root's) and whether the tree lingers, its
// review round — the implementer's count of returns to implementing, which tells each implementing,
// testing, and reviewing pass from the last — and the handoff record the implementer last named
// for a file-backed phase (record.PhaseRow.LastHandoff), its pull request record while it is implementing
// (the one phase the route reads it in), nil in any other phase or while none is recorded, and its
// production check, nil while none was ever opened.
type completionPosition struct {
	issue          *record.Issue
	treeGeneration uint64
	lingers        bool
	round          int
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
	round, lastHandoff := 0, ""
	for _, row := range rows {
		if row.Role == claim.RoleImplementer {
			round, lastHandoff = row.Rounds, row.LastHandoff
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
	return completionPosition{issue: issue, treeGeneration: root.Generation, lingers: root.Lingers(), round: round, lastHandoff: lastHandoff,
		pullRequest: pr, check: check}, tx.Commit(ctx)
}
