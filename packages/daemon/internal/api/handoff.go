package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/handoff"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// HandoffCompleteRequest is what a worker reports when its phase is done. The commit it reports is
// not on it: the daemon reads the issue branch's head on GitHub itself (handoffHead), so a
// completion records what was pushed.
type HandoffCompleteRequest struct {
	GrantID string `json:"grantId"`
	Summary string `json:"summary"`
	Verdict string `json:"verdict"`
	Ready   bool   `json:"ready"`
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
	if !readBody(w, r, &req) || !requireFailureFields(w, field{"grantId", req.GrantID}, field{"summary", req.Summary}) {
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
	// The daemon posts the READY packet as one Dispatch message, the outbox's marker appended, and
	// Dispatch refuses a longer body on every attempt, so a packet over record.MessagePostLimit
	// would never reach the human. It is refused here, before the fact is applied: a refusal the
	// workflow committed is recorded as processed under this completion's key, which leaves the
	// summary out, so the shortened packet at the same commit would be answered
	// HANDOFF_ALREADY_RECORDED and the issue would stay in merging.
	if req.Ready {
		if length := dispatch.MessageBodyLength(req.Summary); length > record.MessagePostLimit {
			writeFailure(w, http.StatusBadRequest, "READY_PACKET_TOO_LONG", fmt.Sprintf(
				"the READY packet is %d characters over the %d the daemon can post as one Dispatch message (%d/%d): link the pull request body's ## Verification section instead of quoting it, then call handoff_complete again; this completion changed nothing",
				length-record.MessagePostLimit, record.MessagePostLimit, length, record.MessagePostLimit))
			return
		}
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
	// The head the completion reports is read from GitHub, and everything the completion is held
	// to is checked there, before the fact is applied: a refusal the workflow committed would be
	// recorded as processed under this completion's key (the READY packet's hazard above), while a
	// refusal here records nothing, and the corrected retry at the same head is applied.
	head, note, ok := s.handoffHead(w, r, grant, issue, req.Ready)
	if !ok {
		return
	}
	// One phase's completion is identified by where the issue stands — its generation, phase, and
	// review round — with the role and what it reported: the head, the verdict, and READY. A
	// retried call for the same phase is the same fact; the next phase's completion at the same
	// head (the implementer's retro, then its production check) is a different one, and so is a
	// corrected report at the same head (a merger's ready: true after READY_REQUIRED), since a
	// refusal is recorded as processed. The position is read before the fact's own transaction:
	// should the issue move in between, the engine re-reads it there and refuses a completion whose
	// role no longer owns the phase.
	//
	// The run the completion is attributed to is part of the key, because a refusal is recorded
	// under it: a stale completion of the run that is over would otherwise take the key of the new
	// run's completion of the same phase at the same head, and the real one would be answered
	// ALREADY_RECORDED and never reach the workflow. The tree's generation is part of it for the
	// same reason: re-admission starts a new generation of the tree by bumping only the root's, and
	// restarts a child's worker in the phase it stood in, so a child's completion refused before
	// (while the tree lingered, or while the old gate was closed) would otherwise take the new
	// generation's key.
	eventID := fmt.Sprintf("handoff:%s:%d:%d:%d:%s:%s:%d:%s:%s:%t", grant.Issue, treeGeneration, issue.Generation, serving, grant.Role, issue.Phase, round, head, req.Verdict, req.Ready)
	result, err := intake.ApplyFact(r.Context(), s.pool, "api", eventID, intake.HandoffComplete{Generation: serving, Issue: grant.Issue, Role: grant.Role, Claim: grant.Claim, Summary: req.Summary, Verdict: req.Verdict, Ready: req.Ready, Commit: head}, s.handlers...)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply handoff fact")
		return
	}
	if result.Duplicate {
		writeFailure(w, http.StatusConflict, "HANDOFF_ALREADY_RECORDED", fmt.Sprintf(
			"the %s completion of phase %s (round %d) at head %s was already received; this call changed nothing",
			grant.Role, issue.Phase, round, head))
		return
	}
	if result.Refusal != nil {
		writeFailure(w, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
		return
	}
	writeJSON(w, http.StatusOK, HandoffCompleteResponse{Note: note})
}

// commitAnswer is what GitHub answers of a commit, under a branch's `commit` and as GET
// /commits/<sha>: its sha and the email its author committed as.
type commitAnswer struct {
	SHA    string `json:"sha"`
	Commit struct {
		Author struct {
			Email string `json:"email"`
		} `json:"author"`
	} `json:"commit"`
}

// handoffHead is the commit a completion of issue's phase reports: the head of the issue's branch
// on GitHub, read by the daemon rather than taken from the worker, so what the completion records
// is what was pushed, and the file the phase ends with is read there. Every refusal is written
// before the fact is applied, so nothing is recorded under the completion's key and the corrected
// retry at the same head is applied:
//
//   - NO_REPOSITORY: the issue's project has no repository configured (a guard; configuration
//     requires one).
//   - HANDOFF_BRANCH_MISSING: legion/<issue> is not on GitHub. The daemon creates it before any
//     role starts (the outbox's issue_branch row), so this names a deleted branch, or a chain never
//     pushed - except after the squash merge deleted it, when a phase that writes no file (retro,
//     the production check, READY) reports the recorded pull request's head, read as a commit for
//     its author.
//   - HANDOFF_AUTHOR_MISMATCH: the head was not authored by the completing role's App
//     (appauth.AppRoleFor: the implementer and merger as the implement App, every other role as
//     the review App). Every role of an issue shares the branch, so a handoff written into the
//     previous role's commit would otherwise be reported as this role's.
//   - HANDOFF_FILE_MISSING, HANDOFF_INVALID: for a phase that ends with a handoff file
//     (phase.HandoffFile), .legion/<issue>/<word>.json is not at the head, or its shape is refused,
//     each `<field>: <reason>` as handoff.Problems names them.
//   - READY's refusals (readyChecks), for ready: true.
//   - GITHUB_READ_FAILED: GitHub failed to answer a read; its failure, not the head's.
//
// The reads are made as the implement App, whose token never leaves the daemon; a runtime that
// boots without Apps completes no phase (leaseForGrant).
func (s *server) handoffHead(w http.ResponseWriter, r *http.Request, grant credential.Grant, issue *record.Issue, ready bool) (head, note string, ok bool) {
	var repository ghrepo.Repository
	if s.repository != nil {
		repository, ok = s.repository(issue.Project)
	}
	if !ok || repository.IsZero() {
		writeFailure(w, http.StatusConflict, "NO_REPOSITORY", fmt.Sprintf("the project %s of %s has no repository configured, so its branch cannot be read", issue.Project, issue.Key))
		return "", "", false
	}
	lease, ok := s.leaseForGrant(w, r, grant, appauth.Implement)
	if !ok {
		return "", "", false
	}
	author := lease
	if role := appauth.AppRoleFor(grant.Role); role != appauth.Implement {
		if author, ok = s.leaseForGrant(w, r, grant, role); !ok {
			return "", "", false
		}
	}
	ctx := r.Context()
	github := githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(s.githubAPI, repository)}
	word, fileBacked := phase.HandoffFile(issue.Phase)
	var pr *record.PullRequest
	if ready || !fileBacked {
		var err error
		if pr, err = s.issuePullRequest(ctx, issue.Key); err != nil {
			s.log.Error("api: read the pull request of a completing issue", "issue", issue.Key, "error", err)
			writeFailure(w, http.StatusInternalServerError, "RECORD_READ_FAILED", "could not read the issue's pull request")
			return "", "", false
		}
	}
	bookmark := workspace.Bookmark(issue.Key)
	var branch struct {
		Commit commitAnswer `json:"commit"`
	}
	// A branch name's slashes stay path segments, as GitHub's branch routes take them.
	err := github.Get(ctx, "/branches/"+strings.ReplaceAll(url.PathEscape(bookmark), "%2F", "/"), &branch)
	switch {
	case isNotFound(err) && !fileBacked && pr != nil && pr.HeadSHA != "":
		if err := github.Get(ctx, "/commits/"+url.PathEscape(pr.HeadSHA), &branch.Commit); err != nil {
			s.log.Error("api: read the pull request's head after the branch was deleted", "issue", issue.Key, "head", pr.HeadSHA, "error", err)
			githubReadFailed(err).write(w)
			return "", "", false
		}
	case isNotFound(err):
		writeFailure(w, http.StatusConflict, "HANDOFF_BRANCH_MISSING", fmt.Sprintf("%s is not on GitHub: push your chain, then complete again", bookmark))
		return "", "", false
	case err != nil:
		s.log.Error("api: read the issue branch's head", "issue", issue.Key, "branch", bookmark, "error", err)
		githubReadFailed(err).write(w)
		return "", "", false
	}
	head = branch.Commit.SHA
	if email := branch.Commit.Commit.Author.Email; email != author.Identity.Email {
		writeFailure(w, http.StatusConflict, "HANDOFF_AUTHOR_MISMATCH", fmt.Sprintf(
			"head %s of %s was authored by %s, not by the %s's App %s (%s): run jj new, then write, commit and push this phase's handoff again",
			shortSHA(head), bookmark, email, grant.Role, author.Identity.Name, author.Identity.Email))
		return "", "", false
	}
	if fileBacked {
		if refused := s.checkHandoffFile(ctx, github, issue.Key, word, bookmark, head); refused != nil {
			refused.write(w)
			return "", "", false
		}
	}
	if ready {
		var refused *refusal
		if note, refused = readyChecks(ctx, github, repository, issue.Key, pr); refused != nil {
			refused.write(w)
			return "", "", false
		}
	}
	return head, note, true
}

// checkHandoffFile reads issue's handoff of phase word at head, .legion/<issue>/<word>.json, and
// holds it to its phase's shape (handoff.Problems). GitHub answers a file's content base64-encoded
// and wrapped; a body that is not a JSON object is refused as the file's shape.
func (s *server) checkHandoffFile(ctx context.Context, github githubrest.Client, issue, word, bookmark, head string) *refusal {
	path := handoff.Path(issue, word)
	var file struct {
		Content string `json:"content"`
	}
	err := github.Get(ctx, "/contents/"+path+"?ref="+url.QueryEscape(head), &file)
	switch {
	case isNotFound(err):
		return &refusal{http.StatusConflict, "HANDOFF_FILE_MISSING", fmt.Sprintf("no %s at head %s of %s: write, commit and push this phase's handoff, then complete again", path, shortSHA(head), bookmark)}
	case err != nil:
		s.log.Error("api: read a handoff file at the issue branch's head", "issue", issue, "path", path, "head", head, "error", err)
		return githubReadFailed(err)
	}
	invalid := func(problems string) *refusal {
		return &refusal{http.StatusUnprocessableEntity, "HANDOFF_INVALID", fmt.Sprintf("invalid %s handoff at %s on head %s of %s: %s; write, commit and push it again, then complete again", word, path, shortSHA(head), bookmark, problems)}
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return invalid("GitHub's content is not base64: " + err.Error())
	}
	var object map[string]any
	if err := json.Unmarshal(decoded, &object); err != nil || object == nil {
		return invalid("the file is not a JSON object")
	}
	if problems := handoff.Problems(word, issue, object); len(problems) > 0 {
		return invalid(strings.Join(problems, "; "))
	}
	return nil
}

// isNotFound is whether err is GitHub answering 404: a branch, a commit or a path it does not have.
func isNotFound(err error) bool {
	var answer *githubrest.Answer
	return errors.As(err, &answer) && answer.Status == http.StatusNotFound
}

// issuePullRequest is the pull request recorded for issue, nil when it has none.
func (s *server) issuePullRequest(ctx context.Context, issue string) (*record.PullRequest, error) {
	if s.pool == nil || s.records == nil {
		return nil, errors.New("record dependencies are unavailable")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return s.records.PullRequest(ctx, tx, issue)
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
