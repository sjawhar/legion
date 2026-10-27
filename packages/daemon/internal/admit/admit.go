// Package admit owns root admission slots and the Dispatch-rank waiting line.
package admit

import (
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
	pending map[string]dispatch.IssueSummary
	// target is the notification stream's own last sequence Reconcile captured when it found the
	// first record behind: every key in pending releases once the Dispatch consumer's position
	// has Reached it. Zero means nothing is held.
	target int64
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
// keeping a second copy of the suspend-and-gate-open re-entry.
func New(store record.Store, engine *workflow.Engine, cap int, project string, log *slog.Logger) *Admission {
	if log == nil {
		log = slog.Default()
	}
	return &Admission{store: store, engine: engine, cap: cap, project: project, log: log, now: time.Now, pending: make(map[string]dispatch.IssueSummary)}
}

// Apply records root and orphan todo observations of issues handed to Legion and every newer
// observation of a recorded issue, wakes the controller for an unrecorded root in triage handed to
// Legion (a controller notice, not a record), releases slots roots returned after they completed
// or began waiting only on a person, promotes waiting roots while capacity remains, and wakes the
// controller when a slot it released is still free after that (wakeForFreeSlot). The workflow
// handler runs first: it records every live-tree
// child, leaving admission to record only a still-unrecorded root or orphan.
func (a *Admission) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	returned, err := a.releaseReturnedSlots(ctx, tx)
	if err != nil {
		return intake.Result{}, err
	}
	released, err := a.applyFact(ctx, tx, fact)
	if err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, a.wakeForFreeSlot(ctx, tx, append(returned, released...))
}

