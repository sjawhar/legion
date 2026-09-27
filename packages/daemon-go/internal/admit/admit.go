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
	cap     int
	project string
	log     *slog.Logger
	now     func() time.Time

	// mu guards pending and target, the only Admission state a caller outside its own
	// transactions touches: intake calls Held for every Dispatch delivery, outside any
	// transaction and the database's own advisory fact lock, concurrently with whatever release
	// call currently holds both while it clears them.
	mu sync.Mutex
	// pending holds every key the last Reconcile found Dispatch's own log ahead of the record for,
	// still waiting for the Dispatch consumer to reach target. It empties all at once, not key by
	// key: target is one position in one stream, not a per-issue property, so nothing
	// distinguishes one held key's own catching-up from another's.
	pending map[string]struct{}
	// target is the notification stream's own last sequence Reconcile captured when it found the
	// first record behind: every key in pending releases once the Dispatch consumer's ack floor
	// reaches it, or once the consumer has nothing left pending or unacknowledged to reach it
	// with. Zero means nothing is held.
	target int64
}

var _ intake.Handler = (*Admission)(nil)
var _ intake.DispatchObserver = (*Admission)(nil)

// New creates an admission handler for one Dispatch project.
func New(store record.Store, cap int, project string, log *slog.Logger) *Admission {
	if log == nil {
		log = slog.Default()
	}
	return &Admission{store: store, cap: cap, project: project, log: log, now: time.Now, pending: make(map[string]struct{})}
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
		if err := a.release(ctx, tx, position.AckFloorStream, position.Idle); err != nil {
			return intake.Result{}, err
		}
		return intake.Result{}, nil
	}

	observation, ok := fact.(intake.DispatchIssue)
	if !ok {
		if err := a.promote(ctx, tx); err != nil {
			return intake.Result{}, err
		}
		return intake.Result{}, nil
	}

	stored, err := a.store.Issue(ctx, tx, observation.Key)
	if err != nil {
		return intake.Result{}, fmt.Errorf("read admission issue %s: %w", observation.Key, err)
	}
	if stored == nil {
		handed := record.CarriesLegionLabel(observation.Labels)
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
			a.log.Debug("admission: not handed to Legion", "issue", observation.Key, "label", record.LegionLabel)
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
	if err := a.promote(ctx, tx); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, nil
}

// Reconcile applies the bounded Dispatch boot read to existing records, then fills newly available
// capacity using the same promotion effects Apply emits. It performs no Dispatch I/O itself.
//
// The read is a snapshot with no actor on it: in it, an agent's own status write during a restart
// looks exactly like a human's move. Dispatch says how far each issue's event log has run, so a
// record behind that sequence is left alone — the stream still holds those events, and delivers
// them with the actor that made each one — unless the Dispatch consumer has already caught up to
// target when this call measured it (its ack floor reaching target, or idle: nothing left pending
// or unacknowledged to reach it with). Then nothing more is coming for that record ever, so this
// applies the listing's own snapshot to it directly instead of deferring a key nothing will later
// release. target, ackFloorStream and idle are the caller's own single measurement of the stream
// and the Dispatch consumer, taken once for the whole call, not per issue.
func (a *Admission) Reconcile(ctx context.Context, tx pgx.Tx, summaries []dispatch.IssueSummary, target, ackFloorStream int64, idle bool) error {
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return fmt.Errorf("list admission slots: %w", err)
	}
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}

	caughtUp := ackFloorStream >= target || idle
	var deferred []string
	for _, summary := range summaries {
		stored, err := a.store.Issue(ctx, tx, summary.Key)
		if err != nil {
			return fmt.Errorf("read reconciled issue %s: %w", summary.Key, err)
		}
		handed := record.CarriesLegionLabel(summary.Labels)
		if stored == nil {
			if summary.Status != "todo" {
				continue
			}
			if !handed {
				a.log.Debug("admission: not handed to Legion", "issue", summary.Key, "label", record.LegionLabel)
				continue
			}
			if err := a.putNewRoot(ctx, tx, intake.DispatchIssue{
				Key: summary.Key, Status: summary.Status, Title: summary.Title, Parent: deref(summary.Parent), Rank: summary.Rank, Labels: summary.Labels,
			}, false); err != nil {
				return err
			}
			continue
		}
		seq := stored.LastDispatchSeq
		if summary.LastSeq > stored.LastDispatchSeq {
			if !caughtUp {
				a.log.Info("admission reconcile: the stream holds newer events for this issue; leaving it to them",
					"issue", stored.Key, "applied", stored.LastDispatchSeq, "dispatch", summary.LastSeq)
				deferred = append(deferred, stored.Key)
				continue
			}
			a.log.Info("admission reconcile: the Dispatch consumer has nothing more to deliver for this issue; applying the boot listing's own snapshot",
				"issue", stored.Key, "applied", stored.LastDispatchSeq, "dispatch", summary.LastSeq)
			seq = summary.LastSeq
		}

		if summary.Status == "todo" && handed && readmittable(*stored) {
			if err := a.readmit(ctx, tx, *stored, summary.Title, deref(summary.Parent), summary.Rank, seq); err != nil {
				return err
			}
			continue
		}
		if summary.Status == "todo" && stored.Status == "in_progress" {
			if _, active := slotted[stored.Key]; active {
				continue
			}
		}
		if err := a.recordObservation(ctx, tx, *stored, observed{
			Title: summary.Title, Parent: deref(summary.Parent), Rank: summary.Rank,
			Status: summary.Status, HandedOver: handed, Seq: seq,
		}); err != nil {
			return err
		}
	}

	if len(deferred) > 0 {
		a.mu.Lock()
		for _, key := range deferred {
			a.pending[key] = struct{}{}
		}
		if target > a.target {
			a.target = target
		}
		a.mu.Unlock()
		a.log.Info("admission reconcile: holding roots for the Dispatch consumer to reach a stream position",
			"count", len(deferred), "target", target)
	}

	if err := a.releaseInactiveSlots(ctx, tx); err != nil {
		return err
	}
	return a.promote(ctx, tx)
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
		HandedOver:      record.CarriesLegionLabel(observation.Labels),
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

