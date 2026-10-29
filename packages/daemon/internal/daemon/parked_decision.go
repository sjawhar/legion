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

// parkDecision hands the project's controller the person's spec decision a notice reports when
// nobody inside its tree will hear it (the notice executor's two undelivered branches, the
// re-hold's stopped-with-tree branch): one parked-decision controller notice for issue, naming
// root, and the decision kept for root's next admission, in a transaction of its own that marks
// key processed and owns the commit hook the write registers (projectRecords, intake.OnCommit), so
// the notice's lane wakes as it commits. It does nothing for a kind that is no spec decision, and
// relays nothing for a notice that names no person (gates.design off opened the gate, or the
// release before this one queued the notice, which then carries no ref either). Each outcome logs
// the line that pairs the undelivered row with what became of its decision: sent, already sent, or
// not relayed.
func (r *outbox) parkDecision(ctx context.Context, issue, root string, notice record.Notice, key string, row any) error {
	decision, ok := record.SpecDecision(notice.Kind)
	if !ok {
		return nil
	}
	args := []any{"row", row, "kind", notice.Kind, "issue", issue, "root", root, "key", key}
	if !record.PersonActor(notice.Actor) {
		r.log.Info("outbox parked decision not relayed: it names no person", append(args, "ref", notice.Ref)...)
		return nil
	}
	now := r.now().UTC()
	outboxRow, err := record.NewOutboxRow(issue, record.ControllerNotice{Kind: record.ParkedDecisionNotice, Decision: decision, Root: root,
		Ref: notice.Ref, Actor: notice.Actor, Version: notice.Version, Reason: notice.Reason}, now)
	if err != nil {
		return err
	}
	sent := false
	scoped, committed := intake.WithCommitHooks(ctx)
	if err := pgx.BeginFunc(scoped, r.pool, func(tx pgx.Tx) error {
		fresh, err := r.records.MarkProcessed(scoped, tx, parkedDecisionSource, key)
		if err != nil || !fresh {
			return err
		}
		sent = true
		if err := r.records.AddParkedDecision(scoped, tx, record.ParkedDecision{Root: root, Ref: notice.Ref, Decision: decision, Actor: notice.Actor, At: now}); err != nil {
			return err
		}
		return r.records.Enqueue(scoped, tx, outboxRow)
	}); err != nil {
		return fmt.Errorf("send the controller the decision %s carried on %s: %w", key, issue, err)
	}
	committed()
	msg := "outbox parked decision sent to the controller"
	if !sent {
		msg = "outbox parked decision already sent to the controller"
	}
	r.log.Info(msg, args...)
	return nil
}
