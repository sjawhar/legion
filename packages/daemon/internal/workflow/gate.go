package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// gateRegistered records the gate an architect registered for its root, and logs it. With the
// design gate off it is approved at once, whatever the gate held before, and an open gate advances
// the admitted tree and releases any READY the gate refused, including one refused before any gate
// was registered. What register_gate read of Dispatch's approval travels with the registration, so
// a person's review given before the architect registered is applied here: an approval of the
// latest version (ApprovedVersion) records the gate open, and the root keeps its slot and starts
// planning; a changes request on it (ChangesRequestedVersion) records it closed with the architect
// told, unless the gate already records that same review as told, and the root keeps its slot at
// admitted to revise. The architect is told the version the gate opened at, which an approval
// carried to a later version makes the later one, and is not told it again when the gate of that
// document was already open at that version.
//
// The approval request the registration's read found open is mirrored as the read found it
// (mirrorRegisteredRequest), before anything decides whether the registration changes the gate:
// the row is the request's, not the gate's, and its write is fenced by the sequence the read began
// at, so a registration that changes no gate still records what its read saw, and an older one
// rolls nothing back. When that row, an approval request of the registered document, then stands
// open on the person's turn as the read left it and the gate the registration leaves is closed and
// armed, the registration is the architect handing the spec to the person, so it declares that
// hand-back at the sequence its read began at (fact.Since), whatever the root's last handout: it
// is a live call, made in the architect's turn (declareRegistration). One whose request Dispatch
// shows waiting on the agent (RequestWaitsOnAgent: a reply or a newer version since the request)
// hands nothing back, since the person is not asked to act. Whether the root then waits on the
// person is admission's next pass (ReconcileAwaiting).
//
// A root already waiting on that gate — its architect, still in the turn the wait began in,
// registered the version anew — is answered by what the read found (answerRegisteredGate), since
// the live event and the gate seed that follow find the gate as the registration left it and
// answer nothing. A changes request whose answer resumes the root with a task naming it is not
// told as well, and one the read shows with no sequence, on a root waiting from a sequenced start,
// is neither told nor recorded as told (unplaceable): its sequenced event, still to come, decides
// whether it answers the wait or reaches the architect.
func (e *Engine) gateRegistered(ctx context.Context, tx pgx.Tx, fact intake.GateRegistered) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	request, err := e.mirrorRegisteredRequest(ctx, tx, *issue, fact)
	if err != nil {
		return intake.Result{}, err
	}
	stored, err := e.store.Gate(ctx, tx, fact.Issue)
	if err != nil {
		return intake.Result{}, err
	}
	off := e.cfg.DesignGate == config.DesignGateOff
	sameDocument := stored != nil && stored.ArtifactID == fact.ArtifactID
	// The version the gate is registered at is the newest either side knows of: the caller's, or
	// Dispatch's latest as register_gate's read found it (register_gate refuses a caller's version
	// above Dispatch's). A same-document registration older than the gate's newest observation (a
	// later artifact event applied first) changes nothing, so it neither rolls the gate back nor
	// drops the changes request recorded as told; it still queues the gate seed at the gate's
	// version, whose read of Dispatch catches a review whose own event was lost, and which resets a
	// gate recorded above the version Dispatch holds (gateAboveDispatch). With the design gate off
	// neither early return applies: the registration approves the gate whatever it held.
	version := max(fact.Version, fact.DispatchVersion)
	if !off && sameDocument && stored.LatestVersion > version {
		e.logOnCommit(ctx, "workflow: a registration whose read predates the gate's newest version changes nothing", "issue", issue.Key,
			"artifact", fact.ArtifactID, "version", version, "gate_version", stored.LatestVersion)
		return intake.Result{}, e.enqueue(ctx, tx, fact.Issue, record.GateSeed{ArtifactID: fact.ArtifactID, Version: stored.LatestVersion, Generation: issue.Generation})
	}
	// A review the daemon applied at this version after the registration's read began is newer
	// than anything that read could show: the person acted after it. The gate keeps it, so a read
	// that predates a told changes request does not tell the architect again, and one that predates
	// an approval does not close the gate the approval opened.
	if !off && sameDocument && classify.ReviewAfter(*stored, version, fact.Since) {
		e.logOnCommit(ctx, "workflow: a registration read Dispatch before the review the gate already holds", "issue", issue.Key,
			"artifact", fact.ArtifactID, "version", version, "open", classify.DesignGateOpen(*stored), "since_seq", fact.Since.Seq)
		return intake.Result{}, e.enqueue(ctx, tx, fact.Issue, record.GateSeed{ArtifactID: fact.ArtifactID, Version: version, Generation: issue.Generation})
	}
	gate := record.DesignGate{Issue: fact.Issue, ArtifactID: fact.ArtifactID, LatestVersion: version}
	// An approval of an older version the gate of this document holds is kept, as a version event
	// keeps it (classify.ApplyDesignGateEvent): a late changes request it answered stays answered.
	if sameDocument && stored.ApprovedVersion != nil && *stored.ApprovedVersion < version {
		gate.ApprovedVersion, gate.ApprovedMark = stored.ApprovedVersion, stored.ApprovedMark
	}
	changesRequested := false
	switch {
	case off:
		gate = classify.ApplyDesignGateEvent(gate, classify.DesignGateApproved, version, record.EventMark{})
	case fact.ApprovedVersion >= version:
		// The approval the gate already holds at that version, read again, keeps the fuller mark,
		// so a read that gave only its time never takes the sequence the live event gave.
		mark := fact.ApprovedMark
		if sameDocument && stored.ApprovedVersion != nil && *stored.ApprovedVersion == fact.ApprovedVersion && stored.ApprovedMark.Same(mark) {
			mark = mark.Merged(stored.ApprovedMark)
		}
		gate = classify.ApplyDesignGateEvent(gate, classify.DesignGateApproved, fact.ApprovedVersion, mark)
	case fact.ChangesRequestedVersion >= version:
		gate = classify.ApplyDesignGateEvent(gate, classify.DesignGateChangesRequested, fact.ChangesRequestedVersion, fact.ChangesRequestedMark)
		changesRequested = true
	}
	// A registration whose read predates a review the live event already told keeps that record,
	// whatever version it was told at: it rebuilds the gate from Dispatch, and the gate seed or a
	// late live event that follows may carry the same review again.
	if !classify.DesignGateOpen(gate) && sameDocument && stored.ToldChangesVersion != nil {
		gate = classify.RecordChangesTold(gate, *stored.ToldChangesVersion, stored.ToldChangesReason, stored.ToldChangesMark)
	}
	answered, err := e.answerRegisteredGate(ctx, tx, fact, gate, changesRequested)
	if err != nil {
		return intake.Result{}, err
	}
	tell := false
	if changesRequested && !unplaceable(*issue, fact.ChangesRequestedMark) {
		tell = !answered && !classify.DesignGateChangesTold(gate, gate.LatestVersion, fact.ChangesReason, fact.ChangesRequestedMark)
		gate = classify.RecordChangesTold(gate, gate.LatestVersion, fact.ChangesReason, fact.ChangesRequestedMark)
	}
	if err := e.store.PutGate(ctx, tx, gate); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: design gate registered", "issue", issue.Key, "artifact", fact.ArtifactID,
		"version", version, "open", classify.DesignGateOpen(gate), "changes_requested", changesRequested, "policy", e.cfg.DesignGate)
	if err := e.enqueue(ctx, tx, fact.Issue, record.GateSeed{ArtifactID: fact.ArtifactID, Version: version, Generation: issue.Generation}); err != nil {
		return intake.Result{}, err
	}
	if err := e.declareRegistration(ctx, tx, *issue, fact, request, gate); err != nil {
		return intake.Result{}, err
	}
	switch {
	case changesRequested && !tell:
		e.logOnCommit(ctx, "workflow: the registration read a changes request the architect hears of otherwise", "issue", issue.Key,
			"version", gate.LatestVersion, "answered", answered)
		return intake.Result{}, nil
	case changesRequested:
		return intake.Result{}, e.tellChanges(ctx, tx, issue.Key, fact.ArtifactID, gate.LatestVersion, fact.ChangesReason, fact.ChangesRequestedMark)
	case !classify.DesignGateOpen(gate) && fact.RequestWaitsOnAgent:
		e.logOnCommit(ctx, "workflow: a registration whose approval request waits on the agent hands nothing to the person", "issue", issue.Key,
			"version", gate.LatestVersion)
		return intake.Result{}, nil
	case !classify.DesignGateOpen(gate):
		return intake.Result{}, nil
	}
	// The gate of this document was already open at this version: the architect was told then, so
	// the registration tells it nothing again. Advancing the admitted tree and releasing a refused
	// READY change nothing that already moved.
	if !sameDocument || !classify.DesignGateOpen(*stored) || stored.LatestVersion != gate.LatestVersion {
		if err := e.notice(ctx, tx, fact.Issue, record.Notice{Kind: "design-approved", Version: gate.LatestVersion}); err != nil {
			return intake.Result{}, err
		}
	} else {
		e.logOnCommit(ctx, "workflow: a registration of a gate already open at its version tells the architect nothing", "issue", issue.Key, "version", gate.LatestVersion)
	}
	if err := e.advanceAdmittedTree(ctx, tx, *issue, gate); err != nil {
		return intake.Result{}, err
	}
	if err := e.advancePendingReady(ctx, tx, fact.Issue, gate); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, nil
}

