package api

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// EmptyResponse is every accepted mutating route whose caller needs no result beyond success.
type EmptyResponse struct{}

// GateRegisterRequest records the document version an architect asked a human to approve.
type GateRegisterRequest struct {
	GrantID    string `json:"grantId"`
	Issue      string `json:"issue"`
	ArtifactID string `json:"artifactId"`
	Version    int    `json:"version"`
}

// GateRegisterResponse confirms the gate registration fact committed.
type GateRegisterResponse struct{}

// WaveReleaseRequest asks an architect to move selected children in its tree to todo.
type WaveReleaseRequest struct {
	GrantID string   `json:"grantId"`
	Issues  []string `json:"issues"`
}

// WaveReleaseResponse answers release_children. A release is accepted whatever the children's
// links say, so released is every issue the request named, unchanged: no child of a wave is lost,
// and the architect is never asked to send the request again. started and held then say what the
// release did with each of them: started compares each child's record read before the request's
// writes with its record read after them (startedByRelease), and held is judged from the blockers
// Dispatch showed the request, against the mirror (blockers.UnfinishedOf) — the same list the
// child's own todo event carries — not from the child's record.
//
// started names the children whose first phase this request started, and held each child the
// request did not start because it waits on an unfinished issue, with the issues it waits on. A
// child the tree's closed design gate withholds is in released alone: the gate, not a blocker,
// holds it, its phase starts when the gate opens, and calling it started would be a lie. Both
// lists follow the request's own order.
type WaveReleaseResponse struct {
	Released []string    `json:"released"`
	Started  []string    `json:"started"`
	Held     []HeldChild `json:"held"`
}

// MarshalJSON keeps `released`, `started` and `held` arrays on the wire, empty rather than `null`:
// the plugin parses this answer through a strict schema.
func (wr WaveReleaseResponse) MarshalJSON() ([]byte, error) {
	type wire WaveReleaseResponse
	out := wire(wr)
	if out.Released == nil {
		out.Released = []string{}
	}
	if out.Started == nil {
		out.Started = []string{}
	}
	if out.Held == nil {
		out.Held = []HeldChild{}
	}
	return json.Marshal(out)
}

// HeldChild is one released child the daemon did not start, and the issues it waits on that are
// not done — the keys of its Dispatch blocked-by links, including any the daemon has not yet
// observed as an issue of its own. It is record.BlockedIssue, the row the state's `blocked` and
// a tick controller notice carry: one issue and what it waits on, declared once below all three
// and validated by the one schema the plugin reads them all through. Its own MarshalJSON is what
// keeps `blockedBy` an array here too, empty rather than `null`.
type HeldChild = record.BlockedIssue

// PhaseBackwardRequest asks a phase worker to return its own issue to an earlier phase, or the
// tree's architect to return an issue it owns from accepting. An architect's grant serves its
// whole tree, so it names the issue; a phase worker's names its own, so issue may be left out and,
// when given, must be the grant's.
type PhaseBackwardRequest struct {
	GrantID string      `json:"grantId"`
	To      phase.Phase `json:"to"`
	Reason  string      `json:"reason"`
	Issue   string      `json:"issue,omitempty"`
}

// PhaseBackwardResponse confirms the phase-backward fact committed.
type PhaseBackwardResponse struct{}

// PhaseRetryRequest records the tree architect's retry-or-escalate choice for a held issue.
type PhaseRetryRequest struct {
	GrantID  string                         `json:"grantId"`
	Issue    string                         `json:"issue"`
	Decision intake.RetryOrEscalateDecision `json:"decision"`
}

// PhaseRetryResponse confirms the retry-or-escalate fact committed.
type PhaseRetryResponse struct{}

// SignOffRequest records the owning architect's acceptance of an issue in accepting.
type SignOffRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
}

// SignOffResponse confirms the sign-off fact committed.
type SignOffResponse struct{}

// RootCloseRequest is a root architect's close of its tree before the tree's first phase starts,
// with the reason posted on the issue.
type RootCloseRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
	Reason  string `json:"reason"`
}

// ChildRequest names the child of the architect's tree that park_child takes out of the workflow
// or rerun_child runs again.
type ChildRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
}

func (s *server) architectGrant(w http.ResponseWriter, r *http.Request, id string) (credential.Grant, bool) {
	grant, ok := s.redeem(w, r, id)
	if !ok {
		return credential.Grant{}, false
	}
	if grant.Controller || grant.Role != claim.RoleArchitect {
		writeFailure(w, http.StatusForbidden, "ARCHITECT_REQUIRED", "architect grant required")
		return credential.Grant{}, false
	}
	return grant, true
}

func (s *server) phaseWorkerGrant(w http.ResponseWriter, r *http.Request, id string) (credential.Grant, bool) {
	grant, ok := s.redeem(w, r, id)
	if !ok {
		return credential.Grant{}, false
	}
	if grant.Controller || !isPhaseWorker(grant.Role) {
		writeFailure(w, http.StatusForbidden, "PHASE_WORKER_REQUIRED", "phase worker grant required")
		return credential.Grant{}, false
	}
	return grant, true
}

func isPhaseWorker(role claim.Role) bool {
	switch role {
	case claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger:
		return true
	default:
		return false
	}
}

