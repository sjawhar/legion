package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The ask handlers below keep the mirror of every ask of a root (record.RootAsk) and record the
// architect's hand-backs to a person (record.Store.Declare); they never move a root into
// awaiting_person themselves. Admission's every pass decides that from the record alone
// (ReconcileAwaiting). A person's act still answers a wait it follows the moment it arrives
// (answerWait): an answer is a durable fact, and the root takes the next free slot on it.

// Unresumable is the one rule for whether a root that returns its admission slot to wait on a
// person may run again in its own architect session: it returns why it may not, or "" when it may.
// Its hand-over label gone, or its leaving the workflow, ends that, and so does a Dispatch claim
// held by a person or by a session outside its tree, which owns the issue instead; the daemon's own
// session and a session that claims within the tree are legitimate holders. ReconcileAwaiting keeps
// a root it answers no for at admitted, holding its slot, and admission removes an awaiting root,
// and skips an answered one, that it answers no for.
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

// recordedRoot is the record of key when it is a tree root, nil otherwise: only a root's asks are
// mirrored, and only a root waits on a person.
func (e *Engine) recordedRoot(ctx context.Context, tx pgx.Tx, key string) (*record.Issue, error) {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || !claim.IsTreeRoot(issue.Key, issue.Tree) {
		return nil, err
	}
	return issue, nil
}

// mirrorAsk applies one Dispatch event of ask on root, at seq, to the ask's mirror row: opening
// (when not nil) records the facts of the ask's opening whatever order its events arrive in, and
// state the row's state, applied only when seq is newer than everything the row holds. It returns
// the row as it stood before (zero for none) and after, and whether the event was fresh. An event
// with no sequence cannot be placed, and changes nothing.
func (e *Engine) mirrorAsk(ctx context.Context, tx pgx.Tx, root record.Issue, ask string, seq int64, opening, state func(*record.RootAsk)) (before, after record.RootAsk, fresh bool, err error) {
	if seq <= 0 {
		return before, before, false, nil
	}
	stored, err := e.store.RootAsk(ctx, tx, root.Key, ask)
	if err != nil {
		return before, before, false, err
	}
	after = record.RootAsk{Issue: root.Key, Ask: ask}
	if stored != nil {
		before, after = *stored, *stored
	}
	if opening != nil {
		opening(&after)
	}
	fresh = stored == nil || seq > stored.Seq
	if fresh {
		state(&after)
		after.Seq = seq
	}
	if after == before {
		return before, after, fresh, nil
	}
	return before, after, fresh, e.store.PutRootAsk(ctx, tx, after)
}

// declare records a hand-back of root's architect to the person at seq, fresh for its ask: one at
// or below the root's last handout belongs to work the daemon already handed out again, and
// declares nothing.
func (e *Engine) declare(ctx context.Context, tx pgx.Tx, root record.Issue, seq int64, what string) error {
	if seq <= root.HandoutSeq {
		e.logOnCommit(ctx, "workflow: a hand-back at or below the root's last handout declares nothing", "issue", root.Key,
			"what", what, "seq", seq, "handout_seq", root.HandoutSeq)
		return nil
	}
	return e.store.Declare(ctx, tx, root.Key, seq)
}

// dispatchAskOpened opens the ask's mirror row. A question a session of the root's own tree opened
// is the architect handing work to the person; an approval request is the gate's, which its
// registration declares, and a restored ask arrives as DispatchAskRestored instead.
func (e *Engine) dispatchAskOpened(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskOpened) (intake.Result, error) {
	root, err := e.recordedRoot(ctx, tx, fact.Key)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	byTree := false
	if fact.AuthorSession != "" {
		if byTree, err = e.store.SessionClaimsTree(ctx, tx, root.Tree, fact.AuthorSession); err != nil {
			return intake.Result{}, err
		}
	}
	_, _, fresh, err := e.mirrorAsk(ctx, tx, *root, fact.Ask, fact.Mark.Seq,
		func(row *record.RootAsk) { row.Kind, row.ByTree = fact.Kind, row.ByTree || byTree },
		func(row *record.RootAsk) { row.Open, row.WaitingOn, row.ClosedBy = true, "human", "" })
	if err != nil || !fresh || fact.Kind != "question" || !byTree {
		return intake.Result{}, err
	}
	return intake.Result{}, e.declare(ctx, tx, *root, fact.Mark.Seq, "question "+fact.Ask+" opened")
}