// tellChanges tells key's architect a changes request on artifact at version, once per review: in
// the notice's own transaction it marks the review processed under its issue, generation, document,
// version and place in the issue's Dispatch event log, and a review already marked is told nothing.
// The place is Dispatch's time of the review, which every carrier has (the live event's created_at
// and the approval read's at are one transaction's now()), else its sequence; the told record holds
// only the newest review, so it alone cannot recognise a request a later review replaced. A new
// generation is told the request again, as its architect was not.
//
// The notice is work the daemon hands the running architect, so it is a handout for a root that
// holds its slot (record.Store.StartHandout): what that architect handed back before it is part of
// the spec it is now asked to revise, and only a hand-back after the notice starts the next wait.
func (e *Engine) tellChanges(ctx context.Context, tx pgx.Tx, key, artifact string, version int, reason string, mark record.EventMark) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return err
	}
	place := "none"
	switch {
	case !mark.At.IsZero():
		place = fmt.Sprint(mark.At.UnixNano())
	case mark.Seq > 0:
		place = fmt.Sprintf("s%d", mark.Seq)
	}
	fresh, err := e.store.MarkProcessed(ctx, tx, "design-changes-told", fmt.Sprintf("%s:%d:%s:%d:%s", key, issue.Generation, artifact, version, place))
	if err != nil || !fresh {
		if err == nil {
			e.logOnCommit(ctx, "workflow: a changes request the architect was already told of is not told again", "issue", key, "version", version)
		}
		return err
	}
	if err := e.notice(ctx, tx, key, record.Notice{Kind: "design-changes-requested", Version: version, Reason: reason}); err != nil {
		return err
	}
	if issue.Phase != phase.Admitted || !claim.IsTreeRoot(issue.Key, issue.Tree) {
		return nil
	}
	return e.store.StartHandout(ctx, tx, issue.Key)
}