// applyFact is Apply's work once the workflow's returned slots are released, and reports the
// issues whose slots it released itself.
func (a *Admission) applyFact(ctx context.Context, tx pgx.Tx, fact intake.Fact) ([]string, error) {
	if position, ok := fact.(intake.DispatchConsumerPosition); ok {
		return a.release(ctx, tx, position)
	}
	if _, ok := fact.(intake.ControllerTick); ok {
		_, returned, err := a.promote(ctx, tx)
		if err != nil {
			return nil, err
		}
		// The tick wakes the controller whatever the slots: with every slot taken, it is the only
		// wake a tree waiting on a refused root claim gets, since only the controller's recheck of
		// that claim moves it, and it is the turn that posts the day's report.
		return returned, a.wakeController(ctx, tx, record.TickNotice, a.project, a.now())
	}
	if handover, ok := fact.(intake.Handover); ok {
		if err := a.consumeHandover(ctx, tx, handover); err != nil {
			return nil, err
		}
		_, returned, err := a.promote(ctx, tx)
		return returned, err
	}

	observation, ok := fact.(intake.DispatchIssue)
	if !ok {
		_, returned, err := a.promote(ctx, tx)
		return returned, err
	}

	stored, err := a.store.Issue(ctx, tx, observation.Key)
	if err != nil {
		return nil, fmt.Errorf("read admission issue %s: %w", observation.Key, err)
	}
	if observation, err = workflow.SettleRetainedRepository(observation, stored); err != nil {
		return nil, err
	}
	if stored == nil {
		handed := observation.HandedOver
		// A root in triage handed to Legion is the controller's to triage, so each observation of it
		// while it is unrecorded wakes the controller: its creation with the label, or the edit that
		// adds it, since the dashboard creates an issue without labels. The controller triages from
		// Dispatch and the daemon's state, never from the wake. A root without the label is not
		// Legion's and wakes nobody. This runs before any hold check: a hold defers only whether an
		// unrecorded todo is admitted, never whether an unrecorded triage root wakes the
		// controller. The record stays empty, as for any root not yet todo; Reconcile's boot
		// listing reads every status now, but a triage root is never a todo candidate it admits.
		if observation.Status == "triage" && observation.Parent == "" && handed {
			if err := a.enqueue(ctx, tx, observation.Key, record.ControllerNotice{Kind: record.TriageNotice}, a.now()); err != nil {
				return nil, err
			}
		}
		// An unrecorded todo issue without the label is someone else's work in a project Legion may
		// share with humans and other agents: Legion records nothing of it until it is handed over.
		// Neither exit records anything, but each still refreshes a held key's own pending summary:
		// an event moving the key out of todo, or taking its label off, is real information about
		// what Dispatch shows now, and leaving the held summary as Reconcile's stale boot listing
		// found it would have release admit on that stale information instead.
		//
		// Every exit below still promotes, though it records nothing: the blockers.Mirror handler,
		// which runs first, has already written this issue's row in this transaction, and a root
		// or a child may be waiting on exactly this issue. Its close is an observation of an
		// unrecorded issue out of todo, so without this look a waiting root whose last blocker is
		// an issue Legion keeps no record of would wait for the controller tick to be admitted.
		if observation.Status != "todo" {
			a.refreshHeldSummary(observation)
			_, returned, err := a.promote(ctx, tx)
			return returned, err
		}
		if !handed {
			a.log.Debug("admission: not handed to Legion", "issue", observation.Key, "label", dispatch.LegionLabel)
			a.refreshHeldSummary(observation)
			// Promotion comes before the slot is judged: a root this look admits takes a slot that
			// was free before it, and the wake must name a slot still free once this fact commits,
			// never one the same transaction has just filled.
			_, returned, err := a.promote(ctx, tx)
			if err != nil {
				return returned, err
			}
			// An issue in todo that nobody handed to Legion wakes the controller while a slot stands
			// free; the controller takes nothing for it, since only a person hands Legion work
			// (skill://legion-controller, "Free slots"). The notice waits todoWakeDelay, so the
			// events of one edit burst find it pending and fold into it.
			free, err := a.slotFree(ctx, tx)
			if err != nil || !free {
				return returned, err
			}
			return returned, a.wakeController(ctx, tx, record.TodoNotice, observation.Key, a.now().Add(todoWakeDelay))
		}
		a.mu.Lock()
		summary, held := a.pending[observation.Key]
		a.mu.Unlock()
		if held && observation.Seq <= summary.LastSeq {
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
			_, returned, err := a.promote(ctx, tx)
			return returned, err
		}
		if err := a.putNewRoot(ctx, tx, observation, true); err != nil {
			return nil, err
		}
	} else if err := a.applyObservation(ctx, tx, *stored, observation); err != nil {
		return nil, err
	}

	released, err := a.releaseInactiveSlots(ctx, tx)
	if err != nil {
		return nil, err
	}
	_, returned, err := a.promote(ctx, tx)
	if err != nil {
		return nil, err
	}
	return append(released, returned...), nil
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

// slotFree says whether an admission slot stands free: fewer slots in use than the cap, counting a
// slot a waiting root will take as taken. A root promotion skips because it waits on an unfinished
// Dispatch issue (blockers.WaitsOn) will take none, so it hides no free capacity from the project: the
// controller hears of the slot and may hand Legion work that can actually run. The controller is
// woken about it and takes nothing for it; only a person hands Legion work
// (skill://legion-controller, "Free slots").
func (a *Admission) slotFree(ctx context.Context, tx pgx.Tx) (bool, error) {
	if a.cap <= 0 {
		return false, nil
	}
	issues, err := a.store.Issues(ctx, tx)
	if err != nil {
		return false, fmt.Errorf("list admission issues: %w", err)
	}
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return false, fmt.Errorf("list admission slots: %w", err)
	}
	rows, err := blockers.Load(ctx, tx, a.store, a.project)
	if err != nil {
		return false, err
	}
	own := ownSlots(issues, slots)
	taking := 0
	for _, waiting := range record.Waiting(issues, own) {
		if len(blockers.WaitsOn(rows, waiting)) == 0 {
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

// blocked is every issue Legion is waiting to start that waits on an unfinished Dispatch issue,
// as the tick notice carries it: the roots promotion skips and the children the workflow holds,
// judged from this transaction's own reads by the function the state's `blocked` is built with
// (blockers.Blocked), so the controller is told exactly what `legion state` shows it.
func (a *Admission) blocked(ctx context.Context, tx pgx.Tx) ([]record.BlockedIssue, error) {
	issues, err := a.store.Issues(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("list admission issues: %w", err)
	}
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("list admission slots: %w", err)
	}
	rows, err := blockers.Load(ctx, tx, a.store, a.project)
	if err != nil {
		return nil, err
	}
	return blockers.Blocked(rows, issues, record.Waiting(issues, ownSlots(issues, slots))), nil
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
func (a *Admission) Reconcile(ctx context.Context, tx pgx.Tx, summaries []dispatch.IssueSummary, target int64, position intake.DispatchConsumerPosition) error {
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return fmt.Errorf("list admission slots: %w", err)
	}
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}

	caughtUp := position.Reached(target)
	deferred := make(map[string]dispatch.IssueSummary)
	for _, summary := range summaries {
		stored, err := a.store.Issue(ctx, tx, summary.Key)
		if err != nil {
			return fmt.Errorf("read reconciled issue %s: %w", summary.Key, err)
		}
		behind := stored == nil && summary.LastSeq > 0 || stored != nil && summary.LastSeq > stored.LastDispatchSeq
		if behind && !caughtUp {
			a.log.Debug("admission reconcile: the stream may still hold or redeliver an event for this issue; holding it for the consumer to catch up",
				"issue", summary.Key, "status", summary.Status, "dispatch", summary.LastSeq)
			deferred[summary.Key] = summary
			continue
		}
		if err := a.applySummary(ctx, tx, summary, slotted); err != nil {
			return err
		}
	}

	if len(deferred) > 0 {
		a.mu.Lock()
		for key, summary := range deferred {
			a.pending[key] = summary
		}
		if target > a.target {
			a.target = target
		}
		a.mu.Unlock()
		a.log.Info("admission reconcile: holding roots for the Dispatch consumer to reach a stream position",
			"count", len(deferred), "target", target, "ack_floor", position.AckFloorStream)
	}

	released, err := a.releaseInactiveSlots(ctx, tx)
	if err != nil {
		return err
	}
	_, returned, err := a.promote(ctx, tx)
	if err != nil {
		return err
	}
	return a.wakeForFreeSlot(ctx, tx, append(released, returned...))
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
// admitted only when the summary itself shows it todo and handed to Legion, and a recorded key a
// live event has already carried strictly past the summary's own sequence is left alone —
// reapplying the summary would overwrite fresher information with stale. Both Reconcile,
// immediately when the consumer is already caught up, and release, once the consumer catches up
// to a summary Reconcile deferred, call this the same way: a key nothing else ever gets around to
// explaining still lands on what the listing already knew.
func (a *Admission) applySummary(ctx context.Context, tx pgx.Tx, summary dispatch.IssueSummary, slotted map[string]struct{}) error {
	stored, err := a.store.Issue(ctx, tx, summary.Key)
	if err != nil {
		return fmt.Errorf("read reconciled issue %s: %w", summary.Key, err)
	}
	handed := summary.HandedOver
	if stored == nil {
		if summary.Status != "todo" {
			return nil
		}
		if !handed {
			a.log.Debug("admission: not handed to Legion", "issue", summary.Key, "label", dispatch.LegionLabel)
			return nil
		}
		return a.putNewRoot(ctx, tx, intake.ListedIssue(summary), false)
	}
	if summary.LastSeq > 0 && summary.LastSeq < stored.LastDispatchSeq {
		return nil
	}
	// The record keeps the repository it was recorded with, as for a live event (the engine's
	// NoteRepositoryKept); the note is said once per resolution, so a listing no newer than the
	// record says nothing again.
	if err := a.engine.NoteRepositoryKept(ctx, tx, *stored, summary.ResolvedRepo); err != nil {
		return err
	}
	seq := stored.LastDispatchSeq
	if summary.LastSeq > seq {
		seq = summary.LastSeq
	}
	status := stored.Status
	if summary.Status != stored.Status && summary.LastSeq > stored.LastDispatchSeq {
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
		if _, err := a.engine.Apply(ctx, tx, intake.ListedIssue(summary)); err != nil {
			return fmt.Errorf("apply engine transition for %s: %w", summary.Key, err)
		}
		refreshed, err := a.store.Issue(ctx, tx, summary.Key)
		if err != nil {
			return fmt.Errorf("re-read %s after its engine transition: %w", summary.Key, err)
		}
		// The engine consumed the snapshot when it recorded the snapshot's sequence: it kept its own
		// status over a status Dispatch already showed (keepStatus), or re-entered a child, and wrote
		// the observation itself. Recording the snapshot's status now would put a set-back's backlog
		// on a running tree, whose slot releaseInactiveSlots would then free. Otherwise the snapshot's
		// move is recorded, as the live event's would be.
		if refreshed.LastDispatchSeq >= summary.LastSeq {
			return nil
		}
		stored, status = refreshed, summary.Status
	}
	if summary.Status == "todo" && handed && readmittable(*stored) {
		return a.readmit(ctx, tx, *stored, summary.Title, deref(summary.Parent), summary.Rank, seq, false)
	}
	if summary.Status == "todo" && stored.Status == "in_progress" {
		if _, active := slotted[stored.Key]; active {
			return nil
		}
	}
	return a.recordObservation(ctx, tx, *stored, observed{
		Title: summary.Title, Parent: deref(summary.Parent), Rank: summary.Rank,
		Status: status, Shown: summary.Status, HandedOver: handed, Seq: seq,
	})
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
				return a.readmit(ctx, tx, stored, observation.Title, observation.Parent, observation.Rank, observation.Seq, stored.CarriesRun())
			}
		}
	}
	if observation.Status == "todo" && handed && readmittable(stored) {
		return a.readmit(ctx, tx, stored, observation.Title, observation.Parent, observation.Rank, observation.Seq, false)
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
		return a.readmit(ctx, tx, stored, observation.Title, observation.Parent, observation.Rank, observation.Seq, stored.CarriesRun())
	}
	return a.recordObservation(ctx, tx, stored, observed{
		Title: observation.Title, Parent: observation.Parent, Rank: observation.Rank,
		Status: observation.Status, Shown: observation.Status, HandedOver: handed, Seq: observation.Seq,
	})
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
	return a.readmit(ctx, tx, *stored, stored.Title, deref(stored.Parent), stored.Rank, stored.LastDispatchSeq, stored.CarriesRun())
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
type observed struct {
	Title, Parent, Rank, Status, Shown string
	HandedOver                         bool
	Seq                                int64
}

