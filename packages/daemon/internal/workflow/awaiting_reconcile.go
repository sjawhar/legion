package workflow

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ReconcileAwaiting moves each admitted root of project that holds a slot and now waits only on a
// person into awaiting_person, or, when a person already acted after its wait would begin, gives
// its architect the resume task where it stands. It also answers each root waiting on a person
// whose design gate is open (answerOpenGates). It reads only the record, so the same record gives
// the same decision, and admission calls it on every pass (Admission.promoteHolds): a guard that
// clears on any path — a member leaving, a claim released, the label put back, the architect's
// start taken — is seen at the next fact. issues and slots are the caller's snapshot of the
// record, and held names each root the caller still holds for the Dispatch consumer to catch up
// (nil holds none): a root whose listing is ahead of the record decides nothing until the consumer
// has reached it, so a question already answered on Dispatch does not start a wait here first. It
// returns the roots that entered awaiting_person, whose slots the caller releases, and whether it
// wrote any issue of the snapshot, which the caller then reads again.
func (e *Engine) ReconcileAwaiting(ctx context.Context, tx pgx.Tx, project string, issues []record.Issue, slots []record.Slot, held func(string) bool) ([]string, bool, error) {
	changed, err := e.answerOpenGates(ctx, tx, project, issues, held)
	if err != nil {
		return nil, false, err
	}
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	var roots []record.Issue
	for _, issue := range issues {
		if held != nil && held(issue.Key) {
			continue
		}
		if _, ok := slotted[issue.Key]; ok && issue.Project == project && claim.IsTreeRoot(issue.Key, issue.Tree) &&
			issue.Phase == phase.Admitted && issue.DeclaredSeq != 0 {
			roots = append(roots, issue)
		}
	}
	if len(roots) == 0 {
		return nil, changed, nil
	}
	token, err := claim.ProjectToken(project)
	if err != nil {
		return nil, false, err
	}
	// A root whose key yields no claim token has no architect claim to read (record.RoleRun's
	// zero value), so it owes its start and keeps its slot.
	keys := make([]string, len(roots))
	tokens := make([]claim.Token, len(roots))
	var runKeys []record.RoleRunKey
	for i, root := range roots {
		keys[i] = root.Key
		architect, err := claim.NewToken(token, root.Key, claim.RoleArchitect)
		if err != nil {
			continue
		}
		tokens[i] = architect
		runKeys = append(runKeys, record.RoleRunKey{Token: architect, Issue: root.Key, Role: claim.RoleArchitect, Generation: root.Generation})
	}
	asks, err := e.store.RootAsks(ctx, tx, keys)
	if err != nil {
		return nil, false, err
	}
	runs, err := e.store.RoleRuns(ctx, tx, runKeys)
	if err != nil {
		return nil, false, err
	}
	var entered []string
	for i, root := range roots {
		unresumable, err := Unresumable(ctx, e.store, tx, root)
		if err != nil {
			return nil, false, err
		}
		gateArtifact, err := e.gateArtifact(ctx, tx, root)
		if err != nil {
			return nil, false, err
		}
		view := awaitView{Root: root, Slotted: true, Unresumable: unresumable, Run: runs[tokens[i]], Asks: asks[root.Key], GateArtifact: gateArtifact}
		for _, member := range issues {
			if member.Tree == root.Key && member.Key != root.Key {
				view.Members = append(view.Members, member)
			}
		}
		decision := decideAwait(view)
		switch decision.Act {
		case actEnter:
			if err := e.enterAwaiting(ctx, tx, root, decision.Waiting, decision.Why); err != nil {
				return nil, false, err
			}
			entered = append(entered, root.Key)
			changed = true
		case actTellInPlace:
			if err := e.tellInPlace(ctx, tx, root, decision); err != nil {
				return nil, false, err
			}
			changed = true
		case actTakeIn:
			if err := e.store.ClearDeclared(ctx, tx, root.Key); err != nil {
				return nil, false, err
			}
			e.logOnCommit(ctx, "workflow: what a root handed back is no longer with the person, so it starts no wait", "issue", root.Key,
				"declared_seq", root.DeclaredSeq)
			changed = true
		default:
			intake.OnCommit(ctx, func() {
				e.log.Debug("workflow: a root that handed work to a person keeps its slot", "issue", root.Key, "why", decision.Why)
			})
		}
	}
	return entered, changed, nil
}

