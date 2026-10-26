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
// from Dispatch's ask events. Each row is written from its ask's own events alone, never from the
// state of the gate or any other record when the event is applied. Each part of a row (whether the
// ask is open, whose turn it is, its newest close) changes only for an event newer than the one
// that last gave that part, so the same events in any order leave the same row, and the facts of
// an ask (its kind, an approval request's document, whether the root's tree opened it) and a
// person's acts on it are kept whatever order its events arrive in. Only a tree root's asks are
// mirrored; an ask on any other issue changes nothing.
//
// They also record the architect's hand-backs to a person (declare), and they never move a root
// into awaiting_person themselves: admission's every pass decides that from the record alone
// (ReconcileAwaiting). A hand-back is an event that gives the person the turn of an ask the root can
// wait on (withHuman), newer than the turn the row held (handsTurn): a question a session of the
// root's tree opened, or a reply of such a session that leaves such a question with the person. A
// person's act still answers a wait it follows the moment it arrives (personAnswered): an answer is
// a durable fact, and the root takes the next free slot on it. When an ask of the wait leaves the
// person by any other act, the wait moves to what is still with them, and is answered only once
// nothing is (itemLeft). These decisions read the record beside the row (the gate's document,
// gateArtifact); the row itself never does.

// recordedRoot is the record of key when it is a tree root, nil otherwise.
func (e *Engine) recordedRoot(ctx context.Context, tx pgx.Tx, key string) (*record.Issue, error) {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || !claim.IsTreeRoot(issue.Key, issue.Tree) {
		return nil, err
	}
	return issue, nil
}

// askState is what an event says of whether an ask is open.
type askState int

const (
	askStateUnsaid askState = iota
	askOpen
	askClosed
)

// askChange is what one Dispatch event of an ask, or a registration's read of it, says of the ask,
// at the event's place in the issue's event log (seq). Each part is applied behind its own fence on
// the row (apply): state when seq is newer than the row's Seq, turn ("" for none) when newer than
// its WaitingOnSeq, and closer ("" when the event is no close) when newer than its ClosedSeq. The
// facts of the ask (kind, artifact, byTree) and a person's acts (personAct's answer and other) are
// kept from any event that carries them, however old.
type askChange struct {
	seq      int64
	kind     string
	artifact string
	byTree   bool
	state    askState
	turn     string
	closer   string
	answer   bool
	other    bool
}

// apply merges the change into row.
func (c askChange) apply(row *record.RootAsk) {
	keepKind(row, c.kind)
	if c.artifact != "" {
		row.Artifact = c.artifact
	}
	row.ByTree = row.ByTree || c.byTree
	if c.answer {
		row.PersonAnswerSeq = max(row.PersonAnswerSeq, c.seq)
	}
	if c.other {
		row.PersonSeq = max(row.PersonSeq, c.seq)
	}
	if c.state != askStateUnsaid && c.seq > row.Seq {
		row.Open, row.Seq = c.state == askOpen, c.seq
	}
	if c.turn != "" && c.seq > row.WaitingOnSeq {
		row.WaitingOn, row.WaitingOnSeq = c.turn, c.seq
	}
	if c.closer != "" && c.seq > row.ClosedSeq {
		row.ClosedBy, row.ClosedSeq = c.closer, c.seq
	}
}

// handsTurn says whether the change gives the person the turn of the ask whose row stood as before:
// it names the person's turn and is newer than the turn the row held (WaitingOnSeq), so the same
// event redelivered, or one a newer turn already superseded, hands nothing back.
func (c askChange) handsTurn(before record.RootAsk) bool {
	return c.turn == "human" && c.seq > before.WaitingOnSeq
}

// mirrorAsk applies change to the mirror row of ask on root, writing it when it moved. It returns
// the row as it stood before (zero for none) and after. A change with no sequence cannot be placed,
// and changes nothing.
func (e *Engine) mirrorAsk(ctx context.Context, tx pgx.Tx, root record.Issue, ask string, change askChange) (before, after record.RootAsk, err error) {
	if change.seq <= 0 {
		return before, before, nil
	}
	stored, err := e.store.RootAsk(ctx, tx, root.Key, ask)
	if err != nil {
		return before, before, err
	}
	after = record.RootAsk{Issue: root.Key, Ask: ask}
	if stored != nil {
		before, after = *stored, *stored
	}
	change.apply(&after)
	if stored != nil && after == before {
		return before, after, nil
	}
	return before, after, e.store.PutRootAsk(ctx, tx, after)
}

