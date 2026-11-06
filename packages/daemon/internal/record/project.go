package record

import (
	"context"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// RankLess compares Dispatch's fractional key, compared as bytes — the order rank.Between
// generates. Equal keys are ordered by issue key.
func RankLess(a, b Issue) bool {
	if a.Rank == b.Rank {
		return a.Key < b.Key
	}
	return a.Rank < b.Rank
}

// DoneStatus is the one Dispatch status that finishes an issue. Nothing else does: an issue
// moved to backlog or icebox has stopped, not finished, and an issue waiting on it still waits
// (blockers.Unfinished). It lives here because the rules that read a record's own status for
// the same question are here too (cycleAncestor).
const DoneStatus = "done"

// OutOfWorkflow says whether a Dispatch status takes an issue out of the workflow: every status
// but todo that no phase writes. Admission holds no slot for such an issue, and the engine leaves
// it; todo admits, and in_progress, testing, needs_review and retro are the workflow's own.
func OutOfWorkflow(status string) bool {
	switch status {
	case "triage", "icebox", "backlog", "done":
		return true
	default:
		return false
	}
}

// InWorkflow says whether an issue is in the workflow: its Dispatch status is one the workflow
// runs in (OutOfWorkflow) and its phase is short of done. One still admitted counts, since it
// will run — a root reopened inside its linger (admit's readmit) stands in admitted with no
// role's phase until its architect reaches planning, and is running again from the reopen. It is
// the production check's "runs again" predicate (workflow's check.go) and the outbox's
// (checkLookHolds): a look of an issue that runs again would resume the implementer and relaunch
// its Sandbox under the run, so none starts.
//
// It lives here, beside the status rule it is written over, because every judgement that reads
// the daemon's own records against Dispatch's mirror needs it — the children-first wait
// (ChildOpen), the acceptance order (OpenBeneath), the state's projection and the API's
// refusals among them — and internal/workflow, where it used to live, is the one package
// several of those may not read.
//
// The rule itself is pods.Standing.InWorkflow, which the pod gate reads with the issue's linger
// (pods.InWorkflowNotLingering) and this reads without it: one statement, so a terminal phase
// added to it moves admission, the linger, the production check and the release gate together.
func InWorkflow(issue Issue) bool {
	return issue.Standing().InWorkflow(OutOfWorkflow)
}

// StandsIn says whether issue stands in one of phases, counting a hold from one of them as
// standing there: a worker whose claim fails leaves its issue at phase.Held with Hold.From
// naming the phase it was working (workflow's claimFailed), and the work has reached exactly
// where it had before. It is the one shape every question about where an issue stands is
// written over — Accepted here, and the workflow's own awaitsMerge, its close of a merged
// issue and its open review round — so no two of those questions read a held issue differently.
func StandsIn(issue Issue, phases ...phase.Phase) bool {
	at := issue.Phase
	if at == phase.Held && issue.Hold != nil {
		at = issue.Hold.From
	}
	return slices.Contains(phases, at)
}

// Accepted says whether the daemon has already accepted issue's own reviewed head and the issue
// has not closed: it stands in its retro, its merge or its wait for that merge, or is held from
// one of them (StandsIn). Work entering the workflow beneath an issue in one of those phases
// would let that issue merge first, which is the one thing a parent must not do
// (dispatch://INFRA-980, "Children first"). The reviewed head of a root whose worker died in
// retro, merging or awaiting_merge is accepted still, so the hold must not let work in beneath
// it: the line held no issue under such a root, the two doors let one in, and the close of a
// root held from retro handed over nobody.
//
// The three phases are named here and nowhere else, so that no rule of the acceptance order can
// read a list of its own: one that did is what let a child held from retro through. Every one
// of those rules asks this predicate instead, through the nearest accepted ancestor
// (AcceptedAncestor): the waiting line's hold (HeldUnderAccepted), the doors' refusal (api's
// CHILD_UNDER_ACCEPTED_PARENT, AcceptedAncestorIn) and the wait a merged issue's close keeps
// (OpenBeneath, which leaves out what that hold stopped) — so none of them can count a root as
// accepted that another counts as still at work.
func Accepted(issue Issue) bool {
	return StandsIn(issue, phase.Retro, phase.Merging, phase.AwaitingMerge)
}

// Ancestors is every issue above issue inside its own tree, nearest first and the tree's own
// root last: the one parent-chain walk every rule that reads "above" is written over, so that
// no two of them answer differently about the same chain. AcceptedAncestor, Above, descends and
// cycleAncestor all read it. byKey is the records the caller holds the chain in — a tree's
// members for a reader of one tree, the project's own for a reader of a whole pass.
//
// The walk ends at the tree's own root, which stands above every member of the tree whatever
// each member's record says it hangs under — the same reading Beneath gives a root, whose
// descendants are every other member of its tree. A chain that leaves byKey, or loops back on
// itself, ends there and the root is read next: a gap in the chain is a gap, not the end of it,
// and the daemon must not judge an issue by how much of the way up to its root it can see.
// Nothing above the root is read at all, because every rule here is a rule about one tree, and
// a root's own Dispatch parent belongs to another: a root has no ancestors here, so the walk is
// never entered for one. A root's recorded parent is a Dispatch fact about where the work was
// filed, not a place in a tree — a root that was handed over keeps the parent it was filed
// under, in a tree that adopted nothing — and read as a chain it put the root's own first phase
// under another tree's acceptance (HeldUnderAccepted) and named a link into that tree a cycle
// (cycleAncestor). The one rule that does read a root's Dispatch chain reads the chain the
// record carries rather than these records (Issue.Ancestry, the ancestry hold in Waiting).
//
// Which records the caller holds is therefore the one thing that can still make two readers
// disagree, and the callers do not hold the same ones: the waiting line is read over the
// project's live records (Store.LiveIssues, which leaves out an issue already done), while the
// two doors that refuse work beneath an accepted ancestor are read over the whole tree
// (Store.TreeIssues, AcceptedAncestorOf, OpenBeneath). So the line's pass reads the records of
// the ancestors its live read leaves out and walks the chain over both (LineFacts.Parents,
// Store.AncestorsOf): without them a parent already done hid every issue above it, and
// admission offered a first-phase slot to a child the doors hold.
func Ancestors(issue Issue, byKey map[string]Issue) []Issue {
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		return nil
	}
	above := make([]Issue, 0, 4)
	seen := map[string]bool{issue.Key: true}
	for key := issue.Parent; key != nil && !seen[*key]; {
		seen[*key] = true
		parent, recorded := byKey[*key]
		if !recorded {
			break
		}
		above = append(above, parent)
		if parent.Key == issue.Tree {
			return above
		}
		key = parent.Parent
	}
	if root, recorded := byKey[issue.Tree]; recorded && !seen[issue.Tree] {
		above = append(above, root)
	}
	return above
}