// unplaceable says whether a review read at mark cannot be placed against root's wait: the root
// waits from a sequenced start and the read gave the review no sequence (a Dispatch that predates
// it). Such a changes request is neither told nor recorded as told, since its own sequenced event,
// still to come, decides whether it answers the wait or reaches the architect; told now, it would
// be told again by the resume that event answers.
func unplaceable(root record.Issue, mark record.EventMark) bool {
	return root.AwaitingPerson() && root.Awaiting.Since.Seq > 0 && mark.Seq == 0
}

// answerAwaitedGate answers a root waiting on a person with an event of its spec document that
// reaches its gate, and reports whether it answered one. gateUnchanged says the event changed no
// gate state: a review the person already followed with another, or an approval of a gate already
// open.
//
// A root waiting on its design gate (Awaiting.GateVersion, on the document Awaiting.Artifact, whose
// events alone answer it, since another document's versions count from one) is answered by the
// person's approval or changes request of the version it waits on or a later one, and by a later
// document version, which hands the approval request back to the architect: the root resumes with
// that version recorded, keeping where its wait began. A version Dispatch recorded at or before
// the wait's start answers nothing: the architect wrote it before it handed the request back, its
// event reached the daemon late, and the person is asked to review it, so the wait moves to that
// version and goes on. A version the gate seed read from Dispatch carries no mark, and answers only
// when that read showed the request waiting on the agent (RequestWaitsOnAgent), as a newer version
// leaves it. A review of a version below the one waited on is an older review the daemon applied
// late, and answers nothing.
//
// A root waiting on its architect's question is answered by any review of its own spec that
// reaches its gate — an approval or a changes request, of the latest version or of an older one the
// daemon applied late — with no check of the version: the person has decided what the root's work
// depends on, and the resume task says how the spec stands now. A new version answers no question
// (it is the architect's own act), and neither does a carried copy's event, whose document is
// another tree's.
//
// A review Dispatch recorded at or before the wait's start answers nothing: the person gave it
// before the work was handed back to them, so it is the review the architect revised against, or
// one the gate seed or the boot catch-up re-read. Only an approval that opens the gate a root waits
// on is exempt, on purpose: a gate wait can begin after an approval whose own event was lost, and
// every later read of that approval finds the gate already open, so nothing else would answer it
// and the root would stay suspended with its gate open. An approval that changes no gate state has
// no such claim, and answers only a wait it follows.
func (e *Engine) answerAwaitedGate(ctx context.Context, tx pgx.Tx, key string, fact intake.DispatchArtifact, carried, gateUnchanged bool) (bool, error) {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || !issue.AwaitingPerson() || issue.Awaiting.AnsweredAt != nil {
		return false, err
	}
	waiting := issue.Awaiting
	onGate := waiting.GateVersion != 0
	if onGate && waiting.Artifact != "" && waiting.Artifact != fact.ArtifactID {
		return false, nil
	}
	before := !fact.Mark.After(waiting.Since)
	by, detail := record.AnsweredByApproval, fmt.Sprintf("version %d", fact.Version)
	switch {
	case !onGate && (fact.Kind == intake.DispatchArtifactVersion || carried):
		return false, nil
	case !onGate && before, gateUnchanged && fact.Kind == intake.DispatchArtifactApproved && before:
		e.logOnCommit(ctx, "workflow: a review given before the wait began answers no wait", "issue", issue.Key,
			"version", fact.Version, "review_seq", fact.Mark.Seq, "since_seq", waiting.Since.Seq)
		return false, nil
	case !onGate:
	case fact.Kind == intake.DispatchArtifactVersion && fact.Version <= waiting.GateVersion:
		return false, nil
	case fact.Kind == intake.DispatchArtifactVersion && !fact.RequestWaitsOnAgent && before:
		e.logOnCommit(ctx, "workflow: a version the person is still asked to review moves the wait to it and answers nothing", "issue", issue.Key,
			"version", fact.Version, "version_seq", fact.Mark.Seq, "since_seq", waiting.Since.Seq)
		waiting.GateVersion = fact.Version
		return false, e.store.PutIssue(ctx, tx, *issue)
	case fact.Kind == intake.DispatchArtifactVersion:
		waiting.GateVersion, by = fact.Version, record.AnsweredByVersion
	case fact.Version < waiting.GateVersion:
		return false, nil
	case fact.Kind == intake.DispatchArtifactChangesRequested && before:
		e.logOnCommit(ctx, "workflow: a changes request given before the wait began answers no wait", "issue", issue.Key,
			"version", fact.Version, "review_seq", fact.Mark.Seq, "since_seq", waiting.Since.Seq)
		return false, nil
	}
	if fact.Kind == intake.DispatchArtifactChangesRequested {
		by, detail = record.AnsweredByChanges, changesDetail(fact.Version, fact.Reason)
	}
	e.logOnCommit(ctx, "workflow: a person's review or a newer spec version answered a root's wait", "issue", issue.Key,
		"version", fact.Version, "event", fact.Kind, "ask", waiting.Ask, "waited_version", waiting.GateVersion)
	return true, e.answerWait(ctx, tx, *issue, by, detail)
}

