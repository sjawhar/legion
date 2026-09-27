package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// gateRegistered records the gate an architect registered for its root, and logs it. With the
// design gate off it is approved at once, and an open gate advances the admitted tree.
func (e *Engine) gateRegistered(ctx context.Context, tx pgx.Tx, fact intake.GateRegistered) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	gate := record.DesignGate{Issue: fact.Issue, ArtifactID: fact.ArtifactID, LatestVersion: fact.Version}
	if e.cfg.DesignGate == config.DesignGateOff {
		gate = classify.ApplyDesignGateEvent(gate, classify.DesignGateApproved, fact.Version)
	}
	if err := e.store.PutGate(ctx, tx, gate); err != nil {
		return intake.Result{}, err
	}
	logged := e.logOnCommit("workflow: design gate registered", "tree", issue.Tree, "issue", issue.Key, "artifact", fact.ArtifactID,
		"version", fact.Version, "open", classify.DesignGateOpen(gate), "policy", e.cfg.DesignGate)
	if err := e.enqueue(ctx, tx, fact.Issue, record.GateSeed{ArtifactID: fact.ArtifactID, Version: fact.Version, Generation: issue.Generation}); err != nil {
		return intake.Result{}, err
	}
	if !classify.DesignGateOpen(gate) {
		return logged, nil
	}
	if err := e.notice(ctx, tx, fact.Issue, record.Notice{Kind: "design-approved", Version: fact.Version}); err != nil {
		return intake.Result{}, err
	}
	if err := e.advanceAdmittedTree(ctx, tx, *issue, gate); err != nil {
		return intake.Result{}, err
	}
	return logged, nil
}

// dispatchArtifact applies an artifact event to the gate registered for that document: an approval
// opens it and advances the admitted tree, a changes request or a new version closes it. Each open
// or close is logged, and a changes request that finds the gate closed has a line of its own.
func (e *Engine) dispatchArtifact(ctx context.Context, tx pgx.Tx, fact intake.DispatchArtifact) (intake.Result, error) {
	gate, err := e.store.Gate(ctx, tx, fact.Key)
	if err != nil || gate == nil || gate.ArtifactID != fact.ArtifactID {
		return intake.Result{}, err
	}
	wasOpen := classify.DesignGateOpen(*gate)
	kind := classify.DesignGateEventKind(fact.Kind)
	updated := classify.ApplyDesignGateEvent(*gate, kind, fact.Version)
	if err := e.store.PutGate(ctx, tx, updated); err != nil {
		return intake.Result{}, err
	}
	isOpen := classify.DesignGateOpen(updated)
	var logged intake.Result
	switch {
	case isOpen != wasOpen:
		verb := "closed"
		if isOpen {
			verb = "opened"
		}
		logged = e.logOnCommit("workflow: design gate "+verb, "issue", fact.Key, "artifact", fact.ArtifactID, "event", fact.Kind, "version", fact.Version)
	case fact.Kind == intake.DispatchArtifactChangesRequested:
		logged = e.logOnCommit("workflow: design gate changes requested", "issue", fact.Key, "artifact", fact.ArtifactID, "version", fact.Version)
	}
	if fact.Kind == intake.DispatchArtifactChangesRequested {
		if err := e.notice(ctx, tx, fact.Key, record.Notice{Kind: "design-changes-requested", Version: fact.Version, Reason: fact.Reason}); err != nil {
			return intake.Result{}, err
		}
	}
	if !wasOpen && isOpen {
		if err := e.notice(ctx, tx, fact.Key, record.Notice{Kind: "design-approved", Version: fact.Version}); err != nil {
			return intake.Result{}, err
		}
		issue, err := e.store.Issue(ctx, tx, fact.Key)
		if err != nil || issue == nil {
			return intake.Result{}, err
		}
		if err := e.advanceAdmittedTree(ctx, tx, *issue, updated); err != nil {
			return intake.Result{}, err
		}
	}
	if err := e.advancePendingReady(ctx, tx, fact.Key, updated); err != nil {
		return intake.Result{}, err
	}
	return logged, nil
}

// logOnCommit is a result that writes one Info line once the fact's transaction commits
// (intake.Result.AfterCommit), so a fact retried after a failed commit writes it once.
func (e *Engine) logOnCommit(msg string, args ...any) intake.Result {
	return intake.Result{AfterCommit: []func(){func() { e.log.Info(msg, args...) }}}
}
