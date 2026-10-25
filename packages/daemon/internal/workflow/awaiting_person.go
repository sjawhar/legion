package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
// opened. Approval asks are represented by the design gate instead. The wait begins at the
// question's own event, and a question at or below the root's wait fence starts none (waitFenced).
// Neither does one Dispatch has already shown a person closing (askClosedByPerson): Dispatch's
// intake retries a delivery, so the answer can reach the daemon first, and a wait started after it
// would wait on an ask the person has already answered.
func (e *Engine) dispatchAskOpened(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskOpened) (intake.Result, error) {
	if fact.Kind != "question" || fact.AuthorSession == "" {
		return intake.Result{}, nil
	}
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	ownsTree, err := e.store.SessionClaimsTree(ctx, tx, issue.Tree, fact.AuthorSession)
	if err != nil || !ownsTree || e.waitFenced(ctx, *issue, fact.Mark, "question opened") {
		return intake.Result{}, err
	}
	closed, err := e.store.AskClosedByPerson(ctx, tx, fact.Key, fact.Ask)
	if err != nil {
		return intake.Result{}, err
	}
	if closed {
		e.logOnCommit(ctx, "workflow: a question a person has already answered starts no wait", "issue", issue.Key, "ask", fact.Ask, "seq", fact.Mark.Seq)
		return intake.Result{}, nil
	}
	return intake.Result{}, e.awaitPerson(ctx, tx, *issue, record.Awaiting{Ask: fact.Ask, Since: fact.Mark}, fmt.Sprintf("question %s is awaiting an answer", fact.Ask))
}

// dispatchAskClosed answers a root waiting on a person when a person answered or resolved an ask
// on it (answeredByPerson), and records that close on a root, so an ask.opened the daemon applies
// after it starts no wait. A session's or Dispatch's own resolution, a settlement retraction among
// them, answers nothing and is recorded nowhere: only a person's act counts. A review (a person's
// answer to an approval request) answers a wait on the design gate through its own
// artifact.approved or artifact.changes_requested event (applyArtifact), which Dispatch appends
// after the answer: woken by the answer, the root could resume before the review reached its gate.
func (e *Engine) dispatchAskClosed(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskClosed) (intake.Result, error) {
	if !fact.Actor.Person() {
		return intake.Result{}, nil
	}
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		if err := e.store.RecordClosedAsk(ctx, tx, record.ClosedAsk{Issue: fact.Key, Ask: fact.Ask, Mark: fact.Mark}); err != nil {
			return intake.Result{}, err
		}
	}
	review := fact.Kind == "approval" && fact.State == "answered"
	return intake.Result{}, e.answeredByPerson(ctx, tx, fact.Key, fact.Ask, !review, fact.Mark)
}

// dispatchAskReply applies a reply on an ask of the issue, or Dispatch's hand-back of an approval
// request. A person's reply answers a root waiting on a person (answeredByPerson) whatever the
// ask's state: on an ask a session or settlement closed, a reply is the only answer a person can
// give. A person's reply always hands an open ask to the agent (Dispatch's askReplyTurn), so one
// leaving it with the human is a person's own hand-back of an approval request, which answers
// nothing. A session of the root's own tree handing an ask to the person starts the root's wait
// (handedBack). A session's reply answers nothing, whatever turn it leaves.
func (e *Engine) dispatchAskReply(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskReply) (intake.Result, error) {
	switch {
	case fact.Actor.Person() && fact.WaitingOn != "human":
		return intake.Result{}, e.answeredByPerson(ctx, tx, fact.Key, fact.Ask, fact.WaitingOn == "agent", fact.Mark)
	case fact.Actor.Kind == "session" && fact.WaitingOn == "human":
		return intake.Result{}, e.handedBack(ctx, tx, fact)
	}
	return intake.Result{}, nil
}

