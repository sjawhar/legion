package daemon

import (
	"context"
	"fmt"
	"log/slog"

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

// parkedDecision is what parkDecision did with a notice: nothing (key empty) for a kind that is no
// spec decision; else whether it relayed it (sent), found it relayed already, or relayed nothing
// because the notice names no person (unnamed: gates.design off opened the gate, or the release
// before this one queued the notice, which then carries no ref either).
type parkedDecision struct {
	key     string
	sent    bool
	unnamed bool
	ref     string
}

// log writes the line that pairs an undelivered decision with what became of it.
func (p parkedDecision) log(log *slog.Logger, args ...any) {
	msg := "outbox parked decision sent to the controller"
	switch {
	case p.key == "":
		return
	case p.unnamed:
		msg = "outbox parked decision not relayed: it names no person"
		args = append(args, "ref", p.ref)
	case !p.sent:
		msg = "outbox parked decision already sent to the controller"
	}
	log.Info(msg, append(args, "key", p.key)...)
}

// parkDecision hands the project's controller the person's spec decision a notice reports when
// nobody inside its tree will hear it (the notice executor's two undelivered branches, the
// re-hold's stopped-with-tree branch): one parked-decision controller notice for issue, naming
// root, and the decision kept for root's next admission, in a transaction of its own that marks
// key processed and owns the commit hook the write registers (projectRecords, intake.OnCommit), so
// the notice's lane wakes as it commits. A notice that names no person relays nothing.
func (r *outbox) parkDecision(ctx context.Context, issue, root string, notice record.Notice, key string) (parkedDecision, error) {
	decision, ok := record.SpecDecision(notice.Kind)
	if !ok {
		return parkedDecision{}, nil
	}
	if !record.PersonActor(notice.Actor) {
		return parkedDecision{key: key, unnamed: true, ref: notice.Ref}, nil
	}
	now := r.now().UTC()
	row, err := record.NewOutboxRow(issue, record.ControllerNotice{Kind: record.ParkedDecisionNotice, Decision: decision, Root: root,
		Ref: notice.Ref, Actor: notice.Actor, Version: notice.Version, Reason: notice.Reason}, now)
	if err != nil {
		return parkedDecision{}, err
	}
	parked := parkedDecision{key: key}
	scoped, committed := intake.WithCommitHooks(ctx)
	if err := pgx.BeginFunc(scoped, r.pool, func(tx pgx.Tx) error {
		fresh, err := r.records.MarkProcessed(scoped, tx, parkedDecisionSource, key)
		if err != nil || !fresh {
			return err
		}
		parked.sent = true
		if err := r.records.AddParkedDecision(scoped, tx, record.ParkedDecision{Root: root, Ref: notice.Ref, Decision: decision, Actor: notice.Actor, At: now}); err != nil {
			return err
		}
		return r.records.Enqueue(scoped, tx, row)
	}); err != nil {
		return parkedDecision{}, fmt.Errorf("send the controller the decision %s carried on %s: %w", key, issue, err)
	}
	committed()
	return parked, nil
}