// keepKind records kind on row when the event names one the mirror knows: a row keeps the kind an
// earlier event named against an event that names none, and a kind the daemon does not know (the
// decoder reports it, intake's askKind) is no kind, so the ask is still mirrored.
func keepKind(row *record.RootAsk, kind string) {
	if kind == record.AskQuestion || kind == record.AskApproval {
		row.Kind = kind
	}
}

// personAct is the one rule for which acts on an ask are a person's acts that would answer a root
// waiting on that person, and the mirror records each on the ask's row: answer is a person's
// answer of the ask (RootAsk.PersonAnswerSeq), which answers such a root unless the ask is an
// approval request of the document the root's design gate is registered on, whose answer is a
// review that answers through its own artifact.* event (RootAsk.PersonActSeq leaves it out by the
// row's own document, so no binding is involved); other is a person's resolution of the ask, or a
// person's reply that does not leave the ask with the person — one handing it to the agent, or a
// comment on a closed ask (RootAsk.PersonSeq). A person's reply that leaves the ask on their own
// turn hands nothing to anyone. The live answer (personAnswered, through answersWait) and
// admission's reconcile (answeredSince, through PersonActSeq) both read only acts this rule names,
// so the same acts answer a wait whichever of the two sees them first.
func personAct(act intake.Fact) (answer, other bool) {
	switch act := act.(type) {
	case intake.DispatchAskClosed:
		if !act.Actor.Person() {
			return false, false
		}
		return act.State == "answered", act.State != "answered"
	case intake.DispatchAskReply:
		return false, act.Actor.Person() && act.WaitingOn != "human"
	}
	return false, false
}

// gateArtifact is the document root's design gate is registered on, "" for no gate.
func (e *Engine) gateArtifact(ctx context.Context, tx pgx.Tx, root record.Issue) (string, error) {
	gate, err := e.store.Gate(ctx, tx, root.Key)
	if err != nil || gate == nil {
		return "", err
	}
	return gate.ArtifactID, nil
}

// answersWait says whether a person's act personAct named (answer, other) on the ask whose row is
// now after answers a root waiting on that person, by the rule RootAsk.PersonActSeq reads: every
// other act, and an answer unless the ask is an approval request of the gate's document.
func (e *Engine) answersWait(ctx context.Context, tx pgx.Tx, root record.Issue, after record.RootAsk, answer, other bool) (bool, error) {
	if other || !answer {
		return other, nil
	}
	artifact, err := e.gateArtifact(ctx, tx, root)
	if err != nil {
		return false, err
	}
	return !after.GateRequestOf(artifact), nil
}

// dispatchAskOpened opens the ask's mirror row on the person's turn, recording its kind, an
// approval request's document, and whether a session of the root's tree opened it. A question a
// session of the root's own tree opened is the architect handing work to the person (declare); an
// approval request hands back nothing a root waits on here, and a restored ask arrives as
// DispatchAskRestored instead.
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
	change := askChange{seq: fact.Mark.Seq, kind: fact.Kind, artifact: fact.Artifact, byTree: byTree, state: askOpen, turn: "human"}
	before, _, err := e.mirrorAsk(ctx, tx, *root, fact.Ask, change)
	if err != nil || fact.Kind != record.AskQuestion || !byTree || !change.handsTurn(before) {
		return intake.Result{}, err
	}
	return intake.Result{}, e.declare(ctx, tx, *root, fact.Mark.Seq, "question "+fact.Ask+" opened")
}

// dispatchAskRestored moves the mirror row of an ask Dispatch's settlement restored: open again on
// the turn it last had (a restore names none, and an open ask with none is the person's), or
// closed again with its earlier answer, a close by nobody who acted. Nobody acted, so it records no
// person's act and declares nothing; a close that takes an ask of the root's wait away from the
// person moves the wait (itemLeft).
func (e *Engine) dispatchAskRestored(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskRestored) (intake.Result, error) {
	root, err := e.recordedRoot(ctx, tx, fact.Key)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	change := askChange{seq: fact.Mark.Seq, kind: fact.Kind, artifact: fact.Artifact, state: askOpen}
	if !fact.Open {
		change.state, change.closer = askClosed, record.ClosedByOther
	}
	before, after, err := e.mirrorAsk(ctx, tx, *root, fact.Ask, change)
	if err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.itemLeft(ctx, tx, *root, before, after, record.AnsweredByClosed, "Dispatch's settlement, which restored its earlier answer")
}

