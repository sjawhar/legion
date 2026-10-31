package record

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// RelayOutcome is what RelayParkedDecision did with one decision, which is what its caller logs.
type RelayOutcome int

const (
	// RelayNone is a decision nobody outside its tree needs: the tree runs and hears it itself.
	// Nothing is kept and nobody is told.
	RelayNone RelayOutcome = iota
	// RelayKept is a person's decision on a tree whose root waits for its slot: it is kept for the
	// root's next architect start, which names it (workflow.Engine.ParkedDecisionTask), and the
	// controller is not told, since the tree already runs again.
	RelayKept
	// RelaySent is a person's decision on a parked tree: kept for the root's next start and relayed
	// to the project's controller, which decides whether the tree runs again.
	RelaySent
	// RelayNoPerson is a decision on a parked tree that no person made — a session's, or the
	// daemon's own — which is nobody's to relay.
	RelayNoPerson
	// RelayNoPersonWaiting is a decision no person made on a tree whose root waits for its slot:
	// nothing is kept, since only a person decides, and nobody is told.
	RelayNoPersonWaiting
	// RelayNotParked is a decision on a lingering root someone has since moved to a status of the
	// workflow (admission's releaseUnslotted leaves one at in_progress): it is nobody's parked
	// tree, so nothing is kept and nobody is told.
	RelayNotParked
)

// WaitsForSlot says whether root is a root handed to Legion that waits in `todo` for an admission
// slot, with no architect running for its tree yet: not lingering, not waiting on a person (whose
// answer resumes it with a task of its own, workflow's ResumeAwaitedRoot), and holding no slot.
func WaitsForSlot(ctx context.Context, tx pgx.Tx, store Store, root Issue) (bool, error) {
	if !root.HandedOver || root.Status != "todo" || root.Lingers() || root.AwaitingPerson() || !claim.IsTreeRoot(root.Key, root.Tree) {
		return false, nil
	}
	slots, err := store.Slots(ctx, tx)
	if err != nil {
		return false, err
	}
	return !slices.ContainsFunc(slots, func(slot Slot) bool { return slot.Issue == root.Key }), nil
}

// RelayParkedDecision is the one rule for a decision made where nobody in its tree hears it, which
// both paths that find one follow: the workflow's answer, resolution or reply on an ask of a tree
// nobody runs (workflow's ask_mirror.go), and the outbox's spec decision whose notice no architect
// took (the daemon's parkDecision). issue is where in Dispatch the decision was made, root its
// tree's root, and nobodyHears says the caller found nobody inside the tree to hear it. The rules,
// in order:
//
//   - A root that waits for its slot (WaitsForSlot) keeps a person's decision for the start its
//     admission will queue, and the controller is told nothing: the tree already runs again.
//   - A tree that runs hears the decision itself (RelayNone), and a lingering root someone has
//     moved back into the workflow is nobody's parked tree (RelayNotParked): either keeps nothing.
//   - Only a person decides (PersonActor): a session's write, the daemon's own included, is no
//     decision to relay (RelayNoPerson).
//   - What is kept is kept under issue, the issue it was decided on, never under a root: the tree
//     that owes the person an answer is the tree that issue stands in whenever a start or a
//     catch-up names it, so a child handed over into a tree of its own, a parked child a person
//     orphans and a queued root adopted into a live tree take what was decided on them with them,
//     with nothing to move. It is kept until the start or catch-up that named it reached an
//     architect of that tree (Store.ForgetKeptDecisions, or the confirmation of the delivery
//     holding it); one that reached none hands it over to a fresh catch-up
//     (workflow.HandOverKept), so it is never left unclaimed while its tree has an architect to
//     tell. The root is read for the two answers that are the root's own: whether it waits for its
//     slot, and the one parked-decision controller notice that names it, the decision, where in
//     Dispatch, who made it, and a spec decision's version and a changes request's reason.
//
// Every write is in the caller's transaction, which the caller keys so that a replay writes none of
// them again.
func RelayParkedDecision(ctx context.Context, tx pgx.Tx, store Store, issue string, root Issue, nobodyHears bool,
	decision ParkedDecision, version int, reason string) (RelayOutcome, error) {
	waiting, err := WaitsForSlot(ctx, tx, store, root)
	if err != nil {
		return RelayNone, err
	}
	if !waiting {
		if !nobodyHears {
			return RelayNone, nil
		}
		if root.Lingers() && !OutOfWorkflow(root.Status) {
			return RelayNotParked, nil
		}
	}
	if !PersonActor(decision.Actor) {
		if waiting {
			return RelayNoPersonWaiting, nil
		}
		return RelayNoPerson, nil
	}
	decision.Issue = issue
	if err := store.AddParkedDecision(ctx, tx, decision); err != nil {
		return RelayNone, err
	}
	if waiting {
		return RelayKept, nil
	}
	row, err := NewOutboxRow(issue, ControllerNotice{Kind: ParkedDecisionNotice, Decision: decision.Decision, Root: root.Key,
		Ref: decision.Ref, Actor: decision.Actor, Version: version, Reason: reason}, decision.At)
	if err != nil {
		return RelayNone, err
	}
	if err := store.Enqueue(ctx, tx, row); err != nil {
		return RelayNone, err
	}
	return RelaySent, nil
}

