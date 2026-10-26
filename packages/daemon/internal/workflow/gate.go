package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// gateRegistered records the gate an architect registered for its root, and logs it. With the
// design gate off it is approved at once, and an open gate advances the admitted tree and releases
// any READY the gate refused, including one refused before any gate was registered. What
// register_gate read of Dispatch's approval travels with the registration, so a person's review
// given before the architect registered is never left waiting on: an approval of the latest version
// (ApprovedVersion) records the gate open, and the root keeps its slot and starts planning; a
// changes request on it (ChangesRequestedVersion) records it closed with the architect told, and
// the root keeps its slot at admitted to revise. A root already waiting on that gate, which handed
// its approval request back before registering the version anew, is answered by that review when
// Dispatch recorded it after the wait began, or by a version newer than the one it waits on
// (answerRegisteredGate), since the event carrying either finds the gate as the registration left
// it and answers nothing. A closed gate whose request Dispatch shows waiting on the agent
// (RequestWaitsOnAgent: a reply or a newer version since the request) starts no wait, since the
// person is not asked to act, and answers none either: the turn reached the agent through an act
// the registration does not carry the author of, and only a person's own act answers a wait
// (dispatchAskReply). The architect's hand-back of the request starts the wait. A wait the
// registration starts begins where its read of Dispatch began (fact.Since), so a review the read
// did not show, its own event still to come, answers it.
// The architect is told the version the gate opened at, which an approval carried to a later
// version makes the later one, and a changes request the gate records as told, or one whose
// answer resumes the root with a task naming it, is not told again.
func (e *Engine) gateRegistered(ctx context.Context, tx pgx.Tx, fact intake.GateRegistered) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	// The registration's read of Dispatch began at fact.Since.Seq and decided the gate from
	// everything up to it: a hand-back at or below it, delivered after, starts no wait on the gate.
	// It read the approval alone, so it fences no ordinary question (record.Issue.GateFenced).
	if issue.RaiseGateFence(fact.Since.Seq) {
		if err := e.store.PutIssue(ctx, tx, *issue); err != nil {
			return intake.Result{}, err
		}
	}
	stored, err := e.store.Gate(ctx, tx, fact.Issue)
	if err != nil {
		return intake.Result{}, err
	}
	// The version the gate is registered at is the newest either side knows of: the caller's, or
	// Dispatch's latest as register_gate's read found it (register_gate refuses a caller's version
	// above Dispatch's). A same-document registration older than the gate's newest observation (a
	// later artifact event applied first) changes nothing, so it neither rolls the gate back nor
	// drops the changes request recorded as told; it still queues the gate seed, whose read of
	// Dispatch catches a review whose own event was lost.
	version := max(fact.Version, fact.DispatchVersion)
	if stored != nil && stored.ArtifactID == fact.ArtifactID && stored.LatestVersion > version {
		e.logOnCommit(ctx, "workflow: a registration whose read predates the gate's newest version changes nothing", "issue", issue.Key,
			"artifact", fact.ArtifactID, "version", version, "gate_version", stored.LatestVersion)
		return intake.Result{}, e.enqueue(ctx, tx, fact.Issue, record.GateSeed{ArtifactID: fact.ArtifactID, Version: stored.LatestVersion, Generation: issue.Generation})
	}
	// A review the daemon applied at this version after the registration's read began is newer
	// than anything that read could show: the person acted after it, and that review is the
	// registration's own. The gate keeps it, so a read that predates a told changes request does
	// not ask the person again, and one that predates an approval does not close the gate the
	// approval opened; neither starts a wait, which only the person's next act could answer.
	if stored != nil && stored.ArtifactID == fact.ArtifactID && classify.ReviewAfter(*stored, version, fact.Since) {
		e.logOnCommit(ctx, "workflow: a registration read Dispatch before the review the gate already holds", "issue", issue.Key,
			"artifact", fact.ArtifactID, "version", version, "open", classify.DesignGateOpen(*stored), "since_seq", fact.Since.Seq)
		return intake.Result{}, e.enqueue(ctx, tx, fact.Issue, record.GateSeed{ArtifactID: fact.ArtifactID, Version: version, Generation: issue.Generation})
	}
	gate := record.DesignGate{Issue: fact.Issue, ArtifactID: fact.ArtifactID, LatestVersion: version}
	changesRequested := false
	switch {
	case e.cfg.DesignGate == config.DesignGateOff:
		gate = classify.ApplyDesignGateEvent(gate, classify.DesignGateApproved, version, record.EventMark{})
	case fact.ApprovedVersion >= version:
		gate = classify.ApplyDesignGateEvent(gate, classify.DesignGateApproved, fact.ApprovedVersion, fact.ApprovedMark)
	case fact.ChangesRequestedVersion >= version:
		gate = classify.ApplyDesignGateEvent(gate, classify.DesignGateChangesRequested, fact.ChangesRequestedVersion, fact.ChangesRequestedMark)
		changesRequested = true
	}
	// A registration whose read predates a review the live event already told keeps that record:
	// it rebuilds the gate from Dispatch, and the gate seed that follows re-reads the same review.
	if !classify.DesignGateOpen(gate) && stored != nil && stored.ArtifactID == gate.ArtifactID &&
		stored.ToldChangesVersion != nil && *stored.ToldChangesVersion == gate.LatestVersion {
		gate = classify.RecordChangesTold(gate, *stored.ToldChangesVersion, stored.ToldChangesReason, stored.ToldChangesMark)
	}
	answered, err := e.answerRegisteredGate(ctx, tx, fact, gate, changesRequested)
	if err != nil {
		return intake.Result{}, err
	}
	tell := false
	if changesRequested {
		told := classify.DesignGateChangesTold(gate, gate.LatestVersion, fact.ChangesReason, fact.ChangesRequestedMark)
		tell = !told && !answered
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
	switch {
	case changesRequested:
		if !tell {
			e.logOnCommit(ctx, "workflow: the registration read a changes request the architect hears of otherwise", "issue", issue.Key, "version", gate.LatestVersion, "answered", answered)
			return intake.Result{}, nil
		}
		return intake.Result{}, e.notice(ctx, tx, fact.Issue, record.Notice{Kind: "design-changes-requested", Version: gate.LatestVersion, Reason: fact.ChangesReason})
	case !classify.DesignGateOpen(gate) && fact.RequestWaitsOnAgent:
		e.logOnCommit(ctx, "workflow: root keeps its admission slot, since its approval request waits on the agent", "issue", issue.Key, "version", gate.LatestVersion)
		return intake.Result{}, nil
	case !classify.DesignGateOpen(gate):
		// A root already waiting (on its question) when its architect registers a closed gate holds
		// that wait pending: the resumed architect is told the spec is still with the person, and
		// handing the request back starts the next wait.
		current, err := e.store.Issue(ctx, tx, fact.Issue)
		if err != nil || current == nil {
			return intake.Result{}, err
		}
		waiting := record.Awaiting{GateVersion: gate.LatestVersion, Artifact: gate.ArtifactID, Since: fact.Since}
		if current.AwaitingPerson() {
			return intake.Result{}, e.coveredByTheWaitItRuns(ctx, tx, *current, waiting, "closed gate registered")
		}
		return intake.Result{}, e.awaitPerson(ctx, tx, *current, waiting, fmt.Sprintf("spec version %d is awaiting a person's review", gate.LatestVersion))
	}
	if err := e.notice(ctx, tx, fact.Issue, record.Notice{Kind: "design-approved", Version: gate.LatestVersion}); err != nil {
		return intake.Result{}, err
	}
	if err := e.advanceAdmittedTree(ctx, tx, *issue, gate); err != nil {
		return intake.Result{}, err
	}
	if err := e.advancePendingReady(ctx, tx, fact.Issue, gate); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, nil
}

