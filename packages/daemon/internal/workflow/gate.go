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
// given before the architect registered is applied here: an approval of the latest version
// (ApprovedVersion) records the gate open, and the root starts planning; a changes request on it
// (ChangesRequestedVersion) records it closed with the architect told, unless the gate already
// records that same review as told. The architect is told the version the gate opened at, which an
// approval carried to a later version makes the later one.
func (e *Engine) gateRegistered(ctx context.Context, tx pgx.Tx, fact intake.GateRegistered) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
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
	// than anything that read could show: the person acted after it. The gate keeps it, so a read
	// that predates a told changes request does not tell the architect again, and one that predates
	// an approval does not close the gate the approval opened.
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
	switch {
	case changesRequested && !tell:
		e.logOnCommit(ctx, "workflow: the registration read a changes request the architect was already told of", "issue", issue.Key, "version", gate.LatestVersion)
		return intake.Result{}, nil
	case changesRequested:
		return intake.Result{}, e.notice(ctx, tx, fact.Issue, record.Notice{Kind: "design-changes-requested", Version: gate.LatestVersion, Reason: fact.ChangesReason})
	case !classify.DesignGateOpen(gate):
		return intake.Result{}, nil
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
// that repeats an earlier one's version and wording still reaches it). Whatever reaches the
// architect is recorded on the gate, so the registration and the gate seed, which both re-read
// Dispatch, never tell it twice and never stay silent about a request nothing else carried.
//
// A review the person has already followed with another changes no gate state: one of a version
// below the gate's latest (the person has a newer version to review), or one at that version whose
// mark is not past the review the gate applied last (classify.StaleReview). The daemon applied the
// newer state first, and this review reached it late, so it neither moves the gate nor tells the
// architect. That is an approval delivered after the changes request that superseded it, which
// would otherwise reopen the gate; a changes request delivered after a newer approval, which would
// otherwise close it; and an approval or changes request of an older version, which would
// otherwise clear the told record or close a gate open at the newer one.
//
// With the design gate off, no artifact event moves a gate: the gate is approved at registration
// (gateRegistered), and a review or a version of the document it names must not close it.
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
		return nil
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
	if tell {
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
