// Package admit owns root admission slots and the Dispatch-rank waiting line.
package admit

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// Admission assigns root issues and orphans to the configured number of architect slots.
// All work occurs in the transaction supplied by intake or daemon boot reconciliation.
type Admission struct {
	store   record.Store
	engine  *workflow.Engine
	cap     int
	project string
	log     *slog.Logger
	now     func() time.Time

	// mu guards pending, target and catchingUp, the only Admission state a caller outside its own
	// transactions touches: the daemon's boot-owned poll calls Held on every tick, outside any
	// transaction and the database's own advisory fact lock, concurrently with whatever release
	// call currently holds both while it clears them.
	mu sync.Mutex
	// pending holds, for every key Reconcile found the Dispatch consumer still needs to catch up
	// before an admission decision on it is safe, the boot listing's own summary of that key —
	// recorded or not, whatever its listed status: the stream may still redeliver a stale event
	// for it (a nak, a slow ack, the outbox's own publish backoff, or a key the listing itself
	// never carried at all because it never hit the record). It empties all at once, not key by
	// key: target is one position in one stream, not a per-issue property, so nothing
	// distinguishes one held key's own catching-up from another's. release applies every key's
	// own summary — exactly as Reconcile's own immediate path would — before it clears them, so a
	// key the stream never gets around to explaining still lands on what the listing already knew.
	pending map[string]intake.DispatchIssue
	// target is the notification stream's own last sequence Reconcile captured when it found the
	// first record behind: every key in pending releases once the Dispatch consumer's position
	// has Reached it. Zero means nothing is held.
	target int64
	// holds counts Reconcile's deferrals into pending (Holds), so a poller of the hold tells one
	// hold from the next, which can wait on the same target and be released by the same position.
	holds uint64
	// catchingUp names each root the daemon's boot catch-up has yet to finish
	// (HoldForCatchUp): its record may still miss what its Dispatch event log holds, so no pass
	// decides whether it waits on a person (workflow's ReconcileAwaiting), nor resumes it once
	// answered — it keeps its place at the front of the waiting line — until the catch-up has
	// applied its window and releases it (ReleaseCatchUp). Unlike pending, a release's own pass
	// does not lift it.
	catchingUp map[string]struct{}

	// openCapabilities answers the deployment capabilities with no decision, by name
	// (capabilities.Deployment.Open), for the tick notice to carry; nil until the daemon registers
	// it (ReportCapabilities), when a tick names none.
	openCapabilities func() []string
}

// ReportCapabilities registers open, whose answer every tick controller notice carries as its
// openCapabilities: the daemon's report of the deployment capabilities with no decision, so the
// controller's periodic turn names each gap in the day's report. No other wake carries it.
func (a *Admission) ReportCapabilities(open func() []string) {
	a.openCapabilities = open
}

var _ intake.Handler = (*Admission)(nil)

// New creates an admission handler for one Dispatch project. engine is the workflow's own, shared
// with intake's other handler: reenterStrandedChildren calls its exported ReenterChild rather than
// keeping a second copy of re-entry and gate-open behavior.
func New(store record.Store, engine *workflow.Engine, cap int, project string, log *slog.Logger) *Admission {
	if log == nil {
		log = slog.Default()
	}
	return &Admission{store: store, engine: engine, cap: cap, project: project, log: log, now: time.Now, pending: make(map[string]intake.DispatchIssue)}
}

// Apply records root and orphan todo observations of issues handed to Legion and every newer
// observation of a recorded issue, queues the todo of an unrecorded root in triage whose legion
// label a person set (queuePersonHandover) and wakes the controller for one whose label a session
// set (a controller notice, not a record), and promotes the waiting line while capacity remains,
// releasing the slot of every issue that no longer needs one on the way (promoteHolds) and waking
// the controller when a slot it released is still free after that (wakeForFreeSlot). The workflow
// handler runs first: it records every live-tree child, leaving admission to record only a
// still-unrecorded root or orphan. A release's adoption of children it cannot adopt
// (adoptChildren) is the one fact admission refuses. A Dispatch issue fact's Reread is written
// first (applyReread), and a fact whose Reread changed a record releases and promotes even when it
// records nothing itself.
func (a *Admission) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	released, refusal, err := a.applyFact(ctx, tx, fact)
	if err != nil {
		return intake.Result{}, err
	}
	return intake.Result{Refusal: refusal}, a.wakeForFreeSlot(ctx, tx, released)
}

// applyFact is Apply's work, and reports the issues whose slots it released and the refusal of a
// release's adoption.
func (a *Admission) applyFact(ctx context.Context, tx pgx.Tx, fact intake.Fact) ([]string, *intake.Refusal, error) {
	if adoption, ok := fact.(intake.ChildAdoption); ok {
		return a.adoptChildren(ctx, tx, adoption)
	}
	if position, ok := fact.(intake.DispatchConsumerPosition); ok {
		released, err := a.release(ctx, tx, position)
		return released, nil, err
	}
	if _, ok := fact.(intake.ControllerTick); ok {
		_, returned, err := a.promote(ctx, tx)
		if err != nil {
			return nil, nil, err
		}
		// The tick wakes the controller whatever the slots: with every slot taken, it is the only
		// wake a tree waiting on a refused root claim gets, since only the controller's recheck of
		// that claim moves it, and it is the turn that posts the day's report.
		return returned, nil, a.wakeController(ctx, tx, record.TickNotice, a.project, a.now())
	}
	if handover, ok := fact.(intake.Handover); ok {
		if err := a.consumeHandover(ctx, tx, handover); err != nil {
			return nil, nil, err
		}
		_, returned, err := a.promote(ctx, tx)
		return returned, nil, err
	}

	observation, ok := fact.(intake.DispatchIssue)
	if !ok {
		_, returned, err := a.promote(ctx, tx)
		return returned, nil, err
	}
	if _, err := a.applyReread(ctx, tx, observation.Reread); err != nil {
		return nil, nil, err
	}

	stored, err := a.store.Issue(ctx, tx, observation.Key)
	if err != nil {
		return nil, nil, fmt.Errorf("read admission issue %s: %w", observation.Key, err)
	}
	if observation, err = workflow.SettleRetainedRepository(observation, stored); err != nil {
		return nil, nil, err
	}
	if stored != nil {
		if err := a.applyObservation(ctx, tx, *stored, observation); err != nil {
			return nil, nil, err
		}
		_, released, err := a.promote(ctx, tx)
		return released, nil, err
	}
	handed := observation.HandedOver
	// A root in triage handed to Legion is queued by the daemon when a person set the label
	// (observation.LabelSetter, read from the issue's event log when the fact was prepared): a
	// person's label is the handover, so the daemon writes its todo through the outbox
	// (queuePersonHandover) and no agent's write admits it. A label a session set, or one whose
	// setter the log did not show (the zero LabelSetter: a fact nothing prepared, or a log with no
	// labelling event) is not a person's, so each observation of the root while it is unrecorded
	// wakes the controller as before: its creation with the label, or the edit that adds it, since
	// the dashboard creates an issue without labels. The controller triages from Dispatch and the
	// daemon's state, never from the wake. A root without the label is not Legion's and wakes
	// nobody. This runs before any hold check: a hold defers only whether an unrecorded todo is
	// admitted, never whether an unrecorded triage root is queued or wakes the controller. The
	// record stays empty, as for any root not yet todo; the daemon's own todo write records it
	// through the path below once Dispatch echoes it. Reconcile's boot listing reads every status
	// now, but a triage root is never a todo candidate it admits.
	if observation.LabelledTriageRoot() {
		if observation.LabelSetByPerson() {
			if err := a.queuePersonHandover(ctx, tx, observation.Key, "triage", "event"); err != nil {
				return nil, nil, err
			}
		} else if err := a.enqueue(ctx, tx, observation.Key, record.ControllerNotice{Kind: record.TriageNotice}, a.now()); err != nil {
			return nil, nil, err
		}
	}
	// An unrecorded todo issue without the label is someone else's work in a project Legion may
	// share with humans and other agents: Legion records nothing of it until it is handed over.
	// Neither exit records anything, but each still refreshes a held key's own pending summary:
	// an event moving the key out of todo, or taking its label off, is real information about
	// what Dispatch shows now, and leaving the held summary as Reconcile's stale boot listing
	// found it would have release admit on that stale information instead.
	//
	// Every exit below still promotes, though it records nothing: the
	// blockers.Mirror handler, which runs first, has already written this issue's row in this
	// transaction, and a root or a child may be waiting on exactly this issue. Its close is an
	// observation of an unrecorded issue out of todo, so without this look a waiting root whose
	// last blocker is an issue Legion keeps no record of would wait for the controller tick to be
	// admitted.
	switch {
	case observation.Status != "todo":
		a.refreshHeldSummary(observation)
		_, released, err := a.promote(ctx, tx)
		return released, nil, err
	case !handed:
		a.log.Debug("admission: not handed to Legion", "issue", observation.Key, "label", dispatch.LegionLabel)
		a.refreshHeldSummary(observation)
		// Promotion comes before the slot is judged: a root this look admits takes a slot that
		// was free before it, and the wake must name a slot still free once this fact commits,
		// never one the same transaction has just filled.
		_, released, err := a.promote(ctx, tx)
		if err != nil {
			return nil, nil, err
		}
		// An issue in todo that nobody handed to Legion wakes the controller while a slot stands
		// free; the controller takes nothing for it, since only a person hands Legion work
		// (skill://legion-controller, "Free slots"). The notice waits todoWakeDelay, so the
		// events of one edit burst find it pending and fold into it.
		free, err := a.slotFree(ctx, tx)
		if err != nil || !free {
			return released, nil, err
		}
		return released, nil, a.wakeController(ctx, tx, record.TodoNotice, observation.Key, a.now().Add(todoWakeDelay))
	}
	a.mu.Lock()
	summary, held := a.pending[observation.Key]
	a.mu.Unlock()
	if held && observation.Seq <= summary.Seq {
		// Reconcile already holds this key on the listing's own summary because it found it
		// behind the stream: an unrecorded key has no record to fence a live event against, so
		// a replay at or behind that summary's own sequence — a nak's redelivery, the outbox's
		// own publish backoff — decides nothing about it. release applies that summary once the
		// consumer catches up. A newer event, past the sequence the summary was taken at, is
		// not a stale replay: it is recorded normally below, even while the key is still
		// technically held — a release that has already committed but not yet run its
		// commit hook (which clears pending) still shows this key as held, and dropping this
		// event here would lose it once that hook clears pending out from under it, the
		// event already acknowledged as processed. promote's own pending membership check —
		// not this one — is what still holds the freshly recorded candidate back from a slot
		// until pending is actually clear; a later pass once the hold clears promotes it. The
		// replay records nothing of itself, but it is still a look at what Legion could now
		// start, as every fact is.
		_, released, err := a.promote(ctx, tx)
		return released, nil, err
	}
	if err := a.putNewRoot(ctx, tx, observation, true); err != nil {
		return nil, nil, err
	}
	_, released, err := a.promote(ctx, tx)
	return released, nil, err
}

