package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// gateRegistered records the gate an architect registered for its root, and logs it. With the
// design gate off it is approved at once, whatever the gate held before, and an open gate advances
// the admitted tree and releases any READY the gate refused, including one refused before any gate
// was registered. What register_gate read of Dispatch's approval travels with the registration, so
// a person's review given before the architect registered is applied here: an approval of the
// latest version (ApprovedVersion) records the gate open, and the root starts planning; a changes
// request on it (ChangesRequestedVersion) records it closed with the architect told, unless the
// gate already records that same review as told. The architect is told the version the gate opened
// at, which an approval carried to a later version makes the later one, and is not told it again
// when the gate of that document was already open at that version.
//
// The registration binds the gate to the approval request its read found open (RequestAsk), with
// the document it is a request of (RequestArtifact), and writes that request's mirror row as the
// read found it (mirrorRegisteredRequest). A read that found no request open keeps the request the
// gate already names when it is a request of the registered document, since the request may be
// closed or answered rather than gone; any other registration rebinds it, to its document's open
// request or to none. The request is compared by its own document rather than the gate's: an
// earlier release, rolled back to, moves the gate to another document and leaves the request as
// it was, and that request is no request of the new document.
func (e *Engine) gateRegistered(ctx context.Context, tx pgx.Tx, fact intake.GateRegistered) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
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
	switch {
	case fact.RequestAsk != "":
		gate.RequestAsk, gate.RequestArtifact = fact.RequestAsk, fact.ArtifactID
	case stored != nil && stored.RequestArtifact == fact.ArtifactID:
		gate.RequestAsk, gate.RequestArtifact = stored.RequestAsk, stored.RequestArtifact
	}
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
	tell := false
	if changesRequested {
		tell = !classify.DesignGateChangesTold(gate, gate.LatestVersion, fact.ChangesReason, fact.ChangesRequestedMark)
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
	if err := e.mirrorRegisteredRequest(ctx, tx, *issue, fact); err != nil {
		return intake.Result{}, err
	}
	switch {
	case changesRequested && !tell:
		e.logOnCommit(ctx, "workflow: the registration read a changes request the architect was already told of", "issue", issue.Key, "version", gate.LatestVersion)
		return intake.Result{}, nil
	case changesRequested:
		return intake.Result{}, e.tellChanges(ctx, tx, issue.Key, fact.ArtifactID, gate.LatestVersion, fact.ChangesReason, fact.ChangesRequestedMark)
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
	return e.notice(ctx, tx, key, record.Notice{Kind: "design-changes-requested", Version: version, Reason: reason})
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
	// The reset gate is of the same document, so it keeps the approval request its registration
	// bound, with that request's document.
	reset := record.DesignGate{Issue: issue.Key, ArtifactID: gate.ArtifactID, LatestVersion: fact.Read.Version, RequestAsk: gate.RequestAsk,
		RequestArtifact: gate.RequestArtifact}
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

// mirrorRegisteredRequest writes the mirror row of the approval request a registration read open,
// as the read found it (open, on the turn RequestWaitsOnAgent names), and records the ask as the
// gate's request (record.RootAsk.GateRequest), whatever order its events arrive in. The read's
// state is written at the issue's sequence the read began at (fact.Since), because Dispatch's
// approval read gives no sequence of its own: every event at or below Since is one the read
// reflects, so it never overwrites the read; every event between Since and the read's moment
// overwrites it whenever it arrives, and the newest of them is the state the read showed, so once
// they have all arrived the row is the read's or newer. A fence placed above Since could instead
// refuse, for good, an event newer than the read. Only a root's asks are mirrored.
func (e *Engine) mirrorRegisteredRequest(ctx context.Context, tx pgx.Tx, root record.Issue, fact intake.GateRegistered) error {
	if fact.RequestAsk == "" || !claim.IsTreeRoot(root.Key, root.Tree) {
		return nil
	}
	turn := "human"
	if fact.RequestWaitsOnAgent {
		turn = "agent"
	}
	return e.mirrorAsk(ctx, tx, root, fact.RequestAsk, askChange{seq: fact.Since.Seq, kind: record.AskApproval, gateRequest: true,
		state: askOpen, turn: turn})
}

// versionMovesRequest moves gate's approval request to the agent's turn for a new version of its
// document: Dispatch hands the request back to the agent once a version moves it past the one
// handed to the person. An event older than the turn the row holds, and a version with no sequence
// (the gate seed's read), change nothing.
func (e *Engine) versionMovesRequest(ctx context.Context, tx pgx.Tx, gate record.DesignGate, fact intake.DispatchArtifact) error {
	root, err := e.recordedRoot(ctx, tx, gate.Issue)
	if err != nil || root == nil {
		return err
	}
	return e.mirrorAsk(ctx, tx, *root, gate.BoundRequest(), askChange{seq: fact.Mark.Seq, kind: record.AskApproval, gateRequest: true, turn: "agent"})
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
// A version event of the gate's own document moves the request the gate binds for that document
// to the agent's turn in the mirror (versionMovesRequest), whatever it does to the gate, the design
// gate off included.
func (e *Engine) applyArtifact(ctx context.Context, tx pgx.Tx, gate record.DesignGate, fact intake.DispatchArtifact, carried bool) error {
	key := gate.Issue
	if fact.Kind == intake.DispatchArtifactVersion && !carried && gate.BoundRequest() != "" {
		if err := e.versionMovesRequest(ctx, tx, gate, fact); err != nil {
			return err
		}
	}
	if e.cfg.DesignGate == config.DesignGateOff {
		e.logOnCommit(ctx, "workflow: the design gate is off, so an artifact event changes no gate", "issue", key,
			"artifact", fact.ArtifactID, "event", fact.Kind, "version", fact.Version, "gate_version", gate.LatestVersion)
		if fact.Kind != intake.DispatchArtifactChangesRequested || carried {
			return nil
		}
		return e.tellChanges(ctx, tx, key, fact.ArtifactID, fact.Version, fact.Reason, fact.Mark)
	}
	if fact.Kind != intake.DispatchArtifactVersion && (fact.Version < gate.LatestVersion || classify.StaleReview(gate, fact.Version, fact.Mark)) {
		e.logOnCommit(ctx, "workflow: a review the person has already followed with another changes no gate state", "issue", key,
			"artifact", fact.ArtifactID, "event", fact.Kind, "version", fact.Version, "gate_version", gate.LatestVersion, "review_seq", fact.Mark.Seq)
		if fact.Kind != intake.DispatchArtifactChangesRequested || classify.ApprovalFollows(gate, fact.Version, fact.Mark) {
			return nil
		}
		e.logOnCommit(ctx, "workflow: the architect is told a changes request no approval followed", "issue", key, "version", fact.Version)
		return e.tellChanges(ctx, tx, key, fact.ArtifactID, fact.Version, fact.Reason, fact.Mark)
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
	tell := fact.Kind == intake.DispatchArtifactChangesRequested && !classify.DesignGateChangesTold(gate, fact.Version, fact.Reason, fact.Mark)
	if fact.Kind == intake.DispatchArtifactChangesRequested {
		updated = classify.RecordChangesTold(updated, fact.Version, fact.Reason, fact.Mark)
	}
	if err := e.store.PutGate(ctx, tx, updated); err != nil {
		return err
	}
	if tell {
		if err := e.tellChanges(ctx, tx, key, fact.ArtifactID, fact.Version, fact.Reason, fact.Mark); err != nil {
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
