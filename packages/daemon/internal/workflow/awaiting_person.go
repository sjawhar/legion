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
// Neither does one a person has already acted on after it opened (awaitPerson's actedSince):
// Dispatch's intake retries a delivery, so the answer can reach the daemon first, and a wait
// started after it would wait on an ask the person has already answered.
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
	if issue.AwaitingPerson() {
		return intake.Result{}, e.coveredByTheWaitItRuns(ctx, tx, *issue, fact.Mark, "question opened")
	}
	return intake.Result{}, e.awaitPerson(ctx, tx, *issue, record.Awaiting{Ask: fact.Ask, Since: fact.Mark}, fmt.Sprintf("question %s is awaiting an answer", fact.Ask))
}

// coveredByTheWaitItRuns raises the root's wait fence to a wait-starting event the root was
// already waiting when it arrived, and reports nothing else. The wait that ran covered the act:
// the architect handed its work back, or asked, within the turn that ended in the wait, and the
// person's answer to that wait is its answer too. Redelivered after the answer resumed the root,
// the same event would otherwise start a second wait on an act already answered, which nothing
// would answer again.
func (e *Engine) coveredByTheWaitItRuns(ctx context.Context, tx pgx.Tx, issue record.Issue, mark record.EventMark, event string) error {
	if !issue.AwaitingPerson() || !issue.RaiseWaitFence(mark.Seq) {
		return nil
	}
	e.logOnCommit(ctx, "workflow: an event that would start a wait arrived while the root already waited; the running wait covers it",
		"issue", issue.Key, "event", event, "seq", mark.Seq)
	return e.store.PutIssue(ctx, tx, issue)
}

// dispatchAskClosed applies a close of an ask. A person's answer or resolution is a person's act
// (personActed). A close by anyone else — a session of the tree, or Dispatch's settlement
// retracting an ask whose decision block left the spec — answers nothing in general, and is
// recorded nowhere; but when it closes the very ask a root waits on, it ends that wait
// (closedAwaitedAsk): the question is gone, no person can answer it, and the root would otherwise
// wait for good. A review (a person's answer to an approval request) answers a wait on the design
// gate through its own artifact.approved or artifact.changes_requested event (applyArtifact),
// which Dispatch appends after the answer: woken by the answer, the root could resume before the
// review reached its gate.
func (e *Engine) dispatchAskClosed(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskClosed) (intake.Result, error) {
	if fact.Actor.Person() {
		review := fact.Kind == "approval" && fact.State == "answered"
		return intake.Result{}, e.personActed(ctx, tx, fact.Key, fact.Ask, !review, fact.Mark, record.AnsweredByAsk)
	}
	by := record.AnsweredByClosed
	if fact.Actor == dispatch.SettlementActor {
		by = record.AnsweredByRetraction
	}
	return intake.Result{}, e.closedAwaitedAsk(ctx, tx, fact.Key, fact.Ask, by, fact.Mark)
}

// closedAwaitedAsk ends a root's wait when the ask it waits on is closed by a session of its tree
// or by Dispatch's settlement. Nobody will answer a closed question: a person's reply in its
// thread is the one act left, and the architect that closed it has moved on, so the root takes a
// slot back rather than waiting for good. The resume task names what closed it. A close of another
// ask, or of an ask of a root waiting on its design gate, changes nothing: the gate's own review
// still answers that wait. Every close of a root's ask is recorded (record.PersonAct on that ask
// alone, which answers no gate), so the ask.opened of a question already closed, delivered after
// it, starts no wait.
func (e *Engine) closedAwaitedAsk(ctx context.Context, tx pgx.Tx, key, ask string, by record.Answer, mark record.EventMark) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return err
	}
	if claim.IsTreeRoot(issue.Key, issue.Tree) && mark.Seq > 0 {
		if err := e.store.RecordPersonAct(ctx, tx, record.PersonAct{Issue: key, Ask: ask, Mark: mark}); err != nil {
			return err
		}
	}
	if !issue.AwaitingPerson() || issue.Awaiting.AnsweredAt != nil || issue.Awaiting.Ask != ask {
		return nil
	}
	e.logOnCommit(ctx, "workflow: the ask a root waits on was closed, so nobody will answer it", "issue", issue.Key, "ask", ask, "closed_by", by)
	return e.answerWait(ctx, tx, *issue, by, "")
}

// dispatchAskReply applies a reply on an ask of the issue, or Dispatch's hand-back of an approval
// request. A person's reply is a person's act (personActed) whatever the ask's state: on an ask a
// session or settlement closed, a reply is the only answer a person can give. A person's reply
// always hands an open ask to the agent (Dispatch's askReplyTurn), so one leaving it with the human
// is a person's own hand-back of an approval request, which answers nothing. A session of the
// root's own tree handing an ask to the person starts the root's wait (handedBack). A session's
// reply answers nothing, whatever turn it leaves.
func (e *Engine) dispatchAskReply(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskReply) (intake.Result, error) {
	switch {
	case fact.Actor.Person() && fact.WaitingOn != "human":
		return intake.Result{}, e.personActed(ctx, tx, fact.Key, fact.Ask, fact.WaitingOn == "agent", fact.Mark, record.AnsweredByAsk)
	case fact.Actor.Kind == "session" && fact.WaitingOn == "human":
		return intake.Result{}, e.handedBack(ctx, tx, fact)
	}
	return intake.Result{}, nil
}