// applyReread writes each Dispatch chain the preparation of an unrecorded or re-parented issue's
// event read again (intake.DispatchIssue.Reread) to its root's record while that record is still
// a todo root, as the engine's Ancestry takes it, and says whether any record changed. The read
// was taken outside the fact lock, so a root adopted or moved on meanwhile is left as it now
// stands.
func (a *Admission) applyReread(ctx context.Context, tx pgx.Tx, reread map[string][]string) (bool, error) {
	changed := false
	for _, key := range slices.Sorted(maps.Keys(reread)) {
		stored, err := a.store.Issue(ctx, tx, key)
		if err != nil {
			return false, fmt.Errorf("read admission issue %s: %w", key, err)
		}
		if stored == nil || stored.Status != "todo" || !claim.IsTreeRoot(stored.Key, stored.Tree) {
			continue
		}
		ancestry, err := a.engine.Ancestry(ctx, tx, *stored, reread[key])
		if err != nil {
			return false, fmt.Errorf("read whether %s ran as a tree of its own: %w", key, err)
		}
		if slices.Equal(stored.Ancestry, ancestry) {
			continue
		}
		stored.Ancestry = ancestry
		if err := a.store.PutIssue(ctx, tx, *stored); err != nil {
			return false, fmt.Errorf("record the Dispatch chain of %s: %w", key, err)
		}
		changed = true
	}
	return changed, nil
}

// wakeForFreeSlot wakes the controller when this transaction released a slot that promotion left
// free: a tree finished or left the workflow and no waiting root took its place. The controller
// names the free slot in its summary and takes no issue for it (skill://legion-controller, "Free
// slots"). It is one `slot-free` notice on the first issue whose slot was released, under the same
// rules as every slot wake: a slot free by slotFree, a registered controller, none already unsent
// (wakeController).
func (a *Admission) wakeForFreeSlot(ctx context.Context, tx pgx.Tx, released []string) error {
	if len(released) == 0 {
		return nil
	}
	free, err := a.slotFree(ctx, tx)
	if err != nil || !free {
		return err
	}
	return a.wakeController(ctx, tx, record.SlotFreeNotice, released[0], a.now())
}

// todoWakeDelay is how long a `todo` controller notice waits in the outbox before it is published.
// Every Dispatch event of an unlabelled todo issue within it finds the notice still pending, so a
// burst of edits is one wake rather than one a poll.
const todoWakeDelay = 30 * time.Second

// slotFree says whether an admission slot stands free: fewer issues holding one than the cap,
// counting a slot a waiting issue will take as taken. The cap counts the issues at work, so the
// line it counts against is a line of issues, children included. A candidate promotion passes
// over because it waits on an unfinished Dispatch issue (candidateWaitsOn) will take none, and
// neither will one whose promotion starts no worker (slotFor), so neither hides free capacity
// from the project: the controller hears of the slot and may hand Legion work that can actually
// run. The controller is woken about it and takes nothing for it; only a person hands Legion
// work (skill://legion-controller, "Free slots").
func (a *Admission) slotFree(ctx context.Context, tx pgx.Tx) (bool, error) {
	if a.cap <= 0 {
		return false, nil
	}
	issues, err := a.store.LiveIssues(ctx, tx, a.project)
	if err != nil {
		return false, fmt.Errorf("list admission issues: %w", err)
	}
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return false, fmt.Errorf("list admission slots: %w", err)
	}
	facts, err := record.ReadLineFacts(ctx, tx, a.store, a.project, issues)
	if err != nil {
		return false, err
	}
	own := ownSlots(issues, slots)
	if facts.ChildWaits, err = record.ChildWaitsOf(ctx, tx, a.store, a.project, issues, facts.Gates); err != nil {
		return false, err
	}
	line := record.Waiting(issues, own, facts)
	rows, err := blockers.Load(ctx, tx, a.store, a.project, blockers.Judged(issues, record.WaitingIssues(line)))
	if err != nil {
		return false, err
	}
	taking := 0
	for _, candidate := range line {
		if len(candidateWaitsOn(rows, candidate)) == 0 && slotFor(candidate, facts.Gates[candidate.Issue.Tree]) {
			taking++
		}
	}
	return len(own)+taking < a.cap, nil
}

// wakeController queues a controller notice of kind on issue, due at dueAt, when a controller is
// registered and no notice of the same kind still waits in the outbox, so a burst of events or
// ticks queues one wake. The wake needs no record: the controller reads Dispatch and the daemon's
// state (skill://legion-controller). A tick alone carries the daemon's open capabilities
// (ReportCapabilities) and every issue Legion is waiting to start behind an unfinished Dispatch
// issue (blocked), the turn that posts the day's report.
func (a *Admission) wakeController(ctx context.Context, tx pgx.Tx, kind record.NoticeKind, issue string, dueAt time.Time) error {
	registered, err := a.store.ControllerRegistered(ctx, tx, a.project)
	if err != nil || !registered {
		return err
	}
	pending, err := a.store.ControllerNoticePending(ctx, tx, a.project, kind)
	if err != nil || pending {
		return err
	}
	notice := record.ControllerNotice{Kind: kind}
	if kind == record.TickNotice {
		if a.openCapabilities != nil {
			notice.OpenCapabilities = a.openCapabilities()
		}
		if notice.Blocked, err = a.blocked(ctx, tx); err != nil {
			return err
		}
	}
	return a.enqueue(ctx, tx, issue, notice, dueAt)
}

// queuePersonHandover queues the todo write of a person's handover of key: a root a person handed
// to Legion, which the daemon admits without an agent's act. Today that is a person's legion label
// on an unrecorded root in triage (applyFact for the event, applySummary for the boot listing);
// the workflow queues the same write for a person's approval of a parked labelled root's spec. The
// controller used to write this todo at a triage wake, so a handover waited on an agent's memory;
// a person's decision is the handover, and the daemon writes it itself and wakes nobody. The row
// carries observedStatus, the status the person's act found the issue in, because its delivery
// (the daemon's outbox) re-reads the issue and writes only while Dispatch still shows that status:
// a person's later park, between the act and the delivery, is never undone. One row per issue at
// a time: while a marked row is pending (record.Store.PersonHandoverPending) a second act — the
// stream's redelivery, another edit of the same root, the boot listing of a root an event already
// queued — queues nothing, since the pending row carries the same write, and the triage wake that
// act would otherwise write is not written either; once the row is delivered the issue is todo, so
// a later look at the same label finds nothing to queue. The workflow's other status rows of the
// issue suppress nothing. The queue is logged once it commits, under one message for every source
// of a person's handover — `event` and `listing` here, `approval` from the workflow — so the boot
// proof reads which path queued a root from the line's source field.
func (a *Admission) queuePersonHandover(ctx context.Context, tx pgx.Tx, key, observedStatus, source string) error {
	pending, err := a.store.PersonHandoverPending(ctx, tx, key)
	if err != nil || pending {
		return err
	}
	if err := a.enqueue(ctx, tx, key, record.StatusWrite{Status: "todo", ObservedStatus: observedStatus, PersonHandover: true}, a.now()); err != nil {
		return err
	}
	intake.OnCommit(ctx, func() { a.log.Info("admission: queued a person's handover", "issue", key, "source", source) })
	return nil
}

// blocked is every issue Legion is waiting to start that waits on something else, as the tick
// notice carries it: the candidates promotion passes over, the children the workflow holds
// behind an unfinished Dispatch issue, and the parents whose own children are still open
// (workflow.ChildWaitRows), judged from this transaction's own reads by the function the state's
// `blocked` is built with (blockers.Blocked), so the controller is told exactly what
// `legion state` shows.
func (a *Admission) blocked(ctx context.Context, tx pgx.Tx) ([]record.BlockedIssue, error) {
	issues, err := a.store.LiveIssues(ctx, tx, a.project)
	if err != nil {
		return nil, fmt.Errorf("list admission issues: %w", err)
	}
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("list admission slots: %w", err)
	}
	facts, err := record.ReadLineFacts(ctx, tx, a.store, a.project, issues)
	if err != nil {
		return nil, err
	}
	rows, facts, line, err := blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, nil)
	if err != nil {
		return nil, err
	}
	return blockers.Blocked(rows, issues, record.WaitingIssues(line), record.ChildWaitRows(facts.ChildWaits)), nil
}

// Reconcile applies the bounded Dispatch boot read to existing records, then fills newly available
// capacity using the same promotion effects Apply emits. It performs no Dispatch I/O itself.
//
// The read is a snapshot with no actor on it: in it, a session's own status write during a restart
// looks exactly like a human's move. Dispatch says how far each issue's event log has run, so a
// key the listing shows behind that log — recorded or not, whatever its listed status — is held
// back rather than decided on now: the stream still holds those events (or the outbox that would
// still publish one), and a stale one can still redeliver after this read, unless the Dispatch
// consumer's position had already Reached target when the caller measured it. Then nothing more
// is coming for any of it, ever, so every summary applies now instead of holding a key nothing
// will later release. target and position are the caller's own single measurement of the stream
// and the Dispatch consumer, taken once for the whole call, not per issue.
func (a *Admission) Reconcile(ctx context.Context, tx pgx.Tx, summaries []intake.DispatchIssue, target int64, position intake.DispatchConsumerPosition) error {
	issues, err := a.store.LiveIssues(ctx, tx, a.project)
	if err != nil {
		return fmt.Errorf("list admission issues: %w", err)
	}
	facts, err := record.ReadLineFacts(ctx, tx, a.store, a.project, issues)
	if err != nil {
		return err
	}
	live := record.LiveRoots(issues, facts)

	caughtUp := position.Reached(target)
	deferred := make(map[string]intake.DispatchIssue)
	for _, summary := range parentsFirst(summaries) {
		stored, err := a.store.Issue(ctx, tx, summary.Key)
		if err != nil {
			return fmt.Errorf("read reconciled issue %s: %w", summary.Key, err)
		}
		behind := stored == nil && summary.Seq > 0 || stored != nil && summary.Seq > stored.LastDispatchSeq
		if behind && !caughtUp {
			a.log.Debug("admission reconcile: the stream may still hold or redeliver an event for this issue; holding it for the consumer to catch up",
				"issue", summary.Key, "status", summary.Status, "dispatch", summary.Seq)
			deferred[summary.Key] = summary
			continue
		}
		if err := a.applySummary(ctx, tx, summary, live); err != nil {
			return err
		}
	}

	if len(deferred) > 0 {
		a.mu.Lock()
		for key, summary := range deferred {
			a.pending[key] = summary
		}
		a.holds++
		if target > a.target {
			a.target = target
		}
		a.mu.Unlock()
		a.log.Info("admission reconcile: holding roots for the Dispatch consumer to reach a stream position",
			"count", len(deferred), "target", target, "ack_floor", position.AckFloorStream)
	}

	_, released, err := a.promote(ctx, tx)
	if err != nil {
		return err
	}
	return a.wakeForFreeSlot(ctx, tx, released)
}

// parentsFirst orders a Dispatch listing by how many of each issue's ancestors the listing itself
// holds, so every issue comes after its parent, and keeps the listing's own order among the issues
// of one such depth. The workflow records a child only under a recorded parent, so an unrecorded
// todo child under an unrecorded todo parent of a live tree is recorded in the same pass as that
// parent, at any depth. A recorded issue is ordered by the same rule, so a root's own listed move —
// a close, a re-admission — is applied before its children's, which is the order the tree's own
// events carry: a child's re-entry then reads the tree as that move left it.
func parentsFirst(summaries []intake.DispatchIssue) []intake.DispatchIssue {
	parents := make(map[string]string, len(summaries))
	for _, summary := range summaries {
		parents[summary.Key] = summary.Parent
	}
	depth := make(map[string]int, len(summaries))
	for _, summary := range summaries {
		// The bound stops a parent cycle Dispatch should never hold; such a cycle only orders late.
		for parent, steps := summary.Parent, 0; parent != "" && steps < len(summaries); steps++ {
			next, listed := parents[parent]
			if !listed {
				break
			}
			depth[summary.Key]++
			parent = next
		}
	}
	ordered := slices.Clone(summaries)
	slices.SortStableFunc(ordered, func(x, y intake.DispatchIssue) int { return cmp.Compare(depth[x.Key], depth[y.Key]) })
	return ordered
}