// chainOf is the records one admission pass walks its parent chains over (Ancestors): the pass's
// own records, by key, and the records above them that its read leaves out (LineFacts.Parents),
// which is how a walk crosses an ancestor already done rather than stopping at it. It is one
// statement of the one map, because the two walks of a pass must answer alike about one chain —
// the line's hold under an accepted ancestor (Waiting) and the children-first cycle
// (ChildWaits) — and because a reader of either must not mistake it for the pass's own records,
// which say what runs and what each issue ranks against: a done ancestor says neither.
func chainOf(byKey, above map[string]Issue) map[string]Issue {
	chain := make(map[string]Issue, len(byKey)+len(above))
	maps.Copy(chain, byKey)
	maps.Copy(chain, above)
	return chain
}

// AcceptedAncestor is the nearest issue above issue (Ancestors) that the daemon has already
// accepted (Accepted, which counts an ancestor held from one of those phases), and "" when no
// ancestor has been.
//
// A parent already done does not end the walk: the spec's grandchild under a done parent is held
// by whichever issue above that parent is still merging, which is what the walk goes on to find.
// A done ancestor holds nothing itself — its merge has happened, so there is no order left to
// keep. The hold and the acceptance rules answer alike, which is what keeps them from
// deadlocking: OpenBeneath leaves out the issues this hold has stopped, so an issue the line
// holds is an issue the ancestor's own close does not wait for, and the expiry of the linger of
// the tree that held it hands it over as a root of its own, as it hands over every member that
// never started (workflow's unfinishedHandOvers).
func AcceptedAncestor(issue Issue, byKey map[string]Issue) string {
	for _, above := range Ancestors(issue, byKey) {
		if Accepted(above) {
			return above.Key
		}
	}
	return ""
}

// HeldUnderAccepted says whether the daemon is itself holding issue where it stands because an
// ancestor of it has already been accepted (AcceptedAncestor): it stands at phase.Admitted, so
// nothing of it has started, and the line will offer it no slot until that ancestor closes
// (Waiting). It is one rule with two readers that must agree exactly: OpenBeneath leaves such an
// issue out too, so the ancestor's own acceptance and close are not deadlocked by the issue its
// hold stopped. The hold never outlives the ancestor that caused it: once that ancestor closes,
// the line reads this rule again and finds no hold, so the issue starts in its running tree; and
// the expiry of a closed root's linger hands it to Legion as a root of its own without reading
// this rule (workflow's unfinishedHandOvers), since the expiry hands over every member that is in
// the workflow and never started, whatever held it.
func HeldUnderAccepted(issue Issue, byKey map[string]Issue) bool {
	return issue.Phase == phase.Admitted && AcceptedAncestor(issue, byKey) != ""
}

// LineKind is why an issue stands in the waiting line, which is also what its promotion does with
// it: the line is a line of issues now that the admission cap counts issues rather than trees, and
// the three reasons an issue can be in it are promoted three different ways (admit's promoteHolds).
type LineKind int

const (
	// LineReturning is an issue that held a slot before and has work again: a transition applied
	// in full and left only the worker's start waiting for a slot (SuperviseRequest.NeedsSlot).
	// Its promotion grants the slot and nothing else — the held start becomes claimable the
	// moment the slot row exists, and the grant's own lane wake delivers it.
	LineReturning LineKind = iota + 1
	// LineAnswered is a root that waited on a person and was answered. Its tree already runs; its
	// promotion resumes its architect where it stands (workflow's ResumeAwaitedRoot).
	LineAnswered
	// LineQueuedRoot is a root whose tree has not been opened: one that never ran, one a person
	// handed back to Legion after it finished, and one handed over from another tree. Its
	// promotion opens the tree lifecycle and hands the architect its tree.
	LineQueuedRoot
	// LineFirstPhase is an issue of a tree that already runs, standing at admitted with its
	// tree's design gate open, whose first phase can start: a released child, and the root itself
	// once its own work comes round. Its promotion starts that phase as the gate's opening used
	// to (workflow's gateOpened).
	LineFirstPhase
)