// answerRegisteredGate answers a root waiting on a person with what the registration's read of
// Dispatch found, and reports whether it answered. register_gate reads Dispatch, so a review given
// between the architect's hand-back and that read, or a version written then whose event the daemon
// never applied, arrives with the registration: the live event and the gate seed that follow find
// the gate as the registration left it and answer nothing, so without this only another act by the
// person would resume the root. Only what Dispatch showed counts, never the version the caller
// named: a document version Dispatch shows newer than the one a gate wait waits on
// (DispatchVersion) answers it when Dispatch shows the approval request waiting on the agent
// (RequestWaitsOnAgent), as a newer version leaves it. With the request still waiting on the person,
// that version came before the request was handed back — the architect wrote it, then asked for its
// review, and its own event reached the daemon late — so it answers nothing, and the wait moves to
// it. The person's approval of Dispatch's latest version answers a gate wait whenever it was given
// (a wait can begin after an approval whose event was lost), and a question wait only when Dispatch
// recorded it after the wait began. The person's changes request answers either only when Dispatch
// recorded it after the wait began. A request Dispatch shows waiting on the agent with no newer
// version carries no author, and a session's note can put it there, so it answers nothing.
//
// A registration of another document is the architect choosing a new spec for the root: a gate
// wait on the old document moves to the new one at the version Dispatch shows, and that document's
// own review answers it, since comparing versions of two documents would wait on a number the new
// one may never reach.
func (e *Engine) answerRegisteredGate(ctx context.Context, tx pgx.Tx, fact intake.GateRegistered, gate record.DesignGate, changesRequested bool) (bool, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil || !issue.AwaitingPerson() || issue.Awaiting.AnsweredAt != nil {
		return false, err
	}
	waiting := issue.Awaiting
	onGate := waiting.GateVersion != 0
	if onGate && waiting.Artifact != "" && waiting.Artifact != fact.ArtifactID {
		e.logOnCommit(ctx, "workflow: a registration of another document moves the wait to it", "issue", issue.Key,
			"artifact", fact.ArtifactID, "was", waiting.Artifact, "version", gate.LatestVersion)
		waiting.Artifact, waiting.GateVersion = fact.ArtifactID, gate.LatestVersion
		if err := e.store.PutIssue(ctx, tx, *issue); err != nil {
			return false, err
		}
	}
	waited := waiting.GateVersion
	if waited == 0 {
		waited = gate.LatestVersion
	}
	var by record.Answer
	version, detail := 0, ""
	switch {
	case onGate && fact.DispatchVersion > waited && fact.RequestWaitsOnAgent:
		by, version = record.AnsweredByVersion, fact.DispatchVersion
	case classify.DesignGateOpen(gate) && fact.ApprovedVersion >= waited && (onGate || fact.ApprovedMark.After(waiting.Since)):
		by, version = record.AnsweredByApproval, fact.ApprovedVersion
	case changesRequested && fact.ChangesRequestedVersion >= waited && !fact.ChangesRequestedMark.After(waiting.Since):
		e.logOnCommit(ctx, "workflow: a registration read a changes request given before the wait began, which answers no wait", "issue", issue.Key,
			"version", fact.ChangesRequestedVersion, "review_seq", fact.ChangesRequestedMark.Seq, "since_seq", waiting.Since.Seq)
		return false, nil
	case changesRequested && fact.ChangesRequestedVersion >= waited:
		by, version, detail = record.AnsweredByChanges, fact.ChangesRequestedVersion, changesDetail(fact.ChangesRequestedVersion, fact.ChangesReason)
	default:
		if fact.RequestWaitsOnAgent {
			e.logOnCommit(ctx, "workflow: a registration shows the request waiting on the agent, which answers no wait", "issue", issue.Key, "version", gate.LatestVersion)
		}
		if onGate && gate.LatestVersion > waiting.GateVersion {
			e.logOnCommit(ctx, "workflow: a registration of a version the person is asked to review moves the wait to it", "issue", issue.Key,
				"version", gate.LatestVersion, "waited", waiting.GateVersion)
			waiting.GateVersion = gate.LatestVersion
			return false, e.store.PutIssue(ctx, tx, *issue)
		}
		return false, nil
	}
	if onGate {
		waiting.GateVersion = version
	}
	if detail == "" {
		detail = fmt.Sprintf("version %d", version)
	}
	e.logOnCommit(ctx, "workflow: a registration answered a root's wait", "issue", issue.Key, "version", version, "event", by, "ask", waiting.Ask)
	return true, e.answerWait(ctx, tx, *issue, by, detail)
}