// putNewRoot records an unrecorded root or orphan handed to Legion in todo, on the repository its
// observation resolves to and that repository's default branch (workflow's ResolveRepository).
// One whose repository resolves to none, or to one Legion does not serve, is not recorded: the
// daemon says why on the issue, once for that resolution, and the issue's next observation in
// todo is admission's next look at it.
func (a *Admission) putNewRoot(ctx context.Context, tx pgx.Tx, observation intake.DispatchIssue, logOrphan bool) error {
	resolution, err := a.engine.ResolveRepository(observation.ResolvedRepo)
	if err != nil {
		return fmt.Errorf("resolve the repository of %s: %w", observation.Key, err)
	}
	if resolution.Refusal != "" {
		noted, err := a.engine.RefuseRecording(ctx, tx, observation.Key, observation.ResolvedRepo, resolution)
		if err != nil {
			return fmt.Errorf("say why %s is not admitted: %w", observation.Key, err)
		}
		if noted {
			intake.OnCommit(ctx, func() {
				a.log.Info("admission: not admitted; its repository is not one Legion serves", "issue", observation.Key,
					"resolved", observation.ResolvedRepo, "reason", resolution.Refusal)
			})
		}
		return nil
	}
	issue := record.Issue{
		Key:             observation.Key,
		Tree:            observation.Key,
		Project:         a.project,
		Title:           observation.Title,
		Parent:          record.ParentOf(observation.Parent),
		Phase:           phase.Admitted,
		Generation:      1,
		Status:          "todo",
		Rank:            observation.Rank,
		HandedOver:      observation.HandedOver,
		LastDispatchSeq: observation.Seq,
		DispatchStatus:  observation.Status,
		Repo:            resolution.Repo,
		DefaultBranch:   resolution.DefaultBranch,
		GitHubRepo:      resolution.GitHubRepo,
	}
	if issue.Ancestry, err = a.engine.Ancestry(ctx, tx, issue, observation.Ancestry); err != nil {
		return fmt.Errorf("read whether %s ran as a tree of its own: %w", observation.Key, err)
	}
	if err := a.store.PutIssue(ctx, tx, issue); err != nil {
		return fmt.Errorf("record admitted root %s: %w", observation.Key, err)
	}
	if logOrphan && observation.Parent != "" {
		a.log.Info("admission orphan", "issue", observation.Key, "parent", observation.Parent)
	}
	return nil
}

// applySummary applies one Dispatch boot-listing summary to admission's own record of the issue
// it names, exactly as it would if nothing had ever deferred it: a still-unrecorded key is
// recorded only when the summary itself shows it todo — into its parent's live tree as the
// workflow records a child, else as a root when it is handed to Legion — or queued when it shows a
// root in triage whose legion label a person set, and a recorded key a live event has already
// carried strictly past the summary's own sequence is left alone — reapplying the summary would
// overwrite fresher information with stale. Both Reconcile, immediately when the consumer is
// already caught up, and release, once the consumer catches up to a summary Reconcile deferred,
// call this the same way: a key nothing else ever gets around to explaining still lands on what
// the listing already knew.
func (a *Admission) applySummary(ctx context.Context, tx pgx.Tx, summary intake.DispatchIssue, live map[string]struct{}) error {
	stored, err := a.store.Issue(ctx, tx, summary.Key)
	if err != nil {
		return fmt.Errorf("read reconciled issue %s: %w", summary.Key, err)
	}
	handed := summary.HandedOver
	if stored == nil {
		// An unrecorded root the listing shows in triage with the label is queued as its event
		// would be (applyFact) when a person set the label, which the listing's preparation read
		// from the issue's event log: the person's label fell while the daemon was down, or on a
		// stream position a fresh consumer never reads, and only the listing can see it. Any other
		// setter returns below as it did before, with no wake: the listing carries no actor, so
		// only a person's label read from the log queues, and a session's label at boot is the
		// controller's to find at its own start, as it always was.
		if summary.LabelledTriageRoot() && summary.LabelSetByPerson() {
			return a.queuePersonHandover(ctx, tx, summary.Key, "triage", "listing")
		}
		if summary.Status != "todo" {
			return nil
		}
		// An unrecorded todo child of a live tree is the workflow's to record, as its own todo event
		// would (the engine's recordChildUnderLiveTree, which runs before admission on every live
		// event), from the parent the listing shows now. Without this, a child no event of it ever
		// recorded stays unrecorded under its running tree, and no later event of it need come:
		// Dispatch already shows it todo, so nothing writes that status again. Two ways that
		// happens — an event the daemon never received (a durable consumer created fresh takes only
		// what is published after it, and the notification stream drops a message 72 hours old
		// whether or not anyone read it), and one it received while the child's parent had no
		// record or its tree was not live, where the workflow records nothing. It runs before the
		// root path, so a labelled child of a live tree is recorded as that tree's member, never as
		// a root of its own that the line then adopts (adoptQueuedRoots). A child the workflow does
		// not record here — its tree is not live, or its parent has no record — is left to the root
		// path below, as an orphan handed to Legion is: one whose Dispatch chain reaches a live root
		// waits in the line held under that tree's unreleased child (its Ancestry).
		if summary.Parent != "" {
			if _, err := a.engine.Apply(ctx, tx, summary); err != nil {
				return fmt.Errorf("record %s under its parent's tree: %w", summary.Key, err)
			}
			recorded, err := a.store.Issue(ctx, tx, summary.Key)
			if err != nil {
				return fmt.Errorf("re-read %s after recording it under its parent's tree: %w", summary.Key, err)
			}
			if recorded != nil {
				return nil
			}
		}
		if !handed {
			a.log.Debug("admission: not handed to Legion", "issue", summary.Key, "label", dispatch.LegionLabel)
			return nil
		}
		return a.putNewRoot(ctx, tx, summary, false)
	}
	if summary.Seq > 0 && summary.Seq < stored.LastDispatchSeq {
		return nil
	}
	// The record keeps the repository it was recorded with, as for a live event (the engine's
	// NoteRepositoryKept); the note is said once per resolution, so a listing no newer than the
	// record says nothing again.
	if err := a.engine.NoteRepositoryKept(ctx, tx, *stored, summary.ResolvedRepo); err != nil {
		return err
	}
	seq := stored.LastDispatchSeq
	if summary.Seq > seq {
		seq = summary.Seq
	}
	status := stored.Status
	if summary.Status != stored.Status && summary.Seq > stored.LastDispatchSeq {
		// A snapshot that changes a recorded issue's status, strictly newer than what is stored, is
		// the fact a live Dispatch event of the same change would carry, so it runs through the
		// engine's own transition table first — leave/beginLinger out of the workflow, or
		// ReenterChild for a live tree's child moved back to todo — exactly as that event would.
		// Engine.dispatchIssue already makes this same newer-or-not decision on its own sequence
		// fence (fact.Seq <= issue.LastDispatchSeq); pre-filtering by status here to
		// OutOfWorkflow alone left a parked child's own re-entry unreached, stranding it recorded
		// todo at phase done under a live tree. Left to recordObservation alone, the record would
		// move to the snapshot's status with no suspend and no linger, and the record's
		// LastDispatchSeq would reach the snapshot's own sequence, so the real event, once it
		// finally arrives, would be dropped by that same sequence fence — nothing would ever run
		// the transition this snapshot stands in for. A snapshot no newer than the record — a
		// session's own status write the daemon already recorded through sessionStatusWrite, echoed
		// back by a boot listing taken before the daemon's own reassert landed — is level, not a
		// change to apply: status stays what recordObservation below already knows, an admitted
		// root's or a live tree's own state, not the record it briefly showed on Dispatch. Re-read
		// afterward: the engine's own write (phase, hold, linger) must not be clobbered by
		// recordObservation writing back the stale copy this call fetched before the engine ran.
		if _, err := a.engine.Apply(ctx, tx, summary); err != nil {
			return fmt.Errorf("apply engine transition for %s: %w", summary.Key, err)
		}
		refreshed, err := a.store.Issue(ctx, tx, summary.Key)
		if err != nil {
			return fmt.Errorf("re-read %s after its engine transition: %w", summary.Key, err)
		}
		// The engine consumed the snapshot when it recorded the snapshot's sequence: it kept its own
		// status over a status Dispatch already showed (keepStatus), or re-entered a child, and wrote
		// the observation itself. Recording the snapshot's status now would put a set-back's backlog
		// on a running tree, whose slot the release would then free. Otherwise the snapshot's
		// move is recorded, as the live event's would be.
		if refreshed.LastDispatchSeq >= summary.Seq {
			return nil
		}
		stored, status = refreshed, summary.Status
	}
	o := observed{
		Title: summary.Title, Parent: summary.Parent, Rank: summary.Rank,
		Status: status, Shown: summary.Status, Ancestry: summary.Ancestry, HandedOver: handed, Seq: seq,
	}
	if summary.Status == "todo" && handed && readmittable(*stored) {
		return a.readmit(ctx, tx, *stored, o, false)
	}
	// A todo echo of an issue the record shows running is the boot listing catching a status the
	// daemon itself wrote and Dispatch had not shown yet. It is taken only while the issue's tree
	// is not live: a running tree's member keeps what the record says, because the tree is the
	// thing that says the work is under way, and a slot of its own no longer is — the cap counts
	// the issues at work, so a member of a running tree holds one only while it works.
	if summary.Status == "todo" && stored.Status == "in_progress" {
		if _, running := live[stored.Tree]; running {
			return nil
		}
	}
	return a.recordObservation(ctx, tx, *stored, o)
}

// applyObservation records a newer Dispatch observation of a recorded issue: the one place a live
// event's title, rank, parent, label, and status reach the record, so a re-rank, a rename, or the
// label taken off or put back at the same status moves the waiting line at once. A todo on a
// lingering or closed root handed to Legion is a re-admission.
func (a *Admission) applyObservation(ctx context.Context, tx pgx.Tx, stored record.Issue, observation intake.DispatchIssue) error {
	if observation.Seq != 0 && observation.Seq <= stored.LastDispatchSeq {
		return nil
	}
	handed := observation.HandedOver
	o := observed{
		Title: observation.Title, Parent: observation.Parent, Rank: observation.Rank,
		Status: observation.Status, Shown: observation.Status, Ancestry: observation.Ancestry, HandedOver: handed, Seq: observation.Seq,
	}
	if observation.DaemonActor && observation.HandedOver && observation.Status == "todo" {
		if handover, err := a.store.Handover(ctx, tx, stored.Key); err != nil || handover == nil {
			if err != nil {
				return err
			}
		} else if stored.Tree == handover.Tree {
			root, err := a.store.Issue(ctx, tx, handover.Tree)
			if err != nil {
				return err
			}
			// The carry's own test is the child's own run (record.Issue.CarriesRun), deliberately
			// and only that: what the child waits on is not read here. A hand-over moves work
			// already under way into a root of its own, and promotion admits such a candidate
			// whatever its links say (blockers.WaitsOn).
			if !handover.Cancelled && root != nil && root.LingersAt(handover.Generation) {
				a.log.Info("admission hand-over", "issue", stored.Key, "tree", handover.Tree, "linger", handover.Generation)
				return a.readmit(ctx, tx, stored, o, stored.CarriesRun())
			}
		}
	}
	if observation.Status == "todo" && handed && readmittable(stored) {
		return a.readmit(ctx, tx, stored, o, false)
	}
	orphan, err := a.orphan(ctx, tx, stored, observation.Status, handed)
	if err != nil {
		return err
	}
	if orphan {
		// An orphan that started a phase is the same work as a hand-over, whoever handed it to
		// Legion: a child stranded by a root that closed before its hand-over existed carries its
		// phase, gate and pull request too (record.Issue.CarriesRun).
		a.log.Info("admission orphan", "issue", stored.Key, "parent", observation.Parent)
		return a.readmit(ctx, tx, stored, o, stored.CarriesRun())
	}
	return a.recordObservation(ctx, tx, stored, o)
}