// applyObservation records a newer Dispatch observation of a recorded issue: the one place a live
// event's title, rank, parent, label, and status reach the record, so a re-rank, a rename, or the
// label taken off or put back at the same status moves the waiting line at once. A todo on a
// lingering or closed root handed to Legion is a re-admission.
func (a *Admission) applyObservation(ctx context.Context, tx pgx.Tx, stored record.Issue, observation intake.DispatchIssue) error {
	if observation.Seq != 0 && observation.Seq <= stored.LastDispatchSeq {
		return nil
	}
	handed := record.CarriesLegionLabel(observation.Labels)
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
// waiting line empties. A candidate a.pending still names is held back: the Dispatch consumer has
// not yet reached the stream position Reconcile captured when it deferred that key. release, not
// promote, is what clears pending — it empties the whole set at once, so this need only check
// membership.
func (a *Admission) promote(ctx context.Context, tx pgx.Tx) error {
	if a.cap <= 0 {
		return nil
	}
	issues, err := a.store.Issues(ctx, tx)
	if err != nil {
		return fmt.Errorf("list admission issues: %w", err)
	}
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return fmt.Errorf("list admission slots: %w", err)
	}
	own := ownSlots(issues, slots)
	waiting := record.Waiting(issues, own)
	for len(own) < a.cap && len(waiting) > 0 {
		candidate := waiting[0]
		waiting = waiting[1:]
		a.mu.Lock()
		_, held := a.pending[candidate.Key]
		a.mu.Unlock()
		if held {
			continue
		}
		now := a.now()
		index := nextSlotIndex(slots)
		candidate.Status = "in_progress"
		if err := a.store.PutIssue(ctx, tx, candidate); err != nil {
			return fmt.Errorf("record admitted issue %s: %w", candidate.Key, err)
		}
		slot := record.Slot{Issue: candidate.Key, Index: index, AdmittedAt: now}
		if err := a.store.PutSlot(ctx, tx, slot); err != nil {
			return fmt.Errorf("put admission slot for %s: %w", candidate.Key, err)
		}
		if err := a.enqueue(ctx, tx, candidate.Key, record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}, now); err != nil {
			return err
		}
		if err := a.enqueue(ctx, tx, candidate.Key, record.SuperviseRequest{Op: "start", Tree: candidate.Tree, Role: claim.RoleArchitect, Generation: candidate.Generation}, now); err != nil {
			return err
		}
		if err := a.startMidPhaseChildren(ctx, tx, candidate, issues, now); err != nil {
			return err
		}
		if err := a.reenterStrandedChildren(ctx, tx, candidate, issues, now); err != nil {
			return err
		}
		slots, own = append(slots, slot), append(own, slot)
	}
	return nil
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

// release applies the Dispatch consumer's current stream position: once its ack floor reaches
// target, or it has nothing left pending or unacknowledged to reach it with (idle covers a target
// set past the consumer's own filter, on a stream that also carries subjects it never matches),
// every key Reconcile deferred releases at once, and this call's own promote is what admits them -
// a release always promotes in the same transaction it clears pending in, so nothing waits on a
// later, unrelated fact to notice.
func (a *Admission) release(ctx context.Context, tx pgx.Tx, ackFloorStream int64, idle bool) error {
	a.mu.Lock()
	count := len(a.pending)
	caughtUp := count > 0 && (ackFloorStream >= a.target || idle)
	if caughtUp {
		a.pending = make(map[string]struct{})
	}
	a.mu.Unlock()
	if caughtUp {
		a.log.Info("admission: the Dispatch consumer has caught up; releasing held roots",
			"count", count, "ack_floor", ackFloorStream, "idle", idle)
	}
	return a.promote(ctx, tx)
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
// else reaches them afterward — reenterChild only re-enters a live tree's reopened child, and a
// later todo observation of one already todo changes nothing.
func (a *Admission) reenterStrandedChildren(ctx context.Context, tx pgx.Tx, candidate record.Issue, issues []record.Issue, now time.Time) error {
	for _, child := range issues {
		if child.Key == candidate.Key || child.Tree != candidate.Tree {
			continue
		}
		if child.Status != "todo" || child.Phase != phase.Done {
			continue
		}
		if err := a.store.ClearGeneration(ctx, tx, child.Key); err != nil {
			return err
		}
		child.Phase = phase.Admitted
		child.Generation++
		if err := a.store.PutIssue(ctx, tx, child); err != nil {
			return fmt.Errorf("re-enter stranded child %s: %w", child.Key, err)
		}
		if err := a.enqueue(ctx, tx, child.Key, workflow.ChildReenteredNotice(child.Key, candidate.Key), now); err != nil {
			return err
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
