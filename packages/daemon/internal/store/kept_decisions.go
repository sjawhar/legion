package store

import (
	"context"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// What a root architect's start task names of the decisions a person made while nobody in its
// tree could hear them (record.KeptDecisions, carried as supervise.Delivery.Kept and persisted as
// pending_task_deliveries.kept_decisions) is held by the delivery that task was queued as, and
// told at that delivery's confirmation. Those rows stand in parked_decisions throughout
// (0065_parked_decisions): the claim's write of the delivery takes each from the start row that
// named it and marks it held by this claim and delivery id, the write recording the confirmation
// tells it, and the write that retires the delivery leaves it KEPT again. Each is the delivery's
// own write, so no ordering of queues, confirmations, retirements and restarts can leave a row
// held by a delivery that is not there or told by one whose turn never started.
//
// The telling follows the one validity rule both sides of this machinery follow: a row is told
// only while its own issue still stands in the claim's tree. An issue that has left it — a child
// a linger handed to Legion, a parked child a person orphaned, a queued root admission adopted —
// is owed its answer by the tree it stands in now, so its row's claim and hold go instead and
// that tree's next start or catch-up names it. The old architect having been prompted with it as
// well is the lesser fault: a person's decision is never lost.

// holdKeptDecisions marks every row the delivery names held by this claim and delivery id, taking
// each from the start row that claimed it, and moves to this delivery id whatever the claim's
// previous delivery held — a task a death took back comes back under a new id, and what it names
// is the same (supervise's interrupted, takeBackPending). A delivery that names nothing holds
// nothing.
func holdKeptDecisions(ctx context.Context, db execer, token claim.Token, delivery string, kept []byte) error {
	if len(kept) == 0 {
		return nil
	}
	if _, err := db.Exec(ctx, `update parked_decisions set held_by_delivery = $2
		where held_by_claim = $1 and held_by_delivery is not null and held_by_delivery <> $2`,
		string(token), delivery); err != nil {
		return fmt.Errorf("move what the task of %s held to delivery %s: %w", token, delivery, err)
	}
	if _, err := db.Exec(ctx, `update readmitted_approvals set held_by_delivery = $2
		where held_by_claim = $1 and held_by_delivery is not null and held_by_delivery <> $2`,
		string(token), delivery); err != nil {
		return fmt.Errorf("move the approval the task of %s held to delivery %s: %w", token, delivery, err)
	}
	if _, err := db.Exec(ctx, `update parked_decisions set claimed_by = null, held_by_claim = $1, held_by_delivery = $2
		from jsonb_to_recordset(coalesce($3::jsonb -> 'decisions', '[]'::jsonb)) as named (ref text, at timestamptz)
		where parked_decisions.ref = named.ref and parked_decisions.at = named.at`,
		string(token), delivery, kept); err != nil {
		return fmt.Errorf("hold what the task queued on %s as delivery %s names: %w", token, delivery, err)
	}
	if _, err := db.Exec(ctx, `update readmitted_approvals set claimed_by = null, held_by_claim = $1, held_by_delivery = $2
		from claims holder, (select ($3::jsonb ->> 'approved')::int as approved, ($3::jsonb ->> 'latest')::int as latest) as named
		where holder.token = $1 and readmitted_approvals.issue = holder.tree
		and readmitted_approvals.approved_version = named.approved and readmitted_approvals.latest_version = named.latest`,
		string(token), delivery, kept); err != nil {
		return fmt.Errorf("hold the approval the task queued on %s as delivery %s names: %w", token, delivery, err)
	}
	return nil
}

// tellKeptDecisions is the confirmation's own write: the turn carrying this delivery has started,
// so every row it holds whose issue still stands in the claim's tree has been told and is deleted,
// and every row whose issue has left is KEPT again for the tree it stands in now to name.
func tellKeptDecisions(ctx context.Context, db execer, token claim.Token, delivery string) error {
	for _, table := range []string{"parked_decisions", "readmitted_approvals"} {
		if _, err := db.Exec(ctx, `delete from `+table+` using claims holder, issues owner
			where holder.token = $1 and owner.key = `+table+`.issue and `+table+`.held_by_claim = $1
			and `+table+`.held_by_delivery = $2 and owner.tree = holder.tree`, string(token), delivery); err != nil {
			return fmt.Errorf("tell what the delivery %s on %s held: %w", delivery, token, err)
		}
	}
	return releaseKeptDecisions(ctx, db, token, delivery)
}

// releaseKeptDecisions clears the hold the delivery holds on every row, which is what a retired
// delivery and a told one's leftovers both leave behind: the row is KEPT, for the next start or
// catch-up of its own issue's tree to name.
func releaseKeptDecisions(ctx context.Context, db execer, token claim.Token, delivery string) error {
	for _, table := range []string{"parked_decisions", "readmitted_approvals"} {
		if _, err := db.Exec(ctx, `update `+table+` set claimed_by = null, held_by_claim = null, held_by_delivery = null
			where held_by_claim = $1 and held_by_delivery = $2`, string(token), delivery); err != nil {
			return fmt.Errorf("leave what the delivery %s on %s held kept: %w", delivery, token, err)
		}
	}
	return nil
}