// consumeHandover completes an outbox hand-over when Dispatch already showed its todo and label.
// It is the direct equivalent of the daemon-authored event path, but cannot be mistaken for an
// ordinary daemon todo echo because it carries the persisted fence identity. It logs the hand-over
// as the event path does, marked as the outbox's, once it commits: the live proof tells the two
// consumers of a person's labelled todo (this, or admission's orphan carry) apart by it. Its carry
// is the event path's: record.Issue.CarriesRun, and not what the child waits on.
func (a *Admission) consumeHandover(ctx context.Context, tx pgx.Tx, fact intake.Handover) error {
	stored, err := a.store.Issue(ctx, tx, fact.Issue)
	if err != nil || stored == nil {
		return err
	}
	handover, err := a.store.Handover(ctx, tx, fact.Issue)
	if err != nil || handover == nil || handover.Cancelled || handover.Tree != fact.Tree || handover.Generation != fact.Generation || stored.Tree != fact.Tree {
		return err
	}
	root, err := a.store.Issue(ctx, tx, fact.Tree)
	if err != nil || root == nil || !root.LingersAt(fact.Generation) {
		return err
	}
	intake.OnCommit(ctx, func() {
		a.log.Info("admission hand-over", "issue", fact.Issue, "tree", fact.Tree, "linger", fact.Generation, "via", "outbox")
	})
	return a.readmit(ctx, tx, *stored, observed{
		Title: stored.Title, Parent: deref(stored.Parent), Rank: stored.Rank, Ancestry: stored.Ancestry, Seq: stored.LastDispatchSeq,
	}, stored.CarriesRun())
}

// orphan says whether an observation makes a recorded child an orphan, admitted as a root of its
// own: the child is todo and handed to Legion, and its tree is not live. Only a change hands it
// over — its move into todo, or the label reaching it while it is todo — so a later edit of a child
// that was both already is none. The tree is read either way: the workflow handler, which runs
// first, re-enters a live tree's child moved into todo, but not one the label reaches while it
// waits in its live tree, where the label changes nothing.
func (a *Admission) orphan(ctx context.Context, tx pgx.Tx, stored record.Issue, status string, handed bool) (bool, error) {
	if status != "todo" || !handed || claim.IsTreeRoot(stored.Key, stored.Tree) || (stored.Status == "todo" && stored.HandedOver) {
		return false, nil
	}
	root, err := a.store.Issue(ctx, tx, stored.Tree)
	if err != nil {
		return false, fmt.Errorf("read the tree root of %s: %w", stored.Key, err)
	}
	if root == nil {
		return true, nil
	}
	live, err := record.TreeLive(ctx, a.store, tx, *root)
	if err != nil {
		return false, fmt.Errorf("read whether %s's tree is live: %w", stored.Key, err)
	}
	return !live, nil
}

// observed is what one Dispatch observation of an issue says about it, from a live event or from
// the boot read: Shown is the status Dispatch showed, and Status the one the record takes, which is
// Shown except from a boot listing no newer than the record, which keeps the record's own.
// Ancestry is the Dispatch chain its preparation read, which a record takes as the engine's
// Ancestry says.
type observed struct {
	Title, Parent, Rank, Status, Shown string
	Ancestry                           []string
	HandedOver                         bool
	Seq                                int64
}

// recordObservation is where admission writes a Dispatch observation to an issue record: a live
// event's and the boot read's, so a rename, a re-rank, a re-parent, a label or a status change is
// written the same way whichever brought it. Each caller owns its own guards — the stream's
// sequence fence, the boot read's — and this writes what they let through. The engine, which runs
// first, writes the observation itself when it keeps its own status over the event's (its
// keepStatus: a session's write it sets back, or an edit that changed no status), and a live event
// it wrote then stops at the sequence fence here. A re-parent never changes the record's tree
// here: the only path that does is adoption, which the same fact's promote runs next
// (adoptQueuedRoots) for a todo root that never ran and whose new parent is in a live tree.
func (a *Admission) recordObservation(ctx context.Context, tx pgx.Tx, stored record.Issue, o observed) error {
	ancestry, err := a.engine.Ancestry(ctx, tx, stored, o.Ancestry)
	if err != nil {
		return fmt.Errorf("read whether %s ran as a tree of its own: %w", stored.Key, err)
	}
	if stored.Title == o.Title && stored.Rank == o.Rank && stored.Status == o.Status && stored.DispatchStatus == o.Shown &&
		stored.HandedOver == o.HandedOver && sameParent(stored.Parent, record.ParentOf(o.Parent)) && stored.LastDispatchSeq == o.Seq &&
		slices.Equal(stored.Ancestry, ancestry) {
		return nil
	}
	stored.Title = o.Title
	stored.Parent = record.ParentOf(o.Parent)
	stored.Rank = o.Rank
	stored.Status = o.Status
	stored.DispatchStatus = o.Shown
	stored.HandedOver = o.HandedOver
	stored.LastDispatchSeq = o.Seq
	stored.Ancestry = ancestry
	if err := a.store.PutIssue(ctx, tx, stored); err != nil {
		return fmt.Errorf("record observation of %s: %w", stored.Key, err)
	}
	return nil
}

// readmittable says whether a todo on this record starts a new generation of its tree: it is a
// root, and its tree lingers after its sign-off or was closed. A child's done is only the child's.
func readmittable(stored record.Issue) bool {
	return claim.IsTreeRoot(stored.Key, stored.Tree) && (stored.Lingers() || stored.Phase == phase.Done)
}

// readmit records a lingering or closed root's todo, or an orphan's, as a new generation waiting
// for a slot, taking o's title, parent, rank, Dispatch chain (as the engine's Ancestry takes it;
// a root that left another tree takes none) and sequence. A root starts a fresh generation: its
// facts clear across its whole tree, as do those of an orphan readmitted without carry. An orphan
// readmitted with carry (one that started a phase) is the same work moving from a finished root to
// a root of its own, so its phase, phase rows, gate and pull request (with its observed head) carry
// into the new generation, and its promotion starts both its architect and the role of that
// carried phase. A merged pull request is never observed as merged again, and a review, acceptance
// or READY would otherwise rerun work already decided or landed.
//
// An issue leaving another tree, a hand-over or an orphan, carried or not, records that tree
// (record.Issue.HandedOverFrom) and stays a tree of its own from then on: re-admitting or reopening
// the root it left never takes it back (workflow's RanAsRoot), nor does any other tree's release.
//
// The record's record.Issue.TakenBack is the workflow's mark that the issue has stood in a phase of
// its own (workflow.Engine.transition writes it), and promotion exempts a root carrying it as a tree
// Legion takes back (blockers.Exempt). Only the issue's own tree coming back is that: so a root
// whose tree ran keeps the mark here and starts again whatever its links say, while a child
// arriving from another tree's run without its run loses it — what it ran was that tree's work, and
// the root of its own it becomes has run nothing — and is checked like any other root, as is a
// waiting root that only left the line and returned, which never earned one. An orphan carrying its
// run keeps its mark with the run: that run is its new tree's from here on, and the tree it roots
// has run when it is taken back. Nothing here writes the mark: a run is earned by running, never by
// arriving.
func (a *Admission) readmit(ctx context.Context, tx pgx.Tx, stored record.Issue, o observed, carry bool) error {
	if stored.Generation == ^uint64(0) {
		return fmt.Errorf("re-admit %s: generation overflows", stored.Key)
	}
	orphan := carry && !claim.IsTreeRoot(stored.Key, stored.Tree)
	oldTree, oldGeneration := stored.Tree, stored.Generation
	var gate *record.DesignGate
	var pr *record.PullRequest
	var err error
	if orphan {
		gate, err = a.store.Gate(ctx, tx, oldTree)
		if err != nil {
			return fmt.Errorf("read gate of re-admitted orphan %s: %w", stored.Key, err)
		}
		pr, err = a.store.PullRequest(ctx, tx, stored.Key)
		if err != nil {
			return fmt.Errorf("read pull request of re-admitted orphan %s: %w", stored.Key, err)
		}
	}
	stored.Project = a.project
	stored.Title = o.Title
	stored.HandedOver = true
	stored.Parent = record.ParentOf(o.Parent)
	stored.Tree = stored.Key
	// A member leaving another tree, carried or not, is a root of its own from here on: the tree it
	// left is recorded so nothing adopts it back (workflow's RanAsRoot). A root re-admitting itself
	// keeps whatever its record says.
	if oldTree != stored.Key {
		stored.HandedOverFrom = oldTree
	}
	// A root that left another tree, a carried orphan with its run's mark (record.Issue.TakenBack)
	// among them, stays its own tree (workflow's RanAsRoot) and records no chain to be held under.
	stored.Ancestry = nil
	if !orphan {
		stored.Phase, stored.Hold, stored.Awaiting, stored.ReadyPendingVersion = phase.Admitted, nil, nil, nil
		// Another tree's work arriving without its run: the run it had was that tree's.
		if !claim.IsTreeRoot(stored.Key, oldTree) {
			stored.TakenBack = false
		}
		// Read once the mark and the tree it left are settled: only a root re-admitting itself that
		// never ran and never left another tree keeps a chain.
		if stored.Ancestry, err = a.engine.Ancestry(ctx, tx, stored, o.Ancestry); err != nil {
			return fmt.Errorf("read whether re-admitted %s ran as a tree of its own: %w", stored.Key, err)
		}
	}
	stored.Generation++
	stored.Status, stored.DispatchStatus = "todo", "todo"
	stored.Rank = o.Rank
	stored.LingerUntil = nil
	stored.LastDispatchSeq = o.Seq
	if err := a.store.PutIssue(ctx, tx, stored); err != nil {
		return fmt.Errorf("record re-admission %s: %w", stored.Key, err)
	}
	// A generation that ended takes what its architect handed back with it: the re-admission is a
	// handout, so an ask.opened or a hand-back of the old run, delivered late, at or below what the
	// record holds now, declares nothing in this one. Its catch-up cursor starts again too, so the
	// next boot reads from this handout rather than replaying the old generation's tail, whose
	// changes requests the new generation would tell again.
	if err := a.store.StartHandout(ctx, tx, stored.Key); err != nil {
		return fmt.Errorf("record re-admission %s: %w", stored.Key, err)
	}
	if err := a.store.RestartCaughtUp(ctx, tx, stored.Key); err != nil {
		return fmt.Errorf("record re-admission %s: %w", stored.Key, err)
	}
	// A child leaving its old tree, carried or fresh, takes its hand-over fence with it: left behind,
	// it would read a later daemon todo of this root (a controller's reopen) as the stale hand-over.
	if oldTree != stored.Key {
		if err := a.store.DeleteHandover(ctx, tx, stored.Key); err != nil {
			return err
		}
	}
	if orphan {
		if pr != nil {
			// The carried pull request starts the generation's fence; its head run goes, as with a
			// new generation (record.ClearGeneration), since its replay would put the old fence back.
			pr.Generation, pr.Run = int64(stored.Generation), nil
			if err := a.store.PutPullRequest(ctx, tx, *pr); err != nil {
				return fmt.Errorf("carry pull request to re-admitted orphan %s: %w", stored.Key, err)
			}
		}
		// Every root a hand-over created is recorded as one, so its architect's catch-up says it was
		// handed over and decomposes nothing anew; it is marked as carrying a gate, which tells the
		// architect never to register its own spec, only when its old root had one. A root reopened
		// in its linger has its gate cleared with the generation, so a second close hands its
		// children over with none: such a child keeps its phase and pull request, and its architect
		// registers its spec as a fresh root's does, the gate its READY needs.
		if gate != nil {
			gate.Issue = stored.Key
			if err := a.store.PutGate(ctx, tx, *gate); err != nil {
				return fmt.Errorf("carry gate to re-admitted orphan %s: %w", stored.Key, err)
			}
		}
		return a.store.PutHandedOverRoot(ctx, tx, record.HandedOverRoot{Issue: stored.Key, Generation: stored.Generation, GateCarried: gate != nil})
	}
	// Only the old tree's own root coming back cancels its pending hand-overs; an orphan of that
	// tree admitted by a person leaves its siblings' fences standing.
	if claim.IsTreeRoot(stored.Key, oldTree) {
		if err := a.store.CancelTreeHandovers(ctx, tx, oldTree, oldGeneration); err != nil {
			return err
		}
	}
	if err := a.keepStaleApproval(ctx, tx, stored.Key); err != nil {
		return err
	}
	// A root set back to todo is a new generation of the whole tree, so the old generation's pull
	// request, gate, handoffs and pending READY go for every issue of it, not only the root's.
	return a.store.ClearTreeGeneration(ctx, tx, stored.Tree)
}

