// Package admit owns root admission slots and the Dispatch-rank waiting line.
package admit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
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

	// mu guards pending and target, the only Admission state a caller outside its own
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
// Legion (a controller notice, not a record), releases slots that the workflow completed, and
// promotes waiting roots while capacity remains. The workflow handler runs first: it records every
// live-tree child, leaving admission to record only a still-unrecorded root or orphan.
func (a *Admission) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if err := a.releaseDoneSlots(ctx, tx); err != nil {
		return intake.Result{}, err
	}

	if position, ok := fact.(intake.DispatchConsumerPosition); ok {
		return a.release(ctx, tx, position)
	}

	observation, ok := fact.(intake.DispatchIssue)
	if !ok {
		if _, err := a.promote(ctx, tx); err != nil {
			return intake.Result{}, err
		}
		return intake.Result{}, nil
	}

	stored, err := a.store.Issue(ctx, tx, observation.Key)
	if err != nil {
		return intake.Result{}, fmt.Errorf("read admission issue %s: %w", observation.Key, err)
	}
	if stored == nil {
		a.mu.Lock()
		summary, held := a.pending[observation.Key]
		a.mu.Unlock()
		if held && observation.Seq <= summary.LastSeq {
			// Reconcile already holds this key on the listing's own summary because it found it
			// behind the stream: an unrecorded key has no record to fence a live event against, so
			// a replay at or behind that summary's own sequence — a nak's redelivery, the outbox's
			// own publish backoff — decides nothing. release applies that summary once the
			// consumer catches up. A newer event, past the sequence the summary was taken at, is
			// not a stale replay: it is recorded normally below, and promote's own pending
			// membership check — not this one — is what still holds the candidate back from a
			// slot until release.
			return intake.Result{}, nil
		}
		handed := observation.HandedOver
		// A root in triage handed to Legion is the controller's to triage, so each observation of it
		// while it is unrecorded wakes the controller: its creation with the label, or the edit that
		// adds it, since the dashboard creates an issue without labels. The controller triages from
		// Dispatch and the daemon's state, never from the wake. A root without the label is not
		// Legion's and wakes nobody. The record stays empty, as for any root not yet todo; the boot
		// listing (Reconcile) never sees it, reading only the workflow's statuses, todo to retro.
		if observation.Status == "triage" && observation.Parent == "" && handed {
			if err := a.enqueue(ctx, tx, observation.Key, record.ControllerNotice{Kind: "triage"}, a.now()); err != nil {
				return intake.Result{}, err
			}
		}
		// An unrecorded todo issue without the label is someone else's work in a project Legion may
		// share with humans and other agents: Legion records nothing of it until it is handed over.
		if observation.Status != "todo" {
			return intake.Result{}, nil
		}
		if !handed {
			a.log.Debug("admission: not handed to Legion", "issue", observation.Key, "label", dispatch.LegionLabel)
			return intake.Result{}, nil
		}
		if err := a.putNewRoot(ctx, tx, observation, true); err != nil {
			return intake.Result{}, err
		}
	} else if err := a.applyObservation(ctx, tx, *stored, observation); err != nil {
		return intake.Result{}, err
	}

	if err := a.releaseInactiveSlots(ctx, tx); err != nil {
		return intake.Result{}, err
	}
	if _, err := a.promote(ctx, tx); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, nil
}

// Reconcile applies the bounded Dispatch boot read to existing records, then fills newly available
// capacity using the same promotion effects Apply emits. It performs no Dispatch I/O itself.
//
// The read is a snapshot with no actor on it: in it, an agent's own status write during a restart
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

	if err := a.releaseInactiveSlots(ctx, tx); err != nil {
		return err
	}
	_, err = a.promote(ctx, tx)
	return err
}