// recordObservation is where admission writes a Dispatch observation to an issue record: a live
// event's and the boot read's, so a rename, a re-rank, a re-parent, a label or a status change is
// written the same way whichever brought it. Each caller owns its own guards — the stream's
// sequence fence, the boot read's — and this writes what they let through. The engine, which runs
// first, writes the observation itself when it keeps its own status over the event's (its
// keepStatus: a session's write it sets back, or an edit that changed no status), and a live event
// it wrote then stops at the sequence fence here.
func (a *Admission) recordObservation(ctx context.Context, tx pgx.Tx, stored record.Issue, o observed) error {
	if stored.Title == o.Title && stored.Rank == o.Rank && stored.Status == o.Status && stored.DispatchStatus == o.Shown &&
		stored.HandedOver == o.HandedOver && sameParent(stored.Parent, record.ParentOf(o.Parent)) && stored.LastDispatchSeq == o.Seq {
		return nil
	}
	stored.Title = o.Title
	stored.Parent = record.ParentOf(o.Parent)
	stored.Rank = o.Rank
	stored.Status = o.Status
	stored.DispatchStatus = o.Shown
	stored.HandedOver = o.HandedOver
	stored.LastDispatchSeq = o.Seq
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
// for a slot. A root starts a fresh generation: its facts clear across its whole tree, as do those
// of an orphan readmitted without carry. An orphan readmitted with carry (one that started a phase)
// is the same work moving from a finished root to a root of its own, so its phase, phase rows, gate
// and pull request (with its observed head) carry into the new generation, and its promotion starts
// both its architect and the role of that carried phase. A merged pull request is never observed as
// merged again, and a review, acceptance or READY would otherwise rerun work already decided or
// landed.
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
func (a *Admission) readmit(ctx context.Context, tx pgx.Tx, stored record.Issue, title, parentKey, rank string, seq int64, carry bool) error {
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
	stored.Title = title
	stored.HandedOver = true
	stored.Parent = record.ParentOf(parentKey)
	stored.Tree = stored.Key
	if !orphan {
		stored.Phase, stored.Hold, stored.Awaiting, stored.ReadyPendingVersion = phase.Admitted, nil, nil, nil
		// Another tree's work arriving without its run: the run it had was that tree's.
		if !claim.IsTreeRoot(stored.Key, oldTree) {
			stored.TakenBack = false
		}
	}
	stored.Generation++
	stored.Status, stored.DispatchStatus = "todo", "todo"
	stored.Rank = rank
	stored.LingerUntil = nil
	stored.LastDispatchSeq = seq
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
			pr.Generation = int64(stored.Generation)
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
	// A root set back to todo is a new generation of the whole tree, so the old generation's pull
	// request, gate, handoffs and pending READY go for every issue of it, not only the root's.
	return a.store.ClearTreeGeneration(ctx, tx, stored.Tree)
}

// releaseReturnedSlots gives back the slots of roots that no longer run workers: a completed root,
// or one in awaiting_person, whose architect is suspended until the person answers.
func (a *Admission) releaseReturnedSlots(ctx context.Context, tx pgx.Tx) ([]string, error) {
	return a.releaseSlots(ctx, tx, "returned", releasesSlot)
}

func (a *Admission) releaseInactiveSlots(ctx context.Context, tx pgx.Tx) ([]string, error) {
	return a.releaseSlots(ctx, tx, "inactive", func(issue record.Issue) bool {
		return releasesSlot(issue) || record.OutOfWorkflow(issue.Status)
	})
}

func releasesSlot(issue record.Issue) bool {
	return issue.Phase == phase.Done || issue.AwaitingPerson()
}

// releaseSlots releases the slot of every issue of this project that release says is done with
// it, and reports those issues in slot order; which describes such an issue in an error. Each
// release is logged once the fact commits.
func (a *Admission) releaseSlots(ctx context.Context, tx pgx.Tx, which string, release func(record.Issue) bool) ([]string, error) {
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("list admission slots: %w", err)
	}
	own := []record.Issue{}
	for _, slot := range slots {
		issue, err := a.store.Issue(ctx, tx, slot.Issue)
		if err != nil {
			return nil, fmt.Errorf("read slotted issue %s: %w", slot.Issue, err)
		}
		if issue == nil {
			return nil, fmt.Errorf("slotted issue %s has no record", slot.Issue)
		}
		if issue.Project == a.project {
			own = append(own, *issue)
		}
	}
	inUse := len(own)
	var released []string
	for _, issue := range own {
		if !release(issue) {
			continue
		}
		if err := a.store.ReleaseSlot(ctx, tx, issue.Key); err != nil {
			return nil, fmt.Errorf("release %s issue %s: %w", which, issue.Key, err)
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

// promote assigns slots to waiting roots and orphans in rank order until the cap is reached or the
// waiting line empties, and reports how many candidates it admitted and the slots it released.
// Before it assigns any, every admitted root that now waits only on a person gives its slot back
// (workflow's ReconcileAwaiting, then releaseReturnedSlots), and every root in the workflow that
// neither holds a slot nor waits for one leaves it (releaseUnslotted), so each pass of admission —
// a live fact, the boot read, a release, a tick — repairs that state rather than keeping it. A
// candidate a.pending still names is held back: the Dispatch consumer has not yet reached the
// stream position Reconcile captured when it deferred that key. release, not promote, is what
// clears pending — it empties the whole set at once, so this need only check membership. An
// admitted candidate's architect start is queued behind a row creating its issue branch
// (record.IssueBranch), which the start waits for.
func (a *Admission) promote(ctx context.Context, tx pgx.Tx) (int, []string, error) {
	return a.promoteHolds(ctx, tx, true)
}

// promoteHolds is promote; respectHolds false is release's own call, once every held key's own
// summary has already been applied and its freed slots released: release always ignores every
// hold, never a subset, since it only ever runs once the consumer has caught up to every key
// Reconcile deferred. The in-memory pending set itself only clears after that whole call's
// transaction commits (see release), so without this, promote's own membership check would hold
// every one of them back in the very call that released them.
//
// It reads the project's mirror of what Dispatch says each issue waits on once (mirror), for two
// judgements: the candidates it skips here, and the held children of every live tree the engine
// starts from the same rows once the waiting line is done with (StartUnblockedChildren). Every
// path that promotes comes through here — a live fact, the controller tick, a hold's release, the
// boot's Reconcile — so each of them is also a look at every child a blocker holds.
func (a *Admission) promoteHolds(ctx context.Context, tx pgx.Tx, respectHolds bool) (int, []string, error) {
	issues, err := a.store.Issues(ctx, tx)
	if err != nil {
		return 0, nil, fmt.Errorf("list admission issues: %w", err)
	}
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return 0, nil, fmt.Errorf("list admission slots: %w", err)
	}
	heldKeys := a.catchingUpFor
	if respectHolds {
		heldKeys = func(key string) bool { return a.held(key) || a.catchingUpFor(key) }
	}
	entered, changed, err := a.engine.ReconcileAwaiting(ctx, tx, a.project, issues, slots, heldKeys)
	if err != nil {
		return 0, nil, fmt.Errorf("reconcile the roots waiting on a person: %w", err)
	}
	var returned []string
	if len(entered) > 0 {
		if returned, err = a.releaseReturnedSlots(ctx, tx); err != nil {
			return 0, nil, err
		}
	}
	// Reconcile wrote roots of the snapshot (a wait entered or answered, a resume told in place), so
	// what follows reads them as they now stand: an answer it recorded puts its root at the front of
	// this pass's waiting line.
	if changed {
		if issues, err = a.store.Issues(ctx, tx); err != nil {
			return 0, returned, fmt.Errorf("list admission issues: %w", err)
		}
		if slots, err = a.store.Slots(ctx, tx); err != nil {
			return 0, returned, fmt.Errorf("list admission slots: %w", err)
		}
	}
	released, err := a.releaseUnslotted(ctx, tx, issues, slots, respectHolds)
	if err != nil {
		return 0, returned, err
	}
	if a.cap <= 0 {
		return 0, returned, nil
	}
	if released {
		if issues, err = a.store.Issues(ctx, tx); err != nil {
			return 0, returned, fmt.Errorf("list admission issues: %w", err)
		}
	}
	rows, err := blockers.Load(ctx, tx, a.store, a.project)
	if err != nil {
		return 0, returned, err
	}
	own := ownSlots(issues, slots)
	waiting := record.Waiting(issues, own)
	admitted, resumed := 0, false
	for len(own) < a.cap && len(waiting) > 0 {
		candidate := waiting[0]
		waiting = waiting[1:]
		if respectHolds && a.held(candidate.Key) {
			continue
		}
		// An answered root the boot's catch-up still holds has not applied everything Dispatch
		// recorded while the daemon was down, so its resume task would miss it; it keeps its place
		// at the front of the line, and nothing behind it takes the slot, until its catch-up
		// releases it and decides it on the next pass.
		if a.catchingUpFor(candidate.Key) {
			break
		}
		answered := candidate.Answered()
		if answered {
			// The root's tree already runs: only a reason it may not resume in its own session
			// holds it here (its label gone, a foreign Dispatch claim), which releaseUnslotted
			// takes out of the workflow on this same pass. Its links are not re-read, as no
			// running tree's are.
			reason, err := workflow.Unresumable(ctx, a.store, tx, candidate)
			if err != nil {
				return admitted, returned, fmt.Errorf("check whether answered root %s can resume: %w", candidate.Key, err)
			}
			if reason != "" {
				a.log.Warn("admission skips an answered root it may not resume", "issue", candidate.Key, "reason", reason)
				continue
			}
		}
		// A root that waits on an unfinished Dispatch issue is skipped before any tree of its own
		// is opened, and keeps its place in the waiting line: the next pass admits it, once the
		// last issue it waits on is done. record.Waiting still carries it, deliberately — dropped
		// from the waiting line it would make record.TreeLive false for itself, and its children
		// would be recorded as orphans admitted as roots of their own. A blocked root stays the
		// live tree of its children; it simply takes no slot (slotFree counts it as taking none).
		if waits := blockers.WaitsOn(rows, candidate); len(waits) > 0 {
			a.log.Info("admission waits for what this root waits on", "issue", candidate.Key, "blockers", waits)
			continue
		}
		if candidate.Key == candidate.Tree && !answered {
			// Claims, tree lifecycles and the runtime key a tree by the normalized project token,
			// not the Dispatch project key the issue record carries. An answered root's tree
			// lifecycle is the one its first admission opened, and still runs.
			project, err := claim.ProjectToken(a.project)
			if err != nil {
				return admitted, returned, fmt.Errorf("open admission lifecycle of %s: %w", candidate.Key, err)
			}
			if _, err := a.store.OpenTreeLifecycle(ctx, tx, project, candidate.Tree, treelifecycle.AuthorityWorkflow); err != nil {
				if errors.Is(err, treelifecycle.ErrCleanupReserved) {
					a.log.Info("admission waits for tree cleanup", "issue", candidate.Key, "tree", candidate.Tree)
					continue
				}
				// The operator opened a tree under this root's key: this root waits until that tree's
				// cleanup confirms, and the roots behind it are admitted meanwhile.
				if errors.Is(err, treelifecycle.ErrAuthorityHeld) {
					a.log.Warn("admission waits for its tree's other authority", "issue", candidate.Key, "tree", candidate.Tree, "error", err)
					continue
				}
				return admitted, returned, fmt.Errorf("open admission lifecycle of %s: %w", candidate.Key, err)
			}
		}
		now := a.now()
		index := nextSlotIndex(slots)
		awaiting := candidate.Awaiting
		// An answered root keeps the status it ran with, so its resume writes none, unless a person
		// set it back to todo while it waited: then, like a fresh root, it is written in_progress.
		writeInProgress := !answered || candidate.Status == "todo"
		if answered {
			candidate.Phase, candidate.Awaiting = phase.Admitted, nil
		}
		if writeInProgress {
			candidate.Status = "in_progress"
		}
		if err := a.store.PutIssue(ctx, tx, candidate); err != nil {
			return admitted, returned, fmt.Errorf("record admitted issue %s: %w", candidate.Key, err)
		}
		slot := record.Slot{Issue: candidate.Key, Index: index, AdmittedAt: now}
		if err := a.store.PutSlot(ctx, tx, slot); err != nil {
			return admitted, returned, fmt.Errorf("put admission slot for %s: %w", candidate.Key, err)
		}
		if writeInProgress {
			if err := a.enqueue(ctx, tx, candidate.Key, record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}, now); err != nil {
				return admitted, returned, err
			}
		}
		if answered {
			// ResumeAwaitedRoot is the resume's handout (record.Store.StartHandout).
			if err := a.engine.ResumeAwaitedRoot(ctx, tx, candidate, *awaiting); err != nil {
				return admitted, returned, fmt.Errorf("resume %s after a person's answer: %w", candidate.Key, err)
			}
			resumed = true
		} else {
			// A fresh admission hands the architect its tree: what it hands back to a person
			// afterwards is what it may wait on (workflow's ReconcileAwaiting).
			if err := a.store.StartHandout(ctx, tx, candidate.Key); err != nil {
				return admitted, returned, fmt.Errorf("record admitted issue %s: %w", candidate.Key, err)
			}
			if err := a.enqueue(ctx, tx, candidate.Key, record.IssueBranch{Generation: candidate.Generation}, now); err != nil {
				return admitted, returned, err
			}
			if err := a.enqueue(ctx, tx, candidate.Key, record.SuperviseRequest{Op: "start", Tree: candidate.Tree, Role: claim.RoleArchitect, Generation: candidate.Generation}, now); err != nil {
				return admitted, returned, err
			}
			if err := a.startMidPhaseMembers(ctx, tx, candidate, append(issues, candidate), now); err != nil {
				return admitted, returned, err
			}
			if err := a.reenterStrandedChildren(ctx, tx, candidate, issues); err != nil {
				return admitted, returned, err
			}
		}
		slots, own = append(slots, slot), append(own, slot)
		admitted++
		a.logSlot(ctx, "taken", candidate.Key, len(own))
	}
	// The waiting line is roots; a child a blocker held is the engine's to start, from the issues
	// and the mirror rows this call already holds. It runs here rather than in promote because
	// release bypasses promote, and a hold's release is one of the looks every held child gets. A
	// resume above may have started children itself (its tree's gate open), so the issues are read
	// again first: on the snapshot such a child would still stand at admitted, and start twice.
	if resumed {
		if issues, err = a.store.Issues(ctx, tx); err != nil {
			return admitted, returned, fmt.Errorf("list admission issues: %w", err)
		}
	}
	if err := a.engine.StartUnblockedChildren(ctx, tx, issues, rows); err != nil {
		return admitted, returned, err
	}
	return admitted, returned, nil
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

// unslotted says whether issue is a root of this project left in the workflow (workflow's
// InWorkflow) with no way to run: not lingering, it holds no slot, and its status is one only a
// running tree has (in_progress, testing, needs_review, retro) rather than todo, where a root
// waits for a slot (record.Waiting). A root in awaiting_person is deliberately not one: it
// released its slot while its architect waits on a person and resumes through Waiting once
// answered, and releaseUnslotted removes it only when its label or Dispatch claim makes a resume
// unsafe. The workflow takes a root out on a status out of the workflow
// (its leave) but does nothing on a running tree's status where no tree runs, and a root's slot
// goes only with its phase done or its status out of the workflow, so only such a move made while
// it waited leaves a root here: a person's, or an outside session's, which is a person's where no
// tree runs (workflow's sessionStatusWrite). A root in todo without the label is not one: taking
// the label off is a pause the label ends.
func (a *Admission) unslotted(issue record.Issue, slotted map[string]struct{}) bool {
	if issue.Project != a.project || !claim.IsTreeRoot(issue.Key, issue.Tree) || issue.Lingers() || !workflow.InWorkflow(issue) || issue.AwaitingPerson() {
		return false
	}
	switch issue.Status {
	case "in_progress", "testing", "needs_review", "retro":
	default:
		return false
	}
	_, running := slotted[issue.Key]
	return !running
}

// releaseUnslotted takes every root no slot can ever reach out of the workflow, as a status
// leaving todo takes a waiting root out (workflow's LeaveUnslotted): nothing would ever give it a
// slot, nothing would ever start it, and its record would keep saying it is admitted. A
// conventional unslotted root has neither a slot nor a place in Waiting; an awaiting-person root
// has no slot by design, and leaves only when its hand-over label or its current Dispatch claim
// makes a resume unsafe (workflow's Unresumable). Legion starts nothing for either — it is
// someone else's work now, or a person's move to make — and the controller is told on the
// controller topic alone (record.UnslottedNotice, one per root, its reason naming why). A key
// Reconcile holds is left for release to apply first, when respectHolds is set. It reports
// whether it released any.
func (a *Admission) releaseUnslotted(ctx context.Context, tx pgx.Tx, issues []record.Issue, slots []record.Slot, respectHolds bool) (bool, error) {
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	released := false
	for _, issue := range issues {
		if respectHolds && a.held(issue.Key) {
			continue
		}
		why, awaiting, err := a.awaitingUnresumable(ctx, tx, issue, slotted)
		if err != nil {
			return released, fmt.Errorf("check whether awaiting root %s can resume: %w", issue.Key, err)
		}
		if !awaiting && !a.unslotted(issue, slotted) {
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
// Unresumable, the one rule ReconcileAwaiting also asks) and whether this pass must remove it.
func (a *Admission) awaitingUnresumable(ctx context.Context, tx pgx.Tx, issue record.Issue, slotted map[string]struct{}) (string, bool, error) {
	if issue.Project != a.project || !claim.IsTreeRoot(issue.Key, issue.Tree) || issue.Lingers() || !workflow.InWorkflow(issue) || !issue.AwaitingPerson() {
		return "", false, nil
	}
	if _, running := slotted[issue.Key]; running {
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
	if !held || observation.Seq <= summary.LastSeq {
		return
	}
	a.pending[observation.Key] = dispatch.IssueSummary{
		Key: observation.Key, Title: observation.Title, Status: observation.Status,
		Parent: record.ParentOf(observation.Parent), Rank: observation.Rank,
		HandedOver: observation.HandedOver, LastSeq: observation.Seq, ResolvedRepo: observation.ResolvedRepo,
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
	var releasing map[string]dispatch.IssueSummary
	if caughtUp {
		releasing = maps.Clone(a.pending)
	}
	a.mu.Unlock()

	var released []string
	if caughtUp {
		slots, err := a.store.Slots(ctx, tx)
		if err != nil {
			return nil, fmt.Errorf("list admission slots: %w", err)
		}
		slotted := make(map[string]struct{}, len(slots))
		for _, slot := range slots {
			slotted[slot.Issue] = struct{}{}
		}
		for _, summary := range releasing {
			if err := a.applySummary(ctx, tx, summary, slotted); err != nil {
				return nil, err
			}
		}
		if released, err = a.releaseInactiveSlots(ctx, tx); err != nil {
			return nil, err
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

// startMidPhaseMembers starts each previous run that promotion brings back, the re-admitted root
// included. A hand-over turns a child into that root while preserving its phase, so its phase role
// needs the same resume start ordinary mid-phase children receive (workflow.ResumeTask, which tells
// a carried merger its carried approved head). A root promoted held, which only a hand-over leaves
// (every other re-admission starts its root at admitted), starts no worker, and the hold's notice
// went to the architect of the tree it was handed over from, so its own architect is told of the
// hold (workflow.HeldNotice), whether or not it carried a gate. Promotion already queued the root's
// branch; each other member queues its own before starting. Every member resumes in
// workflow.ResumePhase, recorded before its start: a child of a re-admitted tree, and a root a
// hand-over created, left awaiting its merge resume in merging, since the old tree's linger disarmed
// the pull request (beginLinger) and only a new READY, whose merge the daemon submits, arms it again.
//
// What an issue waits on is deliberately not read here: every member this starts is mid-phase,
// work already under way, and the only member it leaves at phase.Admitted is left alone anyway,
// since RoleFor(phase.Admitted) is empty.
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
		// Accepting's role is the tree's architect, which promotion already started: nothing to resume.
		role := workflow.RoleFor(member.Phase)
		if role == "" || role == claim.RoleArchitect {
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
		if member.Key != root.Key {
			if err := a.enqueue(ctx, tx, member.Key, record.IssueBranch{Generation: member.Generation}, now); err != nil {
				return err
			}
		}
		task, err := workflow.ResumeTask(ctx, a.store, tx, member, role)
		if err != nil {
			return err
		}
		payload := record.SuperviseRequest{Op: "start", Tree: member.Tree, Role: role, Generation: member.Generation, Phase: member.Phase,
			Task: task, ResumeTask: true}
		if err := a.enqueue(ctx, tx, member.Key, payload, now); err != nil {
			return err
		}
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
		fact := intake.DispatchIssue{
			Key: child.Key, Seq: child.LastDispatchSeq, Status: child.Status, Title: child.Title,
			Parent: deref(child.Parent), Rank: child.Rank, HandedOver: child.HandedOver, ResolvedRepo: child.Repo,
		}
		if err := a.engine.ReenterChild(ctx, tx, child, fact); err != nil {
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