// keepStaleApproval keeps, for the architect start of the root key re-admission is clearing, the
// person's approval its gate holds of a version the spec has since moved past: the new
// generation's architect registers its spec again on a cleared gate, and its start task
// (workflow.Engine.ParkedDecisionTask) tells it to request approval of the latest version. Under
// gates.design off, which asks no person, nothing is kept and any pair an earlier re-admission kept
// is dropped: no start names one there, so no delivery would forget it. Under an armed gate, a gate
// that holds no stale approval drops an earlier pair, which this gate has since outgrown; a root
// with no gate at all (an earlier re-admission cleared it and nothing has registered since) keeps
// the earlier pair, which no start has delivered yet.
func (a *Admission) keepStaleApproval(ctx context.Context, tx pgx.Tx, key string) error {
	if a.engine.DesignGateOff() {
		return a.store.DropReadmittedApproval(ctx, tx, key)
	}
	gate, err := a.store.Gate(ctx, tx, key)
	if err != nil {
		return fmt.Errorf("read the gate of re-admitted %s: %w", key, err)
	}
	switch {
	case gate == nil:
		return nil
	case gate.ApprovedVersion == nil || *gate.ApprovedVersion >= gate.LatestVersion:
		return a.store.DropReadmittedApproval(ctx, tx, key)
	}
	return a.store.KeepReadmittedApproval(ctx, tx, key, *gate.ApprovedVersion, gate.LatestVersion)
}

// needsSlot says whether issue must hold one of the cap's slots now: a worker of it has work
// (atWork), its tree is running — live, with a root that does not linger after its close — and
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
// root is the record of the issue's tree root and gate that root's design gate, both from the
// one read the pass makes of each (record.LineFacts); live is whether the tree is live, from the
// same facts (record.LiveRoots). The slot is accounting only: taking one away suspends no claim,
// moves no phase and writes no Dispatch status.
func needsSlot(issue, root record.Issue, gate record.DesignGate, live bool) bool {
	return live && !root.Lingers() && !record.OutOfWorkflow(issue.Status) && record.AtWork(issue, gate)
}

// releaseSlots releases the slot of every issue of this project that no longer needs one
// (needsSlot) and reports those issues in slot order. Each release is logged once the fact
// commits. It is the one release the daemon makes: a tree leaving the workflow, a root finishing,
// an issue reaching a phase that only waits and a linger all read the same predicate, so there is
// one rule and one place a slot can go.
//
// It reads the slotted issues in one query (record.Store.SlottedIssues), never a record per slot
// and never the whole issues table: every fact applies this under the one fact lock
// (intake.ApplyFact), so a read per slot cost every fact of every consumer, the API and the
// boot's replay a round trip to the database for each slot of the shared table, and a read of
// every record would grow with every issue the project has ever recorded. The read leaves out
// another project's slot, whose own daemon releases it. The two further facts the predicate needs
// of each slotted issue — its tree root and that root's design gate — come from issues and facts,
// which the pass read once for all of them, so this still makes exactly one query.
func (a *Admission) releaseSlots(ctx context.Context, tx pgx.Tx, issues []record.Issue, facts record.LineFacts) ([]string, error) {
	own, err := a.store.SlottedIssues(ctx, tx, a.project)
	if err != nil {
		return nil, fmt.Errorf("list this project's slotted issues: %w", err)
	}
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	live := record.LiveRoots(issues, facts)
	inUse := len(own)
	var released []string
	for _, issue := range own {
		// A slotted issue whose root the live records do not hold belongs to a tree that has
		// finished: LiveRoots does not name it either, so the predicate releases it.
		_, running := live[issue.Tree]
		if needsSlot(issue, byKey[issue.Tree], facts.Gates[issue.Tree], running) {
			continue
		}
		if err := a.store.ReleaseSlot(ctx, tx, issue.Key); err != nil {
			return nil, fmt.Errorf("release the slot of %s: %w", issue.Key, err)
		}
		inUse--
		released = append(released, issue.Key)
		a.logSlot(ctx, "released", issue.Key, inUse)
	}
	return released, nil
}

// logSlot logs a slot released or taken once the fact commits: the issue, the slots the project
// holds after the change, and the cap.
func (a *Admission) logSlot(ctx context.Context, change, issue string, inUse int) {
	intake.OnCommit(ctx, func() { a.log.Info("admission: slot "+change, "issue", issue, "slots", inUse, "cap", a.cap) })
}

// promote gives each issue of the waiting line what its promotion needs — a slot, a tree opened,
// a first phase started, an architect resumed — in the line's own order, until the cap is full
// or the line empties, and reports how many candidates it promoted and the slots it released.
// Before it promotes any, every admitted root that now waits only on a person enters that wait
// (workflow's ReconcileAwaiting), every issue that no longer needs its slot gives it back
// (releaseSlots), and every root left in the workflow that no slot can ever reach leaves it
// (releaseUnslotted), so each pass of admission — a live fact, the boot read, a release, a tick
// — repairs that state rather than keeping it. An admitted root's architect start is queued
// behind a row creating its issue branch (record.IssueBranch), which the start waits for.
func (a *Admission) promote(ctx context.Context, tx pgx.Tx) (int, []string, error) {
	return a.promoteHolds(ctx, tx, true)
}

