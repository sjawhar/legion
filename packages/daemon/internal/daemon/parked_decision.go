package daemon

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// parkedDecisionSource is the processed-event source of the parked-decision notices the outbox
// writes. Its event id is the dedupe key the undelivered notice went under (record.Notice.
// Published: a re-held copy's is its original row's), so one decision writes one notice however
// many rows and reports carry it, and however often a row whose finish did not commit runs again.
// Nothing downstream can: each relay is a controller row of its own, published under its own
// row's key, so neither the listener's delivery caches nor a host's dedupe
// (dedupeKeyNamesItsEvent, packages/contracts/src/envelope.ts) can tell that two rows relay one
// decision.
const parkedDecisionSource = "parked-decision"

// parkDecision hands the person's spec decision a notice reports to the one rule for a decision
// nobody in its tree hears (record.RelayParkedDecision): the project's controller is told, in a
// parked-decision notice of its own, and the decision is kept for the root's next architect start.
// It runs in a transaction of its own that marks key processed and owns the commit hook the write
// registers (projectRecords, intake.OnCommit), so the notice's lane wakes as it commits. It does
// nothing for a kind that is no spec decision. Each outcome logs the line that pairs the
// undelivered row with what became of its decision: sent, already sent, kept for a root that waits
// for its slot, or not relayed — by no person, or on a root that is no parked tree.
func (r *outbox) parkDecision(ctx context.Context, issue, root string, notice record.Notice, key string, row int64) error {
	decision, ok := record.SpecDecision(notice.Kind)
	if !ok {
		return nil
	}
	args := []any{"row", row, "kind", notice.Kind, "issue", issue, "root", root, "key", key}
	now := r.now().UTC()
	outcome, fresh := record.RelayNone, false
	scoped, committed := intake.WithCommitHooks(ctx)
	if err := pgx.BeginFunc(scoped, r.pool, func(tx pgx.Tx) error {
		marked, err := r.records.MarkProcessed(scoped, tx, parkedDecisionSource, key)
		if err != nil || !marked {
			return err
		}
		fresh = true
		recorded, err := r.records.Issue(scoped, tx, root)
		if err != nil || recorded == nil {
			return err
		}
		outcome, err = record.RelayParkedDecision(scoped, tx, r.records, issue, *recorded, true,
			record.ParkedDecision{Ref: notice.Ref, Decision: decision, Actor: notice.Actor, At: now}, notice.Version, notice.Reason)
		return err
	}); err != nil {
		return fmt.Errorf("send the controller the decision %s carried on %s: %w", key, issue, err)
	}
	committed()
	switch {
	case !fresh:
		r.log.Info("outbox parked decision already sent to the controller", args...)
	case outcome == record.RelaySent:
		r.log.Info("outbox parked decision sent to the controller", args...)
	case outcome == record.RelayKept:
		r.log.Info("outbox parked decision kept for its root's start, which waits for its slot", args...)
	case outcome == record.RelayNoPerson:
		r.log.Info("outbox parked decision not relayed: it names no person", append(args, "ref", notice.Ref)...)
	case outcome == record.RelayNotParked:
		r.log.Info("outbox parked decision not relayed: its root is not parked", args...)
	}
	return nil
}

// forgetKept forgets exactly what a delivered architect start task or catch-up named of what its
// root keeps (record.Store.ForgetKeptDecisions), in a transaction of its own after the delivery:
// a row that finishes without delivering leaves every decision for the next start or catch-up to
// name again. A failure returns an error, so the row retries and the architect may be told a
// decision twice rather than never.
func (r *outbox) forgetKept(ctx context.Context, root string, kept *record.KeptDecisions) error {
	if kept == nil {
		return nil
	}
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.ForgetKeptDecisions(ctx, tx, root, *kept)
	}); err != nil {
		return fmt.Errorf("forget the decisions named to %s: %w", root, err)
	}
	return nil
}
