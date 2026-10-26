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
// Neither does one already answered or closed after it opened (awaitPerson's actedSince):
// Dispatch's intake retries a delivery, so the answer can reach the daemon first. A question opened
// while the root already waits is held pending (coveredByTheWaitItRuns), so the architect resumed
// for the first answer is told it is still with the person.
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
	waiting := record.Awaiting{Ask: fact.Ask, Since: fact.Mark}
	if issue.AwaitingPerson() {
		return intake.Result{}, e.coveredByTheWaitItRuns(ctx, tx, *issue, waiting, "question opened")
	}
	return intake.Result{}, e.awaitPerson(ctx, tx, *issue, waiting, fmt.Sprintf("question %s is awaiting an answer", fact.Ask))
}

// coveredByTheWaitItRuns takes a wait-starting event that arrived while the root already waited,
// answered or not: it raises the root's wait fence to it, so a redelivery after the resume starts
// no second wait, and holds the wait it would start pending (holdPendingWait), so what it asks of
// the person is not lost. The resumed architect is told every pending wait still with the person,
// and handing one back starts the next wait.
func (e *Engine) coveredByTheWaitItRuns(ctx context.Context, tx pgx.Tx, issue record.Issue, waiting record.Awaiting, event string) error {
	if !issue.AwaitingPerson() {
		return nil
	}
	sameWait := issue.Awaiting.AnsweredAt == nil && (waiting.Ask != "" && waiting.Ask == issue.Awaiting.Ask ||
		waiting.GateVersion != 0 && waiting.GateVersion == issue.Awaiting.GateVersion && waiting.Artifact == issue.Awaiting.Artifact)
	if issue.RaiseWaitFence(waiting.Since.Seq) {
		e.logOnCommit(ctx, "workflow: an event that would start a wait arrived while the root already waited; it is held pending",
			"issue", issue.Key, "event", event, "seq", waiting.Since.Seq)
		if err := e.store.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
	}
	if sameWait {
		return nil
	}
	return e.holdPendingWait(ctx, tx, issue, waiting)
}

// holdPendingWait keeps waiting as a wait root would start, unless what it waits on has already
// happened (actedSince) or its event carries no sequence to place it by.
func (e *Engine) holdPendingWait(ctx context.Context, tx pgx.Tx, root record.Issue, waiting record.Awaiting) error {
	if waiting.Since.Seq <= 0 || !claim.IsTreeRoot(root.Key, root.Tree) {
		return nil
	}
	if acted, err := e.actedSince(ctx, tx, root, waiting, true); err != nil || acted != "" {
		return err
	}
	return e.store.HoldPendingWait(ctx, tx, record.PendingWait{Issue: root.Key, Ask: waiting.Ask, GateVersion: waiting.GateVersion,
		Artifact: waiting.Artifact, Since: waiting.Since})
}

// livePendingWaits is every wait root holds pending that is still with the person, in event order.
// One whose ask was answered or closed after it, or whose gate a review, a newer version or the
// gate's opening has moved past (actedSince), is dropped.
func (e *Engine) livePendingWaits(ctx context.Context, tx pgx.Tx, root record.Issue) ([]record.PendingWait, error) {
	pending, err := e.store.PendingWaits(ctx, tx, root.Key)
	if err != nil {
		return nil, err
	}
	live := pending[:0]
	for _, wait := range pending {
		acted, err := e.actedSince(ctx, tx, root, wait.Awaiting(), true)
		if err != nil {
			return nil, err
		}
		if acted != "" {
			if err := e.store.DropPendingWait(ctx, tx, root.Key, wait.Since.Seq); err != nil {
				return nil, err
			}
			continue
		}
		live = append(live, wait)
	}
	return live, nil
}

// awaitPending starts the oldest wait root holds pending once nothing keeps its slot any more: the
// member of its tree that ran a phase has left, or the claim a person held is released. awaitPerson
// decides as at any wait's start, and holds the wait pending again when a guard still keeps the slot.
func (e *Engine) awaitPending(ctx context.Context, tx pgx.Tx, key string) error {
	root, err := e.store.Issue(ctx, tx, key)
	if err != nil || root == nil || root.Phase != phase.Admitted || !claim.IsTreeRoot(root.Key, root.Tree) {
		return err
	}
	pending, err := e.livePendingWaits(ctx, tx, *root)
	if err != nil || len(pending) == 0 {
		return err
	}
	wait := pending[0]
	reason := fmt.Sprintf("ask %s is still with a person", wait.Ask)
	if wait.GateVersion != 0 {
		reason = fmt.Sprintf("spec version %d is still awaiting a person's review", wait.GateVersion)
	}
	return e.awaitPerson(ctx, tx, *root, wait.Awaiting(), reason)
}