// LineEntry is one issue of the waiting line with the reason it waits there.
type LineEntry struct {
	Issue Issue
	Kind  LineKind
}

// LineFacts is everything the waiting line and the set of live trees are read from beyond the
// project's own issue records: the tree lifecycles, the roots' design gates, the records above
// the pass's own that its live read leaves out, and the worker starts the project already owes.
// They are gathered once per admission pass (ReadLineFacts) and handed to every judgement that
// pass makes, because each of them is read under the one fact lock (intake.ApplyFact) on every
// fact of every consumer: a read per tree, per gate or per slot would cost the daemon a round
// trip for each of them on each fact.
type LineFacts struct {
	// Lifecycles is the lifecycle of each tree root of the records, by root key. A tree with no
	// row has no entry, which reads as a tree nothing has opened.
	Lifecycles map[string]treelifecycle.Lifecycle
	// Gates is the design gate of each tree root of the records, by root key; a root with no gate
	// registered has no entry.
	Gates map[string]DesignGate
	// Owed is the oldest pending start each issue owes its worker, by issue; an issue owing none
	// has no entry. OwedRows is every one of those rows, oldest first, which the repair of a row
	// that has outlived its fences walks (admit's finishFencedStarts).
	Owed     map[string]OwedStart
	OwedRows []OwedStart
	// Parents is the record of each issue on a parent chain above the pass's own records that
	// those records leave out — an ancestor already done — by key, so that the one chain walk
	// (Ancestors) crosses a done parent rather than stopping at it. A pass whose records hold
	// every parent they name reads nothing for it and has no entry here.
	Parents map[string]Issue
	// Held is the set of issues a Dispatch blocker holds where they stand, by key: the issues
	// whose first phase the line must not offer a slot to, however long they have waited. Only
	// the caller can fill it — the judgement is blockers.Holds over the project's mirror rows,
	// and internal/blockers reads this package, so this package may not read it back — and a
	// caller that fills it with nothing says that nothing is held (blockers.HeldIssues).
	Held map[string]struct{}
	// ChildWaits is the children-first judgement of this pass, by parent key: the issues whose
	// own first phase the line must not offer a slot to because Dispatch still shows an open
	// child under them. Only the caller can fill it — the judgement reads the Dispatch mirror's
	// parent column against the daemon's own records, which takes the transaction the line itself
	// is never given (ChildWaitsOf) — and a caller that fills it with nothing says that nothing
	// waits for its children. It is never stored: each admission pass computes it again, so a
	// mirror row that has gone stale costs one pass and no more.
	ChildWaits map[string]ChildWait
}

// ChildWait is why one parent's own first phase waits: the open Dispatch children under it, in
// Dispatch key order. It is data alone — the rule that fills it is ChildWaits — so that the
// waiting line, the state's `blocked` and the architect's notice all read one judgement.
type ChildWait struct {
	Parent string
	Open   []OpenChild
}

// OpenChild is one Dispatch child that holds its parent's first phase: its key, the Dispatch
// status it holds it with, and, when a Dispatch blocked_by link of the child names an issue above
// it on its own parent chain, that ancestor. Such a cycle is named and nothing more: breaking the
// link is a person's decision.
type OpenChild struct {
	Key    string
	Status string
	Cycle  string
}

// treeRoots is the key of every tree root among issues, which is what a read of the trees of a
// whole pass is keyed by: their lifecycles and their design gates alike.
func treeRoots(issues []Issue) []string {
	roots := make([]string, 0, len(issues))
	for _, issue := range issues {
		if claim.IsTreeRoot(issue.Key, issue.Tree) {
			roots = append(roots, issue.Key)
		}
	}
	return roots
}

// ReadLifecycles is the lifecycle of each tree root among issues, by root key, in one read
// whatever the number of trees: ReadLineFacts's first read, and the whole of what a judgement
// that asks only whether a tree runs needs (workflow.Stranded). project is the Dispatch project
// key; the lifecycles are keyed by the claim project token the tree_lifecycles table uses
// (claim.ProjectToken), which is derived here so no caller has to remember the difference.
func ReadLifecycles(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue) (map[string]treelifecycle.Lifecycle, error) {
	token, err := claim.ProjectToken(project)
	if err != nil {
		return nil, err
	}
	return store.TreeLifecycles(ctx, tx, token, treeRoots(issues))
}

// ReadParents is the record of each issue on a parent chain above issues that issues itself
// leaves out, by key (LineFacts.Parents): the pass's live read holds no issue already done, and
// a done parent in the middle of a chain would otherwise hide every issue above it from the one
// chain walk (Ancestors). It is one read whatever the depth of the chains — the store walks them
// in the database (Store.AncestorsOf) — and no read at all for the ordinary pass, whose records
// hold every parent they name.
func ReadParents(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue) (map[string]Issue, error) {
	held := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		held[issue.Key] = struct{}{}
	}
	var missing []string
	for _, issue := range issues {
		if issue.Parent == nil {
			continue
		}
		if _, inHand := held[*issue.Parent]; !inHand && !slices.Contains(missing, *issue.Parent) {
			missing = append(missing, *issue.Parent)
		}
	}
	if len(missing) == 0 {
		return map[string]Issue{}, nil
	}
	rows, err := store.AncestorsOf(ctx, tx, project, missing)
	if err != nil {
		return nil, err
	}
	parents := make(map[string]Issue, len(rows))
	for _, row := range rows {
		parents[row.Key] = row
	}
	return parents, nil
}