// answeredByPerson records a person's act on ask, an ask of the root key, as the answer of the
// root's wait when it waits on that ask, or, when answersGate, on its design gate: the architect is
// the tree's only agent while it waits, and the approval request's id is not recorded, so a
// person's act on any of the root's asks counts. The act answers only a wait it follows: one
// Dispatch recorded at or before the wait's start (mark against Awaiting.Since, by sequence when
// both have one, else by time) is an answer the person gave before the architect handed the work
// back to them, applied late, and the root waits on their next act, as a changes request given
// before the wait does (answerAwaitedGate).
func (e *Engine) answeredByPerson(ctx context.Context, tx pgx.Tx, key, ask string, answersGate bool, mark record.EventMark) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || !issue.AwaitingPerson() || issue.Awaiting.AnsweredAt != nil {
		return err
	}
	answer := ""
	switch {
	case issue.Awaiting.Ask != "" && issue.Awaiting.Ask == ask:
		answer = "workflow: a person answered an architect ask"
	case issue.Awaiting.GateVersion != 0 && answersGate:
		answer = "workflow: a person replied on a root waiting on its design gate"
	default:
		return nil
	}
	if !mark.After(issue.Awaiting.Since) {
		e.logOnCommit(ctx, "workflow: a person's act recorded before the wait began answers no wait", "issue", issue.Key,
			"ask", ask, "act_seq", mark.Seq, "since_seq", issue.Awaiting.Since.Seq)
		return nil
	}
	e.logOnCommit(ctx, answer, "issue", issue.Key, "ask", ask, "version", issue.Awaiting.GateVersion)
	at := e.now()
	issue.Awaiting.AnsweredAt = &at
	return e.store.PutIssue(ctx, tx, *issue)
}

// handedBack starts a root's wait when a session of its own tree hands an ask on the admitted root
// back to the person: a reply leaving the turn with them, or Dispatch's ask.handed_back of an
// approval request. With its design gate registered and closed, the root waits on that gate at its
// latest version, which the person's approval, changes request or reply answers; otherwise it waits
// on the ask. The wait begins at the hand-back's own event, so a hand-back the daemon applies late
// (replayed at boot) still begins where Dispatch put it, and a changes request Dispatch recorded
// before it answers nothing. A hand-back at or below the root's wait fence starts no wait
// (waitFenced), and one that would wait on the gate starts none at or below its gate fence either
// (gateFenced), since a registration's read of the approval already decided the gate past it.
// awaitPerson's guards hold: a tree member mid-phase, or a root that could not resume, keeps the
// slot.
func (e *Engine) handedBack(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskReply) error {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil || issue.Phase != phase.Admitted || e.waitFenced(ctx, *issue, fact.Mark, "hand-back") {
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
		if e.gateFenced(ctx, *issue, fact.Mark, "hand-back") {
			return nil
		}
		return e.awaitPerson(ctx, tx, *issue, record.Awaiting{GateVersion: gate.LatestVersion, Since: fact.Mark},
			fmt.Sprintf("spec version %d is awaiting a person's review", gate.LatestVersion))
	}
	return e.awaitPerson(ctx, tx, *issue, record.Awaiting{Ask: fact.Ask, Since: fact.Mark}, fmt.Sprintf("ask %s was handed back to a person", fact.Ask))
}

// waitFenced says whether a Dispatch event at mark that would start issue's wait is one a wait
// already covered (record.Issue.WaitFenced), and logs it. Such an event reaches the daemon after
// the act it records was already taken into account: a wait it or a later event started. That wait
// may since have been answered and the root resumed, and a second wait started from the late event
// would wait on an act the person already answered, which nothing would ever answer again.
func (e *Engine) waitFenced(ctx context.Context, issue record.Issue, mark record.EventMark, event string) bool {
	if !issue.WaitFenced(mark) {
		return false
	}
	e.logOnCommit(ctx, "workflow: a late event a wait already covered starts no wait", "issue", issue.Key,
		"event", event, "seq", mark.Seq, "wait_fence_seq", issue.WaitFenceSeq)
	return true
}

// gateFenced says whether a Dispatch event at mark that would start a wait on issue's design gate
// is one a register_gate read of Dispatch already covered (record.Issue.GateFenced), and logs it.
// That read decided the gate from everything up to it and left the root as it meant to — waiting,
// or keeping its slot because Dispatch waits on the agent — so a wait started from an event below
// it would wait on an act the registration already took into account. The read says nothing about
// an ordinary question, which this fence never covers.
func (e *Engine) gateFenced(ctx context.Context, issue record.Issue, mark record.EventMark, event string) bool {
	if !issue.GateFenced(mark) {
		return false
	}
	e.logOnCommit(ctx, "workflow: a late event a registration's read already covered starts no wait on the gate", "issue", issue.Key,
		"event", event, "seq", mark.Seq, "gate_fence_seq", issue.GateFenceSeq)
	return true
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