// promoteHolds is promote; respectHolds false is release's own call, once every held key's own
// summary has already been applied: release always ignores every hold, never a subset, since it
// only ever runs once the consumer has caught up to every key Reconcile deferred. The in-memory
// pending set itself only clears after that whole call's transaction commits (see release), so
// without this, the pass's own membership check would hold every one of them back in the very
// call that released them. A queued root a live tree took in (adoptQueuedRoots) leaves the line
// before anything else reads it.
//
// It makes one read of each kind for the whole pass and shares it: one record.Store.LiveIssues,
// one record.Store.Slots, one bulk tree-lifecycle read, one design-gate read, one read of the
// starts the project owes its workers (the three of record.ReadLineFacts) and one read of the
// Dispatch mirror rows of everything it judges (blockers.Judged, blockers.Load). The release,
// the waiting line, the INFRA-129 safety net, the promotion and the two repair passes all judge
// from those, so no path reads per slot, per tree or per issue under the one fact lock every
// fact takes (intake.ApplyFact). A read is made again only where this pass has itself written
// something that would make it stale (reread).
//
// Every path that promotes comes through here — a live fact, the controller tick, a hold's
// release, the boot's Reconcile — so each of them is also a look at every issue a blocker holds
// and at every slot whose worker has lost its task.
func (a *Admission) promoteHolds(ctx context.Context, tx pgx.Tx, respectHolds bool) (int, []string, error) {
	issues, slots, facts, err := a.adoptQueuedRoots(ctx, tx, respectHolds)
	if err != nil {
		return 0, nil, err
	}
	heldKeys := a.catchingUpFor
	if respectHolds {
		heldKeys = func(key string) bool { return a.held(key) || a.catchingUpFor(key) }
	}
	_, changed, err := a.engine.ReconcileAwaiting(ctx, tx, a.project, issues, heldKeys)
	if err != nil {
		return 0, nil, fmt.Errorf("reconcile the roots waiting on a person: %w", err)
	}
	// Reconcile wrote roots of the snapshot (a wait entered or answered, a resume told in place),
	// so what follows reads them as they now stand: an answer it recorded puts its root at the
	// front of this pass's waiting line.
	if changed {
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return 0, nil, err
		}
	}
	released, err := a.releaseSlots(ctx, tx, issues, facts)
	if err != nil {
		return 0, nil, err
	}
	if len(released) > 0 {
		if slots, err = a.store.Slots(ctx, tx); err != nil {
			return 0, released, fmt.Errorf("list admission slots: %w", err)
		}
	}
	rows, facts, line, err := blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, nil)
	if err != nil {
		return 0, released, err
	}
	unslotted, err := a.releaseUnslotted(ctx, tx, issues, record.LiveRoots(issues, facts), rows, facts.ChildWaits, respectHolds)
	if err != nil {
		return 0, released, err
	}
	if unslotted {
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return 0, released, err
		}
		if rows, facts, line, err = blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, rows); err != nil {
			return 0, released, err
		}
	}
	if err := a.tellChildrenFirst(ctx, tx, issues, facts); err != nil {
		return 0, released, err
	}
	if a.cap <= 0 {
		return 0, released, nil
	}
	admitted := 0
	for {
		own := ownSlots(issues, slots)
		stopped, opened := false, false
		for _, candidate := range line {
			issue := candidate.Issue
			// A hold stops the whole pass, the candidates behind this one included. Every
			// candidate competes for the same slots now that the cap counts the project's
			// issues rather than its trees, so passing one over would invert Dispatch's rank
			// order for all of them and not only inside the held candidate's own tree — and a
			// slot is never taken back, so that inversion would be permanent. The design is
			// that the daemon gives the first issue of the line its slot; skipping the first
			// issue is not that. The cost is bounded: a hold ends when the Dispatch consumer
			// reaches the position Reconcile captured for it, and every fact, tick, hold
			// release and boot runs this pass again, so a hold delays a pass rather than
			// holding a state. heldKeys is the one predicate, the same closure
			// ReconcileAwaiting was given above: with respectHolds false — release's own call —
			// it is the boot catch-up's hold alone, so a release bypasses the Dispatch
			// consumer's hold and never the catch-up's.
			if heldKeys(issue.Key) {
				stopped = true
				break
			}
			// A candidate that waits on an unfinished Dispatch issue is passed over and keeps
			// its place: the next pass promotes it once the last issue it waits on is done.
			// That is a decided skip rather than a hold — the pass knows enough to decide it —
			// so the line moves on. record.Waiting still carries it, deliberately: dropped from
			// the line, a root would make record.TreeLive false for itself and its children
			// would be recorded as orphans admitted as roots of their own.
			if waits := candidateWaitsOn(rows, candidate); len(waits) > 0 {
				a.log.Info("admission waits for what this issue waits on", "issue", issue.Key, "blockers", waits)
				continue
			}
			gate := facts.Gates[issue.Tree]
			// A candidate whose promotion puts no worker to work takes no slot and so is never
			// held by the cap: a handed-over root carrying accepting opens its tree at once, and
			// an answered root whose gate has already opened resumes its architect at once. A
			// start that follows such a resume joins the line's first part, with a slot of its
			// own.
			takes := slotFor(candidate, gate)
			if takes && len(own) >= a.cap {
				break
			}
			if candidate.Kind == record.LineAnswered {
				// The root's tree already runs: only a reason it may not resume in its own
				// session holds it here (its label gone, a foreign Dispatch claim), which
				// releaseUnslotted takes out of the workflow on this same pass. Its links are
				// not re-read, as no running tree's are.
				reason, err := workflow.Unresumable(ctx, a.store, tx, issue)
				if err != nil {
					return admitted, released, fmt.Errorf("check whether answered root %s can resume: %w", issue.Key, err)
				}
				if reason != "" {
					a.log.Warn("admission skips an answered root it may not resume", "issue", issue.Key, "reason", reason)
					continue
				}
			}
			if candidate.Kind == record.LineQueuedRoot {
				// Claims, tree lifecycles and the runtime key a tree by the normalized project
				// token, not the Dispatch project key the issue record carries.
				project, err := claim.ProjectToken(a.project)
				if err != nil {
					return admitted, released, fmt.Errorf("open admission lifecycle of %s: %w", issue.Key, err)
				}
				if _, err := a.store.OpenTreeLifecycle(ctx, tx, project, issue.Tree, treelifecycle.AuthorityWorkflow); err != nil {
					if errors.Is(err, treelifecycle.ErrCleanupReserved) {
						a.log.Info("admission waits for tree cleanup", "issue", issue.Key, "tree", issue.Tree)
						continue
					}
					// The operator opened a tree under this root's key: this root waits until
					// that tree's cleanup confirms, and the candidates behind it go on.
					if errors.Is(err, treelifecycle.ErrAuthorityHeld) {
						a.log.Warn("admission waits for its tree's other authority", "issue", issue.Key, "tree", issue.Tree, "error", err)
						continue
					}
					return admitted, released, fmt.Errorf("open admission lifecycle of %s: %w", issue.Key, err)
				}
			}
			now := a.now()
			if takes {
				slot := record.Slot{Issue: issue.Key, Index: nextSlotIndex(slots), AdmittedAt: now}
				if err := a.store.PutSlot(ctx, tx, slot); err != nil {
					return admitted, released, fmt.Errorf("put admission slot for %s: %w", issue.Key, err)
				}
				slots, own = append(slots, slot), append(own, slot)
				a.logSlot(ctx, "taken", issue.Key, len(own))
			}
			switch candidate.Kind {
			case record.LineReturning:
				// The transition that sent this issue's work back wrote its worker's task in its
				// own transaction and left it unclaimable until a slot row of this issue existed
				// (record.SuperviseRequest.NeedsSlot). The slot above is the whole promotion:
				// the row is claimable the moment it commits, and the grant wakes the issue's
				// lifecycle lane (the daemon's projectRecords.PutSlot), so the task is delivered
				// in the same second rather than at the next sweep.
				a.log.Info("admission gives a slot back to an issue whose work came back", "issue", issue.Key, "phase", issue.Phase)
			case record.LineAnswered:
				awaiting := issue.Awaiting
				// An answered root keeps the status it ran with, so its resume writes none,
				// unless a person set it back to todo while it waited: then, like a fresh root,
				// it is written in_progress.
				writeInProgress := issue.Status == "todo"
				issue.Phase, issue.Awaiting = phase.Admitted, nil
				if writeInProgress {
					issue.Status = "in_progress"
				}
				issue.Ancestry = nil
				if err := a.store.PutIssue(ctx, tx, issue); err != nil {
					return admitted, released, fmt.Errorf("record admitted issue %s: %w", issue.Key, err)
				}
				if writeInProgress {
					if err := a.enqueue(ctx, tx, issue.Key, record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}, now); err != nil {
						return admitted, released, err
					}
				}
				// ResumeAwaitedRoot is the resume's handout (record.Store.StartHandout).
				if err := a.engine.ResumeAwaitedRoot(ctx, tx, issue, *awaiting); err != nil {
					return admitted, released, fmt.Errorf("resume %s after a person's answer: %w", issue.Key, err)
				}
				opened = opened || classify.DesignGateOpen(gate)
			case record.LineQueuedRoot:
				issue.Status = "in_progress"
				// A root whose tree is opened runs as a tree of its own, which no ancestry hold
				// holds (workflow's Ancestry), so the chain it queued under goes now: a root
				// coming back to the line later is never held by a chain left from before it ran.
				issue.Ancestry = nil
				if err := a.store.PutIssue(ctx, tx, issue); err != nil {
					return admitted, released, fmt.Errorf("record admitted issue %s: %w", issue.Key, err)
				}
				if err := a.enqueue(ctx, tx, issue.Key, record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}, now); err != nil {
					return admitted, released, err
				}
				// A fresh admission hands the architect its tree: what it hands back to a person
				// afterwards is what it may wait on (workflow's ReconcileAwaiting).
				if err := a.store.StartHandout(ctx, tx, issue.Key); err != nil {
					return admitted, released, fmt.Errorf("record admitted issue %s: %w", issue.Key, err)
				}
				if err := a.enqueue(ctx, tx, issue.Key, record.IssueBranch{Generation: issue.Generation}, now); err != nil {
					return admitted, released, err
				}
				task, kept, err := a.engine.ParkedDecisionTask(ctx, tx, issue)
				if err != nil {
					return admitted, released, fmt.Errorf("name the decisions on %s while nobody in its tree could hear them: %w", issue.Key, err)
				}
				// The architect start is not one of the starts that wait for a slot: this very
				// transaction grants the tree whatever slot it needs, and an architect starts no
				// phase worker.
				if err := a.engine.EnqueueNaming(ctx, tx, issue.Key, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree,
					Role: claim.RoleArchitect, Generation: issue.Generation, Task: task, Kept: kept}, kept, now); err != nil {
					return admitted, released, fmt.Errorf("enqueue the architect start of %s: %w", issue.Key, err)
				}
				if err := a.startMidPhaseMembers(ctx, tx, issue, append(issues, issue), now); err != nil {
					return admitted, released, err
				}
				if err := a.reenterStrandedChildren(ctx, tx, issue, issues); err != nil {
					return admitted, released, err
				}
				opened = opened || classify.DesignGateOpen(gate)
			case record.LineFirstPhase:
				if err := a.engine.StartFirstPhase(ctx, tx, issue, rows[issue.Key].Blockers); err != nil {
					return admitted, released, err
				}
			}
			admitted++
		}
		// A resume, or a tree opened above whose design gate already stands open, releases
		// members of its own tree that stood at admitted on the snapshot this round read and
		// would otherwise wait a whole fact for their first phase. The records are read again
		// and the line rebuilt so they are promoted in this pass; a pass a hold stopped goes no
		// further, since the hold is the answer for everything behind it too.
		if stopped || !opened {
			break
		}
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return admitted, released, err
		}
		if rows, facts, line, err = blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, rows); err != nil {
			return admitted, released, err
		}
	}
	if err := a.resumeSlottedWorkers(ctx, tx, issues, slots); err != nil {
		return admitted, released, err
	}
	if err := a.finishFencedStarts(ctx, tx, issues, facts); err != nil {
		return admitted, released, err
	}
	return admitted, released, nil
}

// reread takes this pass's shared reads again, after the pass has itself written something that
// makes them stale: the project's live records, the slots, and the facts the line, the release
// and the repair passes are all judged from.
func (a *Admission) reread(ctx context.Context, tx pgx.Tx) ([]record.Issue, []record.Slot, record.LineFacts, error) {
	issues, err := a.store.LiveIssues(ctx, tx, a.project)
	if err != nil {
		return nil, nil, record.LineFacts{}, fmt.Errorf("list admission issues: %w", err)
	}
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return nil, nil, record.LineFacts{}, fmt.Errorf("list admission slots: %w", err)
	}
	facts, err := record.ReadLineFacts(ctx, tx, a.store, a.project, issues)
	return issues, slots, facts, err
}

// candidateWaitsOn is what a candidate of the line still waits on among the Dispatch issues its
// mirror row names, under the rule its kind is judged by. A root whose tree is not open yet is
// judged by blockers.WaitsOn, which exempts a tree Legion takes back. An issue whose first phase
// would start is judged by blockers.Holds, which exempts nothing and answers nothing at all for
// a root: a root at admitted inside a running tree starts whatever its links say, since they
// were read when its own tree was opened and a blocker added to a tree already running never
// re-checks it. An issue whose work is already under way — a return, an answered root's resume —
// waits on nothing here, for that same reason.
func candidateWaitsOn(rows map[string]blockers.Row, entry record.LineEntry) []string {
	switch entry.Kind {
	case record.LineQueuedRoot:
		return blockers.WaitsOn(rows, entry.Issue)
	case record.LineFirstPhase:
		return blockers.Holds(rows, entry.Issue)
	default:
		return nil
	}
}

// slotFor says whether promoting this candidate puts a worker of it to work, so that it must
// take one of the cap's slots before it is promoted at all. It is atWork read on the state the
// promotion leaves the issue in: a returning issue and one starting its first phase are at work
// by definition; an answered root goes back to admitted, where it needs a slot only while its
// architect is still writing the spec; and a root whose tree is opened keeps the phase its
// record carries, so a handed-over root arriving in accepting or held needs none while one
// arriving in a worker phase does. A candidate that needs none is promoted whatever the cap: it
// starts no worker, so holding it back would bound nothing and would leave its tree closed for
// no reason.
func slotFor(entry record.LineEntry, gate record.DesignGate) bool {
	switch entry.Kind {
	case record.LineReturning, record.LineFirstPhase:
		return true
	case record.LineAnswered:
		return !gate.EverOpened
	default:
		return record.AtWork(entry.Issue, gate)
	}
}

// held says whether Reconcile still holds key for the Dispatch consumer to reach the stream
// position it captured for it (a.pending).
func (a *Admission) held(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, held := a.pending[key]
	return held
}

// HoldForCatchUp holds each of keys out of the decision whether it waits on a person until
// ReleaseCatchUp releases it: the daemon's boot holds every root its catch-up will read before the
// boot's own admission pass, so no root is decided on a record that has not yet applied what
// Dispatch recorded while the daemon was down.
func (a *Admission) HoldForCatchUp(keys ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.catchingUp == nil {
		a.catchingUp = make(map[string]struct{}, len(keys))
	}
	for _, key := range keys {
		a.catchingUp[key] = struct{}{}
	}
}

// ReleaseCatchUp ends key's catch-up hold; the next pass decides it.
func (a *Admission) ReleaseCatchUp(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.catchingUp, key)
}

// CatchUpHeld is every key the boot's catch-up still holds, in no order.
func (a *Admission) CatchUpHeld() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Collect(maps.Keys(a.catchingUp))
}

// catchingUpFor says whether the boot's catch-up still holds key (HoldForCatchUp).
func (a *Admission) catchingUpFor(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, holding := a.catchingUp[key]
	return holding
}