// ReadLineFacts reads the facts of one admission pass for issues: one tree-lifecycle read
// (ReadLifecycles), one gate read, one read of the records above the pass's own (ReadParents)
// and one read of the starts the project owes, whatever the number of trees, gates and issues
// involved. project is the Dispatch project key.
func ReadLineFacts(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue) (LineFacts, error) {
	lifecycles, err := ReadLifecycles(ctx, tx, store, project, issues)
	if err != nil {
		return LineFacts{}, err
	}
	gates, err := store.Gates(ctx, tx, treeRoots(issues))
	if err != nil {
		return LineFacts{}, err
	}
	parents, err := ReadParents(ctx, tx, store, project, issues)
	if err != nil {
		return LineFacts{}, err
	}
	rows, err := store.OwedStarts(ctx, tx, project)
	if err != nil {
		return LineFacts{}, err
	}
	owed := make(map[string]OwedStart, len(rows))
	for _, row := range rows {
		// The read is oldest first, and an issue that owes two starts — one the lane holds a
		// lease on and one a later transition wrote — keeps the place the first one took.
		if _, seen := owed[row.Issue]; !seen {
			owed[row.Issue] = row
		}
	}
	return LineFacts{Lifecycles: lifecycles, Gates: gates, Parents: parents, Owed: owed, OwedRows: rows}, nil
}

// GateOpen is whether a person's approval stands on the gate's current document version: the one
// statement of it, which classify.DesignGateOpen and Store.PutGate's own write both call.
// internal/classify reads this package, so the rule lives here, where every reader can reach it.
func GateOpen(gate DesignGate) bool {
	return gate.ApprovedVersion != nil && *gate.ApprovedVersion == gate.LatestVersion
}

// GateValue is the gate a root carries, the zero gate for a root with none registered, which is a
// root whose architect has still to write its spec: the shape AtWork, QueuedRoot and every other
// rule over a gate takes. It is one helper and not one per reader, because a reader writing its
// own is free to read a missing gate as something other than a gate that has never opened, and
// those two readings put a root in different parts of the waiting line.
func GateValue(gate *DesignGate) DesignGate {
	if gate == nil {
		return DesignGate{}
	}
	return *gate
}

// gateReturn is when a root's work came back to it through its design gate: the moment a person
// approved the version the gate stands open on (DesignGate.ApprovedMark), zero for a gate that
// is not open. It is the waiting line's ordering key for a root whose own first phase can start,
// merged on the same time axis as an owed start row's creation and an answered root's answer.
// A gate whose approval carries no moment — one a registration read from Dispatch rather than
// from an event — gives the zero time, which puts that root at the head of its part: the line
// knows its work came back and not when, and the alternative, dropping it to the back by rank,
// loses what it does know.
func gateReturn(gate DesignGate) time.Time {
	if !GateOpen(gate) {
		return time.Time{}
	}
	return gate.ApprovedMark.At
}

// later is whichever of two moments is the later one.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// AtWork says whether a worker of issue has work to do in the phase the issue stands in, which
// is the whole of what the admission cap counts: it is the one definition the release of a slot
// (NeedsSlot, which admit's releaseSlots asks of every issue holding one), the grant of one
// (admit's slotFor, which asks it of the state the promotion leaves the issue in) and the
// waiting line's first part all read, so no two of them can disagree about what work is. gate is
// the design gate of the issue's tree root, the zero value for a root with none.
//
// Planning, implementing, testing, reviewing and retro each have a phase worker with work.
// Merging has one too, unless a closed design gate is holding the READY its merger already built
// (ReadyPendingVersion), which is a wait on a person rather than work. Admitted is work only for
// a root whose design gate has never opened, where its architect is writing the spec; after that
// opening the root's own turns start no phase worker, and its children do the work. Every other
// phase is a wait: a child at admitted, accepting, awaiting_merge, held, awaiting_person, done.
func AtWork(issue Issue, gate DesignGate) bool {
	switch issue.Phase {
	case phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing, phase.Retro:
		return true
	case phase.Merging:
		return issue.ReadyPendingVersion == nil
	case phase.Admitted:
		return claim.IsTreeRoot(issue.Key, issue.Tree) && !gate.EverOpened
	default:
		return false
	}
}

// NeedsSlot says whether issue must hold one of the cap's slots now: a worker of it has work
// (AtWork), its tree is running — live, with a root that does not linger after its close — and
// Dispatch still shows it in the workflow. Nothing of a tree that has left the workflow works
// until its re-admission runs it again, and an issue that only waits — for its merge, in
// accepting, on a person, on a blocker — holds none, so the cap bounds the issues at work
// rather than the trees that are open.
//
// The Dispatch status is asked because a person's park, close or icebox takes the issue out of
// the workflow before anything moves its phase: the workflow's own leave records the linger in
// the same fact, but a pass that reads the record between the two would otherwise count a
// parked root's slot against the cap for good.
//
// It is the one statement of the rule, and the two readers that must never disagree both call
// it: admission releases the slot of every issue it answers false for (admit's releaseSlots),
// and the waiting line's first part offers a slot to exactly the issues it answers true for
// (Waiting). root is the record of the issue's tree root and gate that root's design gate; live
// is whether the tree is live (LiveRoots). The slot is accounting only: taking one away
// suspends no claim, moves no phase and writes no Dispatch status.
func NeedsSlot(issue, root Issue, gate DesignGate, live bool) bool {
	return live && !root.Lingers() && !OutOfWorkflow(issue.Status) && AtWork(issue, gate)
}