// recordedIssue reads the issue record, whose tree root is the single source of tree membership
// after Task 3.15. The read authorizes the caller; the fact itself still enters workflow through
// ApplyFact's one write transaction.
func (s *server) recordedIssue(ctx context.Context, issue string) (*record.Issue, error) {
	if s.pool == nil || s.records == nil {
		return nil, errors.New("record dependencies are unavailable")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	recorded, err := s.records.Issue(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	return recorded, tx.Commit(ctx)
}

// architectForIssue authorizes an architect's operation on a recorded issue of its tree, and
// returns the grant and that record.
func (s *server) architectForIssue(w http.ResponseWriter, r *http.Request, grantID, issue string) (credential.Grant, record.Issue, bool) {
	grant, ok := s.architectGrant(w, r, grantID)
	if !ok {
		return credential.Grant{}, record.Issue{}, false
	}
	recorded, err := s.recordedIssue(r.Context(), issue)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read issue tree membership")
		return credential.Grant{}, record.Issue{}, false
	}
	if recorded == nil {
		writeFailure(w, http.StatusNotFound, "ISSUE_NOT_FOUND", "issue is not recorded")
		return credential.Grant{}, record.Issue{}, false
	}
	if recorded.Tree != grant.Tree {
		writeFailure(w, http.StatusForbidden, "ISSUE_OUTSIDE_TREE", "issue is outside the architect tree")
		return credential.Grant{}, record.Issue{}, false
	}
	return grant, *recorded, true
}

// requestFactID names one API request's fact. A grant serves every request its command makes, so
// the grant cannot name the request: each request is its own fact, applied by its one exchange.
func requestFactID(route string) string {
	return "api:" + route + ":" + rand.Text()
}

func (s *server) applyFact(w http.ResponseWriter, r *http.Request, eventID string, fact intake.Fact, response any) {
	if s.pool == nil {
		writeFailure(w, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "fact intake is unavailable")
		return
	}
	result, err := intake.ApplyFact(r.Context(), s.pool, "api", eventID, fact, s.handlers...)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply fact")
		return
	}
	if result.Refusal != nil {
		writeFailure(w, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) gateRegister(w http.ResponseWriter, r *http.Request) {
	var req GateRegisterRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w,
		field{"grantId", req.GrantID},
		field{"issue", req.Issue},
		field{"artifactId", req.ArtifactID},
	) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	if !validUUID(req.ArtifactID) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ARTIFACT_ID", "artifactId must be a UUID")
		return
	}
	if req.Version < 1 {
		writeFailure(w, http.StatusBadRequest, "INVALID_VERSION", "version must be positive")
		return
	}
	artifactID := strings.ToLower(req.ArtifactID)
	grant, recorded, ok := s.architectForIssue(w, r, req.GrantID, req.Issue)
	// The gate is the tree root's: it opens planning for the whole tree, so a child's document
	// cannot stand in for the root's review.
	if !ok || !s.rootArchitectOf(w, r, grant, recorded, gateRoute) {
		return
	}
	// The issue's event sequence is read before the approval, so every review that read does not
	// show has a later sequence: a review the gate applies after this read places after it.
	since, ok := s.issueSequence(w, r, req.Issue)
	if !ok {
		return
	}
	approval, ok := s.documentOfIssue(w, r, artifactID, req.Issue)
	if !ok {
		return
	}
	// The version registered is the caller's; only Dispatch's read says the document has it. A
	// version Dispatch does not show would record a gate no person's review can open.
	if req.Version > approval.LatestVersion {
		writeFailure(w, http.StatusConflict, "VERSION_NOT_IN_DISPATCH", fmt.Sprintf(
			"Dispatch shows %s at version %d, not %d; register the version you requested approval of", artifactID, approval.LatestVersion, req.Version))
		return
	}
	// What this read found of the person's review travels with the registration, so a spec a person
	// approved or asked changes of before the architect registered is applied at once: the gate
	// opens, or closes with the architect told. The read's own start is its sequence alone: no time
	// this read shows is the read's own (the latest review's time is that review's), and every act
	// Dispatch records after the read carries a later sequence. The approval request the read found
	// open, and whose turn it waits on, travel with it too, for the registration to mirror.
	changes, reason := approval.ChangesRequestedLatest()
	fact := intake.GateRegistered{Issue: req.Issue, ArtifactID: artifactID, Version: req.Version, ApprovedVersion: approval.ApprovedLatest(),
		ChangesRequestedVersion: changes, ChangesReason: reason, DispatchVersion: approval.LatestVersion, Since: record.EventMark{Seq: since},
		RequestAsk: approval.AskID, RequestWaitsOnAgent: approval.AskID != "" && approval.WaitingOn == "agent"}
	review := record.EventMark{Seq: approval.Seq, At: approval.At}
	if changes != 0 {
		fact.ChangesRequestedMark = review
	}
	if fact.ApprovedVersion != 0 {
		fact.ApprovedMark = review
	}
	// A re-admitted root's architect registers its spec again, often at the same version: each
	// generation's registration is its own fact. The fact's id names the read as well as the
	// version: a second registration at one version carries what Dispatch showed when it ran, so a
	// retry of one read stays one fact while a new read — a review or a version since — is a new
	// fact the handlers see.
	s.applyFact(w, r, fmt.Sprintf("gate:%s:%d:%s:%d:%d", req.Issue, recorded.Generation, artifactID, req.Version, since), fact, GateRegisterResponse{})
}

