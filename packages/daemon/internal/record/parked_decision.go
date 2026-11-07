package record

import (
	"context"
	"fmt"
	"slices"
	"strings"
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

// WaitsForSlot says whether root is a root handed to Legion that waits in the line for its tree
// to be opened, with no architect running for it yet: the one queued-root rule (QueuedRoot),
// read with the root's own design gate, not lingering, and holding no slot of its own.
//
// It is that rule and no other reading of it. "Status todo and no slot" used to say the same
// thing and no longer does: a running root holds none while it waits for its merge, in
// accepting or for its children, so a person's todo on such a root read here as a root with no
// architect, and every decision a person made on its tree was kept for a start that is never
// queued instead of reaching the architect that is running (RelayParkedDecision, workflow's
// CatchUpTheTreesOwed). The slot is still asked, after the rule rather than instead of it: a
// root at admitted whose gate has never opened is a queued root by its record and holds the
// slot its architect is writing the spec in, and that architect hears a decision itself.
func WaitsForSlot(ctx context.Context, tx pgx.Tx, store Store, root Issue) (bool, error) {
	if root.Lingers() || !claim.IsTreeRoot(root.Key, root.Tree) {
		return false, nil
	}
	gate, err := store.Gate(ctx, tx, root.Key)
	if err != nil {
		return false, err
	}
	if !QueuedRoot(root, GateValue(gate)) {
		return false, nil
	}
	slots, err := store.Slots(ctx, tx)
	if err != nil {
		return false, err
	}
	return !slices.ContainsFunc(slots, func(slot Slot) bool { return slot.Issue == root.Key }), nil
}

// ParkedTree says whether root is a tree Legion is not running that a person parked: it lingers
// past the close of its generation and its status has left the workflow (OutOfWorkflow). It is
// the one predicate that says a person's decision on an ask of that tree is the parked-decision
// relay's to send to the project's controller (RelayParkedDecision answers RelaySent for it,
// sjawhar/legion#2163's dispatchAskReply and dispatchAskClosed), and so the one the answer loop
// reads to send no second notice of its own for the same decision (the daemon's routedDecisions).
// A lingering root someone has moved back into the workflow is no parked tree, and the relay
// sends nothing for it (RelayNotParked).
func ParkedTree(root Issue) bool { return root.Lingers() && OutOfWorkflow(root.Status) }

// RelaysAskAnswer says whether the parked-decision relay sends a person's answer or resolution of
// an ask of kind, as the ask's own event names that kind: an ordinary question's and no other
// (workflow's dispatchAskClosed). An approval request's answer reaches the tree again as its
// document's review, which the design gate relays; an ask of a kind this daemon does not know —
// a legacy `action` ask (intake's askKind), or an event naming none — is relayed by nothing, so
// the answer loop passes that decision on itself, to the tree's architect or to the controller
// (the daemon's routedDecisions). A reply on an ask's thread is relayed whatever the ask's kind
// (workflow's dispatchAskReply), so no reply asks this.
func RelaysAskAnswer(kind string) bool { return kind == AskQuestion }

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
		if root.Lingers() && !ParkedTree(root) {
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

// DecisionKind is what a person did that a parked-decision notice relays.
type DecisionKind string

const (
	DecisionDesignApproved         DecisionKind = "design-approved"
	DecisionDesignChangesRequested DecisionKind = "design-changes-requested"
	DecisionCarriedGateOpened      DecisionKind = "carried-gate-opened"
	DecisionAskAnswered            DecisionKind = "ask-answered"
	DecisionAskResolved            DecisionKind = "ask-resolved"
	DecisionAskReply               DecisionKind = "ask-reply"
)

// SpecDecision is the decision a notice of kind reports when kind is one of the three a person's
// spec decision writes (workflow's gate.go), and whether it is. Each of the three names its own
// DecisionKind: the notice kinds and the decisions are two vocabularies, and nothing but this
// mapping ties them. The notice executor relays those three to the controller when nobody in the
// tree will hear them; every other kind is the machinery's, which a stopped tree needs nobody to
// hear.
func SpecDecision(kind NoticeKind) (DecisionKind, bool) {
	switch kind {
	case "design-approved":
		return DecisionDesignApproved, true
	case "design-changes-requested":
		return DecisionDesignChangesRequested, true
	case "carried-gate-opened":
		return DecisionCarriedGateOpened, true
	default:
		return "", false
	}
}

func validDecisionKind(kind DecisionKind) bool {
	switch kind {
	case DecisionDesignApproved, DecisionDesignChangesRequested, DecisionCarriedGateOpened,
		DecisionAskAnswered, DecisionAskResolved, DecisionAskReply:
		return true
	default:
		return false
	}
}

// PersonActor says whether actor, as Dispatch names an event's author (`<kind>:<id>`), is a person.
func PersonActor(actor string) bool {
	return strings.HasPrefix(actor, "user:") && len(actor) > len("user:")
}
