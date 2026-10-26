package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The handlers below keep the daemon's mirror of every ask of a root (record.RootAsk) current
// from Dispatch's ask events. A row changes only for an event newer than everything it holds, so
// the same events in any order leave the same row, and the facts of an ask's opening (its kind,
// whether the root's tree opened it) are kept whatever order its events arrive in. Only a tree
// root's asks are mirrored; an ask on any other issue changes nothing.
//
// They also record the architect's hand-backs to a person (declare), and they never move a root
// into awaiting_person themselves: admission's every pass decides that from the record alone
// (ReconcileAwaiting). A hand-back is a fresh event of an ask the root can wait on (withHuman): a
// question a session of the root's tree opened, or a reply of such a session that leaves such a
// question with the person. A person's act still answers a wait it follows the moment it arrives
// (personAnswered): an answer is a durable fact, and the root takes the next free slot on it. When
// an ask of the wait leaves the person by any other act, the wait moves to what is still with them,
// and is answered only once nothing is (itemLeft).

// recordedRoot is the record of key when it is a tree root, nil otherwise.
func (e *Engine) recordedRoot(ctx context.Context, tx pgx.Tx, key string) (*record.Issue, error) {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || !claim.IsTreeRoot(issue.Key, issue.Tree) {
		return nil, err
	}
	return issue, nil
}

// mirrorAsk applies one Dispatch event of ask on root, at seq, to the ask's mirror row: opening
// records what the event says of the ask whatever its order (its kind, who opened it, a person's
// act on it), and state the row's state, applied only when seq is newer than everything the row
// holds. It returns the row as it stood before (zero for none) and after, and whether the event
// was fresh, newer than the row. An event with no sequence cannot be placed, and changes nothing.
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

// keepKind records kind on row when the event names one the mirror knows: a row keeps the kind an
// earlier event named against an event that names none, and a kind the daemon does not know (the
// decoder reports it, intake's askKind) is no kind, so the ask is still mirrored.
func keepKind(row *record.RootAsk, kind string) {
	if kind == record.AskQuestion || kind == record.AskApproval {
		row.Kind = kind
	}
}

// personAnswers is the one rule for whether an act on an ask is a person's act that answers a root
// waiting on that person: a person's answer or resolution of the ask, except an answer of the
// approval request the root's gate names (requestAsk), which is a review, whose own artifact.*
// event is the act; and a person's reply that does not leave the ask with the person — one handing
// it to the agent, or a comment on a closed ask. A person's reply that leaves the ask on their own
// turn hands nothing to anyone. The mirror records every such act (RootAsk.PersonSeq), and the
// live answer (personAnswered) and admission's reconcile (answeredSince) both read only acts this
// rule names, so the same acts answer a wait whichever of the two sees them first.
func personAnswers(act intake.Fact, requestAsk string) bool {
	switch act := act.(type) {
	case intake.DispatchAskClosed:
		return act.Actor.Person() && (act.State != "answered" || requestAsk == "" || act.Ask != requestAsk)
	case intake.DispatchAskReply:
		return act.Actor.Person() && act.WaitingOn != "human"
	}
	return false
}

// requestAskOf is the approval request root's design gate names, "" when it has none.
func (e *Engine) requestAskOf(ctx context.Context, tx pgx.Tx, root record.Issue) (string, error) {
	gate, err := e.store.Gate(ctx, tx, root.Key)
	if err != nil || gate == nil {
		return "", err
	}
	return gate.RequestAsk, nil
}

// dispatchAskOpened opens the ask's mirror row on the person's turn, recording its kind and
// whether a session of the root's tree opened it. A question a session of the root's own tree
// opened is the architect handing work to the person (declare); an approval request hands back
// nothing a root waits on here, and a restored ask arrives as DispatchAskRestored instead.
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
		func(row *record.RootAsk) {
			keepKind(row, fact.Kind)
			row.ByTree = row.ByTree || byTree
		},
		func(row *record.RootAsk) { row.Open, row.WaitingOn, row.ClosedBy = true, "human", "" })
	if err != nil || !fresh || fact.Kind != record.AskQuestion || !byTree {
		return intake.Result{}, err
	}
	return intake.Result{}, e.declare(ctx, tx, *root, fact.Mark.Seq, "question "+fact.Ask+" opened")
}

// dispatchAskRestored moves the mirror row of an ask Dispatch's settlement restored: open again on
// the turn it last had (the person's, when no event named one), or closed again with its earlier
// answer, keeping that turn. Nobody acted, so it records no person's act and declares nothing; a
// close that takes an ask of the root's wait away from the person moves the wait (itemLeft).
func (e *Engine) dispatchAskRestored(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskRestored) (intake.Result, error) {
	root, err := e.recordedRoot(ctx, tx, fact.Key)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	before, after, fresh, err := e.mirrorAsk(ctx, tx, *root, fact.Ask, fact.Mark.Seq,
		func(row *record.RootAsk) { keepKind(row, fact.Kind) },
		func(row *record.RootAsk) {
			if fact.Open {
				row.Open, row.ClosedBy = true, ""
				if row.WaitingOn == "" {
					row.WaitingOn = "human"
				}
				return
			}
			row.Open, row.ClosedSeq, row.ClosedBy = false, fact.Mark.Seq, record.ClosedByOther
		})
	if err != nil || !fresh {
		return intake.Result{}, err
	}
	return intake.Result{}, e.itemLeft(ctx, tx, *root, before, after, record.AnsweredByClosed, "Dispatch's settlement, which restored its earlier answer")
}