// issueSequence reads the issue's latest Dispatch event sequence. A failed read records nothing,
// so the architect's register_gate retries.
func (s *server) issueSequence(w http.ResponseWriter, r *http.Request, issue string) (int64, bool) {
	if s.dispatch == nil {
		writeFailure(w, http.StatusInternalServerError, "DISPATCH_UNAVAILABLE", "Dispatch is unavailable")
		return 0, false
	}
	current, err := s.dispatch.GetIssue(r.Context(), issue)
	if err != nil {
		writeFailure(w, http.StatusBadGateway, "DISPATCH_FAILED", fmt.Sprintf("Dispatch read of %s failed; retry register_gate", issue))
		return 0, false
	}
	return current.LastSeq, true
}

// documentOfIssue refuses a gate document the issue does not carry, as the shipped daemon does: an
// already-approved document of any other issue would otherwise open this gate. A failed read
// records nothing, so the architect's register_gate retries. It returns the approval it read.
func (s *server) documentOfIssue(w http.ResponseWriter, r *http.Request, artifactID, issue string) (dispatch.Approval, bool) {
	if s.dispatch == nil {
		writeFailure(w, http.StatusInternalServerError, "DISPATCH_UNAVAILABLE", "Dispatch is unavailable")
		return dispatch.Approval{}, false
	}
	approval, err := s.dispatch.Approval(r.Context(), artifactID)
	switch {
	case dispatch.Missing(err), err == nil && approval.IssueKey != issue:
		writeFailure(w, http.StatusNotFound, "ARTIFACT_NOT_ON_ISSUE", fmt.Sprintf("%s is not a document of %s", artifactID, issue))
		return dispatch.Approval{}, false
	case err != nil:
		writeFailure(w, http.StatusBadGateway, "DISPATCH_FAILED", fmt.Sprintf("Dispatch read of %s failed; retry register_gate", artifactID))
		return dispatch.Approval{}, false
	}
	return approval, true
}

// releasableChild accepts an issue of the architect's tree that has not entered the workflow: a
// triage, backlog, or icebox issue whose Dispatch parent chain reaches tree. The record cannot
// answer, since it holds a child only once the child is todo; the shipped daemon checks the same
// membership over its mirrored issue graph. An issue already in the workflow is refused, since
// writing todo would move an in-progress child back.
//
// A child already todo is neither: todo is what this call writes, so it needs no write. It is
// accepted, which is what makes the architect's retry of a wave Dispatch failed part way through
// finish it, and waveRelease records it if nothing has (enterTodoChild).
func (s *server) releasableChild(w http.ResponseWriter, r *http.Request, tree, key string, read issueReader) (child dispatch.Issue, write bool, ok bool) {
	child, found := read(w, r, key)
	if !found {
		return dispatch.Issue{}, false, false
	}
	write = true
	switch child.Status {
	case "triage", "backlog", "icebox":
	case "todo":
		write = false
	default:
		writeFailure(w, http.StatusConflict, "ISSUE_ALREADY_RELEASED", fmt.Sprintf("%s is %s; release_children releases only a triage, backlog, or icebox child", key, child.Status))
		return dispatch.Issue{}, false, false
	}
	seen := map[string]bool{key: true}
	for parent := child.Parent; parent != nil && !seen[*parent]; {
		if *parent == tree {
			return child, write, true
		}
		seen[*parent] = true
		ancestor, found := read(w, r, *parent)
		if !found {
			return dispatch.Issue{}, false, false
		}
		parent = ancestor.Parent
	}
	writeFailure(w, http.StatusForbidden, "ISSUE_OUTSIDE_TREE", fmt.Sprintf("%s is outside the architect tree %s", key, tree))
	return dispatch.Issue{}, false, false
}

// maxChildReleaseAttempts and childReleaseBackoff bound release_children's retries of a todo child
// whose parent is not yet recorded (scheduleChildReleaseRetry).
const (
	maxChildReleaseAttempts = 3
	childReleaseBackoff     = time.Second
)