// tellChildrenFirst tells the architect of each parent whose own first phase now waits for its
// Dispatch children, once per issue and per run (record.Store.TellChildrenFirst). The notice
// names the parent, each open child with the Dispatch status that holds it, and each cycle
// among them (workflow.ChildrenFirstNotice).
//
// The wait is judged again every pass and the telling is written once, which is the whole of
// why the two are recorded differently: every fact, tick, hold release and boot runs this pass,
// so a notice decided by the wait alone would reach the architect over and over for one wait,
// while a wait decided by a stored flag would outlive the child that caused it. A child filed
// later during one wait therefore tells nobody again — the architect already knows the issue
// waits, and the state names what for — and a re-admission, which opens a new generation, tells
// it anew, because that run's architect has not heard it.
//
// The parents are told in key order so that one pass's rows read the same whatever order the
// judgement's map iterated in.
func (a *Admission) tellChildrenFirst(ctx context.Context, tx pgx.Tx, issues []record.Issue, facts record.LineFacts) error {
	if len(facts.ChildWaits) == 0 {
		return nil
	}
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	now := a.now()
	for _, key := range slices.Sorted(maps.Keys(facts.ChildWaits)) {
		parent, recorded := byKey[key]
		if !recorded {
			continue
		}
		told, err := a.store.TellChildrenFirst(ctx, tx, key, parent.Generation, now)
		if err != nil {
			return err
		}
		if !told {
			continue
		}
		if err := a.enqueue(ctx, tx, key, workflow.ChildrenFirstNotice(facts.ChildWaits[key]), now); err != nil {
			return err
		}
		a.log.Info("admission: an issue waits for the Dispatch children under it", "issue", key,
			"tree", parent.Tree, "generation", parent.Generation, "children", len(facts.ChildWaits[key].Open))
	}
	return nil
}

// waitsForChildren says whether an issue's own first phase is held because a Dispatch child of
// its own is still open — the children-first rule of dispatch://INFRA-980. It is the named seam
// the safety net below asks, so that the rule arrives in one place rather than as a condition
// spread through this file, and waits is the pass's own judgement of it
// (record.LineFacts.ChildWaits, filled by blockers.JudgeLine from record.ChildWaits): never a stored
// flag, so a parent whose last open child has gone waits no longer than the pass that reads it.
func (a *Admission) waitsForChildren(waits map[string]record.ChildWait, issue record.Issue) bool {
	_, waiting := waits[issue.Key]
	return waiting
}

// unslotted says whether issue is a root of this project left in the workflow (workflow's
// InWorkflow) with no way to run, the state dispatch://INFRA-129 reported: not lingering, at a
// status only a running tree has (in_progress, testing, needs_review, retro) rather than todo,
// where a root waits in the line (record.Waiting), and in none of the states that explain a root
// holding no slot.
//
// Holding no slot is the normal state of a root now: the cap counts the issues at work, so a root
// in accepting, in awaiting_merge, waiting on a person, waiting in the line, waiting on a blocker
// or waiting for its children holds none while its tree works perfectly well. Every one of those
// is a tree whose lifecycle stands open under the workflow, or whose root waits in the line or on
// a person, which is exactly record.LiveRoots: so liveness is the term that replaces the old slot
// test, and it covers the slots, the line and the awaiting_person wait together. The blocker hold
// and the children-first wait are asked of the issue as well, so that a later rule which holds an
// issue somewhere liveness does not reach cannot silently have its work taken out of the
// workflow.
//
// What is left is a root whose tree nothing will ever open again, recorded at a running status.
// The workflow takes a root out on a status out of the workflow (its leave) but does nothing on a
// running tree's status where no tree runs, so only such a move made while the root waited leaves
// one here: a person's, or an outside session's, which is a person's where no tree runs
// (workflow's sessionStatusWrite). A root in todo without the label is not one: taking the label
// off is a pause the label ends.
func (a *Admission) unslotted(issue record.Issue, live map[string]struct{}, rows map[string]blockers.Row, waits map[string]record.ChildWait) bool {
	if issue.Project != a.project || !claim.IsTreeRoot(issue.Key, issue.Tree) || issue.Lingers() || !record.InWorkflow(issue) || issue.AwaitingPerson() {
		return false
	}
	switch issue.Status {
	case "in_progress", "testing", "needs_review", "retro":
	default:
		return false
	}
	if _, running := live[issue.Key]; running {
		return false
	}
	if a.waitsForChildren(waits, issue) || len(blockers.WaitsOn(rows, issue)) > 0 {
		return false
	}
	return true
}

// releaseUnslotted takes every root no slot can ever reach out of the workflow, as a status
// leaving todo takes a waiting root out (workflow's LeaveUnslotted): nothing would ever give it a
// slot, nothing would ever start it, and its record would keep saying it is admitted. A
// conventional unslotted root belongs to no live tree (unslotted); an awaiting-person root is
// live by design, and leaves only when its hand-over label or its current Dispatch claim makes a
// resume unsafe (workflow's Unresumable). Legion starts nothing for either — it is someone else's
// work now, or a person's move to make — and the controller is told on the controller topic alone
// (record.UnslottedNotice, one per root, its reason naming why). A key Reconcile holds is left for
// release to apply first, when respectHolds is set. It reports whether it released any.
func (a *Admission) releaseUnslotted(ctx context.Context, tx pgx.Tx, issues []record.Issue, live map[string]struct{},
	rows map[string]blockers.Row, waits map[string]record.ChildWait, respectHolds bool,
) (bool, error) {
	released := false
	for _, issue := range issues {
		if respectHolds && a.held(issue.Key) {
			continue
		}
		why, awaiting, err := a.awaitingUnresumable(ctx, tx, issue)
		if err != nil {
			return released, fmt.Errorf("check whether awaiting root %s can resume: %w", issue.Key, err)
		}
		if !awaiting && !a.unslotted(issue, live, rows, waits) {
			continue
		}
		reason := fmt.Sprintf("%s was %s with no admission slot, a status only a running tree has, so no slot would ever reach it", issue.Key, issue.Status)
		if why != "" {
			reason = fmt.Sprintf("%s awaits a person without an admission slot, but %s", issue.Key, why)
		}
		if err := a.engine.LeaveUnslotted(ctx, tx, issue); err != nil {
			return released, fmt.Errorf("release unslotted root %s: %w", issue.Key, err)
		}
		reason += "; the daemon took it out of the workflow and starts nothing for it; todo with the legion label queues it again"
		if err := a.enqueue(ctx, tx, issue.Key, record.ControllerNotice{Kind: record.UnslottedNotice, Reason: reason}, a.now()); err != nil {
			return released, err
		}
		intake.OnCommit(ctx, func() {
			a.log.Warn("admission: released a root that held no slot and waited for none", "issue", issue.Key, "status", issue.Status, "phase", issue.Phase, "generation", issue.Generation)
		})
		released = true
	}
	return released, nil
}

// awaitingUnresumable returns why an awaiting root of this project may not resume (workflow's
// Unresumable, the one rule ReconcileAwaiting also asks) and whether this pass must remove it. It
// asks nothing of the slots: a root waiting on a person holds none by the release rule itself
// (needsSlot), so a slot test here would never answer anything.
func (a *Admission) awaitingUnresumable(ctx context.Context, tx pgx.Tx, issue record.Issue) (string, bool, error) {
	if issue.Project != a.project || !claim.IsTreeRoot(issue.Key, issue.Tree) || issue.Lingers() || !record.InWorkflow(issue) || !issue.AwaitingPerson() {
		return "", false, nil
	}
	reason, err := workflow.Unresumable(ctx, a.store, tx, issue)
	return reason, reason != "", err
}

// Held reports whether any key currently waits on the Dispatch consumer reaching the stream
// position Reconcile captured for it. The intake consume loop calls it once per Dispatch message,
// outside any transaction, to decide whether a JetStream Info call and a synthetic position fact
// are worth their cost at all: once nothing is held, every later delivery is a lock check and
// nothing else.
func (a *Admission) Held() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending) > 0
}

// Target is the notification stream position Held's hold is waiting for: the highest target any
// currently deferred key was captured against. Zero while nothing is held. A boot-owned watchdog
// reads it to name what a long-lived hold is still waiting to reach.
func (a *Admission) Target() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.target
}

// Holds counts the times Reconcile has deferred keys into the hold since the daemon started: the
// boot's reconcile, and each recovery of a repository GitHub answered for again. A poller of the
// hold (the daemon's pollHoldRelease) folds it into what it applies, so a later hold, released by
// a position equal to an earlier hold's, is applied rather than taken for that earlier release.
func (a *Admission) Holds() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.holds
}

// refreshHeldSummary keeps a held, unrecorded key's own pending summary — the one release
// eventually applies — current with a live event that is newer than it but creates no record: an
// event moving the key out of todo, or taking its label off, teaches nothing to the record (there
// is none yet), but it is real information about what Dispatch shows now. Left alone, release
// would apply the stale boot summary Reconcile originally deferred, as if this event had never
// arrived. A key that is not held, or an event at or behind the held summary's own sequence, is
// left untouched.
func (a *Admission) refreshHeldSummary(observation intake.DispatchIssue) {
	a.mu.Lock()
	defer a.mu.Unlock()
	summary, held := a.pending[observation.Key]
	if !held || observation.Seq <= summary.Seq {
		return
	}
	a.pending[observation.Key] = intake.DispatchIssue{
		Key: observation.Key, Seq: observation.Seq, Status: observation.Status, Title: observation.Title,
		Parent: observation.Parent, Rank: observation.Rank, HandedOver: observation.HandedOver,
		ResolvedRepo: observation.ResolvedRepo, Ancestry: observation.Ancestry,
	}
}

// release applies the Dispatch consumer's current position: once it has Reached target, every key
// Reconcile deferred is re-applied through applySummary — the listing's own snapshot for it, in
// case the stream never delivers the event that would otherwise have caught it up — before
// promote decides anything, so a held key is never admitted, or left admitted, on stale
// information. The in-memory pending set only clears once this call's own transaction has
// actually committed (intake.OnCommit): clearing it any earlier would say a hold is
// resolved while a failed commit leaves nothing of this release durable, so the next
// promote-triggering call would trust an unreleased hold's authority.
func (a *Admission) release(ctx context.Context, tx pgx.Tx, position intake.DispatchConsumerPosition) ([]string, error) {
	a.mu.Lock()
	caughtUp := len(a.pending) > 0 && position.Reached(a.target)
	var releasing map[string]intake.DispatchIssue
	if caughtUp {
		releasing = maps.Clone(a.pending)
	}
	a.mu.Unlock()

	var released []string
	if caughtUp {
		issues, err := a.store.LiveIssues(ctx, tx, a.project)
		if err != nil {
			return nil, fmt.Errorf("list admission issues: %w", err)
		}
		facts, err := record.ReadLineFacts(ctx, tx, a.store, a.project, issues)
		if err != nil {
			return nil, err
		}
		live := record.LiveRoots(issues, facts)
		held := slices.SortedFunc(maps.Values(releasing), func(x, y intake.DispatchIssue) int { return cmp.Compare(x.Key, y.Key) })
		for _, summary := range parentsFirst(held) {
			if err := a.applySummary(ctx, tx, summary, live); err != nil {
				return nil, err
			}
		}
	}
	admitted, returned, err := a.promoteHolds(ctx, tx, !caughtUp)
	if err != nil {
		return nil, err
	}
	if !caughtUp {
		return returned, nil
	}
	a.log.Info("admission: the Dispatch consumer has caught up; releasing held roots",
		"count", len(releasing), "ack_floor", position.AckFloorStream, "idle", position.Idle, "admitted", admitted)
	intake.OnCommit(ctx, func() {
		a.mu.Lock()
		for key := range releasing {
			delete(a.pending, key)
		}
		a.mu.Unlock()
	})
	return append(released, returned...), nil
}

