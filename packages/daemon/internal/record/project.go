package record

import (
	"context"
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
func InWorkflow(issue Issue) bool {
	return !OutOfWorkflow(issue.Status) && issue.Phase != phase.Done
}

// AcceptedPhases are the phases an issue stands in after its own reviewed head was accepted and
// before it has closed: its retro, its merge, and its wait for that merge. Work entering the
// workflow beneath an issue in one of them would let that issue merge first, which is the one
// thing a parent must not do (dispatch://INFRA-980, "Children first"), so the daemon refuses to
// let it in (api's CHILD_UNDER_ACCEPTED_PARENT) and holds at admitted an issue a person moves to
// todo there (AcceptedAncestor, read by Waiting).
var AcceptedPhases = []phase.Phase{phase.Retro, phase.Merging, phase.AwaitingMerge}

// AcceptedAncestor is the nearest issue strictly above issue, on the chain of recorded parents in
// byKey, that the daemon has already accepted (AcceptedPhases), and "" when no ancestor has been.
// byKey is the records of the issue's own project the caller already holds, which is where the
// chain is read: a parent outside it ends the walk, as does a chain that loops back on itself.
//
// A parent already done does not end the walk: the spec's grandchild under a done parent is held
// by whichever issue above that parent is still merging, which is what the walk goes on to find.
// A done ancestor holds nothing itself — its merge has happened, so there is no order left to
// keep.
func AcceptedAncestor(issue Issue, byKey map[string]Issue) string {
	seen := map[string]bool{issue.Key: true}
	for parent := issue.Parent; parent != nil && !seen[*parent]; {
		seen[*parent] = true
		above, recorded := byKey[*parent]
		if !recorded {
			return ""
		}
		if slices.Contains(AcceptedPhases, above.Phase) {
			return above.Key
		}
		parent = above.Parent
	}
	return ""
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
// project's own issue records: the tree lifecycles, the roots' design gates, and the worker starts
// the project already owes. They are gathered once per admission pass (ReadLineFacts) and handed
// to every judgement that pass makes, because each of them is read under the one fact lock
// (intake.ApplyFact) on every fact of every consumer: a read per tree, per gate or per slot would
// cost the daemon a round trip for each of them on each fact.
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
	// Held is the set of issues a Dispatch blocker holds where they stand, by key: the issues
	// whose first phase the line must not offer a slot to, however long they have waited. Only
	// the caller can fill it — the judgement is blockers.Holds over the project's mirror rows,
	// and internal/blockers reads this package, so this package may not read it back — and a
	// caller that fills it with nothing says that nothing is held (blockers.HeldIssues).
	Held map[string]struct{}
	// ChildWaits is the children-first judgement of this pass, by parent key: the issues whose
	// own first phase the line must not offer a slot to because Dispatch still shows an open
	// child under them. Only the caller can fill it — the judgement reads the Dispatch mirror's
	// parent column against the daemon's own records, and lives in internal/workflow, which
	// reads this package (workflow.ChildWaits) — and a caller that fills it with nothing says
	// that nothing waits for its children. It is never stored: each admission pass computes it
	// again, so a mirror row that has gone stale costs one pass and no more.
	ChildWaits map[string]ChildWait
}

// ChildWait is why one parent's own first phase waits: the open Dispatch children under it, in
// Dispatch key order. It is data alone — the rule that fills it is workflow.ChildWaits — so that
// the waiting line, the state's `blocked` and the architect's notice all read one judgement.
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

// ReadLineFacts reads the facts of one admission pass for issues: one tree-lifecycle read, one
// gate read and one read of the starts the project owes, whatever the number of trees, gates and
// issues involved. project is the Dispatch project key; the lifecycles are keyed by the claim
// project token the tree_lifecycles table uses (claim.ProjectToken), which is derived here so no
// caller has to remember the difference.
func ReadLineFacts(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue) (LineFacts, error) {
	token, err := claim.ProjectToken(project)
	if err != nil {
		return LineFacts{}, err
	}
	roots := make([]string, 0, len(issues))
	for _, issue := range issues {
		if claim.IsTreeRoot(issue.Key, issue.Tree) {
			roots = append(roots, issue.Key)
		}
	}
	lifecycles, err := store.TreeLifecycles(ctx, tx, token, roots)
	if err != nil {
		return LineFacts{}, err
	}
	gates, err := store.Gates(ctx, tx, roots)
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
	return LineFacts{Lifecycles: lifecycles, Gates: gates, Owed: owed, OwedRows: rows}, nil
}

// gateOpen is whether a person's approval stands on the gate's current document version. It is
// classify.DesignGateOpen's rule written out rather than called, as PutGate's own copy of it is:
// internal/classify reads this package, so this package may not read it back.
func gateOpen(gate DesignGate) bool {
	return gate.ApprovedVersion != nil && *gate.ApprovedVersion == gate.LatestVersion
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
	if !gateOpen(gate) {
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
// (admit's needsSlot), the grant of one (its slotFor) and the waiting line's first part all
// read, so no two of them can disagree about what work is. gate is the design gate of the
// issue's tree root, the zero value for a root with none.
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
		// A worker of it must have work in the phase it stands in now (AtWork), the same
		// question the release of a slot asks: an issue that owes a start its phase has moved
		// past — a run that ended before its slot came, whose row the repair pass finishes
		// (admit's finishFencedStarts) — waits for nothing and must not take a slot from an
		// issue that does.
		if OutOfWorkflow(issue.Status) || !runs(issue) || !AtWork(issue, facts.Gates[issue.Tree]) {
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
		if !registered || !gateOpen(gate) {
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
		// merges its own head is meant to carry. Its tree's architect is told once per
		// generation when the wait begins (admit's tellChildrenFirst), and the state names it
		// under admission.blocked meanwhile.
		if _, waits := facts.ChildWaits[issue.Key]; waits {
			continue
		}
		// An issue beneath an ancestor the daemon has already accepted waits for that ancestor
		// to close (AcceptedAncestor): starting it would let the ancestor merge work its own
		// head does not carry. A person's move of such an issue to todo records it here at
		// admitted and leaves it here; the architect is told why when it enters.
		if AcceptedAncestor(issue, byKey) != "" {
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
// slot: one that never ran, a re-admitted one and a handed-over one. It is read before the
// ancestry hold, because the hold is read against the live trees these same roots make
// (liveRoots). A root whose own record says its generation already runs (ran) is not one of
// them, whatever Dispatch now shows.
//
// The tree's lifecycle does not answer this, though it is what says the tree is live
// (liveRoots): a lifecycle that stands open says the tree once opened, not that this generation
// of the root is running, and a root whose tree ran and ended queues again with that row still
// standing until its cleanup confirms. The slot does not answer it either, now that a running
// root holds none while it waits for its merge or in accepting.
func queuedRoots(issues []Issue, facts LineFacts) []Issue {
	queued := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if !issue.HandedOver || !claim.IsTreeRoot(issue.Key, issue.Tree) {
			continue
		}
		if ran(issue, facts.Gates[issue.Key]) {
			continue
		}
		if !issue.AwaitingPerson() && issue.Status == "todo" {
			queued = append(queued, issue)
		}
	}
	return queued
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
// The one record that carries a run and has still to run here is a root that arrived from
// another tree with the phase it ran there (Issue.HandedOverFrom): that run belonged to the tree
// it left, and the tree of its own it becomes has yet to be opened, so its promotion opens it
// and starts the role of whatever phase it carries.
func ran(issue Issue, gate DesignGate) bool {
	return issue.HandedOverFrom == "" && (issue.CarriesRun() || gate.EverOpened)
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
// of its own. It reads that one lifecycle and nothing else; whether a root with no open lifecycle
// waits for one is its own record's to say (queuedRoots), so no other issue is read, and not even
// its gate: a root with no open lifecycle whose gate has opened is a tree that has already ended
// under it, which the linger above answers. A pass that judges many trees reads them all at once
// instead (LiveRoots over ReadLineFacts).
func TreeLive(ctx context.Context, store Store, tx pgx.Tx, root Issue) (bool, error) {
	if root.Lingers() {
		return false, nil
	}
	if root.AwaitingPerson() {
		return true, nil
	}
	project, err := claim.ProjectToken(root.Project)
	if err != nil {
		return false, err
	}
	lifecycle, _, err := store.TreeLifecycle(ctx, tx, project, root.Key)
	if err != nil {
		return false, err
	}
	if lifecycle.OpenUnderWorkflow() {
		return true, nil
	}
	return len(queuedRoots([]Issue{root}, LineFacts{})) == 1, nil
}

// LiveRoots is the set of keys whose trees are live (TreeLive), read from the records and the
// pass's own facts already in hand.
func LiveRoots(issues []Issue, facts LineFacts) map[string]struct{} {
	return liveRoots(issues, facts, queuedRoots(issues, facts))
}

// liveRoots is LiveRoots over queued, the roots waiting for their tree to be opened already read
// from the same records: a tree is live while its lifecycle stands open under the workflow and
// its root does not linger, while its root waits in the line, or while its root waits on a person
// (TreeLive). A root the ancestry hold keeps out of the line is still among queued, so the hold is
// read against these same trees and a todo child of such a root records into its tree.
func liveRoots(issues []Issue, facts LineFacts, queued []Issue) map[string]struct{} {
	live := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if claim.IsTreeRoot(issue.Key, issue.Tree) && facts.Lifecycles[issue.Key].OpenUnderWorkflow() {
			live[issue.Key] = struct{}{}
		}
	}
	for _, issue := range queued {
		live[issue.Key] = struct{}{}
	}
	for _, issue := range issues {
		switch {
		case issue.Lingers():
			delete(live, issue.Key)
		case issue.AwaitingPerson():
			live[issue.Key] = struct{}{}
		}
	}
	return live
}