// CanWaitOnPerson says whether root may enter awaiting_person now: it is a root of a tree in the
// workflow, standing at admitted and not lingering after its close, and it holds the admission
// slot its own work takes (slotted). At admitted, AtWork is true for a tree root whose design
// gate has never opened — the architect writing the spec, which holds the whole of the tree's
// work — so the gate is read here too: the pass that moves a root runs before the pass that
// takes a slot back (admit's promoteHolds calls ReconcileAwaiting, then releaseSlots), so a root
// whose gate opened in this very pass still holds the slot row admission is about to release,
// and its work is its children's by then.
//
// It is the one statement of the rule, and the two readers that must never disagree both call
// it: the pass that starts a wait (workflow's decideAwait, rule 0) and the boot's catch-up of
// the roots that pass can move (the daemon's catchesUp). A root this answers false for starts no
// wait on a person, so the boot need not read its Dispatch window either. gate is the root's own
// design gate, the zero value for a root with none registered.
//
// It is not NeedsSlot read backwards: NeedsSlot says what the cap must hold a slot for, of every
// issue, from the record alone, while this says what one root may hand that slot back for, and
// asks the slot row itself.
func CanWaitOnPerson(root Issue, gate DesignGate, slotted bool) bool {
	return slotted && root.Phase == phase.Admitted && !root.Lingers() && InWorkflow(root) && AtWork(root, gate)
}

