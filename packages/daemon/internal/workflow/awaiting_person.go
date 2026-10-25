package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Unresumable is the one rule for whether a root that returns its admission slot to wait on a
// person may run again in its own architect session: it returns why it may not, or "" when it may.
// Its hand-over label gone, or its leaving the workflow, ends that, and so does a Dispatch claim
// held by a person or by a session outside its tree, which owns the issue instead; the daemon's own
// session and a session that claims within the tree are legitimate holders. awaitPerson refuses to
// release a root it answers no for, and admission removes an awaiting root, and skips an answered
// one, that it answers no for.
func Unresumable(ctx context.Context, store record.Store, tx pgx.Tx, issue record.Issue) (string, error) {
	switch {
	case !issue.HandedOver:
		return "the legion label was removed", nil
	case issue.Lingers() || !InWorkflow(issue):
		return "it left the workflow", nil
	case issue.ClaimedBy == "":
		return "", nil
	}
	kind, id, found := strings.Cut(issue.ClaimedBy, ":")
	if !found || kind == "" || id == "" {
		return fmt.Sprintf("its Dispatch claim is held by %q", issue.ClaimedBy), nil
	}
	if kind == "session" && id == dispatch.DaemonSession(issue.Key) {
		return "", nil
	}
	if kind == "session" {
		ownsTree, err := store.SessionClaimsTree(ctx, tx, issue.Tree, id)
		if err != nil || ownsTree {
			return "", err
		}
	}
	return fmt.Sprintf("its Dispatch claim is held by %s", issue.ClaimedBy), nil
}

// dispatchAskOpened releases a quiet admitted root only for a question its own tree session
// opened. Approval asks are represented by the design gate instead.
func (e *Engine) dispatchAskOpened(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskOpened) (intake.Result, error) {
	if fact.Kind != "question" || fact.AuthorSession == "" {
		return intake.Result{}, nil
	}
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	ownsTree, err := e.store.SessionClaimsTree(ctx, tx, issue.Tree, fact.AuthorSession)
	if err != nil || !ownsTree {
		return intake.Result{}, err
	}
	return intake.Result{}, e.awaitPerson(ctx, tx, *issue, record.Awaiting{Ask: fact.Ask}, fmt.Sprintf("question %s is awaiting an answer", fact.Ask))
}

// dispatchAskClosed answers a root waiting on a person when a person answered or resolved an ask
// on it (answeredByPerson). A session's or Dispatch's own resolution, a settlement retraction among
// them, answers nothing: only a person's act does. A review (a person's answer to an approval
// request) answers a wait on the design gate through its own artifact.approved or
// artifact.changes_requested event (applyArtifact), which Dispatch appends after the answer: woken
// by the answer, the root could resume before the review reached its gate. A person's answer or
// resolution of a question on a tree Legion is not running goes to the controller
// (relayParkedDecision).
func (e *Engine) dispatchAskClosed(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskClosed) (intake.Result, error) {
	if !fact.Actor.Person() {
		return intake.Result{}, nil
	}
	review := fact.Kind == "approval" && fact.State == "answered"
	if err := e.answeredByPerson(ctx, tx, fact.Key, fact.Ask, !review); err != nil {
		return intake.Result{}, err
	}
	// An approval ask's answer arrives again as the document's review, which the gate relays.
	if fact.Kind != "question" {
		return intake.Result{}, nil
	}
	decision := record.DecisionAskResolved
	if fact.State == "answered" {
		decision = record.DecisionAskAnswered
	}
	return intake.Result{}, e.relayParkedDecision(ctx, tx, fact.Key, fact.Ask, fact.Actor, decision, itemRef(fact.Key, "ask", fact.Ask))
}