// answerOpenGates answers each unanswered root of project waiting on a person whose design gate is
// open, whatever opened it and whenever: a person's approval Dispatch recorded before the wait
// began but applied after it, a registration's read of an approval, or a registration under
// gates.design: off. The gate's opening advances no tree while the root waits (advanceAdmittedTree),
// and the person has nothing left to decide that holds the tree, so the root resumes with the next
// free slot, told how its spec stands, and its tree advances then. It reports whether it answered
// any root.
func (e *Engine) answerOpenGates(ctx context.Context, tx pgx.Tx, project string, issues []record.Issue, held func(string) bool) (bool, error) {
	answered := false
	for _, root := range issues {
		if (held != nil && held(root.Key)) || root.Project != project || !claim.IsTreeRoot(root.Key, root.Tree) ||
			!root.AwaitingPerson() || root.Awaiting.AnsweredAt != nil {
			continue
		}
		gate, err := e.store.Gate(ctx, tx, root.Key)
		if err != nil {
			return answered, err
		}
		if gate == nil || !classify.DesignGateOpen(*gate) {
			continue
		}
		e.logOnCommit(ctx, "workflow: the design gate of a root waiting on a person is open, which answers its wait", "issue", root.Key,
			"version", gate.LatestVersion, "ask", root.Awaiting.Ask)
		if err := e.answerWait(ctx, tx, root, record.AnsweredByApproval, fmt.Sprintf("version %d", gate.LatestVersion)); err != nil {
			return answered, err
		}
		answered = true
	}
	return answered, nil
}

// act is what ReconcileAwaiting does with one root.
type act int

const (
	// actStay leaves the root as it is.
	actStay act = iota
	// actEnter moves the root into awaiting_person.
	actEnter
	// actTellInPlace resumes the root's architect where it stands, its slot kept.
	actTellInPlace
	// actTakeIn clears what the root handed back (Store.ClearDeclared): none of it is still with
	// the person, and the running architect has the act that took it away, so no later change of
	// the record may start a wait from that hand-back.
	actTakeIn
)

// awaitView is what decideAwait reads of one root: its record, whether it holds a slot, the other
// members of its tree, why it could not resume (Unresumable, "" when it could), its architect's run
// at the root's generation, its asks' mirror rows, and the document its design gate is registered
// on (GateArtifact, "" for no gate), whose approval request's answer is a review rather than a
// person's act on an ask (RootAsk.PersonActSeq).
type awaitView struct {
	Root         record.Issue
	Slotted      bool
	Members      []record.Issue
	Unresumable  string
	Run          record.RoleRun
	Asks         []record.RootAsk
	GateArtifact string
}

// awaitDecision is decideAwait's answer: the act, why, the wait the root enters or is told of
// (Waiting), and for actTellInPlace the act that already answered it (Answer, Detail).
type awaitDecision struct {
	Act     act
	Why     string
	Waiting record.Awaiting
	Answer  record.Answer
	Detail  string
}