// Waiting is the line of issues that wait for an admission slot, in the order admission gives
// them one. The cap counts the issues at work rather than the trees, so a child waits in this
// line as a root does, and the line has two parts.
//
// First, by when their work came back, oldest first: the issues that held a slot before and have
// work again. Three kinds of record say that work came back, and each gives the moment it did:
//
//   - an issue that owes its worker a start takes that row's own creation time and id
//     (OwedStart), which is durable — the transition wrote the row in its own transaction — so a
//     restart finds the line exactly as it was, and a second transition before the slot replaces
//     that row's payload without moving it (Store.ReplaceOwedStart);
//   - a root a person answered while it waited in awaiting_person takes its Awaiting.AnsweredAt;
//   - a tree's own root standing at admitted under an open gate, whose first phase can start,
//     takes the moment that gate opened (gateReturn). It held a slot while its architect wrote
//     the spec and gave it up at the opening, so its first phase is work coming back rather
//     than work that never started.
//
// Where a root has both an answer and an open gate, the later of the two is when its work came
// back. The three are merged on the one time axis, and ties are broken by the owed row's id and
// then the issue key. A limit on work in progress works by finishing started work first, so work
// that was in progress goes before work that never started.
//
// Then, by the Dispatch rank of their tree's root (RankLess) and, within one root's rank, by
// their own: the issues whose first phase can start and that never held a slot. These are the
// roots whose tree has not been opened — one that never ran, a re-admitted one and a handed-over
// one — and the released children of a tree that already runs, standing at admitted under an
// open design gate. Each takes its tree root's place in the board's order, so a started tree's
// children go before the roots ranked below their root, and a root ranked above a tree is not
// kept out by a tree that keeps filing children.
//
// An unanswered awaiting root waits on the person rather than a slot, whatever Dispatch status it
// currently shows, so it is not in the line until it is Answered. A root without the label waits
// for nothing. A root that never ran, whose recorded parent is a live tree's member, is adopted
// into that tree before admission reads the line (admit's adoptQueuedRoots), so it is not read as
// waiting there. A root under a parent unrecorded or recorded as a root, whose Dispatch chain
// (Ancestry) holds any live root but its own key, is held under an unreleased child of that tree
// (the ancestry hold) and waits for nothing: it is that tree's unreleased work, which its
// architect's release, or its parent's, brings into the tree, exactly as release_children's own
// walk of the chain reaches it. A recorded root on the chain that is not live is passed over. A
// tree is live here as everywhere (LiveRoots), so the tree of a root held under an unreleased
// child, or of a root waiting on a person, is live too; once no live root is left on its chain
// (each lingers or leaves the workflow, or a person moves an ancestor out from under it), the
// root waits as its own again. A root that ran, an answered one among them, records no chain
// (workflow's Ancestry), so the hold never takes an answered root out of the line. The other
// hold, on Dispatch blocked-by links, takes an issue out of here only where it holds it where it
// stands: an issue whose first phase would start waits at admitted until the last issue it names
// is done (LineFacts.Held, blockers.Holds), so the line never offers it a slot. A root's links
// are read at its promotion instead, which passes it over and keeps its place
// (blockers.WaitsOn); either way the state names it under admission.blocked.
//
// A root is in the line as a root that never ran (LineQueuedRoot) only while its own record says
// the generation it carries has not run — it stands at admitted with its design gate never
// opened, or it arrived from another tree carrying the phase it ran there (ran). So a person's
// todo on a running root puts it back in no queue: the root whose gate has opened waits here for
// its own first phase, and the root in accepting, in awaiting_merge or at work waits for
// nothing. The slot used to say this, and cannot any longer: a running root holds one only while
// a worker of it has work.
//
// An issue already holding a slot is not in the line, and neither is one whose tree is not live
// or whose root lingers: nothing of a tree that has left the workflow is started again until its
// re-admission opens it.
func Waiting(issues []Issue, slots []Slot, facts LineFacts) []LineEntry {
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	byKey := make(map[string]Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	// The parent chain is walked over the records above these as well (chainOf): a parent
	// already done is no part of the live read, and a walk that stopped at it would hide every
	// issue above it, so the line would offer a slot to a child the acceptance rules hold
	// (AcceptedAncestor).
	chain := chainOf(byKey, facts.Parents)
	queued := queuedRoots(issues, facts)
	live := liveRoots(issues, facts, queued)
	queuedKeys := make(map[string]struct{}, len(queued))
	for _, issue := range queued {
		queuedKeys[issue.Key] = struct{}{}
	}

	// The tree of a candidate must be live and must not linger, and the root must be recorded:
	// the whole line is work the daemon is about to start, and a tree that has left runs nothing.
	runs := func(issue Issue) bool {
		root, recorded := byKey[issue.Tree]
		if !recorded || root.Lingers() {
			return false
		}
		_, running := live[issue.Tree]
		return running
	}

	type timed struct {
		entry LineEntry
		at    time.Time
		id    int64
	}
	var returned []timed
	for _, issue := range issues {
		if _, held := slotted[issue.Key]; held {
			continue
		}
		if issue.Answered() {
			// An answered root belongs to this part whether or not its tree lifecycle still
			// stands open: it held a slot before, and its place is the moment its work came
			// back, which is its answer or, when a person's approval opened its gate later
			// than that, the approval.
			returned = append(returned, timed{entry: LineEntry{Issue: issue, Kind: LineAnswered},
				at: later(*issue.Awaiting.AnsweredAt, gateReturn(facts.Gates[issue.Tree]))})
			continue
		}
		if _, waitingAsRoot := queuedKeys[issue.Key]; waitingAsRoot {
			// A root whose tree is not open yet is in the second part whatever rows it owes: its
			// promotion opens the tree, and the starts of a run it carries are queued then.
			continue
		}
		// The issue must need a slot where it stands (NeedsSlot), the same question the release
		// of one asks, so that no issue the release would free is offered one here: an issue
		// that owes a start its phase has moved past — a run that ended before its slot came,
		// whose row the repair pass finishes (admit's finishFencedStarts) — waits for nothing
		// and must not take a slot from an issue that does.
		root, recorded := byKey[issue.Tree]
		_, running := live[issue.Tree]
		if !recorded || !NeedsSlot(issue, root, facts.Gates[issue.Tree], running) {
			continue
		}
		owed, owes := facts.Owed[issue.Key]
		if !owes {
			continue
		}
		returned = append(returned, timed{entry: LineEntry{Issue: issue, Kind: LineReturning}, at: owed.CreatedAt, id: owed.ID})
	}

	var starting []LineEntry
	for _, issue := range queued {
		if issue.Answered() {
			continue
		}
		if _, held := slotted[issue.Key]; held {
			continue
		}
		if slices.ContainsFunc(issue.Ancestry, func(key string) bool {
			_, under := live[key]
			return under && key != issue.Key
		}) {
			continue
		}
		starting = append(starting, LineEntry{Issue: issue, Kind: LineQueuedRoot})
	}
	for _, issue := range issues {
		if issue.Phase != phase.Admitted || issue.AwaitingPerson() || OutOfWorkflow(issue.Status) {
			continue
		}
		if _, held := slotted[issue.Key]; held {
			continue
		}
		if _, waitingAsRoot := queuedKeys[issue.Key]; waitingAsRoot || !runs(issue) {
			continue
		}
		// A root that handed work to a person starts nothing of its tree while it waits: its
		// members start with its resume (admit's promotion of the answered root).
		if root := byKey[issue.Tree]; root.AwaitingPerson() {
			continue
		}
		gate, registered := facts.Gates[issue.Tree]
		if !registered || !GateOpen(gate) {
			continue
		}
		// A child an unfinished Dispatch issue holds where it stands waits for that issue, not
		// for a slot: offered one here it would take it from an issue that can work, and its own
		// first phase would start on work its blocker has still to land. Its tree's architect is
		// told when it starts, and the state names it under admission.blocked meanwhile.
		if _, blocked := facts.Held[issue.Key]; blocked {
			continue
		}
		// A parent whose own Dispatch children are still open does its work after theirs
		// (LineFacts.ChildWaits): offered a slot here it would build beside the children whose
		// merges its own head is meant to carry. Its tree's architect is told once per issue and
		// run of its tree when the wait begins (admit's tellChildrenFirst), and the state names
		// it under admission.blocked meanwhile.
		if _, waits := facts.ChildWaits[issue.Key]; waits {
			continue
		}
		// An issue beneath an ancestor the daemon has already accepted waits for that ancestor
		// to close (HeldUnderAccepted): starting it would let the ancestor merge work its own
		// head does not carry. A person's move of such an issue to todo records it here at
		// admitted and leaves it here; the architect is told why when it enters. The chain is
		// walked over the records above this pass's own as well, so that a parent already done
		// does not hide the merging issue above it.
		if HeldUnderAccepted(issue, chain) {
			continue
		}
		entry := LineEntry{Issue: issue, Kind: LineFirstPhase}
		// A tree's own root held a slot while its architect wrote the spec, and gave it up when
		// the gate opened: its first phase is work coming back, so it stands in the first part
		// at the moment its gate opened (gateReturn). A child of the tree never held one, so
		// its first phase is new work and waits in the second part by its tree root's rank.
		if claim.IsTreeRoot(issue.Key, issue.Tree) {
			returned = append(returned, timed{entry: entry, at: gateReturn(gate)})
			continue
		}
		starting = append(starting, entry)
	}
	sort.Slice(returned, func(i, j int) bool {
		left, right := returned[i], returned[j]
		if !left.at.Equal(right.at) {
			return left.at.Before(right.at)
		}
		if left.id != right.id {
			return left.id < right.id
		}
		return left.entry.Issue.Key < right.entry.Issue.Key
	})
	sort.Slice(starting, func(i, j int) bool {
		left, right := starting[i].Issue, starting[j].Issue
		// A root of its own tree compares by its own rank, which is its tree root's; a child
		// takes its tree root's place in the board's order and then its own.
		if left.Tree != right.Tree {
			return RankLess(byKey[left.Tree], byKey[right.Tree])
		}
		return RankLess(left, right)
	})

	line := make([]LineEntry, 0, len(returned)+len(starting))
	for _, waiting := range returned {
		line = append(line, waiting.entry)
	}
	return append(line, starting...)
}

// WaitingIssues is the issues of a waiting line, in its order: what a reader of the line that
// does not act on it wants (the state's admission.waiting, the controller's blocked report).
func WaitingIssues(line []LineEntry) []Issue {
	issues := make([]Issue, len(line))
	for i, entry := range line {
		issues[i] = entry.Issue
	}
	return issues
}

// queuedRoots is every root handed to Legion whose tree has not been opened and which may take a
// slot: one that never ran, a re-admitted one and a handed-over one (QueuedRoot). It is read
// before the ancestry hold, because the hold is read against the live trees these same roots
// make (liveRoots).
func queuedRoots(issues []Issue, facts LineFacts) []Issue {
	queued := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if QueuedRoot(issue, facts.Gates[issue.Key]) {
			queued = append(queued, issue)
		}
	}
	return queued
}