// dispatchAskRestored moves the mirror row of an ask settlement restored: open again, or closed
// again with its earlier answer. Nobody acted, so it declares nothing and answers nothing.
func (e *Engine) dispatchAskRestored(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskRestored) (intake.Result, error) {
	root, err := e.recordedRoot(ctx, tx, fact.Key)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	_, _, _, err = e.mirrorAsk(ctx, tx, *root, fact.Ask, fact.Mark.Seq,
		func(row *record.RootAsk) {
			if fact.Kind != "" {
				row.Kind = fact.Kind
			}
		},
		func(row *record.RootAsk) {
			row.Open, row.WaitingOn, row.ClosedBy = fact.Open, "", ""
			if fact.Open {
				row.WaitingOn = "human"
			} else {
				row.ClosedSeq, row.ClosedBy = fact.Mark.Seq, record.ClosedByOther
			}
		})
	return intake.Result{}, err
}

// dispatchAskClosed closes the ask's mirror row. A person's answer or resolution of a question is
// that person's act on it (RootAsk.PersonSeq), and answers the root's wait (personAnswered). A
// person's answer of an approval request is a review: when it is the request a gate of the root
// names, the review's own artifact.* event answers the wait (applyArtifact), since that event,
// appended after the answer, is what moves the gate; on any other approval ask it answers a wait
// on a question as any act of a person does. A close by anyone else ends no wait unless it closes
// the subject of the root's wait (closedAwaitedAsk).
func (e *Engine) dispatchAskClosed(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskClosed) (intake.Result, error) {
	root, err := e.recordedRoot(ctx, tx, fact.Key)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	closer, err := e.closer(ctx, tx, *root, fact.Actor)
	if err != nil {
		return intake.Result{}, err
	}
	_, after, _, err := e.mirrorAsk(ctx, tx, *root, fact.Ask, fact.Mark.Seq,
		func(row *record.RootAsk) {
			if fact.Kind != "" {
				row.Kind = fact.Kind
			}
			if closer == record.ClosedByPerson && row.Question() {
				row.PersonSeq = max(row.PersonSeq, fact.Mark.Seq)
			}
		},
		func(row *record.RootAsk) {
			row.Open, row.WaitingOn, row.ClosedSeq, row.ClosedBy = false, "", fact.Mark.Seq, closer
		})
	if err != nil {
		return intake.Result{}, err
	}
	if closer != record.ClosedByPerson {
		return intake.Result{}, e.closedAwaitedAsk(ctx, tx, *root, fact, closer)
	}
	// A person's answer of an approval ask is a review; a resolution is not, and answers as any act
	// of theirs does.
	if (fact.Kind == "approval" || after.Kind == "approval") && fact.State == "answered" {
		gate, err := e.store.Gate(ctx, tx, root.Key)
		if err != nil || gate != nil && gate.RequestAsk == fact.Ask {
			return intake.Result{}, err
		}
		return intake.Result{}, e.personAnswered(ctx, tx, *root, fact.Ask, false, fact.Mark)
	}
	return intake.Result{}, e.personAnswered(ctx, tx, *root, fact.Ask, true, fact.Mark)
}

// closer names who closed an ask of root: a person, Dispatch's settlement, a session of the root's
// tree, or anyone else.
func (e *Engine) closer(ctx context.Context, tx pgx.Tx, root record.Issue, actor dispatch.Actor) (string, error) {
	switch {
	case actor.Person():
		return record.ClosedByPerson, nil
	case actor == dispatch.SettlementActor:
		return record.ClosedBySettlement, nil
	case actor.Kind == "session":
		inTree, err := e.store.SessionClaimsTree(ctx, tx, root.Tree, actor.ID)
		if err != nil || !inTree {
			return record.ClosedByOther, err
		}
		return record.ClosedByTree, nil
	}
	return record.ClosedByOther, nil
}