// answerRegisteredGate answers a root waiting on a person with what the registration's read of
// Dispatch found, and reports whether it answered. register_gate reads Dispatch, so a review given
// between the architect's hand-back and that read, or a version written then whose event the
// daemon never applied, arrives with the registration: the live event and the gate seed that
// follow find the gate as the registration left it and answer nothing, so without this only
// another act by the person would resume the root. Only what Dispatch showed counts, never the
// version the caller named: a document version Dispatch shows newer than the one waited on
// (DispatchVersion) answers a gate wait when Dispatch shows the approval request waiting on the
// agent (RequestWaitsOnAgent), as a newer version leaves it, the live artifact.version's answer.
// With the request still waiting on the person, that version came before the request was handed
// back — the architect wrote it, then asked for its review, and its own event reached the daemon
// late — so it answers nothing, and the wait moves to it. The person's approval or changes request
// of Dispatch's latest version answers the wait when that version is at least the one waited on.
// The person's review of the spec answers a root waiting on its question too, as on the live path
// (answerAwaitedGate). A changes request Dispatch recorded before the wait began
// (ChangesRequestedMark not after Awaiting.Since) is one the person gave before the request was
// handed back to them, and answers nothing. A request Dispatch shows waiting on the agent with no
// newer version carries no author, and a session's note can put it there, so it answers nothing.
func (e *Engine) answerRegisteredGate(ctx context.Context, tx pgx.Tx, fact intake.GateRegistered, gate record.DesignGate, changesRequested bool) (bool, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil || !issue.AwaitingPerson() || issue.Awaiting.AnsweredAt != nil {
		return false, err
	}
	// A registration of another document is the architect choosing a new spec for this root: the
	// version it waits on belongs to the old one, so the wait moves to the new document at the
	// version Dispatch shows, and its own review answers it. Comparing the two versions would wait
	// on a number the new document may never reach.
	waited := issue.Awaiting.GateVersion
	if issue.Awaiting.GateVersion != 0 && issue.Awaiting.Artifact != "" && issue.Awaiting.Artifact != fact.ArtifactID {
		e.logOnCommit(ctx, "workflow: a registration of another document moves the wait to it", "issue", issue.Key,
			"artifact", fact.ArtifactID, "was", issue.Awaiting.Artifact, "version", gate.LatestVersion)
		issue.Awaiting.Artifact, issue.Awaiting.GateVersion, waited = fact.ArtifactID, gate.LatestVersion, gate.LatestVersion
		if err := e.store.PutIssue(ctx, tx, *issue); err != nil {
			return false, err
		}
	}
	if waited == 0 {
		waited = gate.LatestVersion
	}
	var by record.Answer
	detail := ""
	version := 0
	switch {
	case issue.Awaiting.GateVersion != 0 && fact.DispatchVersion > waited && fact.RequestWaitsOnAgent:
		by, version = record.AnsweredByVersion, fact.DispatchVersion
	// An approval answers a gate wait whenever it was given (a wait can begin after an approval whose
	// event was lost, and the gate holds it closed until now); a question wait has no such claim on
	// it, and an approval the person gave before the question is not their answer to it.
	case classify.DesignGateOpen(gate) && fact.ApprovedVersion >= waited && (issue.Awaiting.GateVersion != 0 || fact.ApprovedMark.After(issue.Awaiting.Since)):
		by, version = record.AnsweredByApproval, fact.ApprovedVersion
	case changesRequested && fact.ChangesRequestedVersion >= waited && !fact.ChangesRequestedMark.After(issue.Awaiting.Since):
		e.logOnCommit(ctx, "workflow: a registration read a changes request given before the wait began, which answers no wait", "issue", issue.Key,
			"version", fact.ChangesRequestedVersion, "review_seq", fact.ChangesRequestedMark.Seq, "since_seq", issue.Awaiting.Since.Seq)
		return false, nil
	case changesRequested && fact.ChangesRequestedVersion >= waited:
		by, version, detail = record.AnsweredByChanges, fact.ChangesRequestedVersion, changesDetail(fact.ChangesRequestedVersion, fact.ChangesReason)
	default:
		if fact.RequestWaitsOnAgent {
			e.logOnCommit(ctx, "workflow: a registration shows the request waiting on the agent, which answers no wait", "issue", issue.Key, "version", gate.LatestVersion)
		}
		if issue.Awaiting.GateVersion != 0 && gate.LatestVersion > issue.Awaiting.GateVersion {
			e.logOnCommit(ctx, "workflow: a registration of a version the person is asked to review moves the wait to it", "issue", issue.Key,
				"version", gate.LatestVersion, "waited", issue.Awaiting.GateVersion)
			issue.Awaiting.GateVersion = gate.LatestVersion
			return false, e.store.PutIssue(ctx, tx, *issue)
		}
		return false, nil
	}
	if issue.Awaiting.GateVersion != 0 {
		issue.Awaiting.GateVersion = version
	}
	if detail == "" {
		detail = fmt.Sprintf("version %d", version)
	}
	e.logOnCommit(ctx, "workflow: a registration answered a root's wait", "issue", issue.Key, "version", version, "event", by, "ask", issue.Awaiting.Ask)
	return true, e.answerWait(ctx, tx, *issue, by, detail)
}