// QueuedRoot says whether issue is a root handed to Legion that waits in the line for its tree
// to be opened: a root that never ran, a re-admitted one and a handed-over one, each standing in
// todo, not waiting on a person (whose answer resumes it where it stands) and with a record that
// does not already say this generation runs (ran). It is the one statement of the rule, read by
// the line itself, by the set of live trees (liveRoots), by the set of running ones (TreeRuns)
// and by the relay of a person's decision (WaitsForSlot), so that no two of them disagree about
// a root that has still to start.
//
// The tree's lifecycle does not answer this, though it is what says the tree is live
// (liveRoots): a lifecycle that stands open says the tree once opened, not that this generation
// of the root is running, and a root whose tree ran and ended queues again with that row still
// standing until its cleanup confirms. The slot does not answer it either, now that a running
// root holds none while it waits for its merge or in accepting.
func QueuedRoot(issue Issue, gate DesignGate) bool {
	if !issue.HandedOver || !claim.IsTreeRoot(issue.Key, issue.Tree) || ran(issue, gate) {
		return false
	}
	return !issue.AwaitingPerson() && issue.Status == "todo"
}

// ran says whether this root's own record shows the generation it carries already running under
// Legion. Two things say so, and the slot says neither any more. The record stands in a phase of
// its own (Issue.CarriesRun — neither admitted nor done): it is at work, waiting for its merge,
// in accepting, held or waiting on a person. Or its design gate has opened at some point
// (DesignGate.EverOpened), after which the root stands at admitted while its architect revises
// the spec and its own first phase waits in the line as a member of its tree (LineFirstPhase).
// A re-admission clears the gate with the rest of the generation, so a tree Legion takes back
// queues again as a root that never ran, which is what it is.
//
// The one record that carries a run and has still to run here is a root that has just arrived
// from another tree with the phase it ran there (Issue.HandedOverFrom): that run belonged to the
// tree it left, and the tree of its own it becomes has yet to be opened, so its promotion opens
// it and starts the role of whatever phase it carries. The mark is the arrival and not the
// root's whole life: the promotion that opens its tree clears it (admit's promoteHolds), because
// from then on the run the record carries is this tree's own. Without that a person's todo on
// such a root, running and holding no slot in accepting or awaiting_merge, would queue it here
// again and promote it a second time, moving its awaiting_merge to merging where its merge
// closes nothing.
func ran(issue Issue, gate DesignGate) bool {
	return issue.HandedOverFrom == "" && (issue.CarriesRun() || gate.EverOpened)
}

// treeLive is TreeLive over the facts of one root already in hand: its tree's lifecycle and its
// own design gate. It is the one statement of the rule; the pass that judges many trees reads it
// through liveRoots and the one that judges a single root through TreeLive, so no reader of
// "this tree is live" can answer differently from another.
func treeLive(root Issue, lifecycle treelifecycle.Lifecycle, gate DesignGate) bool {
	switch {
	case root.Lingers():
		return false
	case root.AwaitingPerson():
		return true
	}
	return lifecycle.OpenUnderWorkflow() || QueuedRoot(root, gate)
}

// TreeLive says whether root's tree is live: its lifecycle stands open under the workflow's own
// authority and the root does not linger after its close, or the root waits in the line for its
// tree to be opened (held under an unreleased child or not), or it waits on a person with its
// architect live. A todo child under a live tree runs in it, or waits with its root held under an
// unreleased child and moves with it into the tree that adopts it; under any other it is an
// orphan.
//
// The lifecycle is what says a tree runs, not the slot rows: a slot now counts one issue at work,
// so a tree runs with every one of its issues waiting for a slot, and a slotted child is no tree
// of its own. A pass that judges many trees reads them all at once instead (LiveRoots over
// ReadLineFacts).
//
// Live is wider than running (TreeRuns): a root still waiting in the line for its tree to be
// opened is live, so that a todo child recorded under it joins the tree its promotion opens,
// and runs nothing yet.
func TreeLive(ctx context.Context, store Store, tx pgx.Tx, root Issue) (bool, error) {
	if root.Lingers() {
		return false, nil
	}
	if root.AwaitingPerson() {
		return true, nil
	}
	lifecycle, gate, err := rootFacts(ctx, store, tx, root)
	if err != nil {
		return false, err
	}
	return treeLive(root, lifecycle, gate), nil
}