// enterTodoChild records a released child Dispatch already shows todo that the daemon has no
// record of. The workflow records a child from an observation of it in todo while its tree is live
// (workflow's recordChildUnderLiveTree), and a child set todo before its tree was live, by a person
// or another session, has none: the release writes it nothing, and Dispatch would make no event for
// a status the issue already has. So the release applies Dispatch's view of the child, as just read,
// as that observation, at the child's own Dispatch sequence: the child takes the path its todo event
// would, recorded under the live tree and started once its gate is open, and a live event of it
// from before the read stops at the sequence fence. The view applies only while the child stays
// unrecorded (intake.DispatchIssue.Observed): an event that recorded it meanwhile has the record
// carried on by the child's own events, each with its actor. A child already recorded, which the
// earlier attempt of a wave this call retries released, has had that observation.
//
// The workflow records a child only under a recorded parent, and a parent this same wave released
// is recorded only once its own todo event arrives. A child the observation did not record is
// answered CHILD_NOT_RECORDED, retryable: each attempt is its own fact (requestFactID), so the
// retry applies the observation again once the parent is recorded. Its durable retry record bounds
// attempts with exponential backoff; after the limit, it queues one notice to the tree architect
// instead of asking an architect to retry forever.
func (s *server) enterTodoChild(w http.ResponseWriter, r *http.Request, tree string, child dispatch.Issue) bool {
	recorded, err := s.recordedIssue(r.Context(), child.Key)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", fmt.Sprintf("could not read the record of %s", child.Key))
		return false
	}
	if recorded != nil {
		return s.clearChildReleaseRetry(w, r, child.Key)
	}
	observed := intake.ObservedIssue(child)
	if s.prepare != nil {
		prepared, err := s.prepare(r.Context(), observed)
		var later *intake.RetryLater
		switch {
		case errors.As(err, &later):
			writeFailure(w, http.StatusConflict, "CHILD_RELEASE_BACKOFF", fmt.Sprintf("%s: %v; retry after %s", child.Key, err, later.After))
			return false
		case err != nil:
			writeFailure(w, http.StatusBadGateway, "REPOSITORY_READ_FAILED", fmt.Sprintf("could not read the repository of %s: %v; retry release_children", child.Key, err))
			return false
		}
		observed = prepared
	}
	if _, err := intake.ApplyFact(r.Context(), s.pool, "api", requestFactID("waves/release"), observed, s.handlers...); err != nil {
		writeFailure(w, http.StatusInternalServerError, "FACT_APPLY_FAILED", fmt.Sprintf("could not record %s", child.Key))
		return false
	}
	if recorded, err = s.recordedIssue(r.Context(), child.Key); err != nil {
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", fmt.Sprintf("could not read the record of %s", child.Key))
		return false
	}
	if recorded != nil {
		return s.clearChildReleaseRetry(w, r, child.Key)
	}
	return s.scheduleChildReleaseRetry(w, r, tree, child, intake.ObservedIssue(child).Parent)
}

func (s *server) clearChildReleaseRetry(w http.ResponseWriter, r *http.Request, issue string) bool {
	if err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		return s.records.DeleteReleaseRetry(r.Context(), tx, issue)
	}); err != nil {
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", fmt.Sprintf("could not clear the release retry of %s", issue))
		return false
	}
	return true
}

func (s *server) scheduleChildReleaseRetry(w http.ResponseWriter, r *http.Request, tree string, child dispatch.Issue, parent string) bool {
	now := time.Now().UTC()
	var retry record.ReleaseRetry
	var exhausted, backoff bool
	// The retry is its own transaction, so it owns the commit hook the stalled notice's write
	// registers (the daemon's projectRecords, intake.OnCommit), as ApplyFact does for a fact's rows.
	scoped, committed := intake.WithCommitHooks(r.Context())
	if err := pgx.BeginFunc(scoped, s.pool, func(tx pgx.Tx) error {
		current, err := s.records.ReleaseRetry(scoped, tx, child.Key)
		if err != nil {
			return err
		}
		switch {
		case current != nil && current.Sequence == child.LastSeq && current.Exhausted:
			retry, exhausted = *current, true
			return nil
		case current != nil && current.Sequence == child.LastSeq && now.Before(current.NextAt):
			retry, backoff = *current, true
			return nil
		case current != nil && current.Sequence == child.LastSeq:
			retry = *current
			retry.Attempts++
		default:
			retry = record.ReleaseRetry{Issue: child.Key, Sequence: child.LastSeq, Attempts: 1}
		}
		retry.NextAt = now.Add(childReleaseBackoff << (retry.Attempts - 1))
		retry.Exhausted = retry.Attempts >= maxChildReleaseAttempts
		if retry.Exhausted {
			row, err := record.NewOutboxRow(tree, record.Notice{Kind: "child-release-stalled", Role: claim.RoleArchitect,
				Reason: fmt.Sprintf("%s is todo but its parent %s has not reached the daemon after %d releases; inspect the tree before trying again", child.Key, parent, retry.Attempts)}, now)
			if err != nil {
				return err
			}
			if err := s.records.Enqueue(scoped, tx, row); err != nil {
				return err
			}
			exhausted = true
		}
		return s.records.PutReleaseRetry(scoped, tx, retry)
	}); err != nil {
		writeFailure(w, http.StatusInternalServerError, "RETRY_UNAVAILABLE", fmt.Sprintf("could not record the release retry of %s", child.Key))
		return false
	}
	committed()
	if exhausted {
		writeFailure(w, http.StatusConflict, "CHILD_RELEASE_STALLED", fmt.Sprintf("%s is todo but its parent %s has not reached the daemon after %d attempts; the tree architect was notified", child.Key, parent, retry.Attempts))
		return false
	}
	if backoff {
		writeFailure(w, http.StatusConflict, "CHILD_RELEASE_BACKOFF", fmt.Sprintf("%s is waiting for %s's release; retry after %s", child.Key, parent, time.Until(retry.NextAt).Round(time.Millisecond)))
		return false
	}
	writeFailure(w, http.StatusConflict, "CHILD_NOT_RECORDED", fmt.Sprintf("%s is todo but not recorded: the daemon records a child only under a recorded parent (%s) of a live tree; retry after %s", child.Key, parent, childReleaseBackoff<<(retry.Attempts-1)))
	return false
}