// changesDetail is how a changes request is named to the architect it resumes: its version and the
// reason the person gave.
func changesDetail(version int, reason string) string {
	return fmt.Sprintf("version %d: %q", version, reason)
}

// untellUnresumedChanges clears the gate's record that its architect was told a changes request
// when that request answered root's wait and root leaves the workflow before it resumed. The
// request was recorded as told because the resume task would name it (applyArtifact), and that
// task was never written: a root swept for a lost label or a foreign claim, or moved out by a
// person, never resumed. Cleared, the request reaches the architect when the root is admitted
// anew, through its registration, the gate seed or the boot catch-up, rather than staying silent.
func (e *Engine) untellUnresumedChanges(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	if !root.AwaitingPerson() || root.Awaiting.AnsweredBy != record.AnsweredByChanges {
		return nil
	}
	gate, err := e.store.Gate(ctx, tx, root.Key)
	if err != nil || gate == nil || gate.ToldChangesVersion == nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a changes request whose resume never ran is no longer recorded as told", "issue", root.Key, "version", *gate.ToldChangesVersion)
	gate.ToldChangesVersion, gate.ToldChangesReason, gate.ToldChangesMark = nil, "", record.EventMark{}
	return e.store.PutGate(ctx, tx, *gate)
}

// mirrorRegisteredRequest writes the mirror row of the approval request a registration read open
// on the registered document, as the read found it: an approval request of that document, open, on
// the turn RequestWaitsOnAgent names. The read's state is written at the issue's sequence the read
// began at (fact.Since), because Dispatch's approval read gives no sequence of its own. Every event
// at or below Since is one the read reflects, so it never overwrites the read; every turn or state
// the request took between Since and the read is an event of the request itself (an opening, a
// reply, a hand-back, the ask.edited of a version that moved it, its close), which overwrites the
// read whenever it arrives, and the newest of them is the state the read showed. So once they have
// all arrived the row is the read's or newer. A fence placed above Since could instead refuse, for
// good, an event newer than the read. Only a root's asks are mirrored.
func (e *Engine) mirrorRegisteredRequest(ctx context.Context, tx pgx.Tx, root record.Issue, fact intake.GateRegistered) (record.RootAsk, error) {
	if fact.RequestAsk == "" || !claim.IsTreeRoot(root.Key, root.Tree) {
		return record.RootAsk{}, nil
	}
	turn := "human"
	if fact.RequestWaitsOnAgent {
		turn = "agent"
	}
	_, after, err := e.mirrorAsk(ctx, tx, root, fact.RequestAsk, askChange{seq: fact.Since.Seq, kind: record.AskApproval, artifact: fact.ArtifactID,
		state: askOpen, turn: turn})
	return after, err
}

