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

// ReconcileAwaiting moves each admitted root of project that now waits only on a person into
// awaiting_person, or, when a person already acted after its wait would begin, gives its
// architect the resume task where it stands. It also answers each root waiting on a person whose
// design gate is open (answerOpenGates). It reads only the record, so the same record gives the
// same decision, and admission calls it on every pass (Admission.promoteHolds): a guard that
// clears on any path — a member leaving, a claim released, the label put back, the architect's
// start taken — is seen at the next fact. issues is the caller's snapshot of the record, and
// held names each root the caller still holds for the Dispatch consumer to catch up (nil holds
// none): a root whose listing is ahead of the record decides nothing until the consumer has
// reached it, so a question already answered on Dispatch does not start a wait here first. It
// returns the roots that entered awaiting_person, whose slots the caller releases, and whether
// it wrote any issue of the snapshot, which the caller then reads again.
//
// It reads the slot rows once for the pass, because the wait is the root's slot going back: a
// root enters it only while it holds a slot for work of its own (record.CanWaitOnPerson,
// decideAwait's rule 0), which for an admitted root is the architect writing the spec, before
// the design gate first opens. That is the spec's own condition, and the boot's catch-up reads
// the roots this pass can move (the daemon's catchesUp) by that same predicate.
func (e *Engine) ReconcileAwaiting(ctx context.Context, tx pgx.Tx, project string, issues []record.Issue, held func(string) bool) ([]string, bool, error) {
	changed, err := e.answerOpenGates(ctx, tx, project, issues, held)
	if err != nil {
		return nil, false, err
	}
	var roots []record.Issue
	for _, issue := range issues {
		if held != nil && held(issue.Key) {
			continue
		}
		if issue.Project == project && claim.IsTreeRoot(issue.Key, issue.Tree) &&
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
	slots, err := e.store.Slots(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	var entered []string
	for i, root := range roots {
		gates, err := e.store.GatesOf(ctx, tx, root.Key)
		if err != nil {
			return nil, false, err
		}
		unresumable, err := Unresumable(ctx, e.store, tx, root)
		if err != nil {
			return nil, false, err
		}
		_, holds := slotted[root.Key]
		view := awaitView{Root: root, Slotted: holds, Unresumable: unresumable, Run: runs[tokens[i]], Asks: asks[root.Key],
			Steps: e.cfg.Steps, Gates: gates}
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

// answerOpenGates answers each unanswered root of project waiting on a person whose design gate
// is open, whatever opened it and whenever: a person's approval Dispatch recorded before the wait
// began but applied after it, a registration's read of an approval, or a registration of a step
// the deployment does not gate. The gate it reads is the one the wait's own document names, since
// that is the step the person was asked about; a wait on a question names no document, and every
// required step being open answers it, as the one gate's opening did. The gate's opening starts
// no phase of the tree while the root waits, since nothing of a waiting root's tree takes an
// admission slot, and the person has nothing left to decide that holds the tree, so the root
// resumes with the next free slot, told how its spec stands, and its tree advances then. A wait
// on a gate under a deployment that gates no step (the operator emptied the list while the root
// waited on it) whose gate is not open is decided in the same pass, since no approval request
// counts then (gateRequestArtifact): the wait moves to a question of the tree still with the
// person, and is otherwise answered, so the architect resumes and registers its spec, which
// opens the gate. It reports whether it answered or moved any root's wait.
func (e *Engine) answerOpenGates(ctx context.Context, tx pgx.Tx, project string, issues []record.Issue, held func(string) bool) (bool, error) {
	answered := false
	for _, root := range issues {
		if (held != nil && held(root.Key)) || root.Project != project || !claim.IsTreeRoot(root.Key, root.Tree) ||
			!root.AwaitingPerson() || root.Awaiting.AnsweredAt != nil {
			continue
		}
		gates, err := e.store.GatesOf(ctx, tx, root.Key)
		if err != nil {
			return answered, err
		}
		if waitGateOpen(e.cfg.Steps, gates, root.Awaiting.Artifact) {
			version := root.Awaiting.GateVersion
			if gate := gateOfDocument(gates, root.Awaiting.Artifact); gate != nil {
				version = gate.LatestVersion
			}
			e.logOnCommit(ctx, "workflow: the design gate of a root waiting on a person is open, which answers its wait", "issue", root.Key,
				"version", version, "ask", root.Awaiting.Ask)
			if err := e.answerWait(ctx, tx, root, record.AnsweredByApproval, fmt.Sprintf("version %d", version)); err != nil {
				return answered, err
			}
			answered = true
			continue
		}
		if root.Awaiting.GateVersion != 0 && !e.armed() {
			if err := e.leaveGateOff(ctx, tx, root, gates); err != nil {
				return answered, err
			}
			answered = true
		}
	}
	return answered, nil
}

// leaveGateOff decides a wait on root's design gate, which is not open, once the deployment gates
// no step: the wait moves to the oldest question of the tree still with the person, keeping where
// it began, and is otherwise answered (AnsweredByApproval, at the version it waited on, its
// detail marked gateOffAnswer), so the architect resumes told that the gate opens, with no
// approval, when it registers its spec, or, should the gate be on again by then, that no person
// approved it (resumeTask).
func (e *Engine) leaveGateOff(ctx context.Context, tx pgx.Tx, root record.Issue, gates map[record.DesignStep]record.DesignGate) error {
	asks, err := e.store.RootAsks(ctx, tx, []string{root.Key})
	if err != nil {
		return err
	}
	// The empty list is passed as the configured one, so no approval request is still with the
	// person and the wait can only move to a question (gateRequestArtifact).
	if next, ok := waitSubject(asks[root.Key], nil, gates, root.Awaiting.Since); ok {
		e.logOnCommit(ctx, "workflow: the deployment gates no step, so a root's wait on its gate moves to what is still with the person",
			"issue", root.Key, "version", root.Awaiting.GateVersion, "next_ask", next.Ask)
		root.Awaiting.Ask, root.Awaiting.GateVersion, root.Awaiting.Artifact = next.Ask, next.GateVersion, next.Artifact
		return e.store.PutIssue(ctx, tx, root)
	}
	e.logOnCommit(ctx, "workflow: the deployment gates no step, which answers a root's wait on its gate", "issue", root.Key,
		"version", root.Awaiting.GateVersion)
	return e.answerWait(ctx, tx, root, record.AnsweredByApproval, fmt.Sprintf("version %d", root.Awaiting.GateVersion)+gateOffAnswer)
}

// gateOffAnswer ends the detail of a gate wait the empty-list rule answered (leaveGateOff).
// That answer is recorded, and shown on the API, as AnsweredByApproval: the contract's answeredBy
// enum, which refuses a value it does not list, has no value of its own for it. But no person
// approved anything, so the detail tells the resume (resumeTask) never to say so, should the gate
// be on again by then.
const gateOffAnswer = ", answered with the design gate off"

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

// awaitView is what decideAwait reads of one root: its record, whether it holds an admission slot
// (Slotted, read from the slot rows by ReconcileAwaiting), the other members of its tree, why it
// could not resume (Unresumable, "" when it could), its architect's run at the root's
// generation, its asks' mirror rows, the design-gate steps the deployment gates (Steps) and the
// gates the root holds by step (Gates). A gate's document names its approval requests — every
// approval row of that document — and their answer is a review rather than a person's act on an
// ask (RootAsk.PersonActSeq).
type awaitView struct {
	Root        record.Issue
	Slotted     bool
	Members     []record.Issue
	Unresumable string
	Run         record.RoleRun
	Asks        []record.RootAsk
	Steps       []record.DesignStep
	Gates       map[record.DesignStep]record.DesignGate
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
//  0. a root not at admitted, lingering, out of the workflow, or holding no admission slot for
//     work of its own (record.CanWaitOnPerson) stays;
//  1. one whose architect handed nothing back since it was last given work (DeclaredSeq) stays;
//  2. one with nothing still with the person (withHuman) takes in what it handed back
//     (actTakeIn): that was answered or closed first, and the running architect has that act;
//  3. one whose tree has a member in a phase stays, since that member's work needs the slot;
//  4. one that could not resume (Unresumable) stays, since released, nothing could resume it;
//  5. one whose architect still owes the work it was given — a start queued, a claim still
//     starting, a task not yet taken (owed) — stays: the wait begins only after that task is taken;
//  6. otherwise the wait begins at the newest hand-back (DeclaredSeq), on the design gate when its
//     approval request is with the person, else on the oldest question that is. When a person's
//     act Dispatch recorded after that start is already in the record (answeredSince), the
//     architect is told of it where it stands (actTellInPlace); otherwise the root enters
//     awaiting_person (actEnter).
//
// Rule 0 is the spec's own condition (dispatch://INFRA-980, "Which issue needs a slot"): a root
// enters awaiting_person only while it holds a slot, since the wait is that slot going back, and
// it is read as the root's own slot row (awaitView.Slotted). A root still waiting in line for
// its first slot holds none, so it starts no wait however its architect's hand-back stands —
// a re-admitted root the old architect declared on again before the linger's suspend ran is
// kept out by this rule, not by rule 1. The boot's catch-up of the roots this pass can move
// calls the same predicate (the daemon's catchesUp).
//
// The predicate reads the design gate as well, because a root whose gate has opened waits for
// its children by the children-first rule: the slot row it still holds in this pass is one
// admission's own release takes back later in the pass (admit's promoteHolds), so the row alone
// would say work it no longer has. Entering the wait there would give nothing back; its own
// architect stays live to answer the person either way; and it would take every first-phase
// member of the tree out of the line (record.Waiting), stopping the children it waits for.
func decideAwait(v awaitView) awaitDecision {
	root := v.Root
	switch {
	// Rule 0 is record.CanWaitOnPerson alone: it tests the phase, the linger and the workflow,
	// and through AtWork that the issue is a tree root whose design gate has never opened, so
	// every standing a second case here could reject it rejects already. Only what to say of a
	// rejection is decided here, and the slot row is what divides the two: a root that holds no
	// slot at all, and one holding a slot its own work no longer takes.
	case !record.CanWaitOnPerson(root, record.TreeGate{Steps: v.Steps, Rows: v.Gates}, v.Slotted):
		if !v.Slotted {
			return awaitDecision{Why: "it holds no admission slot for work of its own"}
		}
		return awaitDecision{Why: "it is not an admitted root of a tree in the workflow whose spec its architect is still writing"}
	case root.DeclaredSeq == 0:
		return awaitDecision{Why: "its architect has handed nothing to a person since it was last given work"}
	}
	since := record.EventMark{Seq: root.DeclaredSeq, At: root.DeclaredAt}
	waiting, ok := waitSubject(v.Asks, v.Steps, v.Gates, since)
	if !ok {
		return awaitDecision{Act: actTakeIn, Why: "nothing it handed back is still with the person"}
	}
	for _, member := range v.Members {
		if record.InWorkflow(member) && member.Phase != phase.Admitted {
			return awaitDecision{Why: fmt.Sprintf("%s of its tree is in %s", member.Key, member.Phase)}
		}
	}
	if v.Unresumable != "" {
		return awaitDecision{Why: "it could not resume: " + v.Unresumable}
	}
	if why, ok := owed(v.Run); ok {
		return awaitDecision{Why: "its architect " + why}
	}
	if answer, detail, ok := answeredSince(waiting, v.Asks, gateDocuments(v.Gates)); ok {
		return awaitDecision{Act: actTellInPlace, Why: "a person already acted after its hand-back", Waiting: waiting, Answer: answer, Detail: detail}
	}
	why := fmt.Sprintf("question %s is awaiting an answer", waiting.Ask)
	if waiting.GateVersion != 0 {
		why = fmt.Sprintf("spec version %d is awaiting a person's review", waiting.GateVersion)
	}
	return awaitDecision{Act: actEnter, Why: why, Waiting: waiting}
}

// withHuman is what of a root is still with the person: its tree's open questions that wait on
// the person, the one on the person's turn longest first (the turn's own place, WaitingOnSeq; an
// open ask no event gave a turn is the person's since it opened, so it sorts first), and whether
// an approval request of requestOf, the one document a root can wait on now
// (gateRequestArtifact), is open on the person's turn. An approval request of any other document
// asks the person for nothing the root waits on.
func withHuman(asks []record.RootAsk, requestOf string) (questions []record.RootAsk, request bool) {
	for _, ask := range asks {
		switch {
		case ask.GateRequestOf(requestOf):
			request = request || ask.WithHuman()
		case ask.ByTree && ask.Question() && ask.WithHuman():
			questions = append(questions, ask)
		}
	}
	slices.SortFunc(questions, func(a, b record.RootAsk) int {
		return cmp.Or(cmp.Compare(a.WaitingOnSeq, b.WaitingOnSeq), cmp.Compare(a.Seq, b.Seq), cmp.Compare(a.Ask, b.Ask))
	})
	return questions, request
}

// gateRequestArtifact is the document whose approval requests a root can wait on: the document of
// the step a person is being asked about (classify.StandingGate). A deployment that gates no step
// waits on no approval, and so does a root no closed required step holds a row for, whose document
// the record does not know: both answer "" (RootAsk.GateRequestOf matches no row against ""). A
// root waits on one document at a time, and the step after it is asked for once that one is open.
func gateRequestArtifact(steps []record.DesignStep, gates map[record.DesignStep]record.DesignGate) string {
	gate, standing := classify.StandingGate(steps, gates)
	if !standing {
		return ""
	}
	return gate.ArtifactID
}

// gateRequestOf is gateRequestArtifact for one gate the caller already holds: its document while
// the deployment gates its step (gated) and the gate is closed, "" otherwise.
func gateRequestOf(gate record.DesignGate, gated bool) string {
	if !gated || classify.DesignGateOpen(gate) {
		return ""
	}
	return gate.ArtifactID
}

// gateArtifactOf is the document gate is registered on, "" for no gate.
func gateArtifactOf(gate *record.DesignGate) string {
	if gate == nil {
		return ""
	}
	return gate.ArtifactID
}

// gateDocuments is the document of each gate the root holds, in no order: the approval requests
// of any of them are answered by a review of their own, not by a person's act on an ask
// (RootAsk.PersonActSeq).
func gateDocuments(gates map[record.DesignStep]record.DesignGate) []string {
	documents := make([]string, 0, len(gates))
	for _, gate := range gates {
		documents = append(documents, gate.ArtifactID)
	}
	return documents
}

// waitGateOpen says whether the gate a root's wait names is open: the row of document, the
// document the wait records, when it names one; otherwise, for a wait on a question, every
// required step being open (classify.DesignGatesOpen), which is what the one gate's opening
// answered before a deployment could name a list.
func waitGateOpen(steps []record.DesignStep, gates map[record.DesignStep]record.DesignGate, document string) bool {
	if document == "" {
		return classify.DesignGatesOpen(steps, gates)
	}
	gate := gateOfDocument(gates, document)
	return gate != nil && classify.DesignGateOpen(*gate)
}

// waitSubject is the wait a root starts from since on what is still with the person: the gate a
// person is being asked about (classify.StandingGate, the step gateRequestArtifact answers for) at
// its latest version, on its own document, when its request is; otherwise the oldest question of
// its tree; false when nothing is. It asks which step the person has, not which step holds the
// work: a step nobody has registered holds the work and raises no wait of its own, and it does not
// take the wait away from a registered step after it either.
func waitSubject(asks []record.RootAsk, steps []record.DesignStep, gates map[record.DesignStep]record.DesignGate, since record.EventMark) (record.Awaiting, bool) {
	standsOn, _ := classify.StandingGate(steps, gates)
	questions, request := withHuman(asks, standsOn.ArtifactID)
	switch {
	case request:
		return record.Awaiting{GateVersion: standsOn.LatestVersion, Artifact: standsOn.ArtifactID, Since: since}, true
	case len(questions) > 0:
		return record.Awaiting{Ask: questions[0].Ask, Since: since}, true
	}
	return record.Awaiting{}, false
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
// on (record.RootAsk.PersonActSeq against the documents the root's gates are registered on, read
// from the acts the ask handlers record under one rule, personAct). A review of one of those
// documents is not read here: the architect is already live, and the daemon's own
// design-changes-requested notice is the work that reaches it (a handout of its own).
func answeredSince(waiting record.Awaiting, asks []record.RootAsk, gateArtifacts []string) (record.Answer, string, bool) {
	since := waiting.Since.Seq
	for _, ask := range asks {
		if ask.Ask == waiting.Ask && ask.PersonActSeq(gateArtifacts...) > since {
			return record.AnsweredByAsk, ask.Ask, true
		}
	}
	for _, ask := range asks {
		if ask.PersonActSeq(gateArtifacts...) > since {
			return record.AnsweredByReply, ask.Ask, true
		}
	}
	return "", "", false
}

// enterAwaiting moves root into awaiting_person on waiting: admission releases its slot in the
// same intake transaction and fills it from Waiting before the fact commits. The architect stays
// live to read the person's answer, and the controller is told.
func (e *Engine) enterAwaiting(ctx context.Context, tx pgx.Tx, root record.Issue, waiting record.Awaiting, reason string) error {
	root.Phase, root.Awaiting = phase.AwaitingPerson, &waiting
	if err := e.store.PutIssue(ctx, tx, root); err != nil {
		return err
	}
	waitReason := fmt.Sprintf("%s waits on a person (%s): its slot is released until they answer", root.Key, reason)
	if err := e.enqueue(ctx, tx, root.Key, record.ControllerNotice{Kind: record.AwaitingPersonNotice, Reason: waitReason}); err != nil {
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

// ResumeAwaitedRoot gives the architect's live claim a task telling it what answered its wait,
// how the document of the step in question stands now (gateInQuestion), and what of the root is
// still with the person or waits on it. It is a handout (Store.StartHandout): what the architect
// handed back before it is answered or listed here, and only a hand-back after it starts the
// next wait. The members an open gate releases are not started here: each is an issue the cap
// counts, so each takes its own place in the waiting line and starts at its own slot
// (record.Waiting's LineFirstPhase), in the same admission pass that resumed this root.
func (e *Engine) ResumeAwaitedRoot(ctx context.Context, tx pgx.Tx, root record.Issue, waiting record.Awaiting) error {
	gates, err := e.store.GatesOf(ctx, tx, root.Key)
	if err != nil {
		return err
	}
	step, gate := gateInQuestion(e.cfg.Steps, gates)
	asks, err := e.store.RootAsks(ctx, tx, []string{root.Key})
	if err != nil {
		return err
	}
	if err := e.store.StartHandout(ctx, tx, root.Key); err != nil {
		return err
	}
	task := resumeTask(root.Key, waiting, step, gate, asks[root.Key], e.armed())
	if err := e.enqueue(ctx, tx, root.Key, record.SuperviseRequest{
		Op: "start", Tree: root.Tree, Role: claim.RoleArchitect, Generation: root.Generation, Task: task,
	}); err != nil {
		return err
	}
	return nil
}

// gateInQuestion is the step a resume speaks of and the row that step holds (resumeTask): the
// step the person was asked about (classify.StandingGate), which is the one the resume answers
// for; else the first required step that is not open, which nobody has registered and whose
// document the architect has still to register; and when every required step is open the first
// required step, whose version the resume says the work runs at. The row is nil for the
// unregistered step, as a root nobody has registered a gate for holds none.
func gateInQuestion(steps []record.DesignStep, gates map[record.DesignStep]record.DesignGate) (record.DesignStep, *record.DesignGate) {
	if gate, standing := classify.StandingGate(steps, gates); standing {
		return gate.Step, &gate
	}
	step, _, closed := classify.FirstClosedStep(steps, gates)
	if !closed {
		step = classify.RequiredSteps(steps)[0]
	}
	gate, ok := gates[step]
	if !ok {
		return step, nil
	}
	return step, &gate
}

// DesignApprovedWake is the phrase a resume task carries when the root's design gate is open at
// the resume. The daemon sends no design-approved notice to a root waiting on a person
// (applyArtifact, gateRegistered), so this resume is that root's design-approved wake: the
// architect skill, its references and the root architect's prompt name this phrase, and a test
// holds them to it.
const DesignApprovedWake = "this resume is your design-approved wake"

// resumeTask is the task an answered root's architect resumes with: what answered its wait
// (Awaiting.AnsweredBy) and that act's own detail (Awaiting.AnsweredDetail, recorded when the wait
// was answered, so a later act of the same kind does not rename it); how the document of the step
// in question stands at the resume, read from that step's gate now, so an approval a newer
// version superseded, or a gate closed again since, is said as it is, and an open gate is said as
// the design-approved wake (DesignApprovedWake) — a changes request a later approval superseded
// asks for no revision; a changes request the gate records as told after the wait began that did
// not answer it, which no notice told (tellChangesOnce); what is still with the person (withHuman)
// and what waits on the architect (a tree question, or the closed gate's approval request, on the
// agent's turn). Handing back is advice only while the gate is not open, since an open gate moves
// the root to planning, where a hand-back starts no wait; the request's own thread is named only
// while the request is open. armed says whether the design gate is armed: with it off, no approval
// is asked for, register_gate is never the advice, and an open gate is no person's approval. A
// wait the gates.design: off rule answered (gateOffAnswer) is never said to be approved: under an
// armed gate it is said as answered while the gate was off.
//
// Every sentence about the gate names step's own document (record.DesignStep.Document), which is
// the document gate holds and the one whose version it prints: under a deployment that gates the
// decisions or the criteria step first, the step in question is not the spec, and a task saying
// "your spec" would send the architect to the wrong document. The READY refusal names a step's
// document the same way (Engine.handoff).
func resumeTask(key string, waiting record.Awaiting, step record.DesignStep, gate *record.DesignGate, asks []record.RootAsk, armed bool) string {
	document := step.Document()
	gateOpen := gate != nil && classify.DesignGateOpen(*gate)
	gateClosed := gate != nil && !gateOpen
	version, gateOff := strings.CutSuffix(waiting.AnsweredDetail, gateOffAnswer)
	gateOff = gateOff && waiting.AnsweredBy == record.AnsweredByApproval
	var task strings.Builder
	switch waiting.AnsweredBy {
	case record.AnsweredByApproval:
		switch {
		case armed && gateOff:
			fmt.Fprintf(&task, "The design gate on %s was off when your wait was answered, so no person approved your %s %s then; the design gate is on again.",
				key, document, version)
		case armed:
			fmt.Fprintf(&task, "Your %s %s on %s was approved.", document, version, key)
		case gateOpen:
			fmt.Fprintf(&task, "Your %s %s on %s is registered, and the design gate is off, so it needs no approval.", document, version, key)
		default:
			fmt.Fprintf(&task, "The design gate on %s is off, so no approval of your %s %s is asked for.", key, document, version)
		}
	case record.AnsweredByChanges:
		fmt.Fprintf(&task, "A person requested changes to your %s on %s at %s.", document, key, waiting.AnsweredDetail)
		if gateOpen && armed {
			fmt.Fprintf(&task, " A person approved the %s after that review, so it asks for no revision now; read it.", document)
		} else {
			fmt.Fprintf(&task, " Read the review, and revise the %s as they ask.", document)
		}
	case record.AnsweredByVersion:
		fmt.Fprintf(&task, "A newer version of your %s on %s, %s, handed the approval request back to you. Read it and the request's thread.", document, key, waiting.AnsweredDetail)
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
	switch {
	case gate == nil:
	case gateOpen && !armed:
		fmt.Fprintf(&task, " The design gate is open at version %d: %s, so act on it as on design-approved.",
			gate.LatestVersion, DesignApprovedWake)
	case gateOpen:
		fmt.Fprintf(&task, " Your %s is approved at version %d and the design gate is open: %s, so act on it as on design-approved.",
			document, gate.LatestVersion, DesignApprovedWake)
	case !armed:
		fmt.Fprintf(&task, " The design gate is closed at version %d, as no registration of that version has opened it: when the %s is ready, "+
			"register it at its current version, which opens the gate with no approval.", gate.LatestVersion, document)
	case waiting.AnsweredBy == record.AnsweredByApproval && !gateOff:
		fmt.Fprintf(&task, " That approval no longer stands: the %s is at version %d and is not approved.", document, gate.LatestVersion)
	default:
		fmt.Fprintf(&task, " Your %s is not approved at version %d.", document, gate.LatestVersion)
	}
	if gate != nil && gate.ToldChangesVersion != nil && !(waiting.Since.Seq > 0 && gate.ToldChangesMark.Seq == 0) &&
		gate.ToldChangesMark.After(waiting.Since) {
		detail := changesDetail(*gate.ToldChangesVersion, gate.ToldChangesReason)
		if waiting.AnsweredBy != record.AnsweredByChanges || waiting.AnsweredDetail != detail {
			fmt.Fprintf(&task, " A person then requested changes to your %s at %s. Read the review, and revise the %s as they ask.", document, detail, document)
		}
	}
	requestOf := ""
	if gate != nil {
		requestOf = gateRequestOf(*gate, armed)
	}
	questions, request := withHuman(asks, requestOf)
	var still, yours []string
	if request {
		still = append(still, fmt.Sprintf("the approval request for %s version %d", document, gate.LatestVersion))
	}
	for _, ask := range questions {
		still = append(still, "ask "+ask.Ask)
	}
	requestOpen := false
	for _, ask := range asks {
		isRequest := ask.GateRequestOf(gateArtifactOf(gate))
		if isRequest && ask.Open {
			requestOpen = true
		}
		if ask.Open && ask.WaitingOn == "agent" && (isRequest && gateClosed || ask.ByTree && ask.Question()) {
			yours = append(yours, "ask "+ask.Ask)
		}
	}
	if len(yours) > 0 {
		fmt.Fprintf(&task, " Waiting on you: %s; read each thread and reply.", strings.Join(yours, ", "))
	}
	switch {
	case len(still) > 0 && !gateOpen:
		fmt.Fprintf(&task, " Still with the person: %s. Hand each one back once you have done what the answer asks — a reply in its thread that "+
			"leaves the turn with the person", strings.Join(still, ", "))
		if armed && gateClosed {
			task.WriteString(", or request approval again and register that version with register_gate")
		}
		task.WriteString(" — or close it if you no longer need it. Handing one back starts your next wait and returns the slot; then end your turn.")
	case len(still) > 0:
		fmt.Fprintf(&task, " Still with the person: %s. Continue.", strings.Join(still, ", "))
	case gateClosed && armed:
		fmt.Fprintf(&task, " When the %s is ready, hand the request back to the person: request approval again and register that version with register_gate", document)
		if requestOpen {
			task.WriteString(", or answer in the request's thread")
		}
		task.WriteString(". Handing it back starts your next wait and returns the slot; then end your turn.")
	default:
		task.WriteString(" Continue.")
	}
	return task.String()
}
