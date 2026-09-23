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
		if stored.Status == summary.Status && stored.Title == summary.Title && stored.Rank == summary.Rank && sameParent(stored.Parent, summary.Parent) {
			continue
		}
		stored.Status = summary.Status
		stored.Title = summary.Title
		stored.Rank = summary.Rank
		stored.Parent = copyParent(summary.Parent)
		if err := a.store.PutIssue(ctx, tx, *stored); err != nil {
			return fmt.Errorf("update reconciled issue %s: %w", summary.Key, err)
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
		Parent:          parent(observation.Parent),
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
	stored.Title = observation.Title
	stored.Parent = parent(observation.Parent)
	stored.Rank = observation.Rank
	stored.Status = observation.Status
	stored.LastDispatchSeq = observation.Seq
	if err := a.store.PutIssue(ctx, tx, stored); err != nil {
		return fmt.Errorf("record observation of %s: %w", observation.Key, err)
	}
	return nil
}

// readmittable says whether a todo on this record starts a new generation of its tree: the tree
// lingers after its sign-off or was closed.
func readmittable(stored record.Issue) bool {
	return stored.LingerUntil != nil || stored.Phase == phase.Done
}

// readmit records a lingering or closed root's todo as a new generation waiting for a slot.
func (a *Admission) readmit(ctx context.Context, tx pgx.Tx, stored record.Issue, title, parentKey, rank string, seq int64) error {
	if stored.Generation == ^uint64(0) {
		return fmt.Errorf("re-admit %s: generation overflows", stored.Key)
	}
	stored.Project = a.project
	stored.Title = title
	stored.Parent = parent(parentKey)
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
	return nil
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
		if issue.Phase == phase.Done || staleStatus(issue.Status) {
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
	waiting := record.Waiting(issues, slots)
	for len(slots) < a.cap && len(waiting) > 0 {
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
		if err := a.enqueue(ctx, tx, candidate.Key, record.SuperviseRequest{Op: "start", Tree: candidate.Tree, Role: claim.RoleArchitect}, now); err != nil {
			return err
		}
		slots = append(slots, slot)
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

func staleStatus(status string) bool {
	switch status {
	case "triage", "icebox", "backlog", "done":
		return true
	default:
		return false
	}
}

func parent(key string) *string {
	if key == "" {
		return nil
	}
	return &key
}

func copyParent(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
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