// gateAboveDispatch resets a gate recorded above the version Dispatch holds for its document to
// that version, and applies the review the gate seed read there. The seed reads Dispatch after the
// daemon applied every observation that moved the gate, and Dispatch's versions only rise, so a
// gate above the seed's read is one no Dispatch version ever matched: a daemon that registered the
// caller's version unchecked recorded it. Left there, every review of the document's real latest
// version would be dropped as a review of an older one. With the design gate off the reset gate is
// approved, as a registration would leave it.
func (e *Engine) gateAboveDispatch(ctx context.Context, tx pgx.Tx, fact intake.GateAboveDispatch) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Read.Key)
	if err != nil || issue == nil || issue.Generation != fact.Generation {
		return intake.Result{}, err
	}
	gate, err := e.store.Gate(ctx, tx, issue.Key)
	if err != nil || gate == nil || gate.ArtifactID != fact.Read.ArtifactID || gate.LatestVersion <= fact.Read.Version {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: a gate above the version Dispatch holds is reset to it", "issue", issue.Key,
		"artifact", gate.ArtifactID, "gate_version", gate.LatestVersion, "dispatch_version", fact.Read.Version)
	reset := record.DesignGate{Issue: issue.Key, ArtifactID: gate.ArtifactID, LatestVersion: fact.Read.Version}
	off := e.cfg.DesignGate == config.DesignGateOff
	if off {
		reset = classify.ApplyDesignGateEvent(reset, classify.DesignGateApproved, fact.Read.Version, record.EventMark{})
	}
	if err := e.store.PutGate(ctx, tx, reset); err != nil {
		return intake.Result{}, err
	}
	if !off && fact.Read.Kind != intake.DispatchArtifactVersion {
		return intake.Result{}, e.applyArtifact(ctx, tx, reset, fact.Read, false)
	}
	return intake.Result{}, e.advancePendingReady(ctx, tx, issue.Key, reset)
}

// declareRegistration declares the hand-back a registration makes (gateRegistered): when the
// request row it mirrored (request) is an approval request of the gate's document that stands open
// on the person's turn as the read left it — no event newer than the read has given it another
// turn — and the gate is closed and armed, it records the hand-back at the read's sequence
// (Store.Declare), whatever the root's last handout. A row an event newer than the read already
// moved is that event's to declare.
func (e *Engine) declareRegistration(ctx context.Context, tx pgx.Tx, root record.Issue, fact intake.GateRegistered, request record.RootAsk, gate record.DesignGate) error {
	since := fact.Since.Seq
	if fact.RequestAsk == "" || since <= 0 || !claim.IsTreeRoot(root.Key, root.Tree) || request.WaitingOnSeq > since || request.Seq > since ||
		!request.WithHuman() || !request.GateRequestOf(gateRequestArtifact(&gate, e.armed())) {
		return nil
	}
	e.logOnCommit(ctx, "workflow: a registration hands the spec to the person", "issue", root.Key, "version", gate.LatestVersion, "since_seq", since)
	return e.store.Declare(ctx, tx, root.Key, since)
}

// gateRequestFound mirrors the approval request the boot catch-up read open on a root's
// registered document, as a registration's read is mirrored, when the mirror holds no row of that
// request: a gate a daemon of an earlier release registered, before the mirror existed, or one
// whose request the daemon never saw an event of. When the gate is closed and armed and the request
// waits on the person, that is the architect's hand-back of the spec, declared at the read's
// sequence as a registration would declare it (declareRegistration), so a root already waiting on
// its approval at the deploy returns its slot rather than holding it until its architect registers
// again. A request the mirror already holds is the mirror's; a gate of another generation or
// document is left as it is.
func (e *Engine) gateRequestFound(ctx context.Context, tx pgx.Tx, fact intake.GateRequestFound) (intake.Result, error) {
	root, err := e.recordedRoot(ctx, tx, fact.Issue)
	if err != nil || root == nil || root.Generation != fact.Generation || fact.RequestAsk == "" {
		return intake.Result{}, err
	}
	gate, err := e.store.Gate(ctx, tx, root.Key)
	if err != nil || gate == nil || gate.ArtifactID != fact.ArtifactID {
		return intake.Result{}, err
	}
	if stored, err := e.store.RootAsk(ctx, tx, root.Key, fact.RequestAsk); err != nil || stored != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: the boot catch-up mirrors the approval request open on a gate's document", "issue", root.Key,
		"artifact", gate.ArtifactID, "ask", fact.RequestAsk, "waits_on_agent", fact.WaitsOnAgent)
	registered := intake.GateRegistered{Issue: root.Key, ArtifactID: gate.ArtifactID, Version: gate.LatestVersion,
		Since: record.EventMark{Seq: fact.Since}, RequestAsk: fact.RequestAsk, RequestWaitsOnAgent: fact.WaitsOnAgent}
	request, err := e.mirrorRegisteredRequest(ctx, tx, *root, registered)
	if err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.declareRegistration(ctx, tx, *root, registered, request, *gate)
}

