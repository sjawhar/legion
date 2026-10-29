package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// relayParkedDecision hands a person's answer, resolution or reply on an ask of key to the one
// rule for a decision nobody in its tree hears (record.RelayParkedDecision), and logs what became
// of it: relayed to the project's controller, which decides whether the tree runs again
// (skill://legion-controller/references/parked-decisions.md), or kept alone for the next architect
// start of a root that waits for its slot, whose tree already runs again. It returns the outcome,
// so a caller can log what the rule left to it (a session's reply), and the tree's root. The writes
// are in the event's own transaction, which ApplyFact keys by the Dispatch event's id, so a replay
// writes none of them again.
func (e *Engine) relayParkedDecision(ctx context.Context, tx pgx.Tx, key string, decision record.ParkedDecision) (record.RelayOutcome, string, error) {
	root, err := e.treeRoot(ctx, tx, key)
	if err != nil || root == nil {
		return record.RelayNone, "", err
	}
	outcome, err := record.RelayParkedDecision(ctx, tx, e.store, key, *root, root.Lingers(), decision, 0, "")
	if err != nil {
		return record.RelayNone, root.Key, err
	}
	args := []any{"issue", key, "root", root.Key, "decision", decision.Decision, "ref", decision.Ref, "actor", decision.Actor}
	switch outcome {
	case record.RelaySent:
		e.logOnCommit(ctx, "workflow: a person's decision on a parked tree goes to the controller", args...)
	case record.RelayKept:
		e.logOnCommit(ctx, "workflow: a person's decision on a tree waiting for its slot is kept for its architect's start", args...)
	}
	return outcome, root.Key, nil
}

// treeRoot is the record of the root of key's tree, nil when key is not recorded: key's own row
// when it is its tree's root, read once.
func (e *Engine) treeRoot(ctx context.Context, tx pgx.Tx, key string) (*record.Issue, error) {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || issue.Tree == key {
		return issue, err
	}
	return e.store.Issue(ctx, tx, issue.Tree)
}

// ParkedDecisionTask is what root's architect is told at its start when the daemon kept a person's
// decision on its tree while nobody in the tree could hear it (relayParkedDecision, the outbox's
// parkDecision): each decision's ref, kind and author, newest first, so the architect reads what
// brought it back, as ResumeAwaitedRoot names an answered root's answer. Admission's start carries
// it as its task; every later launch's ready (claimReady: a relaunch, `legion claims retry`) adds
// what was kept since to its catch-up. When the re-admission found the root's spec approved at a
// version below its latest (the admission's readmit, Store.KeepReadmittedApproval: a person
// approved a version the spec has since moved past), it also names both versions, so the architect
// requests approval of the latest; never under gates.design off, which asks no person. It names
// what the root keeps and deletes nothing: the record of what it named travels with the task
// (record.KeptDecisions), and the delivery of that task forgets exactly that
// (Store.ForgetKeptDecisions), so a start or catch-up that never reaches its architect leaves every
// decision for the next one to name. Both are zero when there was nothing.
func (e *Engine) ParkedDecisionTask(ctx context.Context, tx pgx.Tx, root record.Issue) (string, *record.KeptDecisions, error) {
	decisions, err := e.store.ParkedDecisions(ctx, tx, root.Key)
	if err != nil {
		return "", nil, err
	}
	approved, latest, err := e.store.ReadmittedApproval(ctx, tx, root.Key)
	if err != nil {
		return "", nil, err
	}
	var parts []string
	var kept record.KeptDecisions
	if len(decisions) > 0 {
		named := make([]string, 0, len(decisions))
		for _, d := range decisions {
			named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
			kept.Decisions = append(kept.Decisions, record.DecisionMark{Ref: d.Ref, At: d.At})
		}
		parts = append(parts, fmt.Sprintf("While nobody in %s's tree could hear it, a person decided on the tree: %s. Read each before you continue.",
			root.Key, strings.Join(named, "; ")))
	}
	if approved > 0 && e.cfg.DesignGate != config.DesignGateOff {
		parts = append(parts, fmt.Sprintf("The spec is approved at version %d and is now at version %d: request approval of version %d, "+
			"as skill://legion-architect/references/design-gate.md (\"The armed gate\") says.", approved, latest, latest))
		kept.Approved, kept.Latest = approved, latest
	}
	if len(parts) == 0 {
		return "", nil, nil
	}
	return strings.Join(parts, " "), &kept, nil
}

// specRef is the Dispatch reference of an issue's spec, the document its gate reviews.
func specRef(key string) string { return "dispatch://" + key + "/spec" }

// itemRef is the Dispatch reference of an ask or comment of an issue.
func itemRef(key, kind, id string) string { return "dispatch://" + key + "/" + kind + "/" + id }