// closedAwaitedAsk applies a close by anyone but a person of the subject of root's wait: the ask
// it waits on, or the approval request of the gate it waits on. Nobody will answer a closed ask,
// so the wait moves to what else is still with the person (withHuman), keeping where it began, and
// ends — answered by the retraction or the close, which the resume task names with who closed it —
// only once nothing else is. A close of any other ask changes no wait.
func (e *Engine) closedAwaitedAsk(ctx context.Context, tx pgx.Tx, root record.Issue, fact intake.DispatchAskClosed, closer string) error {
	if !root.AwaitingPerson() || root.Awaiting.AnsweredAt != nil {
		return nil
	}
	gate, err := e.store.Gate(ctx, tx, root.Key)
	if err != nil {
		return err
	}
	onGate := root.Awaiting.GateVersion != 0 && gate != nil && gate.RequestAsk == fact.Ask
	if root.Awaiting.Ask != fact.Ask && !onGate {
		return nil
	}
	asks, err := e.store.RootAsks(ctx, tx, []string{root.Key})
	if err != nil {
		return err
	}
	if next, ok := waitSubject(asks[root.Key], gate, e.armed(), root.Awaiting.Since); ok {
		e.logOnCommit(ctx, "workflow: the ask a root waits on was closed, and its wait moves to what is still with the person", "issue", root.Key,
			"ask", fact.Ask, "next_ask", next.Ask, "next_version", next.GateVersion, "closed_by", closer)
		root.Awaiting.Ask, root.Awaiting.GateVersion, root.Awaiting.Artifact = next.Ask, next.GateVersion, next.Artifact
		return e.store.PutIssue(ctx, tx, root)
	}
	by, detail := record.AnsweredByClosed, "someone outside your tree"
	switch closer {
	case record.ClosedBySettlement:
		by, detail = record.AnsweredByRetraction, ""
	case record.ClosedByTree:
		detail = "a session of your tree"
	}
	if onGate {
		root.Awaiting = &record.Awaiting{Ask: fact.Ask, Since: root.Awaiting.Since}
	}
	e.logOnCommit(ctx, "workflow: the ask a root waits on was closed, and nothing else is with the person", "issue", root.Key, "ask", fact.Ask,
		"closed_by", fmt.Sprintf("%s:%s", fact.Actor.Kind, fact.Actor.ID))
	return e.answerWait(ctx, tx, root, by, detail)
}

// dispatchAskReply applies a reply on an ask of the issue, or Dispatch's hand-back of an approval
// request, to the ask's mirror row. A person's reply that leaves the ask with the agent is their
// act on it (RootAsk.PersonSeq) and answers the root's wait; one that leaves it with the person (a
// person's own hand-back of an approval request) moves the row alone. A session of the root's own
// tree leaving the ask with the person hands it back. A session's reply that moves an ask of the
// root's wait to the agent's turn — a progress note — answers the wait (AnsweredByTurn): the
// person is no longer asked anything there, and the architect looks again.
func (e *Engine) dispatchAskReply(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskReply) (intake.Result, error) {
	root, err := e.recordedRoot(ctx, tx, fact.Key)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	person := fact.Actor.Person()
	inTree := false
	if fact.Actor.Kind == "session" {
		if inTree, err = e.store.SessionClaimsTree(ctx, tx, root.Tree, fact.Actor.ID); err != nil {
			return intake.Result{}, err
		}
	}
	before, _, fresh, err := e.mirrorAsk(ctx, tx, *root, fact.Ask, fact.Mark.Seq,
		func(row *record.RootAsk) {
			if fact.Kind != "" {
				row.Kind = fact.Kind
			}
			if person && fact.WaitingOn == "agent" {
				row.PersonSeq = max(row.PersonSeq, fact.Mark.Seq)
			}
		},
		func(row *record.RootAsk) {
			row.Open, row.WaitingOn = fact.WaitingOn != "", fact.WaitingOn
			if row.Open {
				row.ClosedBy = ""
			} else if row.ClosedBy == "" {
				row.ClosedSeq, row.ClosedBy = fact.Mark.Seq, record.ClosedByOther
			}
		})
	if err != nil {
		return intake.Result{}, err
	}
	switch {
	case person && fact.WaitingOn != "human":
		return intake.Result{}, e.personAnswered(ctx, tx, *root, fact.Ask, fact.WaitingOn == "agent", fact.Mark)
	case !person && fact.WaitingOn == "human" && inTree && fresh:
		return intake.Result{}, e.declare(ctx, tx, *root, fact.Mark.Seq, "ask "+fact.Ask+" handed back")
	case !person && fact.WaitingOn == "agent" && fresh && before.ByTree && before.Question() && before.WithHuman():
		return intake.Result{}, e.turnMoved(ctx, tx, *root, fact)
	}
	return intake.Result{}, nil
}