// dispatchAskClosed closes the ask's mirror row, keeping the turn it last had, with who closed it
// (closer), and records a person's act that personAnswers names, which answers the root's wait
// (personAnswered). A close by anyone else moves the wait when it takes an ask of the wait away
// from the person (itemLeft): settlement's is a retraction, any other a close the resume names.
func (e *Engine) dispatchAskClosed(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskClosed) (intake.Result, error) {
	root, err := e.recordedRoot(ctx, tx, fact.Key)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	closer, err := e.closer(ctx, tx, *root, fact.Actor)
	if err != nil {
		return intake.Result{}, err
	}
	requestAsk, err := e.requestAskOf(ctx, tx, *root)
	if err != nil {
		return intake.Result{}, err
	}
	acted := personAnswers(fact, requestAsk)
	before, after, fresh, err := e.mirrorAsk(ctx, tx, *root, fact.Ask, fact.Mark.Seq,
		func(row *record.RootAsk) {
			keepKind(row, fact.Kind)
			if acted {
				row.PersonSeq = max(row.PersonSeq, fact.Mark.Seq)
			}
		},
		func(row *record.RootAsk) { row.Open, row.ClosedSeq, row.ClosedBy = false, fact.Mark.Seq, closer })
	switch {
	case err != nil:
		return intake.Result{}, err
	case acted:
		return intake.Result{}, e.personAnswered(ctx, tx, *root, fact.Ask, fact.Mark)
	case !fresh || closer == record.ClosedByPerson:
		return intake.Result{}, nil
	case closer == record.ClosedBySettlement:
		return intake.Result{}, e.itemLeft(ctx, tx, *root, before, after, record.AnsweredByRetraction, "")
	case closer == record.ClosedByTree:
		return intake.Result{}, e.itemLeft(ctx, tx, *root, before, after, record.AnsweredByClosed, "a session of your tree")
	}
	return intake.Result{}, e.itemLeft(ctx, tx, *root, before, after, record.AnsweredByClosed, "someone outside your tree")
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

// dispatchAskReply applies a reply on an ask of the root, or Dispatch's hand-back of an approval
// request, to the ask's mirror row: an open ask moves to the turn the reply leaves it on, and a
// reply on a closed ask (no turn) leaves it closed with the turn it last had, recorded as closed
// when the row had not yet seen its close. A person's act that personAnswers names is recorded,
// and answers the root's wait (personAnswered). A session of the root's own tree leaving a question
// that tree opened with the person hands it back (declare). Any other reply that takes an ask of
// the root's wait away from the person — a session's progress note handing it to the agent's turn
// — moves the wait (itemLeft).
func (e *Engine) dispatchAskReply(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskReply) (intake.Result, error) {
	root, err := e.recordedRoot(ctx, tx, fact.Key)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	inTree := false
	if fact.Actor.Kind == "session" {
		if inTree, err = e.store.SessionClaimsTree(ctx, tx, root.Tree, fact.Actor.ID); err != nil {
			return intake.Result{}, err
		}
	}
	acted := personAnswers(fact, "")
	before, after, fresh, err := e.mirrorAsk(ctx, tx, *root, fact.Ask, fact.Mark.Seq,
		func(row *record.RootAsk) {
			keepKind(row, fact.Kind)
			if acted {
				row.PersonSeq = max(row.PersonSeq, fact.Mark.Seq)
			}
		},
		func(row *record.RootAsk) {
			if fact.WaitingOn != "" {
				row.Open, row.WaitingOn, row.ClosedBy = true, fact.WaitingOn, ""
				return
			}
			if row.Open || row.ClosedBy == "" {
				row.Open, row.ClosedSeq, row.ClosedBy = false, fact.Mark.Seq, record.ClosedByOther
			}
		})
	switch {
	case err != nil:
		return intake.Result{}, err
	case acted:
		return intake.Result{}, e.personAnswered(ctx, tx, *root, fact.Ask, fact.Mark)
	case !fresh || fact.Actor.Person():
		return intake.Result{}, nil
	case inTree && fact.WaitingOn == "human" && after.ByTree && after.Question():
		return intake.Result{}, e.declare(ctx, tx, *root, fact.Mark.Seq, "question "+fact.Ask+" handed back")
	}
	return intake.Result{}, e.itemLeft(ctx, tx, *root, before, after, record.AnsweredByTurn, fact.Ask)
}