// dispatchAskReply applies a reply on an ask of the issue, or Dispatch's hand-back of an approval
// request. A person's reply answers a root waiting on a person (answeredByPerson) whatever the
// ask's state: on an ask a session or settlement closed, a reply is the only answer a person can
// give. A person's reply always hands an open ask to the agent (Dispatch's askReplyTurn), so one
// leaving it with the human is a person's own hand-back of an approval request, which answers
// nothing. A session of the root's own tree handing an ask to the person starts the root's wait
// (handedBack). A session's reply answers nothing, whatever turn it leaves. On a tree Legion is not
// running, a person's reply that leaves the turn with the agent, or one under a closed ask (which
// answers a wait as well), goes to the controller, and a session's agent-turn reply is logged
// (relayParkedDecision).
func (e *Engine) dispatchAskReply(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskReply) (intake.Result, error) {
	switch {
	case fact.Actor.Person() && fact.WaitingOn != "human":
		if err := e.answeredByPerson(ctx, tx, fact.Key, fact.Ask, fact.WaitingOn == "agent"); err != nil {
			return intake.Result{}, err
		}
	case fact.Actor.Kind == "session" && fact.WaitingOn == "human":
		return intake.Result{}, e.handedBack(ctx, tx, fact)
	}
	if fact.WaitingOn == "human" || fact.WaitingOn == "" && !fact.Actor.Person() {
		return intake.Result{}, nil
	}
	ref := itemRef(fact.Key, "ask", fact.Ask)
	if fact.Comment != "" {
		ref = itemRef(fact.Key, "comment", fact.Comment)
	}
	return intake.Result{}, e.relayParkedDecision(ctx, tx, fact.Key, fact.Ask, fact.Actor, record.DecisionAskReply, ref)
}

// answeredByPerson records a person's act on ask, an ask of the root key, as the answer of the
// root's wait when it waits on that ask, or, when answersGate, on its design gate: the architect is
// the tree's only agent while it waits, and the approval request's id is not recorded, so a
// person's act on any of the root's asks counts.
func (e *Engine) answeredByPerson(ctx context.Context, tx pgx.Tx, key, ask string, answersGate bool) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || !issue.AwaitingPerson() || issue.Awaiting.AnsweredAt != nil {
		return err
	}
	switch {
	case issue.Awaiting.Ask != "" && issue.Awaiting.Ask == ask:
		e.logOnCommit(ctx, "workflow: a person answered an architect ask", "issue", issue.Key, "ask", ask)
	case issue.Awaiting.GateVersion != 0 && answersGate:
		e.logOnCommit(ctx, "workflow: a person replied on a root waiting on its design gate", "issue", issue.Key, "ask", ask, "version", issue.Awaiting.GateVersion)
	default:
		return nil
	}
	at := e.now()
	issue.Awaiting.AnsweredAt = &at
	return e.store.PutIssue(ctx, tx, *issue)
}

// handedBack starts a root's wait when a session of its own tree hands an ask on the admitted root
// back to the person: a reply leaving the turn with them, or Dispatch's ask.handed_back of an
// approval request. With its design gate registered and closed, the root waits on that gate at its
// latest version, which the person's approval, changes request or reply answers; otherwise it waits
// on the ask. awaitPerson's guards hold: a tree member mid-phase, or a root that could not resume,
// keeps the slot.
func (e *Engine) handedBack(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskReply) error {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil || issue.Phase != phase.Admitted {
		return err
	}
	ownsTree, err := e.store.SessionClaimsTree(ctx, tx, issue.Tree, fact.Actor.ID)
	if err != nil || !ownsTree {
		return err
	}
	gate, err := e.store.Gate(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	if gate != nil && !classify.DesignGateOpen(*gate) {
		return e.awaitPerson(ctx, tx, *issue, record.Awaiting{GateVersion: gate.LatestVersion},
			fmt.Sprintf("spec version %d is awaiting a person's review", gate.LatestVersion))
	}
	return e.awaitPerson(ctx, tx, *issue, record.Awaiting{Ask: fact.Ask}, fmt.Sprintf("ask %s was handed back to a person", fact.Ask))
}

// dispatchClaim keeps the newest Dispatch claim separate from LastDispatchSeq: claim events do
// not carry the full issue observation admission records, but their holder fences an awaited root's
// resume. An issue not yet recorded keeps no claim from here: its hand-over event, the one that
// records it, carries the claim Dispatch shows then (intake.DispatchIssue.ClaimedBy).
func (e *Engine) dispatchClaim(ctx context.Context, tx pgx.Tx, fact intake.DispatchClaim) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil || !issue.ObserveClaim(fact.ClaimedBy, fact.Seq) {
		return intake.Result{}, err
	}
	return intake.Result{}, e.store.PutIssue(ctx, tx, *issue)
}
