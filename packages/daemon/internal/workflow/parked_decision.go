package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// relayParkedDecision hands a person's answer, resolution or reply on ask of key to the one rule
// for a decision nobody in its tree hears (record.RelayParkedDecision), and logs one line for what
// became of it: relayed to the project's controller, which decides whether the tree runs again
// (skill://legion-controller/references/parked-decisions.md); kept alone for the next architect
// start of a root that waits for its slot, whose tree already runs again; not relayed, for a
// session's write on a parked tree or on a root waiting for its slot, or for a lingering root
// someone moved back into the workflow. A decision on a tree that runs, which hears it itself, and
// an ask of an issue Legion does not record log nothing: that is every ordinary ask. The writes are
// in the event's own transaction, which ApplyFact keys by the Dispatch event's id, so a replay
// writes none of them again.
func (e *Engine) relayParkedDecision(ctx context.Context, tx pgx.Tx, key, ask string, decision record.ParkedDecision) error {
	root, err := e.treeRoot(ctx, tx, key)
	if err != nil || root == nil {
		return err
	}
	outcome, err := record.RelayParkedDecision(ctx, tx, e.store, key, *root, root.Lingers(), decision, 0, "")
	if err != nil {
		return err
	}
	args := []any{"issue", key, "root", root.Key, "ask", ask, "decision", decision.Decision, "ref", decision.Ref, "actor", decision.Actor}
	switch outcome {
	case record.RelaySent:
		e.logOnCommit(ctx, "workflow: a person's decision on a parked tree goes to the controller", args...)
	case record.RelayKept:
		e.logOnCommit(ctx, "workflow: a person's decision on a tree waiting for its slot is kept for its architect's start", args...)
	case record.RelayNoPerson:
		if decision.Decision == record.DecisionAskReply {
			e.logOnCommit(ctx, "workflow: a session's reply on a parked tree is not relayed", args...)
		} else {
			e.logOnCommit(ctx, "workflow: a session's answer or resolution on a parked tree is not relayed", args...)
		}
	case record.RelayNoPersonWaiting:
		e.logOnCommit(ctx, "workflow: a session's decision on a tree waiting for its slot is not kept", args...)
	case record.RelayNotParked:
		e.logOnCommit(ctx, "workflow: a decision on a lingering root someone moved back into the workflow is not relayed", args...)
	}
	return nil
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
// decision on an issue of its tree while nobody in the tree could hear it (relayParkedDecision,
// the outbox's parkDecision): each decision's ref, kind and author, newest first, so the architect
// reads what brought it back, as ResumeAwaitedRoot names an answered root's answer. Admission's
// start carries it as its task; every later launch's ready (claimReady: a relaunch, `legion claims
// retry`) adds what was kept since to its catch-up. When the re-admission found the root's spec
// approved at a version below its latest (the admission's readmit, Store.KeepReadmittedApproval: a
// person approved a version the spec has since moved past), it also names both versions, so the
// architect requests approval of the latest; never under gates.design off, which asks no person.
//
// What it may name is what root's tree is owed now: every row whose issue stands in that tree and
// that no counting claim or hold holds (Store.UnclaimedKept). A claim or a hold an issue left
// behind in another tree counts for nothing, so a child handed over, a parked child orphaned, a
// grandchild handed over beside its parent and a queued root adopted each bring what was decided
// on them to this task with nothing moved, in whatever order their admissions run.
//
// The text names no generation: a start's task waits on its claim and is prompted whenever that
// claim is next ready, which can be generations later, and the one architect session of the root
// reads it then as what a person decided on its tree. It deletes nothing: the caller enqueues the
// task with the record of what it named (record.KeptDecisions) and claims those rows for it in the
// same transaction (EnqueueNaming). A catch-up that published tells what it named
// (Store.ForgetKeptDecisions) and a start's task tells it at the confirmation of the delivery the
// claim queued it as; one that reached no architect hands it over (HandOverKept). Both results are
// zero when there was nothing.
func ParkedDecisionTask(ctx context.Context, tx pgx.Tx, store record.Store, gate config.DesignGate, root record.Issue) (string, *record.KeptDecisions, error) {
	decisions, approved, latest, err := store.UnclaimedKept(ctx, tx, root.Key)
	if err != nil {
		return "", nil, err
	}
	var kept record.KeptDecisions
	for _, d := range decisions {
		kept.Decisions = append(kept.Decisions, record.DecisionMark{Ref: d.Ref, At: d.At, Issue: d.Issue,
			Decision: d.Decision, Actor: d.Actor})
	}
	if approved > 0 && gate != config.DesignGateOff {
		kept.Approved, kept.Latest = approved, latest
	}
	task := KeptDecisionsTask(root.Key, kept)
	if task == "" {
		return "", nil, nil
	}
	return task, &kept, nil
}

// KeptDecisionsTask is the text that names kept to the architect of tree, the one wording a start
// task and a catch-up both use (ParkedDecisionTask) and the one a held task is written back to
// when an issue it names leaves that tree while the task waits to be sent (supervise's
// Deps.RootTask, the daemon's rootTask): a task whose decisions have all left is written back to
// "", which is a task that names nothing and is retired rather than sent. The approval pair is
// the root's own, so it is named whenever kept carries one — the gate was read where the pair was
// first kept.
func KeptDecisionsTask(root string, kept record.KeptDecisions) string {
	var parts []string
	if len(kept.Decisions) > 0 {
		named := make([]string, 0, len(kept.Decisions))
		for _, d := range kept.Decisions {
			named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
		}
		parts = append(parts, fmt.Sprintf("While nobody in %s's tree could hear it, a person decided on the tree: %s. Read each before you continue.",
			root, strings.Join(named, "; ")))
	}
	if kept.Approved > 0 {
		parts = append(parts, fmt.Sprintf("The spec is approved at version %d and is now at version %d: request approval of version %d, "+
			"as skill://legion-architect/references/design-gate.md (\"The armed gate\") says.", kept.Approved, kept.Latest, kept.Latest))
	}
	return strings.Join(parts, " ")
}

// ParkedDecisionTask is ParkedDecisionTask over the engine's store and design gate.
func (e *Engine) ParkedDecisionTask(ctx context.Context, tx pgx.Tx, root record.Issue) (string, *record.KeptDecisions, error) {
	return ParkedDecisionTask(ctx, tx, e.store, e.cfg.DesignGate, root)
}

// EnqueueNaming enqueues payload for issue, due at, a root-architect start or a catch-up notice
// whose task names kept of what tree is owed (ParkedDecisionTask), and claims those rows for that
// outbox row in the same transaction (Store.ClaimKeptDecisions), so no other start or catch-up of
// that tree names them until that row settles them: a catch-up that published tells them, a start
// whose task the claim queued leaves them to that delivery's confirmation, and a row that reached
// no architect hands them over (HandOverKept). A nil kept enqueues payload as enqueueAt does.
func EnqueueNaming(ctx context.Context, tx pgx.Tx, store record.Store, issue, tree string, payload record.OutboxPayload,
	kept *record.KeptDecisions, at time.Time) error {
	row, err := record.NewOutboxRow(issue, payload, at)
	if err != nil {
		return err
	}
	if kept == nil {
		return store.Enqueue(ctx, tx, row)
	}
	id, err := store.EnqueueID(ctx, tx, row)
	if err != nil {
		return err
	}
	return store.ClaimKeptDecisions(ctx, tx, tree, *kept, id)
}

// EnqueueNaming is EnqueueNaming over the engine's store.
func (e *Engine) EnqueueNaming(ctx context.Context, tx pgx.Tx, issue, tree string, payload record.OutboxPayload, kept *record.KeptDecisions, at time.Time) error {
	return EnqueueNaming(ctx, tx, e.store, issue, tree, payload, kept, at)
}

// HandOverKept is the one rule a release of kept decisions follows: a release never leaves a kept
// decision unclaimed while the tree it is owed to has an architect to tell. It unclaims
// everything outbox row from holds (Store.ReleaseKeptClaims) — from told no architect what it
// named — and writes a fresh catch-up to the architect of each tree the released rows are owed to
// now (CatchUpTheTreesOwed). That is read from the rows rather than from the row's own issue,
// because an issue the row named can have left for a tree of its own since the row was composed:
// the tree it stands in now is what owes the person an answer, and its architect is the one told
// at once. A row that holds nothing returns at once, so a row whose settle committed and whose
// finish did not writes no second catch-up when it runs again.
//
// Three finishes hand over, and only three: an architect start whose task reached no claim (the
// outbox's settleKept), a catch-up a newer one supersedes (catchUpSuperseded, the outbox's
// notice), and a receipt timeout on a catch-up the re-hold finds superseded, which queues no copy
// to carry its claims on (the daemon's reholdNotice, through the same supersededKept). Every other
// undelivered finish keeps the older rule — the row's deletion releases what it claimed, and the
// architect's next ready names every unclaimed decision in its catch-up. Each of those endings is
// one where the architect can take no notice at all now — its row was deleted under its lease, it
// is unroutable, its architect stopped with its finished tree or will not hold its role again, its
// holder refused the delivery or the listener's grant denied it — so a fresh catch-up would reach
// nobody either, and what the ending releases waits for that architect's next launch, whose ready
// names every unclaimed decision. A receipt timeout that draws a copy neither tells nor hands
// over: the re-held copy carries the claims on (Store.MoveKeptClaims).
func HandOverKept(ctx context.Context, tx pgx.Tx, store record.Store, gate config.DesignGate, from int64, now time.Time) error {
	trees, err := store.ReleaseKeptClaims(ctx, tx, from)
	if err != nil || len(trees) == 0 {
		return err
	}
	return CatchUpTheTreesOwed(ctx, tx, store, gate, trees, now)
}

// CatchUpTheTreesOwed writes a fresh catch-up to the architect of each of trees that has one to
// hear it now (ArchitectHears), naming everything that tree is owed and nothing claims, for the
// launch its architect claim stands at (Store.ArchitectLaunches), so a failed forward of an older
// catch-up is not queued again behind it. A tree with no architect to hear it — no record, no
// tree root, lingering, out of the workflow, or waiting in `todo` for its slot — is written
// nothing, and what it is owed waits KEPT for its next start or ready to name, since a catch-up
// queued for nobody would only be dropped by the ready that follows.
func CatchUpTheTreesOwed(ctx context.Context, tx pgx.Tx, store record.Store, gate config.DesignGate, trees []string, now time.Time) error {
	launches, err := store.ArchitectLaunches(ctx, tx, trees)
	if err != nil {
		return err
	}
	for _, key := range trees {
		root, err := store.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		if root == nil {
			continue
		}
		hears, err := ArchitectHears(ctx, tx, store, *root)
		if err != nil {
			return err
		}
		if !hears {
			continue
		}
		if err := QueueCatchUp(ctx, tx, store, gate.OrDefault(), *root, launches[key], now); err != nil {
			return err
		}
	}
	return nil
}

// ArchitectHears says whether root has an architect to hear a person's decision now: it is its
// tree's root, it does not linger, it has not left the workflow, and it does not wait in `todo`
// for an admission slot (record.WaitsForSlot). It is the one reading of that question the daemon
// has, so the hand-over of what a row named (HandOverKept) and the hold of a root architect's task
// that names decisions (supervise's Deps.RootHears, the daemon's rootHears) never drift: a root
// that hears is told at once, and one that does not keeps what it is owed until it runs again.
func ArchitectHears(ctx context.Context, tx pgx.Tx, store record.Store, root record.Issue) (bool, error) {
	if !claim.IsTreeRoot(root.Key, root.Tree) || root.Lingers() || record.OutOfWorkflow(root.Status) {
		return false, nil
	}
	waits, err := record.WaitsForSlot(ctx, tx, store, root)
	return !waits, err
}

// DesignGateOff says whether the engine runs with gates.design off, which asks no person to
// approve a spec.
func (e *Engine) DesignGateOff() bool { return e.cfg.DesignGate == config.DesignGateOff }

// specRef is the Dispatch reference of an issue's spec, the document its gate reviews.
func specRef(key string) string { return "dispatch://" + key + "/spec" }

// itemRef is the Dispatch reference of an ask or comment of an issue.
func itemRef(key, kind, id string) string { return "dispatch://" + key + "/" + kind + "/" + id }