// personActed applies a person's act on ask, an ask of the issue key, at mark. On a root it is
// recorded, whatever the root's phase (record.PersonAct), so a wait whose starting event reaches
// the daemon after it starts none (awaitPerson). It answers the root's wait, whether that wait is
// on a question or on the design gate: the architect is the tree's only agent while it waits, no
// ask of the root is anyone else's, and the approval request's id is not recorded, so a person's
// act on any of the root's asks counts — including an act on another question, which the architect
// may have asked in place of the one the wait names. Only the act on the awaited ask itself counts
// when it answers no gate (a reply under an ask a session or settlement closed, and a person's
// answer of an approval request, whose artifact.* event answers the gate after it). The act
// answers only a wait it follows: one Dispatch recorded at or before the wait's start
// (record.EventMark.After against Awaiting.Since) is an answer the person gave before the
// architect handed the work back to them, applied late, and the root waits on their next act, as a
// changes request given before the wait does (answerAwaitedGate).
func (e *Engine) personActed(ctx context.Context, tx pgx.Tx, key, ask string, answersGate bool, mark record.EventMark, onAsk record.Answer) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return err
	}
	if claim.IsTreeRoot(issue.Key, issue.Tree) && mark.Seq > 0 {
		if err := e.store.RecordPersonAct(ctx, tx, record.PersonAct{Issue: key, Ask: ask, Mark: mark, AnswersGate: answersGate}); err != nil {
			return err
		}
	}
	if !issue.AwaitingPerson() || issue.Awaiting.AnsweredAt != nil {
		return nil
	}
	answer, by := "", record.Answer("")
	switch {
	case issue.Awaiting.Ask != "" && issue.Awaiting.Ask == ask:
		answer, by = "workflow: an architect ask was answered", onAsk
	case answersGate:
		answer, by = "workflow: a person acted on an ask of a root waiting on them", record.AnsweredByReply
	default:
		return nil
	}
	if !mark.After(issue.Awaiting.Since) {
		e.logOnCommit(ctx, "workflow: a person's act recorded before the wait began answers no wait", "issue", issue.Key,
			"ask", ask, "act_seq", mark.Seq, "since_seq", issue.Awaiting.Since.Seq)
		return nil
	}
	e.logOnCommit(ctx, answer, "issue", issue.Key, "ask", ask, "version", issue.Awaiting.GateVersion)
	return e.answerWait(ctx, tx, *issue, by, ask)
}

// answerWait records that by answered root's wait now, with detail, which puts the root at the
// front of the waiting line (record.Waiting) and names the act in the task its architect resumes
// with (resumeTask). The detail is the act's own: the ask a person acted on, or the version and
// reason of the review that answered, so a later review of the same version does not rename it.
func (e *Engine) answerWait(ctx context.Context, tx pgx.Tx, root record.Issue, by record.Answer, detail string) error {
	at := e.now()
	root.Awaiting.AnsweredAt, root.Awaiting.AnsweredBy, root.Awaiting.AnsweredDetail = &at, by, detail
	return e.store.PutIssue(ctx, tx, root)
}

// actedSince names the person's act the root already holds that answers waiting, a wait about to
// start, and that Dispatch recorded after the wait's start, or returns "" when there is none. Such
// an act reached the daemon before the event that starts the wait (Dispatch's intake retries a
// delivery, and the daemon naks a failed fact while its consumer moves on), and a wait started now
// would wait on what the person has already done, with nothing left to answer it. It is a
// person's act on the awaited ask, or for a wait on the design gate one that answers the gate
// (record.PersonAct.AnswersGate), or the review the gate applied last (classify.LatestReview) at
// the gate's latest version, which the architect has been told or the gate holds open. Only a mark
// placed by sequence counts (record.EventMark.Follows): every wait starts at a sequenced event or
// read, and every act the daemon records carries its event's sequence.
func (e *Engine) actedSince(ctx context.Context, tx pgx.Tx, root record.Issue, waiting record.Awaiting) (string, error) {
	acts, err := e.store.PersonActs(ctx, tx, root.Key)
	if err != nil {
		return "", err
	}
	for _, act := range acts {
		if act.Mark.Follows(waiting.Since) && (act.Ask == waiting.Ask || act.AnswersGate) {
			return fmt.Sprintf("a person acted on ask %s at sequence %d", act.Ask, act.Mark.Seq), nil
		}
	}
	gate, err := e.store.Gate(ctx, tx, root.Key)
	if err != nil || gate == nil {
		return "", err
	}
	if mark, version, ok := classify.LatestReview(*gate); ok && version >= gate.LatestVersion && mark.Follows(waiting.Since) {
		return fmt.Sprintf("a person reviewed spec version %d at sequence %d", version, mark.Seq), nil
	}
	return "", nil
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
	if err != nil || issue == nil || e.waitFenced(ctx, *issue, fact.Mark, "hand-back") {
		return err
	}
	if issue.Phase != phase.Admitted {
		return e.coveredByTheWaitItRuns(ctx, tx, *issue, fact.Mark, "hand-back")
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
		return e.awaitPerson(ctx, tx, *issue, record.Awaiting{GateVersion: gate.LatestVersion, Artifact: gate.ArtifactID, Since: fact.Mark},
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