// DecisionMark identifies one kept decision an architect's start task or catch-up named: the
// issue it was decided on, its Dispatch reference and the time of the decision the row held then
// (ParkedDecision.At) are the row's identity — a newer decision a person made on the same
// reference since replaces that row with a later time, so the telling leaves it for the next
// start or catch-up to name — and its kind and its author are what the task's text says of it
// (workflow.KeptDecisionsTask), which is rewritten from these marks whenever a held task is
// narrowed at send time (supervise's Deps.RootTask). The issue is also what says which tree the
// decision is owed to now, which is what that narrowing and the telling both read.
type DecisionMark struct {
	Ref      string       `json:"ref"`
	At       time.Time    `json:"at"`
	Issue    string       `json:"issue,omitempty"`
	Decision DecisionKind `json:"decision,omitempty"`
	Actor    string       `json:"actor,omitempty"`
}

// KeptDecisions is what one architect start task or catch-up named of what its tree is owed
// (workflow.Engine.ParkedDecisionTask): each decision, and the pair of approved and latest spec
// versions when it named one. It travels with the task (SuperviseRequest.Kept, CatchUp.Kept) and
// is persisted with the delivery a claim queues it as, which is what says which rows that
// delivery holds. A published catch-up tells exactly what it named (Store.ForgetKeptDecisions),
// and a queued start's task tells it at its delivery's confirmation. A start whose task reached
// no claim, and a catch-up a newer one supersedes, hand every row over instead
// (workflow.HandOverKept): a fresh catch-up names them while the tree each is owed to has an
// architect to tell, and otherwise they wait, KEPT, for the next start or ready to name them.
type KeptDecisions struct {
	Decisions []DecisionMark `json:"decisions,omitempty"`
	Approved  int            `json:"approved,omitempty"`
	Latest    int            `json:"latest,omitempty"`
}

// KeptToldSource is the processed-event source a catch-up marks, under the dedupe key its publish
// went out on (Notice.Published, which a re-held copy shares with the row it copies, since one key
// is one telling however many rows publish it), once that publish has told the architect what it
// named — before it forgets those rows, in a transaction of its own. Only that mark says the
// architect was told when the forget fails or the daemon dies before its commit: the row's next run
// then forgets what it named rather than hand it over, and no drop of the root's queued catch-ups
// takes the row away before it does (Store.DropCatchUps, whose SQL computes that same key from the
// row it names, its original for a copy). A start needs none: its claim records the row with the
// delivery (supervise.Claim.DeliveredRows).
const KeptToldSource = "kept-told"

func (k KeptDecisions) validate() error {
	for _, decision := range k.Decisions {
		if decision.Ref == "" || decision.At.IsZero() {
			return fmt.Errorf("a kept decision names its Dispatch reference and the time it was kept at")
		}
	}
	if (k.Approved != 0 || k.Latest != 0) && (k.Approved <= 0 || k.Latest <= k.Approved) {
		return fmt.Errorf("a kept approval names a version a person approved below the spec's latest version")
	}
	if len(k.Decisions) == 0 && k.Approved == 0 {
		return fmt.Errorf("a kept-decisions record names nothing")
	}
	return nil
}