// TreeRuns says whether this generation of root's tree is running under the workflow: its
// lifecycle stands open under the workflow's own authority, the root does not linger after its
// close, and the root is not itself waiting in the line for that tree to be opened (QueuedRoot).
//
// It is the one statement of "the tree runs", and the three readers that must agree all call it:
// the claims nothing will wake (workflow.Stranded), the boot's resume of a suspended phase
// worker (workflow's booted) and the question whether a session's status write lands on a
// running tree (workflow's sessionStatusWrite). Each of them acts on the tree as it stands
// now, and a root waiting in the line runs nothing yet: its architect's first task comes with
// its promotion, so a claim of it is not stranded, a suspended worker of it is not resumed, and
// a status write on it is a person's move. The last clause is what a re-admission inside a
// linger needs: readmit clears the linger while the lifecycle of the run that ended still
// stands open, so the lifecycle alone would call such a root running.
func TreeRuns(root Issue, lifecycle treelifecycle.Lifecycle, gate DesignGate) bool {
	return lifecycle.OpenUnderWorkflow() && !root.Lingers() && !QueuedRoot(root, gate)
}

// TreeRunning is TreeRuns for a caller holding one root and no facts of a pass: it reads that
// root's tree lifecycle and its design gate, and asks the one rule.
func TreeRunning(ctx context.Context, store Store, tx pgx.Tx, root Issue) (bool, error) {
	if !claim.IsTreeRoot(root.Key, root.Tree) || root.Lingers() {
		return false, nil
	}
	lifecycle, gate, err := rootFacts(ctx, store, tx, root)
	if err != nil {
		return false, err
	}
	return TreeRuns(root, lifecycle, gate), nil
}

// RunningRoots is the set of keys whose trees are running (TreeRuns), read from the records and
// the pass's own facts already in hand.
func RunningRoots(issues []Issue, facts LineFacts) map[string]struct{} {
	running := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if claim.IsTreeRoot(issue.Key, issue.Tree) && TreeRuns(issue, facts.Lifecycles[issue.Key], facts.Gates[issue.Key]) {
			running[issue.Key] = struct{}{}
		}
	}
	return running
}

// ReadRunningRoots is RunningRoots for a caller that holds no facts of the pass: the lifecycles
// and the gates of the roots among issues, in two reads whatever the number of trees, and then
// the one rule. project is the Dispatch project key.
func ReadRunningRoots(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue) (map[string]struct{}, error) {
	lifecycles, err := ReadLifecycles(ctx, tx, store, project, issues)
	if err != nil {
		return nil, err
	}
	gates, err := store.Gates(ctx, tx, treeRoots(issues))
	if err != nil {
		return nil, err
	}
	return RunningRoots(issues, LineFacts{Lifecycles: lifecycles, Gates: gates}), nil
}

// rootFacts is one root's tree lifecycle and design gate, for the single-root readings of the two
// rules above — TreeLive, which the recording of a todo child under a live tree asks, and
// TreeRunning, which workflow's sessionStatusWrite asks of a session's status write: a pass that
// judges many trees reads them in bulk instead (ReadLineFacts).
func rootFacts(ctx context.Context, store Store, tx pgx.Tx, root Issue) (treelifecycle.Lifecycle, DesignGate, error) {
	project, err := claim.ProjectToken(root.Project)
	if err != nil {
		return treelifecycle.Lifecycle{}, DesignGate{}, err
	}
	lifecycle, _, err := store.TreeLifecycle(ctx, tx, project, root.Key)
	if err != nil {
		return treelifecycle.Lifecycle{}, DesignGate{}, err
	}
	gate, err := store.Gate(ctx, tx, root.Key)
	if err != nil {
		return lifecycle, DesignGate{}, err
	}
	return lifecycle, GateValue(gate), nil
}

// LiveRoots is the set of keys whose trees are live (TreeLive), read from the records and the
// pass's own facts already in hand.
func LiveRoots(issues []Issue, facts LineFacts) map[string]struct{} {
	return liveRoots(issues, facts, queuedRoots(issues, facts))
}

// liveRoots is LiveRoots over queued, the roots waiting for their tree to be opened already read
// from the same records: a root among them is live by treeLive's own last clause, and reading
// them once keeps the ancestry hold and the line judged against the same set of trees, so a todo
// child of such a root records into its tree.
func liveRoots(issues []Issue, facts LineFacts, queued []Issue) map[string]struct{} {
	live := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if claim.IsTreeRoot(issue.Key, issue.Tree) && treeLive(issue, facts.Lifecycles[issue.Key], facts.Gates[issue.Key]) {
			live[issue.Key] = struct{}{}
		}
	}
	for _, issue := range queued {
		live[issue.Key] = struct{}{}
	}
	// A root of a tree that lingers is live in no reading of it, whatever its own record says it
	// waits for: its re-admission is what opens the tree again, and it is that write, not this,
	// that ends the linger.
	for _, issue := range issues {
		if issue.Lingers() {
			delete(live, issue.Key)
		}
	}
	return live
}