// issueReader reads one issue for a request, answering its own failures.
type issueReader func(w http.ResponseWriter, r *http.Request, key string) (dispatch.Issue, bool)

// cachedIssueReader reads each issue from Dispatch once per request. A wave's children share the
// ancestors between them and the tree root, and the membership walk read every one of them again
// for every child.
func (s *server) cachedIssueReader() issueReader {
	seen := map[string]dispatch.Issue{}
	return func(w http.ResponseWriter, r *http.Request, key string) (dispatch.Issue, bool) {
		if issue, read := seen[key]; read {
			return issue, true
		}
		issue, ok := s.dispatchIssue(w, r, key)
		if ok {
			seen[key] = issue
		}
		return issue, ok
	}
}

// dispatchIssue reads one issue from Dispatch, answering a missing issue 404 and a failed read 502.
func (s *server) dispatchIssue(w http.ResponseWriter, r *http.Request, key string) (dispatch.Issue, bool) {
	issue, err := s.dispatch.GetIssue(r.Context(), key)
	switch {
	case dispatch.Missing(err):
		writeFailure(w, http.StatusNotFound, "ISSUE_NOT_FOUND", fmt.Sprintf("%s is not a Dispatch issue", key))
		return dispatch.Issue{}, false
	case err != nil:
		writeFailure(w, http.StatusBadGateway, "DISPATCH_FAILED", fmt.Sprintf("Dispatch read of %s failed; retry release_children", key))
		return dispatch.Issue{}, false
	}
	return issue, true
}

func (s *server) waveRelease(w http.ResponseWriter, r *http.Request) {
	var req WaveReleaseRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w, field{"grantId", req.GrantID}) {
		return
	}
	grant, ok := s.architectGrant(w, r, req.GrantID)
	if !ok {
		return
	}
	if s.dispatch == nil {
		writeFailure(w, http.StatusInternalServerError, "DISPATCH_UNAVAILABLE", "Dispatch is unavailable")
		return
	}
	if req.Issues == nil {
		req.Issues = []string{}
	}
	// One wave's children share ancestors, and the walk to the tree root re-read each of them
	// per child: the reads are cached for the life of this request.
	read := s.cachedIssueReader()
	children := make([]releasedChild, 0, len(req.Issues))
	for _, issue := range req.Issues {
		if !claim.IsIssueKey(issue) {
			writeFailure(w, http.StatusBadRequest, "INVALID_ISSUE", "issues must contain issue keys")
			return
		}
		child, write, ok := s.releasableChild(w, r, grant.Tree, issue, read)
		if !ok {
			return
		}
		children = append(children, releasedChild{view: child, write: write})
	}
	if err := s.readReleasedBefore(r.Context(), children); err != nil {
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the records of the children to release")
		return
	}
	// Each child the release writes todo is recorded by its own write in this request
	// (applyOwnStatusWrite), not when the write's event comes back through the Dispatch consumer,
	// and a child Dispatch already shows todo by Dispatch's view of it (enterTodoChild). The
	// workflow records a child only under a recorded parent, so a parent this wave releases goes
	// first.
	for _, child := range releaseOrder(children) {
		if !child.write {
			if !s.enterTodoChild(w, r, grant.Tree, child.view) {
				return
			}
			continue
		}
		written, ok := s.setDispatchStatus(w, r, child.view.Key, "todo")
		if !ok || !s.applyOwnStatusWrite(w, r, written) {
			return
		}
	}
	started, held, err := s.releaseOutcome(r.Context(), children)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read what the released children wait on")
		return
	}
	writeJSON(w, http.StatusOK, WaveReleaseResponse{Released: req.Issues, Started: started, Held: held})
}

// releasedChild is one child of a release as its answer judges it: Dispatch's view of it as the
// request read it (releasableChild), whether the request writes it todo or finds it todo already,
// and its record as it stood before any of the request's writes (readReleasedBefore).
type releasedChild struct {
	view   dispatch.Issue
	write  bool
	before *record.Issue
}