// ownSlots is the slots of issues, this project's. The slots table is shared by every project's
// daemon, and a slot's index is unique across it (0004_record), so the next index is chosen over
// every slot while the cap counts this project's alone.
func ownSlots(issues []record.Issue, slots []record.Slot) []record.Slot {
	known := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		known[issue.Key] = struct{}{}
	}
	own := make([]record.Slot, 0, len(slots))
	for _, slot := range slots {
		if _, ok := known[slot.Issue]; ok {
			own = append(own, slot)
		}
	}
	return own
}

// startMidPhaseMembers brings back every role of every member of the tree promotion has just
// opened, and gives the one role whose phase the member stands in the task that carries that
// phase on. A hand-over turns a child into that root while preserving its phase, so its phase
// role needs the same resume start ordinary mid-phase children receive (workflow.ResumeTask,
// which tells a carried merger its carried approved head). A root promoted held, which only a
// hand-over leaves (every other re-admission starts its root at admitted), starts no worker, and
// the hold's notice went to the architect of the tree it was handed over from, so its own
// architect is told of the hold (workflow.HeldNotice), whether or not it carried a gate.
// Promotion already queued the root's branch; each other member queues its own before starting.
// Every member resumes in workflow.ResumePhase, recorded before its start: a child of a
// re-admitted tree, and a root a hand-over created, left awaiting its merge resume in merging,
// since the old tree's linger disarmed the pull request (beginLinger) and only a new READY,
// whose merge the daemon submits, arms it again.
//
// It relaunches every role a member can hold rather than the phase's owner alone, because the
// admission cap counts the issues at work now: the phase task is the only thing that waits for a
// slot (workflow.StartOrOwe), and the live sessions of a member's other roles are exactly what
// the design keeps so that a person can still talk to any of them. A relaunch row carries no
// task, so a role with nothing to do comes back and is told nothing. The roles are enumerated as
// every other sweep of them does, over claim.Roles (the engine's everyClaim).
//
// What an issue waits on is deliberately not read here: every member this starts is mid-phase,
// work already under way, and a member standing at admitted is passed over, since nothing of it
// has run and its first phase is the waiting line's to start.
func (a *Admission) startMidPhaseMembers(ctx context.Context, tx pgx.Tx, root record.Issue, issues []record.Issue, now time.Time) error {
	project, err := claim.ProjectToken(a.project)
	if err != nil {
		return err
	}
	if root.Phase == phase.Held && root.Hold != nil {
		if err := a.enqueue(ctx, tx, root.Key, workflow.HeldNotice(root), now); err != nil {
			return err
		}
	}
	for _, member := range issues {
		if member.Tree != root.Tree || record.OutOfWorkflow(member.Status) {
			continue
		}
		if resume := workflow.ResumePhase(member); resume != member.Phase {
			member.Phase = resume
			if err := a.store.PutIssue(ctx, tx, member); err != nil {
				return fmt.Errorf("resume %s in %s: %w", member.Key, resume, err)
			}
		}
		// A member standing at admitted has run nothing in this generation: it holds no session
		// to bring back, and its own first phase starts when the waiting line gives it a slot
		// (record.LineFirstPhase). A root whose tree is opened for the first time is exactly
		// that, so admitting a fresh root relaunches nobody and creates no claim for a worker
		// that has never worked.
		if member.Phase == phase.Admitted {
			continue
		}
		// Accepting's role is the tree's architect, which promotion already started: nothing of
		// that phase is resumed here.
		owner := workflow.RoleFor(member.Phase)
		if owner == claim.RoleArchitect {
			owner = ""
		}
		if member.Key != root.Key {
			if err := a.enqueue(ctx, tx, member.Key, record.IssueBranch{Generation: member.Generation}, now); err != nil {
				return err
			}
		}
		for _, role := range claim.Roles {
			// The tree's architect is promotion's own start, with the task that hands it its
			// tree; a member's own sub-architect is no phase worker and comes back with the rest.
			if claim.IsTreeArchitect(role, member.Key, member.Tree) {
				continue
			}
			if role != owner {
				if err := a.enqueue(ctx, tx, member.Key, record.SuperviseRequest{Op: "start", Tree: member.Tree, Role: role,
					Generation: member.Generation, Phase: member.Phase}, now); err != nil {
					return err
				}
				continue
			}
			token, err := claim.NewToken(project, member.Key, role)
			if err != nil {
				return err
			}
			run, err := a.store.RoleRun(ctx, tx, token, member.Key, role, member.Generation)
			if err != nil {
				return err
			}
			if !workflow.StartFor(run, root, member.Generation, member.Phase) {
				continue
			}
			task, err := workflow.ResumeTask(ctx, a.store, tx, member, role)
			if err != nil {
				return err
			}
			if err := workflow.StartOrOwe(ctx, tx, a.store, member.Key, record.SuperviseRequest{Op: "start", Tree: member.Tree,
				Role: role, Generation: member.Generation, Phase: member.Phase, Task: task, ResumeTask: true}, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// resumeSlottedWorkers is the repair of a slot whose worker has no task: an issue holding a slot
// in a worker phase for which no start is queued and no delivery the claim is still working
// (workflow.StartFor, the pending delivery's StillWorked). That can happen where a start row was
// finished without acting while the slot stayed — a row the outbox fenced out, a row
// finishFencedStarts below finished — and nothing else would ever start that worker again, so
// the issue would hold its slot for ever doing nothing. Each one gets the start a retry of a
// held phase gives (workflow's restart): the assignment its phases row already names, so no
// completion in flight is refused for an assignment the worker was never given, and the phase's
// resume task.
//
// It runs after the cap loop on every pass admission makes, over at most the cap's worth of
// slotted issues, through one batched RoleRuns read rather than a read per slot.
func (a *Admission) resumeSlottedWorkers(ctx context.Context, tx pgx.Tx, issues []record.Issue, slots []record.Slot) error {
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	project, err := claim.ProjectToken(a.project)
	if err != nil {
		return err
	}
	type holder struct {
		issue record.Issue
		root  record.Issue
		role  claim.Role
		token claim.Token
	}
	var holders []holder
	var keys []record.RoleRunKey
	for _, slot := range slots {
		issue, recorded := byKey[slot.Issue]
		if !recorded || issue.Project != a.project {
			continue
		}
		role := workflow.RoleFor(issue.Phase)
		if role == "" || role == claim.RoleArchitect {
			continue
		}
		root, rooted := byKey[issue.Tree]
		if !rooted || root.Lingers() {
			continue
		}
		// An issue whose key yields no claim token has no run to read, as the wait reconcile
		// reads none for such a root (workflow's ReconcileAwaiting): it keeps its slot and this
		// repair passes over it rather than failing the whole fact.
		token, err := claim.NewToken(project, issue.Key, role)
		if err != nil {
			continue
		}
		holders = append(holders, holder{issue: issue, root: root, role: role, token: token})
		keys = append(keys, record.RoleRunKey{Token: token, Issue: issue.Key, Role: role, Generation: issue.Generation})
	}
	if len(keys) == 0 {
		return nil
	}
	runs, err := a.store.RoleRuns(ctx, tx, keys)
	if err != nil {
		return err
	}
	for _, held := range holders {
		run := runs[held.token]
		if !workflow.StartFor(run, held.root, held.issue.Generation, held.issue.Phase) {
			continue
		}
		if c := run.Claim; c != nil && c.Pending != nil && c.Pending.StillWorked(held.issue.Generation, held.issue.Phase, c.State) {
			continue
		}
		if err := a.engine.ResumeSlottedWorker(ctx, tx, held.issue, held.role); err != nil {
			return fmt.Errorf("start the worker of slotted issue %s again: %w", held.issue.Key, err)
		}
		intake.OnCommit(ctx, func() {
			a.log.Warn("admission: a slot held an issue whose worker had no task, so its phase is started again",
				"issue", held.issue.Key, "role", held.role, "phase", held.issue.Phase)
		})
	}
	return nil
}

// finishFencedStarts is the repair of a held start row that has outlived the fences the outbox
// would have run it under: its issue has left the workflow, its tree lingers, or the issue has
// moved past the phase or the generation the row names. The outbox applies those fences when it
// runs a row, but a row waiting for a slot may never be run at all, and one that could never act
// would keep its issue in the waiting line for ever and take a slot from work that could use it.
// Finishing the row deletes it, exactly as the outbox's own fence would.
func (a *Admission) finishFencedStarts(ctx context.Context, tx pgx.Tx, issues []record.Issue, facts record.LineFacts) error {
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	for _, owed := range facts.OwedRows {
		issue, recorded := byKey[owed.Issue]
		root := byKey[issue.Tree]
		if recorded && !record.OutOfWorkflow(issue.Status) && !root.Lingers() &&
			owed.Generation == issue.Generation && owed.Phase == issue.Phase {
			continue
		}
		// The row has never been leased (record.Store.OwedStarts), so it carries no lease token.
		if err := a.store.FinishOutbox(ctx, tx, owed.ID, ""); err != nil {
			return fmt.Errorf("finish the start %s owed a worker it can no longer reach: %w", owed.Issue, err)
		}
		intake.OnCommit(ctx, func() {
			a.log.Info("admission: finished a held start whose issue has moved past it", "issue", owed.Issue, "row", owed.ID,
				"role", owed.Role, "phase", owed.Phase, "generation", owed.Generation)
		})
	}
	return nil
}

// reenterStrandedChildren re-enters, at candidate's promotion, every tree member admission left
// recorded todo with its previous run's phase at done: a reopen admission declined to re-enter for
// want of the label while the tree was not live, which recordObservation otherwise records with the
// phase untouched. Candidate's own promotion is what makes the tree live for these, since nothing
// else reaches them afterward — Engine.ReenterChild only re-enters a live tree's reopened child on
// its own live event, and a later todo observation of one already todo changes nothing. The
// synthetic fact carries the child's own already-recorded fields: no live event reopened it, so
// there is nothing newer to apply, only its own next generation to start.
//
// What a child waits on is deliberately not read here either: this re-enters a child whose
// previous run finished (phase.Done), and Engine.ReenterChild's own entry withholds the start of
// one that waits on unfinished work.
func (a *Admission) reenterStrandedChildren(ctx context.Context, tx pgx.Tx, candidate record.Issue, issues []record.Issue) error {
	for _, child := range issues {
		if child.Key == candidate.Key || child.Tree != candidate.Tree {
			continue
		}
		if child.Status != "todo" || child.Phase != phase.Done {
			continue
		}
		if err := a.engine.ReenterChild(ctx, tx, child, workflow.RecordedFact(child)); err != nil {
			return fmt.Errorf("re-enter stranded child %s: %w", child.Key, err)
		}
	}
	return nil
}

func (a *Admission) enqueue(ctx context.Context, tx pgx.Tx, issue string, payload record.OutboxPayload, now time.Time) error {
	row, err := record.NewOutboxRow(issue, payload, now)
	if err != nil {
		return fmt.Errorf("build %s outbox row for %s: %w", payload.OutboxKind(), issue, err)
	}
	if err := a.store.Enqueue(ctx, tx, row); err != nil {
		return fmt.Errorf("enqueue %s outbox row for %s: %w", payload.OutboxKind(), issue, err)
	}
	return nil
}

func nextSlotIndex(slots []record.Slot) int {
	used := make(map[int]struct{}, len(slots))
	for _, slot := range slots {
		used[slot.Index] = struct{}{}
	}
	for index := 0; ; index++ {
		if _, present := used[index]; !present {
			return index
		}
	}
}

func sameParent(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
