// Package admit owns root admission slots and the Dispatch-rank waiting line.
package admit

import (
	"context"
	"fmt"
	"log/slog"
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
}

var _ intake.Handler = (*Admission)(nil)

// New creates an admission handler for one Dispatch project.
func New(store record.Store, cap int, project string, log *slog.Logger) *Admission {
	if log == nil {
		log = slog.Default()
	}
	return &Admission{store: store, cap: cap, project: project, log: log, now: time.Now}
}

// Apply records root and orphan todo observations and every newer observation of a recorded issue,
// releases slots that the workflow completed, and promotes waiting roots while capacity remains.
// The workflow handler runs first: it records every live-tree child, leaving admission to record
// only a still-unrecorded root or orphan.
func (a *Admission) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if err := a.releaseDoneSlots(ctx, tx); err != nil {
		return intake.Result{}, err
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
		if observation.Status != "todo" {
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
// them with the actor that made each one. A record level with Dispatch has nothing coming, and is
// reconciled here.
func (a *Admission) Reconcile(ctx context.Context, tx pgx.Tx, summaries []dispatch.IssueSummary) error {
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return fmt.Errorf("list admission slots: %w", err)
	}
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}

	for _, summary := range summaries {
		stored, err := a.store.Issue(ctx, tx, summary.Key)
		if err != nil {
			return fmt.Errorf("read reconciled issue %s: %w", summary.Key, err)
		}
		if stored == nil {
			if summary.Status != "todo" {
				continue
			}
			if err := a.putNewRoot(ctx, tx, intake.DispatchIssue{
				Key: summary.Key, Status: summary.Status, Title: summary.Title, Parent: deref(summary.Parent), Rank: summary.Rank,
			}, false); err != nil {
				return err
			}
			continue
		}
		if summary.LastSeq > stored.LastDispatchSeq {
			a.log.Info("admission reconcile: the stream holds newer events for this issue; leaving it to them",
				"issue", stored.Key, "applied", stored.LastDispatchSeq, "dispatch", summary.LastSeq)
			continue
		}

		if summary.Status == "todo" && readmittable(*stored) {
			if err := a.readmit(ctx, tx, *stored, summary.Title, deref(summary.Parent), summary.Rank, stored.LastDispatchSeq); err != nil {
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
			Status: summary.Status, Seq: stored.LastDispatchSeq,
		}); err != nil {
			return err
		}
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
// event's title, rank, parent, and status reach the record, so a re-rank or a rename at the same
// status moves the waiting line at once. A todo on a lingering or closed root is a re-admission.
func (a *Admission) applyObservation(ctx context.Context, tx pgx.Tx, stored record.Issue, observation intake.DispatchIssue) error {
	if observation.Seq != 0 && observation.Seq <= stored.LastDispatchSeq {
		return nil
	}
	if observation.Status == "todo" && readmittable(stored) {
		return a.readmit(ctx, tx, stored, observation.Title, observation.Parent, observation.Rank, observation.Seq)
	}
	// The workflow handler runs first and re-enters a live tree's child reopened to todo, recording
	// the observation; a child still newly todo here has no live tree. It is an orphan, admitted as
	// a root of its own, as an unrecorded orphan is.
	if observation.Status == "todo" && !claim.IsTreeRoot(stored.Key, stored.Tree) && stored.Status != "todo" {
		a.log.Info("admission orphan", "issue", stored.Key, "parent", observation.Parent)
		return a.readmit(ctx, tx, stored, observation.Title, observation.Parent, observation.Rank, observation.Seq)
	}
	return a.recordObservation(ctx, tx, stored, observed{
		Title: observation.Title, Parent: observation.Parent, Rank: observation.Rank,
		Status: observation.Status, Seq: observation.Seq,
	})
}

// observed is what one Dispatch observation of an issue says about it, from a live event or from
// the boot read.
type observed struct {
	Title, Parent, Rank, Status string
	Seq                         int64
}

// recordObservation is the one place a Dispatch observation reaches an issue record: a live
// event's and the boot read's, so a rename, a re-rank, a re-parent or a status change is written
// the same way whichever brought it. Each caller owns its own guards — the stream's sequence
// fence, the boot read's — and this writes what they let through.
func (a *Admission) recordObservation(ctx context.Context, tx pgx.Tx, stored record.Issue, o observed) error {
	if stored.Title == o.Title && stored.Rank == o.Rank && stored.Status == o.Status &&
		sameParent(stored.Parent, record.ParentOf(o.Parent)) && stored.LastDispatchSeq == o.Seq {
		return nil
	}
	stored.Title = o.Title
	stored.Parent = record.ParentOf(o.Parent)
	stored.Rank = o.Rank
	stored.Status = o.Status
	stored.LastDispatchSeq = o.Seq
	if err := a.store.PutIssue(ctx, tx, stored); err != nil {
		return fmt.Errorf("record observation of %s: %w", stored.Key, err)
	}
	return nil
}

// readmittable says whether a todo on this record starts a new generation of its tree: it is a
// root, and its tree lingers after its sign-off or was closed. A child's done is only the child's.
func readmittable(stored record.Issue) bool {
	return claim.IsTreeRoot(stored.Key, stored.Tree) && (stored.LingerUntil != nil || stored.Phase == phase.Done)
}

// readmit records a lingering or closed root's todo as a new generation waiting for a slot. The new
// generation owns its facts: the old one's pull request, design gate, handoffs, review rounds, and
// pending READY are cleared, so its architect registers the gate again and its implementer waits
// for its own pull request.
func (a *Admission) readmit(ctx context.Context, tx pgx.Tx, stored record.Issue, title, parentKey, rank string, seq int64) error {
	if stored.Generation == ^uint64(0) {
		return fmt.Errorf("re-admit %s: generation overflows", stored.Key)
	}
	stored.Project = a.project
	stored.Title = title
	stored.Parent = record.ParentOf(parentKey)
	stored.Tree = stored.Key
	stored.Phase = phase.Admitted
	stored.Generation++
	stored.Status = "todo"
	stored.Rank = rank
	stored.LingerUntil = nil
	stored.HeldFrom = nil
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
		slots, own = append(slots, slot), append(own, slot)
	}
	return nil
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
// transcript with nothing asked of it, and only its own handoff moves the phase. A child whose
// worker is already started for this run (a fact moved it while the root waited for its slot, and
// that start is queued or has run) is not started a second time: the task would be delivered
// twice. A child whose claim a stop from the tree's close will still suspend is started, so this
// start ends last or supersedes the stop (workflow.RoleStarted, over the role's queued rows and
// this daemon's own claim). The check lives here, where the second start would be written, rather
// than in the start's executor, which could tell a repeated task only if the claim also recorded
// the phase of the task it serves.
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
		if workflow.RoleStarted(run, child.Generation, child.Phase) {
			continue
		}
		payload := record.SuperviseRequest{Op: "start", Tree: child.Tree, Role: role, Generation: child.Generation,
			Phase: child.Phase, Task: workflow.ResumePhaseTask(child)}
		if err := a.enqueue(ctx, tx, child.Key, payload, now); err != nil {
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
