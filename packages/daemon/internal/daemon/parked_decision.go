package daemon

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
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
// nothing for a kind that is no spec decision. Every outcome logs one line that pairs the
// undelivered row with what became of its decision: sent, kept for a root that waits for its slot,
// not relayed (by no person, on a root that is no parked tree, on a tree that hears it, or on a
// root with no record), or handled before under the same key, by an earlier run or another row
// carrying the same notice, whose own line said what became of it.
func (r *outbox) parkDecision(ctx context.Context, issue, root string, notice record.Notice, key string, row int64) error {
	decision, ok := record.SpecDecision(notice.Kind)
	if !ok {
		return nil
	}
	args := []any{"row", row, "kind", notice.Kind, "issue", issue, "root", root, "key", key}
	now := r.now().UTC()
	outcome, fresh, recorded := record.RelayNone, false, false
	scoped, committed := intake.WithCommitHooks(ctx)
	if err := pgx.BeginFunc(scoped, r.pool, func(tx pgx.Tx) error {
		marked, err := r.records.MarkProcessed(scoped, tx, parkedDecisionSource, key)
		if err != nil || !marked {
			return err
		}
		fresh = true
		stored, err := r.records.Issue(scoped, tx, root)
		if err != nil || stored == nil {
			return err
		}
		recorded = true
		outcome, err = record.RelayParkedDecision(scoped, tx, r.records, issue, *stored, true,
			record.ParkedDecision{Ref: notice.Ref, Decision: decision, Actor: notice.Actor, At: now}, notice.Version, notice.Reason)
		return err
	}); err != nil {
		return fmt.Errorf("send the controller the decision %s carried on %s: %w", key, issue, err)
	}
	committed()
	switch {
	case !fresh:
		r.log.Info("outbox parked decision handled before under this key", args...)
	case !recorded:
		r.log.Info("outbox parked decision not relayed: its root has no record", args...)
	case outcome == record.RelaySent:
		r.log.Info("outbox parked decision sent to the controller", args...)
	case outcome == record.RelayKept:
		r.log.Info("outbox parked decision kept for its root's start, which waits for its slot", args...)
	case outcome == record.RelayNoPerson, outcome == record.RelayNoPersonWaiting:
		r.log.Info("outbox parked decision not relayed: it names no person", append(args, "ref", notice.Ref)...)
	case outcome == record.RelayNotParked:
		r.log.Info("outbox parked decision not relayed: its root is not parked", args...)
	default:
		r.log.Info("outbox parked decision not relayed: its tree hears it", args...)
	}
	return nil
}

// settleKept settles what an architect start row's task named of what its root keeps, once the row
// has run and whatever its generation or phase made of it. Two things say the task reached the
// architect: gave, which the run that handed it to the claim knows, and the claim's own record of
// every start row whose task it queued (supervise.Claim.DeliveredRows), which outlives the
// delivery and answers for a later run of the same row. Either way the row forgets exactly what it
// named (record.Store.ForgetKeptDecisions). A row neither says reached no architect — an earlier
// generation, a phase left, a lingering tree, a claim that never took the task — and hands what it
// claimed over (workflow.HandOverKept), so no release leaves a decision unclaimed while the root
// has an architect to tell.
//
// Every supervise row is settled after its work (the outbox's execute), and the one step that
// names decisions inside a start's own run — the ClaimReady a start applies when it finds the root
// architect already running — settles first, so the catch-up that fact writes never names a
// decision the start's task carries. A failure returns an error, so the row retries: a retried
// start queues its task no second time (the claim holds the row), so it settles the same way
// again, and a settlement that committed finds nothing left to do.
func (r *outbox) settleKept(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest, gave bool) error {
	if payload.Kept == nil {
		return nil
	}
	root := payload.Tree
	token, err := claim.NewToken(r.project, root, claim.RoleArchitect)
	if err != nil {
		return fmt.Errorf("derive the architect of %s: %w", root, err)
	}
	held := r.supervisedClaim(token).Claim
	if gave || slices.Contains(held.DeliveredRows, row.ID) {
		return r.writeSettlement(ctx, root, row.ID, func(ctx context.Context, tx pgx.Tx) error {
			return r.records.ForgetKeptDecisions(ctx, tx, root, *payload.Kept, row.ID)
		})
	}
	r.log.Info("outbox start finished without giving its claim the task; the decisions it named are handed over", "row", row.ID,
		"root", root, "generation", payload.Generation, "launch", held.Generation)
	return r.handOverKept(ctx, root, row.ID, held.Generation)
}