// decideAwait is the rule for whether a root waits on a person, checked cheapest first:
//
//  0. a root not at admitted, lingering, out of the workflow, or holding no slot stays;
//  1. one whose architect handed nothing back since it was last given work (DeclaredSeq) stays;
//  2. one with no question still with the person (withHuman) takes in what it handed back
//     (actTakeIn): that was answered or closed first, and the running architect has that act;
//  3. one whose tree has a member in a phase stays, since that member's work needs the slot;
//  4. one that could not resume (Unresumable) stays, since released, nothing could resume it;
//  5. one whose architect still owes the work it was given — a start queued, a claim still
//     starting, a task not yet taken (owed) — stays: a suspension then would cancel that work;
//  6. otherwise the wait begins at the newest hand-back (DeclaredSeq), on the oldest question
//     still with the person. When a person's act Dispatch recorded after that start is already in
//     the record (answeredSince), the architect is told of it where it stands (actTellInPlace);
//     otherwise the root enters awaiting_person (actEnter).
func decideAwait(v awaitView) awaitDecision {
	root := v.Root
	switch {
	case root.Phase != phase.Admitted || !claim.IsTreeRoot(root.Key, root.Tree) || root.Lingers() || !InWorkflow(root) || !v.Slotted:
		return awaitDecision{Why: "it is not an admitted root holding its slot"}
	case root.DeclaredSeq == 0:
		return awaitDecision{Why: "its architect has handed nothing to a person since it was last given work"}
	}
	since := record.EventMark{Seq: root.DeclaredSeq, At: root.DeclaredAt}
	waiting, ok := waitSubject(v.Asks, since)
	if !ok {
		return awaitDecision{Act: actTakeIn, Why: "nothing it handed back is still with the person"}
	}
	for _, member := range v.Members {
		if InWorkflow(member) && member.Phase != phase.Admitted {
			return awaitDecision{Why: fmt.Sprintf("%s of its tree is in %s", member.Key, member.Phase)}
		}
	}
	if v.Unresumable != "" {
		return awaitDecision{Why: "it could not resume: " + v.Unresumable}
	}
	if why, ok := owed(v.Run); ok {
		return awaitDecision{Why: "its architect " + why}
	}
	if answer, detail, ok := answeredSince(waiting, v.Asks, v.GateArtifact); ok {
		return awaitDecision{Act: actTellInPlace, Why: "a person already acted after its hand-back", Waiting: waiting, Answer: answer, Detail: detail}
	}
	return awaitDecision{Act: actEnter, Why: fmt.Sprintf("question %s is awaiting an answer", waiting.Ask), Waiting: waiting}
}

// withHuman is what of a root is still with the person: its tree's open questions that wait on the
// person, the one on the person's turn longest first (the turn's own place, WaitingOnSeq; an open
// ask no event gave a turn is the person's since it opened, so it sorts first). A root never waits
// on its gate's approval request here, so that request keeps the root's slot as it did before this
// release.
func withHuman(asks []record.RootAsk) []record.RootAsk {
	var questions []record.RootAsk
	for _, ask := range asks {
		if ask.ByTree && ask.Question() && ask.WithHuman() {
			questions = append(questions, ask)
		}
	}
	slices.SortFunc(questions, func(a, b record.RootAsk) int {
		return cmp.Or(cmp.Compare(a.WaitingOnSeq, b.WaitingOnSeq), cmp.Compare(a.Seq, b.Seq), cmp.Compare(a.Ask, b.Ask))
	})
	return questions
}

// waitSubject is the wait a root starts from since on what is still with the person: the oldest
// question of its tree; false when none is.
func waitSubject(asks []record.RootAsk, since record.EventMark) (record.Awaiting, bool) {
	questions := withHuman(asks)
	if len(questions) == 0 {
		return record.Awaiting{}, false
	}
	return record.Awaiting{Ask: questions[0].Ask, Since: since}, true
}

// owed says whether the architect has not yet taken the work it was last given, and how: a start
// still queued for it, no claim yet, a claim still starting, or a task it has not confirmed.
func owed(run record.RoleRun) (string, bool) {
	for _, queued := range run.Queued {
		if queued.Request.Op == "start" {
			return "has a start still queued", true
		}
	}
	held := run.Claim
	switch {
	case held == nil:
		return "has no claim yet", true
	case slices.Contains([]supervise.ClaimState{supervise.StateQueued, supervise.StateLaunchUncertain, supervise.StateLaunching,
		supervise.StateShimConnected, supervise.StateRegistered}, held.State):
		return fmt.Sprintf("is still starting (%s)", held.State), true
	case held.Pending != nil && held.Pending.ConfirmedAt.IsZero():
		return "has not taken its task yet", true
	}
	return "", false
}