// readReleasedBefore reads each child's record before the release writes anything, in one
// read-only transaction, so releaseOutcome can tell a first phase this request started from one
// the child was already in.
func (s *server) readReleasedBefore(ctx context.Context, children []releasedChild) error {
	if s.pool == nil || s.records == nil {
		return errors.New("record dependencies are unavailable")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for i := range children {
		if children[i].before, err = s.records.Issue(ctx, tx, children[i].view.Key); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// releaseOrder is the wave in the request's order, except that each child comes after every child
// of the wave on its Dispatch parent chain: the workflow records a child only under a recorded
// parent, so a parent the wave releases must be recorded before its child is applied. A chain is
// followed through the wave's own children only, since a parent outside the wave is recorded or not
// whatever the order.
func releaseOrder(children []releasedChild) []releasedChild {
	byKey := make(map[string]releasedChild, len(children))
	for _, child := range children {
		byKey[child.view.Key] = child
	}
	depth := make(map[string]int, len(children))
	for _, child := range children {
		seen := map[string]bool{child.view.Key: true}
		for parent := child.view.Parent; parent != nil && !seen[*parent]; {
			above, inWave := byKey[*parent]
			if !inWave {
				break
			}
			seen[*parent] = true
			depth[child.view.Key]++
			parent = above.view.Parent
		}
	}
	ordered := slices.Clone(children)
	slices.SortStableFunc(ordered, func(a, b releasedChild) int { return cmp.Compare(depth[a.view.Key], depth[b.view.Key]) })
	return ordered
}

// releaseOutcome answers what this request did with each of its children, once its writes and
// observations are in, in the request's own order. It reads each child's record again and the
// project's whole mirror with one query (record.Store.DispatchIssues), as every blocker judgement
// reads it, in one read-only transaction: a wave's children share the issues they wait on, and the
// mirror is one row per issue of the project.
//
// A child is started when this request started its first phase (startedByRelease): the request
// records every child it releases, one it writes todo — out of backlog, or parked at done — by its
// own write (applyOwnStatusWrite), and one Dispatch already shows todo by Dispatch's view of it
// (enterTodoChild). Otherwise it is held when it waits on an issue that is not done, unless it is
// running a phase it was already in: what it waits on is judged from the blockers Dispatch showed
// the request (blockers.UnfinishedOf), which the workflow's enterChild held a recorded child at
// phase.Admitted on, and which still answers for a written child the workflow could not record yet
// (a parent outside the wave that the daemon does not record), whose write's event, when the
// consumer delivers it, enters it on that same wait.
//
// A child in neither list is one this request started nothing for and that waits on nothing — its
// tree's closed design gate holds it until the gate's opening starts it, or the workflow could not
// record it yet and its write's event is still to start it — or one already running a phase before
// the request, which holds nothing back.
func (s *server) releaseOutcome(ctx context.Context, children []releasedChild) ([]string, []HeldChild, error) {
	if s.pool == nil || s.records == nil {
		return nil, nil, errors.New("record dependencies are unavailable")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The mirror's rows are keyed by the Dispatch project key the issues are filed under, never
	// by the claim project token: a read with the token finds nothing wherever the two differ,
	// which in a deployment is always (Options.Project, Options.DispatchProject).
	rows, err := blockers.Load(ctx, tx, s.records, s.dispatchProject)
	if err != nil {
		return nil, nil, err
	}
	started, held := []string{}, []HeldChild{}
	for _, child := range children {
		after, err := s.records.Issue(ctx, tx, child.view.Key)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case startedByRelease(child.before, after):
			started = append(started, child.view.Key)
		case runsPhase(after):
		default:
			if unfinished := blockers.UnfinishedOf(rows, child.view.BlockedBy); len(unfinished) > 0 {
				held = append(held, HeldChild{Issue: child.view.Key, BlockedBy: unfinished})
			}
		}
	}
	return started, held, tx.Commit(ctx)
}

// startedByRelease says whether the release started a child's first phase: its record after the
// release runs a phase of a run that, before it, had none — the child was unrecorded, still at
// phase.Admitted, or in an earlier run (a parked child at done) that the release's todo re-entered
// as a new generation. A record already running the same run, or a parked one the todo event has
// not reached yet, was started by no part of this request.
func startedByRelease(before, after *record.Issue) bool {
	if !runsPhase(after) {
		return false
	}
	return before == nil || before.Phase == phase.Admitted || before.Generation != after.Generation
}

// runsPhase says whether a recorded child is in a phase of a run: past phase.Admitted, where it
// waits to start, and short of phase.Done, where it stands out of the workflow until a todo
// re-enters it.
func runsPhase(issue *record.Issue) bool {
	return issue != nil && issue.Phase != phase.Admitted && issue.Phase != phase.Done
}

// phaseBackward applies a backward move: a phase worker's on its own issue (the grant's), or the
// tree architect's on an issue it owns (architectOwns, as sign_off), named by the request, from
// accepting. The workflow decides whether the requester runs the phase (BACKWARD_REFUSED).
func (s *server) phaseBackward(w http.ResponseWriter, r *http.Request) {
	var req PhaseBackwardRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w,
		field{"grantId", req.GrantID},
		field{"to", string(req.To)},
		field{"reason", req.Reason},
	) {
		return
	}
	if !validPhase(req.To) {
		writeFailure(w, http.StatusBadRequest, "INVALID_PHASE", "to is not a phase")
		return
	}
	if req.Issue != "" && !claim.IsIssueKey(req.Issue) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return
	}
	if !grant.Controller && grant.Role == claim.RoleArchitect {
		if !requireFailureFields(w, field{"issue", req.Issue}) {
			return
		}
		architect, recorded, ok := s.architectForIssue(w, r, req.GrantID, req.Issue)
		if !ok || !s.architectOwns(w, r, architect, recorded, "request_backward_move") {
			return
		}
		s.applyFact(w, r, requestFactID("phase/backward"),
			intake.BackwardMove{Issue: req.Issue, Requester: claim.RoleArchitect, To: req.To, Reason: req.Reason}, PhaseBackwardResponse{})
		return
	}
	grant, ok = s.phaseWorkerGrant(w, r, req.GrantID)
	if !ok {
		return
	}
	if req.Issue != "" && req.Issue != grant.Issue {
		writeFailure(w, http.StatusBadRequest, "ISSUE_NOT_GRANTS", fmt.Sprintf("a phase worker moves only its own issue %s, not %s", grant.Issue, req.Issue))
		return
	}
	s.applyFact(w, r, requestFactID("phase/backward"),
		intake.BackwardMove{Issue: grant.Issue, Requester: grant.Role, To: req.To, Reason: req.Reason}, PhaseBackwardResponse{})
}

func (s *server) phaseRetry(w http.ResponseWriter, r *http.Request) {
	var req PhaseRetryRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w,
		field{"grantId", req.GrantID},
		field{"issue", req.Issue},
		field{"decision", string(req.Decision)},
	) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	if req.Decision != intake.RetryDecision && req.Decision != intake.EscalateDecision {
		writeFailure(w, http.StatusBadRequest, "INVALID_DECISION", "decision must be retry or escalate")
		return
	}
	grant, recorded, ok := s.architectForIssue(w, r, req.GrantID, req.Issue)
	if !ok || !s.architectOwns(w, r, grant, recorded, "retry_or_escalate") {
		return
	}
	s.applyFact(w, r, requestFactID("phase/retry"),
		intake.RetryOrEscalate{Issue: req.Issue, Decision: req.Decision}, PhaseRetryResponse{})
}