// handOverKept hands the decisions outbox row claims to a fresh catch-up of root's architect, or
// leaves them unclaimed when the root has no architect to tell (workflow.HandOverKept). launch is
// the architect claim's current launch, which the catch-up is written for.
func (r *outbox) handOverKept(ctx context.Context, root string, row int64, launch uint64) error {
	return r.writeSettlement(ctx, root, row, func(ctx context.Context, tx pgx.Tx) error {
		return workflow.HandOverKept(ctx, tx, r.records, r.designGate, root, row, launch, r.now().UTC())
	})
}

// writeSettlement runs one settlement of what outbox row named of what root keeps, in a
// transaction of its own that serializes with every fact (intake.SerializeFacts), since a hand-over
// reads the root and writes the catch-up a ready would write, and owns the commit hooks its writes
// register (projectRecords, intake.OnCommit), so a queued catch-up's lane wakes as it commits.
func (r *outbox) writeSettlement(ctx context.Context, root string, row int64, write func(context.Context, pgx.Tx) error) error {
	scoped, committed := intake.WithCommitHooks(ctx)
	if err := pgx.BeginFunc(scoped, r.pool, func(tx pgx.Tx) error {
		if err := intake.SerializeFacts(scoped, tx); err != nil {
			return fmt.Errorf("serialize the settlement with every fact: %w", err)
		}
		return write(scoped, tx)
	}); err != nil {
		return fmt.Errorf("settle the decisions outbox row %d named to %s: %w", row, root, err)
	}
	committed()
	return nil
}

// toldKept records that a catch-up published under key told the architect the decisions it named,
// in a transaction of its own and before anything forgets them (settleTold). The forget can fail —
// a database error, or the daemon dying between the publish and its commit — and this mark is then
// the only thing that says the architect was told: without it the row's ending would release what
// it named and the next start or catch-up would tell it a second time. The mark goes under the
// telling's dedupe key, which a re-held copy shares with the row it copies
// (record.Notice.Published), since one key is one telling however many rows publish it. A start
// needs none: its claim records the row with the delivery (supervise.Claim.DeliveredRows).
func (r *outbox) toldKept(ctx context.Context, root, key string) error {
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := r.records.MarkProcessed(ctx, tx, record.KeptToldSource, key)
		return err
	}); err != nil {
		return fmt.Errorf("record that the catch-up of %s told the decisions it named: %w", root, err)
	}
	return nil
}

// settleTold forgets what outbox row's catch-up named of what its root keeps, once the row has run
// and told it (toldKept), however that run ended: the publish that told it, a newer catch-up that
// superseded the row since, a holder that refused it, a grant that denied it, an architect that
// will not hold its role again, or the row's own deletion under its lease. Only a row that told
// nothing is left to settle another way — the hand-over of a superseded catch-up (supersededKept),
// and otherwise the architect's next ready, which names every unclaimed decision. A failure
// returns an error, so the row retries and forgets again; a forget of what is already gone deletes
// nothing.
func (r *outbox) settleTold(ctx context.Context, row record.OutboxRow, payload record.Notice) error {
	if payload.CatchUp == nil || payload.CatchUp.Kept == nil {
		return nil
	}
	// A catch-up is enqueued for the root whose tree it states (workflow.QueueCatchUp), so the
	// row's issue is that root; the key is the one its publish went out under, a re-held copy's
	// being the row it copies (record.Notice.Published).
	root := row.Issue
	_, key := payload.Published(row.ID)
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		told, err := r.records.Processed(ctx, tx, record.KeptToldSource, key)
		if err != nil || !told {
			return err
		}
		return r.records.ForgetKeptDecisions(ctx, tx, root, *payload.CatchUp.Kept, row.ID)
	}); err != nil {
		return fmt.Errorf("forget the decisions the catch-up of %s told: %w", root, err)
	}
	return nil
}

// supersededKept hands what outbox row's catch-up named over to a fresh catch-up once a newer one
// has superseded it, so that it will never publish: the newer catch-up was composed from what
// nobody claimed and left this row's decisions out (workflow.HandOverKept). A row that published
// before — its forget failed, or the daemon died between the two — told what it named and hands
// nothing over: settleTold forgets it when the row finishes here.
func (r *outbox) supersededKept(ctx context.Context, root string, row int64, key string, launch uint64) error {
	return r.writeSettlement(ctx, root, row, func(ctx context.Context, tx pgx.Tx) error {
		told, err := r.records.Processed(ctx, tx, record.KeptToldSource, key)
		if err != nil || told {
			return err
		}
		return workflow.HandOverKept(ctx, tx, r.records, r.designGate, root, row, launch, r.now().UTC())
	})
}