// answeredSince names a person's act the record already holds that Dispatch recorded after waiting
// began: their act on the subject question, or on any other ask of the root the mirror recorded one
// on (record.RootAsk.PersonActSeq against the gate's document, read from the acts the ask handlers
// record under one rule, personAct). A review of the root's spec is not read here: the architect it
// would resume is running, and the daemon's own design-changes-requested notice is the work that
// reaches it (a handout of its own).
func answeredSince(waiting record.Awaiting, asks []record.RootAsk, gateArtifact string) (record.Answer, string, bool) {
	since := waiting.Since.Seq
	for _, ask := range asks {
		if ask.Ask == waiting.Ask && ask.PersonActSeq(gateArtifact) > since {
			return record.AnsweredByAsk, ask.Ask, true
		}
	}
	for _, ask := range asks {
		if ask.PersonActSeq(gateArtifact) > since {
			return record.AnsweredByReply, ask.Ask, true
		}
	}
	return "", "", false
}

// enterAwaiting moves root into awaiting_person on waiting: admission releases its slot in the same
// intake transaction and fills it from Waiting before the fact commits. The architect's daemon
// suspension waits for its current turn to end, keeping the session for the answer's resume, and
// the controller is told.
func (e *Engine) enterAwaiting(ctx context.Context, tx pgx.Tx, root record.Issue, waiting record.Awaiting, reason string) error {
	root.Phase, root.Awaiting = phase.AwaitingPerson, &waiting
	if err := e.store.PutIssue(ctx, tx, root); err != nil {
		return err
	}
	suspendReason := fmt.Sprintf("%s waits on a person (%s): its slot is released until they answer", root.Key, reason)
	if err := e.suspend(ctx, tx, root, claim.RoleArchitect, suspendReason); err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, root.Key, record.ControllerNotice{Kind: record.AwaitingPersonNotice, Reason: suspendReason}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: root waits on a person and returns its admission slot", "issue", root.Key, "tree", root.Tree,
		"reason", reason, "since_seq", waiting.Since.Seq)
	return nil
}

// tellInPlace gives the architect of a root it would have released the resume task naming the act
// that already answered its hand-back: the root keeps its slot and its phase.
func (e *Engine) tellInPlace(ctx context.Context, tx pgx.Tx, root record.Issue, decision awaitDecision) error {
	waiting := decision.Waiting
	at := e.now()
	waiting.AnsweredAt, waiting.AnsweredBy, waiting.AnsweredDetail = &at, decision.Answer, decision.Detail
	e.logOnCommit(ctx, "workflow: a person already acted on what the root handed back; its architect is told where it stands", "issue", root.Key,
		"answered_by", decision.Answer, "detail", decision.Detail, "since_seq", waiting.Since.Seq)
	return e.ResumeAwaitedRoot(ctx, tx, root, waiting)
}

// ResumeAwaitedRoot starts an architect in the session its daemon suspension kept (or, told in
// place, the one it runs), told what answered its wait, how its spec stands now, and what of the
// root is still with the person or waits on it. It is a handout (Store.StartHandout): what the
// architect handed back before it is answered or listed here, and only a hand-back after it starts
// the next wait. When the gate is open it advances the now-admitted tree only after the root has
// its slot again.
func (e *Engine) ResumeAwaitedRoot(ctx context.Context, tx pgx.Tx, root record.Issue, waiting record.Awaiting) error {
	gate, err := e.store.Gate(ctx, tx, root.Key)
	if err != nil {
		return err
	}
	asks, err := e.store.RootAsks(ctx, tx, []string{root.Key})
	if err != nil {
		return err
	}
	if err := e.store.StartHandout(ctx, tx, root.Key); err != nil {
		return err
	}
	task := resumeTask(root.Key, waiting, gate, asks[root.Key], e.armed())
	if err := e.enqueue(ctx, tx, root.Key, record.SuperviseRequest{
		Op: "start", Tree: root.Tree, Role: claim.RoleArchitect, Generation: root.Generation, Task: task,
	}); err != nil {
		return err
	}
	if gate != nil {
		return e.advanceAdmittedTree(ctx, tx, root, *gate)
	}
	return nil
}

