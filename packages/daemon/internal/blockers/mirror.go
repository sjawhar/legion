package blockers

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Mirror writes what each Dispatch issue event says into the mirror table, and decides nothing: it
// refuses no fact, ever, so the handlers after it see exactly the facts they would have seen
// without it. It runs before them, inside their transaction, because they read the row it writes.
//
// One path it never sees: an intake.ObservedIssue of an issue already recorded whose event was
// prepared in place of none applies no handler at all (intake.settleObserved sets handlers to
// nil), so its view of the issue reaches no mirror write. Nothing depends on that observation —
// the issue's next event, the periodic turn or the next boot's reconcile writes the row, and until
// then the row holds the last thing Dispatch said.
type Mirror struct {
	store   record.Store
	project string
}

var _ intake.Handler = (*Mirror)(nil)

// NewMirror creates the mirror handler for one Dispatch project: every row it writes is that
// project's, since the daemon mirrors the issues of the project it runs.
func NewMirror(store record.Store, project string) *Mirror {
	return &Mirror{store: store, project: project}
}

// Apply writes the row of a Dispatch issue fact and does nothing for every other kind of fact.
func (m *Mirror) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	issue, ok := fact.(intake.DispatchIssue)
	if !ok {
		return intake.Result{}, nil
	}
	row := record.DispatchIssueRow{
		Key: issue.Key, Project: m.project, Status: issue.Status, Blockers: issue.Blockers, Seq: issue.Seq,
	}
	if err := m.store.PutDispatchIssue(ctx, tx, row); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, nil
}

// Reconcile mirrors the boot listing: every summary Dispatch returned for project, through the
// same sequence-fenced write a live event takes, so a listing older than an event already applied
// changes nothing.
func Reconcile(ctx context.Context, tx pgx.Tx, store record.Store, project string, summaries []dispatch.IssueSummary) error {
	for _, summary := range summaries {
		row := record.DispatchIssueRow{
			Key: summary.Key, Project: project, Status: summary.Status, Blockers: summary.BlockedBy, Seq: summary.LastSeq,
		}
		if err := store.PutDispatchIssue(ctx, tx, row); err != nil {
			return fmt.Errorf("mirror the boot listing of %s: %w", summary.Key, err)
		}
	}
	return nil
}