// dispatchAskClosed applies a close of an ask. A person's answer or resolution is a person's act
// (personActed). A close by anyone else — a session, or Dispatch's settlement retracting an ask
// whose decision block left the spec — answers nothing in general; but when it closes the very ask
// a root waits on, it ends that wait (closedAwaitedAsk): the question is gone, no person can answer
// it, and the root would otherwise wait for good. A review (a person's answer to an approval
// request) answers a wait on the design gate through its own artifact.approved or
// artifact.changes_requested event (applyArtifact), which Dispatch appends after the answer: woken
// by the answer, the root could resume before the review reached its gate.
func (e *Engine) dispatchAskClosed(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskClosed) (intake.Result, error) {
	if fact.Actor.Person() {
		review := fact.Kind == "approval" && fact.State == "answered"
		return intake.Result{}, e.personActed(ctx, tx, fact.Key, fact.Ask, !review, fact.Mark, record.AnsweredByAsk)
	}
	return intake.Result{}, e.closedAwaitedAsk(ctx, tx, fact)
}

// closedAwaitedAsk ends a root's wait when the ask it waits on is closed by anyone but a person:
// a session or Dispatch's settlement. Nobody will answer a closed question: a person's reply in its
// thread is the one act left, and whoever closed it has moved on, so the root takes a slot back
// rather than waiting for good. The resume task says who closed it — Dispatch's settlement, a
// session of the root's own tree, or someone outside it. A close of another ask, or of an ask of a
// root waiting on its design gate, ends no wait: the gate's own review still answers that wait.
// Every close of a root's ask is recorded (record.PersonAct on that ask alone, by no person), so
// the ask.opened of a question already closed, delivered after it, starts no wait, and a pending
// wait on it is dropped.
func (e *Engine) closedAwaitedAsk(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskClosed) error {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil {
		return err
	}
	if claim.IsTreeRoot(issue.Key, issue.Tree) && fact.Mark.Seq > 0 {
		if err := e.store.RecordPersonAct(ctx, tx, record.PersonAct{Issue: fact.Key, Ask: fact.Ask, Mark: fact.Mark}); err != nil {
			return err
		}
	}
	if !issue.AwaitingPerson() || issue.Awaiting.AnsweredAt != nil || issue.Awaiting.Ask != fact.Ask {
		return nil
	}
	by, closer := record.AnsweredByClosed, "someone outside your tree"
	switch {
	case fact.Actor == dispatch.SettlementActor:
		by, closer = record.AnsweredByRetraction, ""
	case fact.Actor.Kind == "session":
		inTree, err := e.store.SessionClaimsTree(ctx, tx, issue.Tree, fact.Actor.ID)
		if err != nil {
			return err
		}
		if inTree {
			closer = "a session of your tree"
		}
	}
	e.logOnCommit(ctx, "workflow: the ask a root waits on was closed, so nobody will answer it", "issue", issue.Key, "ask", fact.Ask,
		"closed_by", fmt.Sprintf("%s:%s", fact.Actor.Kind, fact.Actor.ID))
	return e.answerWait(ctx, tx, *issue, by, closer)
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
// the daemon after it starts none (awaitPerson). It answers the root's wait: the architect is the
// tree's only agent while it waits, no ask of the root is anyone else's, and the approval
// request's id is not recorded, so a person's act on any of the root's asks counts — including an
// act on another question, which the architect may have asked in place of the one the wait names,
// and a person's answer of the approval request while the root waits on a question, which no gate
// may carry on. Only on a gate wait does an act that answers no gate (answersGate false: a person's
// answer of the approval request, whose artifact.* event answers the gate after it) count solely
// on the awaited ask itself. The act answers only a wait it follows: one Dispatch recorded at or
// before the wait's start (record.EventMark.After against Awaiting.Since) is an answer the person
// gave before the architect handed the work back to them, applied late, and the root waits on
// their next act, as a changes request given before the wait does (answerAwaitedGate).
func (e *Engine) personActed(ctx context.Context, tx pgx.Tx, key, ask string, answersGate bool, mark record.EventMark, onAsk record.Answer) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return err
	}
	if claim.IsTreeRoot(issue.Key, issue.Tree) && mark.Seq > 0 {
		act := record.PersonAct{Issue: key, Ask: ask, Mark: mark, AnswersGate: answersGate, ByPerson: true}
		if err := e.store.RecordPersonAct(ctx, tx, act); err != nil {
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
	case answersGate || issue.Awaiting.GateVersion == 0:
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
// with (resumeTask). The detail is the act's own: the ask a person acted on, the version and
// reason of the review that answered, or who closed the ask, so a later review of the same version
// does not rename it. A question wait answered by something other than an act on that question —
// a person's act on another ask, a review of the spec — leaves the question with the person, so it
// is held pending and the resumed architect is told so.
func (e *Engine) answerWait(ctx context.Context, tx pgx.Tx, root record.Issue, by record.Answer, detail string) error {
	waiting := *root.Awaiting
	at := e.now()
	root.Awaiting.AnsweredAt, root.Awaiting.AnsweredBy, root.Awaiting.AnsweredDetail = &at, by, detail
	if err := e.store.PutIssue(ctx, tx, root); err != nil {
		return err
	}
	switch {
	case waiting.Ask == "":
		return nil
	case by == record.AnsweredByAsk, by == record.AnsweredByRetraction, by == record.AnsweredByClosed:
		return nil
	}
	return e.holdPendingWait(ctx, tx, root, record.Awaiting{Ask: waiting.Ask, Since: waiting.Since})
}

// actedSince names the act the root already holds that settles waiting, a wait about to start or
// held pending, and that Dispatch recorded after the wait's start, or returns "" when there is
// none. Such an act reached the daemon before the event that starts the wait (Dispatch's intake
// retries a delivery, and the daemon naks a failed fact while its consumer moves on), and a wait
// started now would wait on what has already happened, with nothing left to answer it. For a wait
// on a question it is an act on that ask: a person's answer, resolution or reply, or a close by
// anyone. For a wait on the design gate it is the review the gate applied last
// (classify.LatestReview) at the version waited on or later, a newer version of the gate's
// document, the gate's opening, or a gate registered on another document; for a gate wait about to
// start (pending false) also a person's act that answers the gate (record.PersonAct.AnswersGate),
// which the running architect has. A gate wait held pending is not settled by such an act: the
// approval request's id is not recorded, so a person's answer of some other question cannot be
// told from a reply on the request, and the resumed architect is told the request is still with
// the person until the gate itself moves. A person's act on another ask settles no question: the
// running architect has that act, and the question is still with the person. Only a mark placed by
// sequence counts (record.EventMark.Follows): every wait starts at a sequenced event or read, and
// every act the daemon records carries its event's sequence.
func (e *Engine) actedSince(ctx context.Context, tx pgx.Tx, root record.Issue, waiting record.Awaiting, pending bool) (string, error) {
	acts, err := e.store.PersonActs(ctx, tx, root.Key)
	if err != nil {
		return "", err
	}
	for _, act := range acts {
		if !act.Mark.Follows(waiting.Since) {
			continue
		}
		switch {
		case waiting.Ask != "" && act.Ask == waiting.Ask:
			return fmt.Sprintf("ask %s was answered or closed at sequence %d", act.Ask, act.Mark.Seq), nil
		case waiting.GateVersion != 0 && !pending && act.ByPerson && act.AnswersGate:
			return fmt.Sprintf("a person acted on ask %s at sequence %d", act.Ask, act.Mark.Seq), nil
		}
	}
	if waiting.GateVersion == 0 {
		return "", nil
	}
	gate, err := e.store.Gate(ctx, tx, root.Key)
	if err != nil || gate == nil {
		return "", err
	}
	switch {
	case waiting.Artifact != "" && gate.ArtifactID != waiting.Artifact:
		return fmt.Sprintf("the gate now names document %s", gate.ArtifactID), nil
	case gate.LatestVersion > waiting.GateVersion:
		return fmt.Sprintf("the spec moved on to version %d", gate.LatestVersion), nil
	case classify.DesignGateOpen(*gate):
		return fmt.Sprintf("the gate is open at version %d", gate.LatestVersion), nil
	}
	if mark, version, ok := classify.LatestReview(*gate); ok && version >= waiting.GateVersion && mark.Follows(waiting.Since) {
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
// slot, and the wait is held pending until the guard clears. A hand-back that arrives while the
// root already waits is held pending too (coveredByTheWaitItRuns).
func (e *Engine) handedBack(ctx context.Context, tx pgx.Tx, fact intake.DispatchAskReply) error {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil || e.waitFenced(ctx, *issue, fact.Mark, "hand-back") {
		return err
	}
	if issue.Phase != phase.Admitted && !issue.AwaitingPerson() {
		return nil
	}
	ownsTree, err := e.store.SessionClaimsTree(ctx, tx, issue.Tree, fact.Actor.ID)
	if err != nil || !ownsTree {
		return err
	}
	gate, err := e.store.Gate(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	waiting, reason := record.Awaiting{Ask: fact.Ask, Since: fact.Mark}, fmt.Sprintf("ask %s was handed back to a person", fact.Ask)
	if gate != nil && !classify.DesignGateOpen(*gate) {
		if e.gateFenced(ctx, *issue, fact.Mark, "hand-back") {
			return nil
		}
		waiting = record.Awaiting{GateVersion: gate.LatestVersion, Artifact: gate.ArtifactID, Since: fact.Mark}
		reason = fmt.Sprintf("spec version %d is awaiting a person's review", gate.LatestVersion)
	}
	if issue.AwaitingPerson() {
		return e.coveredByTheWaitItRuns(ctx, tx, *issue, waiting, "hand-back")
	}
	return e.awaitPerson(ctx, tx, *issue, waiting, reason)
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
// records it, carries the claim Dispatch shows then (intake.DispatchIssue.ClaimedBy). A claim that
// moves on an admitted root may clear the guard that kept its slot while it waited on a person,
// so the wait it holds pending starts then (awaitPending).
func (e *Engine) dispatchClaim(ctx context.Context, tx pgx.Tx, fact intake.DispatchClaim) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil || !issue.ObserveClaim(fact.ClaimedBy, fact.Seq) {
		return intake.Result{}, err
	}
	if err := e.store.PutIssue(ctx, tx, *issue); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.awaitPending(ctx, tx, issue.Key)
}
