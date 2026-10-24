package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// relayParkedDecision tells the project's controller a person's answer, resolution or reply on an
// ask of an issue whose tree lingers or has closed, and keeps it for the root's next architect start
// (ParkedDecisionTask): the tree's architect is suspended or gone, so nobody inside the tree hears
// it, and the controller decides whether the tree runs again
// (skill://legion-controller/references/parked-decisions.md). On a tree whose root waits for its
// slot (re-admitted, its architect not started yet), it only keeps the decision: the tree already
// runs again, and its architect's start names it. A session's write, the daemon's included, is no
// person's decision and relays nothing. A session's reply on a parked tree is logged: it stays on
// the Dispatch ask for the person who asked, and the controller's walk skips it. The writes are in
// the event's own transaction, which ApplyFact keys by the Dispatch event's id, so a replay writes
// none of them again.
func (e *Engine) relayParkedDecision(ctx context.Context, tx pgx.Tx, key, ask string, actor dispatch.Actor, decision record.DecisionKind, ref string) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return err
	}
	root, err := e.store.Issue(ctx, tx, issue.Tree)
	if err != nil || root == nil {
		return err
	}
	waiting := false
	if !root.Lingers() {
		if waiting, err = e.waitsForSlot(ctx, tx, *root); err != nil || !waiting {
			return err
		}
	}
	name := actor.Name()
	if !record.PersonActor(name) {
		if decision == record.DecisionAskReply && !waiting {
			e.logOnCommit(ctx, "workflow: a session's reply on a parked tree is not relayed", "issue", key, "root", root.Key, "ask", ask, "actor", name)
		}
		return nil
	}
	if err := e.store.AddParkedDecision(ctx, tx, record.ParkedDecision{Root: root.Key, Ref: ref, Decision: decision, Actor: name, At: e.now()}); err != nil {
		return err
	}
	if waiting {
		e.logOnCommit(ctx, "workflow: a person's decision on a tree waiting for its slot is kept for its architect's start",
			"issue", key, "root", root.Key, "decision", decision, "ref", ref, "actor", name)
		return nil
	}
	if err := e.enqueue(ctx, tx, key, record.ControllerNotice{Kind: record.ParkedDecisionNotice, Decision: decision, Root: root.Key, Ref: ref, Actor: name}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a person's decision on a parked tree goes to the controller",
		"issue", key, "root", root.Key, "decision", decision, "ref", ref, "actor", name)
	return nil
}

// waitsForSlot says whether root is a root handed to Legion that waits in `todo` for an admission
// slot, with no architect running for its tree yet: not lingering, not waiting on a person (whose
// answer resumes it with a task of its own, ResumeAwaitedRoot), and holding no slot.
func (e *Engine) waitsForSlot(ctx context.Context, tx pgx.Tx, root record.Issue) (bool, error) {
	if !root.HandedOver || root.Status != "todo" || root.Lingers() || root.AwaitingPerson() || !claim.IsTreeRoot(root.Key, root.Tree) {
		return false, nil
	}
	admitted, err := e.admittedRoot(ctx, tx, root)
	return !admitted, err
}

// ParkedDecisionTask is what root's architect is told at its start when the daemon kept a person's
// decision on its tree while nobody in the tree could hear it (relayParkedDecision, the outbox's
// parkDecision): each decision's ref, kind and author, newest first, so the architect reads what
// brought it back, as ResumeAwaitedRoot names an answered root's answer. Admission's start carries
// it as its task; every later launch's ready (claimReady: a relaunch, `legion claims retry`) adds
// what was kept since to its catch-up. When the re-admission found the root's spec approved at a
// version below its latest (the admission's readmit, Store.KeepReadmittedApproval: a person
// approved a version the spec has since moved past), it also names both versions, so the architect
// requests approval of the latest; never under gates.design off, which asks no person. It takes
// what it names, so a later start names only what came after; "" when there was nothing.
func (e *Engine) ParkedDecisionTask(ctx context.Context, tx pgx.Tx, root record.Issue) (string, error) {
	decisions, err := e.store.TakeParkedDecisions(ctx, tx, root.Key)
	if err != nil {
		return "", err
	}
	approved, latest, err := e.store.TakeReadmittedApproval(ctx, tx, root.Key)
	if err != nil {
		return "", err
	}
	var parts []string
	if len(decisions) > 0 {
		named := make([]string, 0, len(decisions))
		for _, d := range decisions {
			named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
		}
		parts = append(parts, fmt.Sprintf("While nobody in %s's tree could hear it, a person decided on the tree: %s. Read each before you continue.",
			root.Key, strings.Join(named, "; ")))
	}
	if approved > 0 && e.cfg.DesignGate != config.DesignGateOff {
		parts = append(parts, fmt.Sprintf("The spec is approved at version %d and is now at version %d: request approval of version %d, as skill://legion-architect section 1 says.",
			approved, latest, latest))
	}
	return strings.Join(parts, " "), nil
}

// specRef is the Dispatch reference of an issue's spec, the document its gate reviews.
func specRef(key string) string { return "dispatch://" + key + "/spec" }

// itemRef is the Dispatch reference of an ask or comment of an issue.
func itemRef(key, kind, id string) string { return "dispatch://" + key + "/" + kind + "/" + id }