// changesDetail is how a changes request is named to the architect it resumes: its version and the
// reason the person gave.
func changesDetail(version int, reason string) string {
	return fmt.Sprintf("version %d: %q", version, reason)
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
// that repeats an earlier one's version and wording still reaches it), or unless the request
// answers the root's wait, where the resume task names it. Whatever reaches the architect is
// recorded on the gate, so the registration, the gate seed and the boot catch-up, which all re-read
// Dispatch, never tell it twice and never stay silent about a request nothing else carried.
//
// A review the person has already followed with another changes no gate state: one of a version
// below the gate's latest (the person has a newer version to review), or one at that version whose
// mark is not past the review the gate applied last (classify.StaleReview). The daemon applied the
// newer state first, and this review reached it late, so it neither moves the gate nor tells the
// architect. That is an approval delivered after the changes request that superseded it, which
// would otherwise reopen the gate; a changes request delivered after a newer approval, which would
// otherwise close it; and an approval or changes request of an older version, which would
// otherwise clear the told record or close a gate open at the newer one. It still answers a wait
// it follows (answerAwaitedGate): the person reviewed the spec after the architect handed the work
// back, and a root left waiting for a review already applied to the gate would wait for good.
//
// With the design gate off, no artifact event moves a gate: the gate is approved at registration
// (gateRegistered), and a review or a version of the document it names must not close it. A wait
// on a question is still answered, as any person's act answers one.
func (e *Engine) applyArtifact(ctx context.Context, tx pgx.Tx, gate record.DesignGate, fact intake.DispatchArtifact, carried bool) error {
	key := gate.Issue
	stale := fact.Kind != intake.DispatchArtifactVersion && (fact.Version < gate.LatestVersion || classify.StaleReview(gate, fact.Version, fact.Mark))
	if stale || e.cfg.DesignGate == config.DesignGateOff {
		why := "workflow: a review the person has already followed with another changes no gate state"
		if !stale {
			why = "workflow: the design gate is off, so an artifact event changes no gate"
		}
		e.logOnCommit(ctx, why, "issue", key,
			"artifact", fact.ArtifactID, "event", fact.Kind, "version", fact.Version, "gate_version", gate.LatestVersion, "review_seq", fact.Mark.Seq)
		if fact.Kind == intake.DispatchArtifactVersion || carried {
			return nil
		}
		_, err := e.answerAwaitedGate(ctx, tx, key, fact, carried, true)
		return err
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
	answered := false
	if fact.Kind == intake.DispatchArtifactChangesRequested || !wasOpen && isOpen || fact.Kind == intake.DispatchArtifactVersion {
		var err error
		if answered, err = e.answerAwaitedGate(ctx, tx, key, fact, carried, false); err != nil {
			return err
		}
	}
	tell := fact.Kind == intake.DispatchArtifactChangesRequested && !answered &&
		!classify.DesignGateChangesTold(gate, fact.Version, fact.Reason, fact.Mark)
	if fact.Kind == intake.DispatchArtifactChangesRequested && (tell || answered) {
		updated = classify.RecordChangesTold(updated, fact.Version, fact.Reason, fact.Mark)
	}
	if err := e.store.PutGate(ctx, tx, updated); err != nil {
		return err
	}
	if tell {
		if err := e.notice(ctx, tx, key, record.Notice{Kind: "design-changes-requested", Version: fact.Version, Reason: fact.Reason}); err != nil {
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

// answerAwaitedGate records the person result on an awaiting root, and reports whether it
// answered one. A later document version hands its approval request back to the architect, so it
// is an answer too: the root resumes with the latest version recorded, keeping where its wait
// began, and lets the architect continue it. A version whose event Dispatch recorded at or before
// the wait's start answers nothing: the architect wrote it before it handed the request back, its
// event reached the daemon late, and the person is asked to review it, so the wait moves to that
// version and goes on. A version the daemon read from Dispatch rather than from its event (the
// gate seed, which reads a version written after the registration's read) carries no mark and
// answers as before. A review of a version below the one waited on is an older review the daemon
// applied late, and answers nothing. Admission promotes the answered root after this handler, so
// an opened gate never advances children before the root has its slot again.
//
// A root waiting on its architect's question is answered by the person's review of its own spec
// too: an approval that opens its gate, or a changes request of its latest version. The person
// has decided what the root's work depends on, and the root would otherwise sit without its slot
// until they also acted on that exact question, or for good once the architect's question was
// closed by someone else. A newer version answers no question, and a carried copy's opening
// answers none either: the document is another tree's.
//
// A changes request Dispatch recorded before the wait began (its Mark not after Awaiting.Since)
// answers nothing: the person gave it before the request was handed back to them, so it is the
// review the architect revised against or one it already heard of, re-read by the gate seed or
// the boot catch-up, and the root waits on the person's next act. An approval is exempt on
// purpose. It reaches here only when it opens the gate (applyArtifact), and a wait on a gate the
// record still shows closed can begin after an approval whose own event was lost; skipped, that
// approval would leave the root suspended in awaiting_person with its gate open, and nothing later
// could answer it, since every further read of that approval finds the gate already open.
func (e *Engine) answerAwaitedGate(ctx context.Context, tx pgx.Tx, key string, fact intake.DispatchArtifact, carried, gateUnchanged bool) (bool, error) {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || !issue.AwaitingPerson() || issue.Awaiting.AnsweredAt != nil {
		return false, err
	}
	// A gate wait names the document it waits on, and only that document's events answer it. The
	// version a review names means nothing on another document, whose versions count from one.
	if issue.Awaiting.GateVersion != 0 && issue.Awaiting.Artifact != "" && issue.Awaiting.Artifact != fact.ArtifactID {
		return false, nil
	}
	// An approval that opens the gate answers the wait whenever it was given: a wait can begin after
	// an approval whose own event was lost, and every later read of that approval finds the gate
	// already open, so nothing else would answer it. One that changes no gate state has no such
	// claim — the gate already holds this review or a newer one, which the architect was told of or
	// the gate shows — so it answers only a wait it follows, as every other review does.
	if gateUnchanged && fact.Kind == intake.DispatchArtifactApproved && !fact.Mark.After(issue.Awaiting.Since) {
		return false, nil
	}
	by, detail := record.AnsweredByApproval, fmt.Sprintf("version %d", fact.Version)
	switch {
	case issue.Awaiting.Ask != "" && (fact.Kind == intake.DispatchArtifactVersion || carried):
		return false, nil
	case issue.Awaiting.Ask != "":
	case fact.Kind == intake.DispatchArtifactVersion && fact.Version <= issue.Awaiting.GateVersion:
		return false, nil
	case fact.Kind == intake.DispatchArtifactVersion && !fact.RequestWaitsOnAgent && !fact.Mark.After(issue.Awaiting.Since):
		e.logOnCommit(ctx, "workflow: a version Dispatch does not show the person waiting past moves the wait to it and answers nothing", "issue", issue.Key,
			"version", fact.Version, "version_seq", fact.Mark.Seq, "since_seq", issue.Awaiting.Since.Seq)
		issue.Awaiting.GateVersion = fact.Version
		return false, e.store.PutIssue(ctx, tx, *issue)
	case fact.Kind == intake.DispatchArtifactVersion:
		issue.Awaiting.GateVersion, by = fact.Version, record.AnsweredByVersion
	case fact.Version < issue.Awaiting.GateVersion:
		return false, nil
	}
	if fact.Kind == intake.DispatchArtifactChangesRequested {
		if !fact.Mark.After(issue.Awaiting.Since) {
			e.logOnCommit(ctx, "workflow: a changes request given before the wait began answers no wait", "issue", issue.Key,
				"version", fact.Version, "review_seq", fact.Mark.Seq, "since_seq", issue.Awaiting.Since.Seq)
			return false, nil
		}
		by, detail = record.AnsweredByChanges, changesDetail(fact.Version, fact.Reason)
	}
	e.logOnCommit(ctx, "workflow: a person answered a root's wait on the design gate", "issue", issue.Key, "version", fact.Version,
		"event", fact.Kind, "ask", issue.Awaiting.Ask)
	return true, e.answerWait(ctx, tx, *issue, by, detail)
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
