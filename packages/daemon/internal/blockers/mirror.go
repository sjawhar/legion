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
// The periodic turn's own re-read of Dispatch rides on the controller tick
// (intake.ControllerTick.Observed), and is written here through the very same fenced write, so a
// blocker whose closing event never arrived is repaired before those handlers judge.
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

// Apply writes the row of a Dispatch issue fact and every row the periodic turn's re-read
// observed, and does nothing for every other kind of fact — a tick that observed nothing, which
// is almost every turn, included.
func (m *Mirror) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	switch fact := fact.(type) {
	case intake.DispatchIssue:
		observed := intake.IssueStatus{Key: fact.Key, Status: fact.Status, Blockers: fact.Blockers, Seq: fact.Seq}
		if err := m.put(ctx, tx, observed); err != nil {
			return intake.Result{}, err
		}
	case intake.ControllerTick:
		for _, observed := range fact.Observed {
			if err := m.put(ctx, tx, observed); err != nil {
				return intake.Result{}, fmt.Errorf("mirror the turn's re-read of %s: %w", observed.Key, err)
			}
		}
	}
	return intake.Result{}, nil
}

// put is one observation's row, under the daemon's project and behind the sequence fence
// PutDispatchIssue applies: an observation older than the row's own leaves it exactly as it
// stands.
func (m *Mirror) put(ctx context.Context, tx pgx.Tx, observed intake.IssueStatus) error {
	return m.store.PutDispatchIssue(ctx, tx, record.DispatchIssueRow{
		Key: observed.Key, Project: m.project, Status: observed.Status, Blockers: observed.Blockers, Seq: observed.Seq,
	})
}

// Observed is one summary of a Dispatch listing as an observation of that issue, the four facts
// a mirror row is written from: the boot's reconcile and the periodic turn's listing both read
// the project this way, and both write what it returns.
func Observed(summary dispatch.IssueSummary) intake.IssueStatus {
	return intake.IssueStatus{Key: summary.Key, Status: summary.Status, Blockers: summary.BlockedBy, Seq: summary.LastSeq}
}

// Reconcile mirrors the boot listing: every summary Dispatch returned for project, through the
// same sequence-fenced write a live event takes, so a listing older than an event already applied
// changes nothing.
func Reconcile(ctx context.Context, tx pgx.Tx, store record.Store, project string, summaries []dispatch.IssueSummary) error {
	mirror := NewMirror(store, project)
	for _, summary := range summaries {
		if err := mirror.put(ctx, tx, Observed(summary)); err != nil {
			return fmt.Errorf("mirror the boot listing of %s: %w", summary.Key, err)
		}
	}
	return nil
}