func (a *Admission) putNewRoot(ctx context.Context, tx pgx.Tx, observation intake.DispatchIssue, logOrphan bool) error {
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
		return a.putNewRoot(ctx, tx, intake.DispatchIssue{
			Key: summary.Key, Status: summary.Status, Title: summary.Title, Parent: deref(summary.Parent), Rank: summary.Rank, HandedOver: summary.HandedOver,
		}, false)
	}
	if summary.LastSeq > 0 && summary.LastSeq < stored.LastDispatchSeq {
		return nil
	}
	seq := stored.LastDispatchSeq
	if summary.LastSeq > seq {
		seq = summary.LastSeq
	}
	if summary.Status == "todo" && handed && readmittable(*stored) {
		return a.readmit(ctx, tx, *stored, summary.Title, deref(summary.Parent), summary.Rank, seq)
	}
	if summary.Status == "todo" && stored.Status == "in_progress" {
		if _, active := slotted[stored.Key]; active {
			return nil
		}
	}
	return a.recordObservation(ctx, tx, *stored, observed{
		Title: summary.Title, Parent: deref(summary.Parent), Rank: summary.Rank,
		Status: summary.Status, HandedOver: handed, Seq: seq,
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
	if observation.Status == "todo" && handed && readmittable(stored) {
		return a.readmit(ctx, tx, stored, observation.Title, observation.Parent, observation.Rank, observation.Seq)
	}
	orphan, err := a.orphan(ctx, tx, stored, observation.Status, handed)
	if err != nil {
		return err
	}
	if orphan {
		a.log.Info("admission orphan", "issue", stored.Key, "parent", observation.Parent)
		return a.readmit(ctx, tx, stored, observation.Title, observation.Parent, observation.Rank, observation.Seq)
	}
	return a.recordObservation(ctx, tx, stored, observed{
		Title: observation.Title, Parent: observation.Parent, Rank: observation.Rank,
		Status: observation.Status, HandedOver: handed, Seq: observation.Seq,
	})
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
// the boot read.
type observed struct {
	Title, Parent, Rank, Status string
	HandedOver                  bool
	Seq                         int64
}

// recordObservation is the one place a Dispatch observation reaches an issue record: a live
// event's and the boot read's, so a rename, a re-rank, a re-parent, a label or a status change is
// written the same way whichever brought it. Each caller owns its own guards — the stream's
// sequence fence, the boot read's — and this writes what they let through.
func (a *Admission) recordObservation(ctx context.Context, tx pgx.Tx, stored record.Issue, o observed) error {
	if stored.Title == o.Title && stored.Rank == o.Rank && stored.Status == o.Status && stored.HandedOver == o.HandedOver &&
		sameParent(stored.Parent, record.ParentOf(o.Parent)) && stored.LastDispatchSeq == o.Seq {
		return nil
	}
	stored.Title = o.Title
	stored.Parent = record.ParentOf(o.Parent)
	stored.Rank = o.Rank
	stored.Status = o.Status
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

// readmit records a lingering or closed root's todo, or an orphan's, as a new generation waiting for
// a slot. Each caller re-admits an issue handed to Legion. The new generation owns its facts: the old
// one's pull request, design gate, handoffs, review rounds, and pending READY are cleared, so its
// architect registers the gate again and its implementer waits for its own pull request.
func (a *Admission) readmit(ctx context.Context, tx pgx.Tx, stored record.Issue, title, parentKey, rank string, seq int64) error {
	if stored.Generation == ^uint64(0) {
		return fmt.Errorf("re-admit %s: generation overflows", stored.Key)
	}
	stored.Project = a.project
	stored.Title = title
	stored.HandedOver = true
	stored.Parent = record.ParentOf(parentKey)
	stored.Tree = stored.Key
	stored.Phase = phase.Admitted
	stored.Generation++
	stored.Status = "todo"
	stored.Rank = rank
	stored.LingerUntil = nil
	stored.Hold = nil
	stored.LastDispatchSeq = seq
	if err := a.store.PutIssue(ctx, tx, stored); err != nil {
		return fmt.Errorf("record re-admission %s: %w", stored.Key, err)
	}
	// A root set back to todo is a new generation of the whole tree, so the old generation's pull
	// request, gate, handoffs and pending READY go for every issue of it, not only the root's.
	return a.store.ClearTreeGeneration(ctx, tx, stored.Tree)
}

func (a *Admission) releaseDoneSlots(ctx context.Context, tx pgx.Tx) error {
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return fmt.Errorf("list admission slots: %w", err)
	}
	for _, slot := range slots {
		issue, err := a.store.Issue(ctx, tx, slot.Issue)
		if err != nil {
			return fmt.Errorf("read slotted issue %s: %w", slot.Issue, err)
		}
		if issue == nil {
			return fmt.Errorf("slotted issue %s has no record", slot.Issue)
		}
		if issue.Project != a.project {
			continue
		}
		if issue.Phase == phase.Done {
			if err := a.store.ReleaseSlot(ctx, tx, issue.Key); err != nil {
				return fmt.Errorf("release completed issue %s: %w", issue.Key, err)
			}
		}
	}
	return nil
}

func (a *Admission) releaseInactiveSlots(ctx context.Context, tx pgx.Tx) error {
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return fmt.Errorf("list admission slots: %w", err)
	}
	for _, slot := range slots {
		issue, err := a.store.Issue(ctx, tx, slot.Issue)
		if err != nil {
			return fmt.Errorf("read slotted issue %s: %w", slot.Issue, err)
		}
		if issue == nil {
			return fmt.Errorf("slotted issue %s has no record", slot.Issue)
		}
		if issue.Project != a.project {
			continue
		}
		if issue.Phase == phase.Done || record.OutOfWorkflow(issue.Status) {
			if err := a.store.ReleaseSlot(ctx, tx, issue.Key); err != nil {
				return fmt.Errorf("release inactive issue %s: %w", issue.Key, err)
			}
		}
	}
	return nil
}

// promote assigns slots to waiting roots and orphans in rank order until the cap is reached or the
// waiting line empties, and reports how many candidates it admitted. A candidate a.pending still
// names is held back: the Dispatch consumer has not yet reached the stream position Reconcile
// captured when it deferred that key. release, not promote, is what clears pending — it empties
// the whole set at once, so this need only check membership.
func (a *Admission) promote(ctx context.Context, tx pgx.Tx) (int, error) {
	return a.promoteExcept(ctx, tx, nil)
}

// promoteExcept is promote, treating every key in releasing as already unheld: release calls this
// with the keys its own caughtUp position just resolved, which still name entries in a.pending —
// clearing them only happens after this whole call's transaction commits (see release) — so
// without this exclusion promote's own membership check would hold them back in the very call
// that released them.
func (a *Admission) promoteExcept(ctx context.Context, tx pgx.Tx, releasing map[string]dispatch.IssueSummary) (int, error) {
	if a.cap <= 0 {
		return 0, nil
	}
	issues, err := a.store.Issues(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("list admission issues: %w", err)
	}
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("list admission slots: %w", err)
	}
	own := ownSlots(issues, slots)
	waiting := record.Waiting(issues, own)
	admitted := 0
	for len(own) < a.cap && len(waiting) > 0 {
		candidate := waiting[0]
		waiting = waiting[1:]
		if _, releasing := releasing[candidate.Key]; !releasing {
			a.mu.Lock()
			_, held := a.pending[candidate.Key]
			a.mu.Unlock()
			if held {
				continue
			}
		}
		now := a.now()
		index := nextSlotIndex(slots)
		candidate.Status = "in_progress"
		if err := a.store.PutIssue(ctx, tx, candidate); err != nil {
			return admitted, fmt.Errorf("record admitted issue %s: %w", candidate.Key, err)
		}
		slot := record.Slot{Issue: candidate.Key, Index: index, AdmittedAt: now}
		if err := a.store.PutSlot(ctx, tx, slot); err != nil {
			return admitted, fmt.Errorf("put admission slot for %s: %w", candidate.Key, err)
		}
		if err := a.enqueue(ctx, tx, candidate.Key, record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}, now); err != nil {
			return admitted, err
		}
		if err := a.enqueue(ctx, tx, candidate.Key, record.SuperviseRequest{Op: "start", Tree: candidate.Tree, Role: claim.RoleArchitect, Generation: candidate.Generation}, now); err != nil {
			return admitted, err
		}
		if err := a.startMidPhaseChildren(ctx, tx, candidate, issues, now); err != nil {
			return admitted, err
		}
		if err := a.reenterStrandedChildren(ctx, tx, candidate, issues); err != nil {
			return admitted, err
		}
		slots, own = append(slots, slot), append(own, slot)
		admitted++
	}
	return admitted, nil
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