// dispatchAskClosed closes the ask's mirror row, keeping the turn it last had, records who closed
// it (closer), and records a person's act personAct names, which answers the root's wait when it
// would answer one (answersWait, personAnswered). A close by anyone else moves the wait when it
// takes an ask of the wait away from the person (itemLeft): settlement's is a retraction, any other
// a close the resume names.
func (e *Engine) dispatchAskClosed(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskClosed) (intake.Result, error) {
	root, err := e.recordedRoot(ctx, tx, fact.Key)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	closer, err := e.closer(ctx, tx, *root, fact.Actor)
	if err != nil {
		return intake.Result{}, err
	}
	answer, other := personAct(fact)
	before, after, err := e.mirrorAsk(ctx, tx, *root, fact.Ask, askChange{seq: fact.Mark.Seq, kind: fact.Kind, artifact: fact.Artifact,
		state: askClosed, closer: closer, answer: answer, other: other})
	if err != nil {
		return intake.Result{}, err
	}
	answers, err := e.answersWait(ctx, tx, *root, after, answer, other)
	switch {
	case err != nil:
		return intake.Result{}, err
	case answers:
		return intake.Result{}, e.personAnswered(ctx, tx, *root, fact.Ask, fact.Mark)
	case closer == record.ClosedByPerson:
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
// request, to the ask's mirror row: a reply on an open ask (one with a turn) says it is open on
// that turn, and a reply on a closed ask (no turn) says only that it is closed — not who closed it
// or where, which its own close records whenever it arrives. A reply is the newest reply of its
// thread, and Dispatch takes an ask's turn from its newest reply, the person's when that reply
// records none, as one on a closed ask never does: so a reply on a closed ask leaves the person's
// turn, which is the turn the ask reopens on if Dispatch's settlement restores it. (Only a question
// is ever restored; a closed approval request never reopens, so the moved-request rule that could
// give it the agent's turn never applies here.) A person's act personAct names is recorded, and
// answers the root's wait (personAnswered). A session of the root's own tree leaving a question that
// tree opened with the person hands it back (declare), unless a newer turn of the ask is already
// recorded (handsTurn). Any other reply that takes an ask of the root's wait away from the person —
// a session's progress note handing it to the agent's turn — moves the wait (itemLeft).
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
	answer, other := personAct(fact)
	change := askChange{seq: fact.Mark.Seq, kind: fact.Kind, artifact: fact.Artifact, state: askOpen, turn: fact.WaitingOn, answer: answer, other: other}
	if fact.WaitingOn == "" {
		change.state, change.turn = askClosed, "human"
	}
	before, after, err := e.mirrorAsk(ctx, tx, *root, fact.Ask, change)
	if err != nil {
		return intake.Result{}, err
	}
	answers, err := e.answersWait(ctx, tx, *root, after, answer, other)
	switch {
	case err != nil:
		return intake.Result{}, err
	case answers:
		return intake.Result{}, e.personAnswered(ctx, tx, *root, fact.Ask, fact.Mark)
	case fact.Actor.Person():
		return intake.Result{}, nil
	case inTree && fact.WaitingOn == "human" && after.ByTree && after.Question():
		if !change.handsTurn(before) {
			return intake.Result{}, nil
		}
		return intake.Result{}, e.declare(ctx, tx, *root, fact.Mark.Seq, "question "+fact.Ask+" handed back")
	}
	return intake.Result{}, e.itemLeft(ctx, tx, *root, before, after, record.AnsweredByTurn, fact.Ask)
}

// dispatchApprovalMoved moves an approval request to its agent's turn for a version that moved it
// past the version handed to the person, as Dispatch does, from the request's own ask.edited event:
// whichever request of whichever document it is, bound by any registration or none, and whatever
// order it arrives in beside the request's other events and the gate's registration. It says
// nothing of whether the request is open, and records no person's act. A root never waits on an
// approval request in this release (withHuman), so the move changes no wait.
func (e *Engine) dispatchApprovalMoved(ctx context.Context, tx pgx.Tx, fact intake.DispatchApprovalMoved) (intake.Result, error) {
	root, err := e.recordedRoot(ctx, tx, fact.Key)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	_, _, err = e.mirrorAsk(ctx, tx, *root, fact.Ask, askChange{seq: fact.Mark.Seq, kind: record.AskApproval, artifact: fact.Artifact,
		turn: "agent"})
	return intake.Result{}, err
}