// dispatchArtifact applies an artifact event to the gates registered on that document: the root
// that owns it, by whose key Dispatch sends its events, takes every event (applyArtifact), and each
// copy a hand-over carried to a child of its tree, now a root of its own (admit's readmit), takes
// only an approval that opens it or keeps it open (copyTakes). register_gate refuses a document of
// another issue, so no other gate names it.
func (e *Engine) dispatchArtifact(ctx context.Context, tx pgx.Tx, fact intake.DispatchArtifact) (intake.Result, error) {
	gates, err := e.store.GatesForArtifact(ctx, tx, fact.ArtifactID)
	if err != nil {
		return intake.Result{}, err
	}
	for _, gate := range gates {
		carried := gate.Issue != fact.Key
		if carried && !copyTakes(gate, fact) {
			continue
		}
		if err := e.applyArtifact(ctx, tx, gate, fact, carried); err != nil {
			return intake.Result{}, err
		}
	}
	return intake.Result{}, nil
}

// copyTakes says whether a carried copy of another root's gate takes an event of that root's
// document. The carried work was approved under the design the copy carries, and the document
// belongs to the old tree now, so nothing on it closes the copy: not a new version, a changes
// request, or an approval of a version older than the copy's (which ApplyDesignGateEvent would
// record as the approved version, closing it). Only an approval of the copy's version or a later
// one applies, opening a copy carried closed and releasing the READYs it refused.
func copyTakes(gate record.DesignGate, fact intake.DispatchArtifact) bool {
	return fact.Kind == intake.DispatchArtifactApproved && fact.Version >= gate.LatestVersion
}