func (s *server) signOff(w http.ResponseWriter, r *http.Request) {
	var req SignOffRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w, field{"grantId", req.GrantID}, field{"issue", req.Issue}) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	grant, recorded, ok := s.architectForIssue(w, r, req.GrantID, req.Issue)
	if !ok || !s.architectOwns(w, r, grant, recorded, "sign_off") {
		return
	}
	s.applyFact(w, r, requestFactID("signoff"), intake.SignOff{Issue: req.Issue}, SignOffResponse{})
}

// closeRoot ends the architect's tree while its root is admitted and no phase has started
// (workflow's closeRoot): the root's own architect only, with a reason short enough to post as a
// message.
func (s *server) closeRoot(w http.ResponseWriter, r *http.Request) {
	var req RootCloseRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w, field{"grantId", req.GrantID}, field{"issue", req.Issue}, field{"reason", req.Reason}) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	fact := intake.CloseRoot{Issue: req.Issue, Reason: req.Reason}
	if length := dispatch.MessageBodyLength(fact.Message()); length > record.MessagePostLimit {
		writeFailure(w, http.StatusBadRequest, "CLOSE_REASON_TOO_LONG",
			fmt.Sprintf("the reason makes a %d-character message, over the %d one message holds; shorten it", length, record.MessagePostLimit))
		return
	}
	grant, recorded, ok := s.architectForIssue(w, r, req.GrantID, req.Issue)
	if !ok || !s.rootArchitectOf(w, r, grant, recorded, closeRootRoute) {
		return
	}
	s.applyFact(w, r, requestFactID("roots-close"), fact, EmptyResponse{})
}

// rootRoute is a route an architect takes only on its tree's root: its refusal code, and its two
// refusals, each a whole message whose one verb is the tree root: forChild when the grant names a
// child of the tree, notOwner when its architect does not own the root.
type rootRoute struct{ code, forChild, notOwner string }

var (
	gateRoute = rootRoute{"GATE_ROOT_ONLY",
		"the design gate belongs to the tree root %s; register it there",
		"the design gate belongs to the tree root %s; its root architect registers it"}
	closeRootRoute = rootRoute{"ROOT_REQUIRED",
		"close_root ends the tree root %s; a child leaves with park_child",
		"close_root ends the tree root %s; its root architect closes it"}
)

// rootArchitectOf says whether grant acts on recorded, an issue of its tree, as a route that only
// the tree root's own architect takes: recorded is the root, and the grant's architect owns it
// (architectOwns), which for the root only its own architect does. It writes the route's 403
// otherwise.
func (s *server) rootArchitectOf(w http.ResponseWriter, r *http.Request, grant credential.Grant, recorded record.Issue, route rootRoute) bool {
	if !claim.IsTreeRoot(recorded.Key, grant.Tree) {
		writeFailure(w, http.StatusForbidden, route.code, fmt.Sprintf(route.forChild, grant.Tree))
		return false
	}
	owns, err := s.owns(r.Context(), grant, recorded)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read issue tree membership")
		return false
	}
	if !owns {
		writeFailure(w, http.StatusForbidden, route.code, fmt.Sprintf(route.notOwner, grant.Tree))
	}
	return owns
}

// architectOwns says whether grant's architect owns recorded, an issue of its tree, for the
// operation op, writing a 403 ISSUE_NOT_OWNED when it does not (owns).
func (s *server) architectOwns(w http.ResponseWriter, r *http.Request, grant credential.Grant, recorded record.Issue, op string) bool {
	owns, err := s.owns(r.Context(), grant, recorded)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read issue tree membership")
		return false
	}
	if !owns {
		writeFailure(w, http.StatusForbidden, "ISSUE_NOT_OWNED", fmt.Sprintf("%s of %s belongs to its own architect or one above it in the tree, not the architect of %s", op, recorded.Key, grant.Issue))
	}
	return owns
}