// turnMoved answers root's wait when a session moves a tree question that was with the person to
// the agent's turn after the wait began.
func (e *Engine) turnMoved(ctx context.Context, tx pgx.Tx, root record.Issue, fact intake.DispatchAskReply) error {
	if !root.AwaitingPerson() || root.Awaiting.AnsweredAt != nil || !fact.Mark.After(root.Awaiting.Since) {
		return nil
	}
	e.logOnCommit(ctx, "workflow: a session handed a question of a root's wait to the agent's turn", "issue", root.Key, "ask", fact.Ask,
		"session", fact.Actor.ID)
	return e.answerWait(ctx, tx, root, record.AnsweredByTurn, fact.Ask)
}

// personAnswered answers root's wait with a person's act on ask at mark. The architect is the
// tree's only agent while it waits, so a person's act on any ask of the root counts — an act on
// another question, which the architect may have asked in place of the one the wait names, among
// them. Only on a gate wait does an act that answers no gate (answersGate false: a person's answer
// of an approval request, whose review event answers the gate) count solely on the awaited ask
// itself. The act answers only a wait it follows: one Dispatch recorded at or before the wait's
// start (record.EventMark.After against Awaiting.Since) is an answer the person gave before the
// architect handed the work back to them, and the root waits on their next act.
func (e *Engine) personAnswered(ctx context.Context, tx pgx.Tx, root record.Issue, ask string, answersGate bool, mark record.EventMark) error {
	if !root.AwaitingPerson() || root.Awaiting.AnsweredAt != nil {
		return nil
	}
	answer, by := "", record.Answer("")
	switch {
	case root.Awaiting.Ask != "" && root.Awaiting.Ask == ask:
		answer, by = "workflow: an architect ask was answered", record.AnsweredByAsk
	case answersGate || root.Awaiting.GateVersion == 0:
		answer, by = "workflow: a person acted on an ask of a root waiting on them", record.AnsweredByReply
	default:
		return nil
	}
	if !mark.After(root.Awaiting.Since) {
		e.logOnCommit(ctx, "workflow: a person's act recorded before the wait began answers no wait", "issue", root.Key,
			"ask", ask, "act_seq", mark.Seq, "since_seq", root.Awaiting.Since.Seq)
		return nil
	}
	e.logOnCommit(ctx, answer, "issue", root.Key, "ask", ask, "version", root.Awaiting.GateVersion)
	return e.answerWait(ctx, tx, root, by, ask)
}

// answerWait records that by answered root's wait now, with detail, which puts the root at the
// front of the waiting line (record.Waiting) and names the act in the task its architect resumes
// with (resumeTask). The detail is the act's own: the ask a person acted on, the version and
// reason of the review that answered, or who closed the ask, so a later review of the same version
// does not rename it.
func (e *Engine) answerWait(ctx context.Context, tx pgx.Tx, root record.Issue, by record.Answer, detail string) error {
	at := e.now()
	root.Awaiting.AnsweredAt, root.Awaiting.AnsweredBy, root.Awaiting.AnsweredDetail = &at, by, detail
	return e.store.PutIssue(ctx, tx, root)
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

// armed says whether the design gate is armed: with it off, no approval is asked for.
func (e *Engine) armed() bool { return e.cfg.DesignGate != config.DesignGateOff }