// applyArtifact applies an artifact event to one gate, for the issue that holds it: an approval
// opens it and advances that issue's admitted tree, a changes request or a new version closes it,
// and an open gate releases the READYs it refused in that issue's tree. Each open or close is
// logged, and a changes request that finds the gate closed has a line of its own. The architect is
// told an opening as design-approved, which starts its tree's work, except for a carried copy
// (carried): that tree's design was approved before the carry, so its architect is told
// carried-gate-opened, which says only what the opening released.
//
// One rule governs the design-changes-requested notice here and at a registration: the architect
// is told a changes request unless the gate already records that same review as told (the same
// version, the same reason and the same place in the issue's Dispatch event log, so a later review
// that repeats an earlier one's version and wording still reaches it). Every changes request the
// gate applies is recorded as told, completing the record of the same review with what this read
// adds of its mark, so the registration and the gate seed, which both re-read Dispatch, never tell
// it twice and never stay silent about a request nothing else carried.
//
// A review the person has already followed with another changes no gate state: one of a version
// below the gate's latest (the person has a newer version to review), or one at that version whose
// mark is not past the review the gate applied last (classify.StaleReview). The daemon applied the
// newer state first, and this review reached it late, so it does not move the gate or its told
// record. That is an approval delivered after the changes request that superseded it, which would
// otherwise reopen the gate; a changes request delivered after a newer approval, which would
// otherwise close it; and an approval or changes request of an older version, which would
// otherwise clear the told record or close a gate open at the newer one. A stale changes request
// is still the person's words: it is told, with its own version and reason, unless an approval came
// after it (classify.ApprovalFollows), which the person gave in its place.
//
// Every design-changes-requested notice goes through tellChanges, which sends it once per review
// whatever carries it (the live event, a registration's read, the gate seed's read) and in whatever
// order: the told record holds only the newest review, so it alone cannot recognise a request a
// later review replaced, and the key per review can.
//
// With the design gate off, no artifact event moves a gate: the gate is approved at registration
// (gateRegistered), and a review or a version of the document it names must not close it. That
// return also keeps an approval from opening a carried copy that was closed before the switch;
// nobody requests an approval under off, and the copy's registration approves it. A changes
// request's live event is still told, as the person's words, without closing the gate.
//
// An event of the root's own spec also answers a root waiting on a person (answerAwaitedGate): a
// review answers a wait on its question or on its gate, a stale one included when Dispatch recorded
// it after the wait began, and a newer version answers a wait on its gate. A changes request that
// answered is named by the resume task, so the notice is not sent too (tellUnanswered). A changes
// request that cannot be placed against that wait (unplaceable) is neither told nor recorded as
// told.
func (e *Engine) applyArtifact(ctx context.Context, tx pgx.Tx, gate record.DesignGate, fact intake.DispatchArtifact, carried bool) error {
	key := gate.Issue
	off := e.cfg.DesignGate == config.DesignGateOff
	stale := fact.Kind != intake.DispatchArtifactVersion &&
		(fact.Version < gate.LatestVersion || classify.StaleReview(gate, fact.Version, fact.Mark))
	if off || stale {
		why := "workflow: a review the person has already followed with another changes no gate state"
		if off {
			why = "workflow: the design gate is off, so an artifact event changes no gate"
		}
		e.logOnCommit(ctx, why, "issue", key, "artifact", fact.ArtifactID, "event", fact.Kind, "version", fact.Version,
			"gate_version", gate.LatestVersion, "review_seq", fact.Mark.Seq)
		if fact.Kind == intake.DispatchArtifactVersion || carried {
			return nil
		}
		answered, err := e.answerAwaitedGate(ctx, tx, key, fact, carried, true)
		if err != nil {
			return err
		}
		if fact.Kind != intake.DispatchArtifactChangesRequested {
			return nil
		}
		if stale && classify.ApprovalFollows(gate, fact.Version, fact.Mark) {
			return nil
		}
		if !off {
			e.logOnCommit(ctx, "workflow: the architect is told a changes request no approval followed", "issue", key, "version", fact.Version)
		}
		return e.tellUnanswered(ctx, tx, key, fact, answered)
	}
	wasOpen := classify.DesignGateOpen(gate)
	kind := classify.DesignGateEventKind(fact.Kind)
	updated := classify.ApplyDesignGateEvent(gate, kind, fact.Version, fact.Mark)
	isOpen := classify.DesignGateOpen(updated)
	switch {
	case isOpen != wasOpen:
		verb := "closed"
		if isOpen {
			verb = "opened"
		}
		e.logOnCommit(ctx, "workflow: design gate "+verb, "issue", key, "artifact", fact.ArtifactID, "event", fact.Kind, "version", fact.Version)
	case fact.Kind == intake.DispatchArtifactChangesRequested:
		e.logOnCommit(ctx, "workflow: design gate changes requested", "issue", key, "artifact", fact.ArtifactID, "version", fact.Version)
	}
	// An approval of a gate already open changes no gate state, and answers only a wait it follows.
	answered, err := e.answerAwaitedGate(ctx, tx, key, fact, carried, fact.Kind == intake.DispatchArtifactApproved && wasOpen)
	if err != nil {
		return err
	}
	placeable, err := e.placeable(ctx, tx, key, fact)
	if err != nil {
		return err
	}
	tell := fact.Kind == intake.DispatchArtifactChangesRequested && placeable &&
		!classify.DesignGateChangesTold(gate, fact.Version, fact.Reason, fact.Mark)
	if fact.Kind == intake.DispatchArtifactChangesRequested && placeable {
		updated = classify.RecordChangesTold(updated, fact.Version, fact.Reason, fact.Mark)
	}
	if err := e.store.PutGate(ctx, tx, updated); err != nil {
		return err
	}
	if tell {
		if err := e.tellUnanswered(ctx, tx, key, fact, answered); err != nil {
			return err
		}
	}
	if !wasOpen && isOpen {
		opened := record.Notice{Kind: "design-approved", Version: fact.Version}
		if carried {
			opened = record.Notice{Kind: "carried-gate-opened", Version: fact.Version, Reason: fmt.Sprintf(
				"a human approved %s's spec document at version %d, which opens the gate %s carried; any READY it refused is released and any child waiting on it starts", fact.Key, fact.Version, key)}
		}
		if err := e.notice(ctx, tx, key, opened); err != nil {
			return err
		}
		issue, err := e.store.Issue(ctx, tx, key)
		if err != nil || issue == nil {
			return err
		}
		if err := e.advanceAdmittedTree(ctx, tx, *issue, updated); err != nil {
			return err
		}
	}
	return e.advancePendingReady(ctx, tx, key, updated)
}

// tellUnanswered tells key's architect a changes request, unless that request already answered the
// root's wait: the task resuming its architect names the review, so a notice would say it twice.
func (e *Engine) tellUnanswered(ctx context.Context, tx pgx.Tx, key string, fact intake.DispatchArtifact, answered bool) error {
	if answered {
		e.logOnCommit(ctx, "workflow: a changes request that answered a root's wait is named by its resume, not by a notice",
			"issue", key, "version", fact.Version)
		return nil
	}
	return e.tellChanges(ctx, tx, key, fact.ArtifactID, fact.Version, fact.Reason, fact.Mark)
}

// placeable says whether a changes request can be placed against key's wait, and so may be told
// and recorded as told (unplaceable). Every other event is placeable.
func (e *Engine) placeable(ctx context.Context, tx pgx.Tx, key string, fact intake.DispatchArtifact) (bool, error) {
	if fact.Kind != intake.DispatchArtifactChangesRequested {
		return true, nil
	}
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return true, err
	}
	if unplaceable(*issue, fact.Mark) {
		e.logOnCommit(ctx, "workflow: a changes request with no sequence cannot be placed against a root's wait, and is neither told nor recorded",
			"issue", key, "version", fact.Version, "since_seq", issue.Awaiting.Since.Seq)
		return false, nil
	}
	return true, nil
}