// resumeTask is the task an answered root's architect resumes with: what answered its wait
// (Awaiting.AnsweredBy) and that act's own detail (Awaiting.AnsweredDetail, recorded when the wait
// was answered, so a later act of the same kind does not rename it); how the spec stands at the
// resume, read from the gate now, so an approval a newer version superseded, or a gate closed
// again since, is said as it is; what is still with the person (withHuman) and what waits on the
// architect (a tree question on the agent's turn). Handing back is advice only while the gate is
// not open, since an open gate moves the root to planning, where a hand-back starts no wait. armed
// says whether the design gate is armed: with it off, no approval is asked for and register_gate is
// never the advice.
func resumeTask(key string, waiting record.Awaiting, gate *record.DesignGate, asks []record.RootAsk, armed bool) string {
	var task strings.Builder
	switch waiting.AnsweredBy {
	case record.AnsweredByApproval:
		if armed {
			fmt.Fprintf(&task, "Your spec %s on %s was approved.", waiting.AnsweredDetail, key)
		} else {
			fmt.Fprintf(&task, "Your spec %s on %s is registered, and the design gate is off, so it needs no approval.", waiting.AnsweredDetail, key)
		}
	case record.AnsweredByChanges:
		fmt.Fprintf(&task, "A person requested changes to your spec on %s at %s. Read the review, and revise the spec as they ask.", key, waiting.AnsweredDetail)
	case record.AnsweredByRetraction:
		fmt.Fprintf(&task, "Your ask %s on %s was retracted when its decision block left the spec, so no person will answer it. "+
			"Read the spec as it stands; if you still need the decision, ask again.", waiting.Ask, key)
	case record.AnsweredByClosed:
		fmt.Fprintf(&task, "Your ask %s on %s was closed by %s, so no person will answer it. Read its thread; if you still need the decision, ask again.",
			waiting.Ask, key, waiting.AnsweredDetail)
	case record.AnsweredByAsk:
		fmt.Fprintf(&task, "A person answered your ask %s on %s, or replied in its thread; read it.", waiting.Ask, key)
	case record.AnsweredByReply:
		fmt.Fprintf(&task, "A person answered or replied in the thread of ask %s on %s; read that thread.", waiting.AnsweredDetail, key)
	case record.AnsweredByTurn:
		fmt.Fprintf(&task, "A session's reply handed your question %s on %s to your turn, so no person is asked anything there; read its thread.",
			waiting.AnsweredDetail, key)
	default:
		fmt.Fprintf(&task, "A person acted on %s; read the issue's asks and its spec.", key)
	}
	gateOpen := gate != nil && classify.DesignGateOpen(*gate)
	gateClosed := gate != nil && !gateOpen
	switch {
	case gate == nil:
	case gateOpen:
		if waiting.AnsweredBy != record.AnsweredByApproval || waiting.AnsweredDetail != fmt.Sprintf("version %d", gate.LatestVersion) {
			fmt.Fprintf(&task, " Your spec is approved at version %d.", gate.LatestVersion)
		}
	case waiting.AnsweredBy == record.AnsweredByApproval:
		fmt.Fprintf(&task, " That approval no longer stands: the spec is at version %d and is not approved.", gate.LatestVersion)
	default:
		fmt.Fprintf(&task, " Your spec is not approved at version %d.", gate.LatestVersion)
	}
	var still, yours []string
	for _, ask := range withHuman(asks) {
		still = append(still, "ask "+ask.Ask)
	}
	for _, ask := range asks {
		if ask.Open && ask.WaitingOn == "agent" && ask.ByTree && ask.Question() {
			yours = append(yours, "ask "+ask.Ask)
		}
	}
	if len(yours) > 0 {
		fmt.Fprintf(&task, " Waiting on you: %s; read each thread and reply.", strings.Join(yours, ", "))
	}
	switch {
	case len(still) > 0 && !gateOpen:
		fmt.Fprintf(&task, " Still with the person: %s. Hand each one back once you have done what the answer asks — a reply in its thread that "+
			"leaves the turn with the person — or close it if you no longer need it. Handing one back starts your next wait and returns the slot; "+
			"then end your turn.", strings.Join(still, ", "))
	case len(still) > 0:
		fmt.Fprintf(&task, " Still with the person: %s. Continue.", strings.Join(still, ", "))
	case gateClosed && armed:
		task.WriteString(" When the spec is ready, request approval and register that version with register_gate; then end your turn.")
	default:
		task.WriteString(" Continue.")
	}
	return task.String()
}