// release applies the Dispatch consumer's current position: once it has Reached target, every key
// Reconcile deferred is re-applied through applySummary — the listing's own snapshot for it, in
// case the stream never delivers the event that would otherwise have caught it up — before
// promote decides anything, so a held key is never admitted, or left admitted, on stale
// information. The in-memory pending set only clears once this call's own transaction has
// actually committed (intake.Result.AfterCommit): clearing it any earlier would say a hold is
// resolved while a failed commit leaves nothing of this release durable, so the next
// promote-triggering call would trust an unreleased hold's authority.
func (a *Admission) release(ctx context.Context, tx pgx.Tx, position intake.DispatchConsumerPosition) (intake.Result, error) {
	a.mu.Lock()
	caughtUp := len(a.pending) > 0 && position.Reached(a.target)
	var releasing map[string]dispatch.IssueSummary
	if caughtUp {
		releasing = a.pending
	}
	a.mu.Unlock()

	if caughtUp {
		slots, err := a.store.Slots(ctx, tx)
		if err != nil {
			return intake.Result{}, fmt.Errorf("list admission slots: %w", err)
		}
		slotted := make(map[string]struct{}, len(slots))
		for _, slot := range slots {
			slotted[slot.Issue] = struct{}{}
		}
		for _, summary := range releasing {
			if err := a.applySummary(ctx, tx, summary, slotted); err != nil {
				return intake.Result{}, err
			}
		}
		if err := a.releaseInactiveSlots(ctx, tx); err != nil {
			return intake.Result{}, err
		}
	}
	admitted, err := a.promoteExcept(ctx, tx, releasing)
	if err != nil {
		return intake.Result{}, err
	}
	if !caughtUp {
		return intake.Result{}, nil
	}
	a.log.Info("admission: the Dispatch consumer has caught up; releasing held roots",
		"count", len(releasing), "ack_floor", position.AckFloorStream, "idle", position.Idle, "admitted", admitted)
	return intake.Result{AfterCommit: []func(){func() {
		a.mu.Lock()
		for key := range releasing {
			delete(a.pending, key)
		}
		a.mu.Unlock()
	}}}, nil
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

// startMidPhaseChildren starts the worker of every child of the admitted tree that a previous run
// left mid-phase. A tree that closed retired every member's claim, and a re-admitted root only
// brings back its own architect: a child left in testing has no worker, only its worker's handoff
// moves it, and the architect cannot release a child already in the workflow. Each start carries
// the child's own generation and phase, which is what the outbox fences it against, and the task
// that says to carry the phase on: a start with no task leaves the resumed agent holding its old
// transcript with nothing asked of it, and only its own handoff moves the phase. The task is marked
// as the phase's resume task, so the executor does not deliver it to a claim that still holds a
// task for the same generation and phase when the start runs: that claim is given the one it holds
// once it is ready. A child whose worker is already started for this run (a fact moved it while
// the root waited for its slot, and that start is queued or has run) is not started a second time.
// A child whose claim a stop from the tree's close will still suspend is started, so this start
// ends last or supersedes the stop (workflow.StartFor, over the role's queued rows and this
// daemon's own claim).
func (a *Admission) startMidPhaseChildren(ctx context.Context, tx pgx.Tx, root record.Issue, issues []record.Issue, now time.Time) error {
	project, err := claim.ProjectToken(a.project)
	if err != nil {
		return err
	}
	for _, child := range issues {
		if child.Key == root.Key || child.Tree != root.Tree {
			continue
		}
		role := workflow.RoleFor(child.Phase)
		if role == "" || record.OutOfWorkflow(child.Status) {
			continue
		}
		token, err := claim.NewToken(project, child.Key, role)
		if err != nil {
			return err
		}
		run, err := a.store.RoleRun(ctx, tx, token, child.Key, role, child.Generation)
		if err != nil {
			return err
		}
		if !workflow.StartFor(run, root, child.Generation, child.Phase) {
			continue
		}
		payload := record.SuperviseRequest{Op: "start", Tree: child.Tree, Role: role, Generation: child.Generation, Phase: child.Phase,
			Task: workflow.ResumePhaseTask(child), ResumeTask: true}
		if err := a.enqueue(ctx, tx, child.Key, payload, now); err != nil {
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
			Parent: deref(child.Parent), Rank: child.Rank, HandedOver: child.HandedOver,
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