// owns says whether grant's architect owns recorded, an issue of its tree: recorded is the grant's
// own issue or lies under it, through its recorded parents. So the tree root's architect owns every
// issue of its tree, and a sub-architect its own issue and those under it, never the root or a
// sibling: a sub-architect holds a grant for the whole tree and can name any issue of it. A chain
// that leaves the tree before reaching the grant's issue (no parent on a child, a parent not
// recorded or recorded in another tree, a cycle) ends at the tree root, whose architect owns the
// issue: a human's re-parent in Dispatch leaves an issue's tree as it was (admit's
// recordObservation), and the notice router ends the same walk at the same place
// (owningArchitect, internal/daemon/notice_routing.go), so the architect the reviewer's
// phase-finished notice reaches is the one that may accept the issue. The walk reads every parent
// in one read-only transaction, opened only when it has a parent to read.
func (s *server) owns(ctx context.Context, grant credential.Grant, recorded record.Issue) (bool, error) {
	var tx pgx.Tx
	defer func() {
		if tx != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	byRoot := claim.IsTreeRoot(grant.Issue, grant.Tree)
	seen := map[string]bool{}
	for current := &recorded; ; {
		if current.Key == grant.Issue {
			return true, nil
		}
		if current.Parent == nil || claim.IsTreeRoot(current.Key, current.Tree) || seen[current.Key] {
			return byRoot, nil
		}
		seen[current.Key] = true
		if tx == nil {
			if s.pool == nil || s.records == nil {
				return false, errors.New("record dependencies are unavailable")
			}
			var err error
			if tx, err = s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}); err != nil {
				return false, err
			}
		}
		parent, err := s.records.Issue(ctx, tx, *current.Parent)
		if err != nil {
			return false, err
		}
		if parent == nil || parent.Tree != grant.Tree {
			return byRoot, nil
		}
		current = parent
	}
}

// parkChild takes a running child of the architect's tree out of the workflow by moving it to
// backlog in Dispatch, the move a human would make, and applies that move to the record before it
// answers (applyOwnStatusWrite), as the move's event would: the child is parked as any child leaving
// the workflow is (the workflow's leave), its workers suspended and its phase parked, and the
// architect is told. The record does not wait for the event to come back through the Dispatch
// consumer, which can run far behind: until then the workflow would keep running the child, start
// its next phase's worker on a completion, and refuse rerun_child. The event, once delivered, finds
// the record at its sequence and changes nothing. rerun_child, or release_children on the backlog
// child, runs it again.
func (s *server) parkChild(w http.ResponseWriter, r *http.Request) {
	child, ok := s.childOfTree(w, r)
	if !ok {
		return
	}
	if child.Phase == phase.Done {
		writeFailure(w, http.StatusConflict, "CHILD_NOT_RUNNING", fmt.Sprintf("%s is already out of the workflow; rerun_child runs it again", child.Key))
		return
	}
	if written, ok := s.setDispatchStatus(w, r, child.Key, "backlog"); ok && s.applyOwnStatusWrite(w, r, written) {
		writeJSON(w, http.StatusOK, EmptyResponse{})
	}
}

// rerunChild runs a child of the architect's tree again once it is out of the workflow, parked or
// signed off, by moving it to todo in Dispatch, the move a human would make, and applies that move to
// the record before it answers (applyOwnStatusWrite), as the move's event would: the child's next
// run starts under the tree from planning (the workflow's reenterChild), and the event, once
// delivered, changes nothing. A running child is parked first, and a lingering tree runs nothing
// more; a todo it reached anyway would admit the child as an orphan root of its own.
func (s *server) rerunChild(w http.ResponseWriter, r *http.Request) {
	child, ok := s.childOfTree(w, r)
	if !ok {
		return
	}
	if child.Phase != phase.Done {
		writeFailure(w, http.StatusConflict, "CHILD_RUNNING", fmt.Sprintf("%s is in phase %s; park_child takes it out of the workflow before rerun_child runs it again", child.Key, child.Phase))
		return
	}
	root, err := s.recordedIssue(r.Context(), child.Tree)
	if err != nil || root == nil {
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the tree root")
		return
	}
	if root.Lingers() {
		writeFailure(w, http.StatusConflict, "TREE_LINGERING", fmt.Sprintf("the tree %s is lingering after it left the workflow, so %s cannot run in it again", root.Key, child.Key))
		return
	}
	if written, ok := s.setDispatchStatus(w, r, child.Key, "todo"); ok && s.applyOwnStatusWrite(w, r, written) {
		writeJSON(w, http.StatusOK, EmptyResponse{})
	}
}

// childOfTree reads a ChildRequest and authorizes it: an architect's grant, and an issue of its tree
// other than the tree root, which leaves the workflow only with the whole tree.
func (s *server) childOfTree(w http.ResponseWriter, r *http.Request) (record.Issue, bool) {
	var req ChildRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w, field{"grantId", req.GrantID}, field{"issue", req.Issue}) {
		return record.Issue{}, false
	}
	if !claim.IsIssueKey(req.Issue) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return record.Issue{}, false
	}
	_, child, ok := s.architectForIssue(w, r, req.GrantID, req.Issue)
	if !ok {
		return record.Issue{}, false
	}
	if claim.IsTreeRoot(child.Key, child.Tree) {
		writeFailure(w, http.StatusForbidden, "CHILD_REQUIRED", fmt.Sprintf("%s is the tree root; park_child and rerun_child take a child of it", child.Key))
		return record.Issue{}, false
	}
	return child, true
}

func validPhase(value phase.Phase) bool {
	return slices.Contains(phase.All, value)
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') ||
			(character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}
