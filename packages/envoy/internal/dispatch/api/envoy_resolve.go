package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Dispatch's Envoy listener client is a cross-service HTTP call bounded only by its five
// second timeout (internal/dispatch/envoy/client.go), and production runs one Dispatch task
// on pgx's default pool of four connections. A listener call made while a transaction is open
// therefore holds one of those four connections - and the rows that transaction locked - for
// as long as the listener takes to answer, so four concurrent ones empty the pool and stall
// every unrelated Dispatch request until the listener replies. A listener restart, which
// happens on every listener deploy, is enough to trigger it.
//
// So no listener call runs with a transaction or a pooled connection held, and this file is
// the one place the shape is spelled:
//
//  1. resolve - the listener call, with nothing held;
//  2. lock    - open the transaction and take the locks the write needs;
//  3. verify  - re-read under those locks whatever the resolution depended on, and decide
//               with the resolution only while the locked rows still agree with it;
//  4. redo    - a resolution the lock contradicts is taken once more, bounded, before the
//               write answers with its own conflict.
//
// An outbound listener call (Envoy.Send) never runs inside a transaction at all, and this file
// spells that half too:
//
//  1. claim  - one short transaction commits the attempt as pending and takes it;
//  2. send   - the listener call, with nothing held;
//  3. settle - a second short transaction records the outcome and appends the receipt.

// errStaleResolution is how a locked write reports that a row the pre-transaction listener
// resolution depended on had changed by the time it held the lock. It never reaches a client:
// resolveThenLock either redoes the pair or answers with the caller's conflict.
var errStaleResolution = errors.New("listener resolution is stale")

// resolutionAttempts is how many times resolveThenLock resolves before it gives up: the
// first, and one redo for a row that moved underneath it.
const resolutionAttempts = 2

// resolveThenLock runs resolve with no transaction or pooled connection held, hands what it
// resolved to write, and runs the pair again when write reports errStaleResolution - at most
// resolutionAttempts times, after which stale is the answer.
//
// write owns its own transaction, and owes two things beyond opening it. It MUST re-read,
// under the locks it takes, every row the resolution depended on, and compare: a write that
// merely uses R without checking it against what the lock found is the bug this shape exists
// to prevent, because the interesting case is precisely the row that moved while the listener
// was answering. And it MUST have rolled back before it reports errStaleResolution, since
// resolveThenLock calls it again.
func resolveThenLock[R, T any](
	ctx context.Context,
	resolve func(context.Context) (R, error),
	write func(context.Context, R) (T, error),
	stale error,
) (T, error) {
	var zero T
	for range resolutionAttempts {
		resolved, err := resolve(ctx)
		if err != nil {
			return zero, err
		}
		result, err := write(ctx, resolved)
		if !errors.Is(err, errStaleResolution) {
			return result, err
		}
	}
	return zero, stale
}

// resolveDeliveryTarget is the listener read behind every delivery: which live session a route
// names right now, what the listener calls it, and why it cannot receive this mode. It is the
// resolve half above, so it runs with nothing held.
func (s *server) resolveDeliveryTarget(ctx context.Context, route model.Route, delivery string) (string, string, string) {
	if s.deps.Envoy == nil {
		return "", "", "envoy listener: ENVOY_URL is not configured"
	}
	var target dispatchenvoy.Session
	var err error
	if route.Kind == "role" {
		target, err = s.deps.Envoy.Role(ctx, route.ID)
	} else {
		var sessions []dispatchenvoy.Session
		sessions, err = s.deps.Envoy.Sessions(ctx)
		if err == nil {
			for _, candidate := range sessions {
				if candidate.SessionID == route.ID {
					target = candidate
					break
				}
			}
			if target.SessionID == "" {
				return route.ID, "", "no live session " + route.ID
			}
		}
	}
	if err != nil {
		return "", "", deliveryErrorText(err)
	}
	if !hasCapability(target.Capabilities, delivery) {
		return target.SessionID, target.Title,
			fmt.Sprintf("session %s (%s) does not advertise %s", target.SessionID, target.Title, delivery)
	}
	return target.SessionID, target.Title, ""
}

func hasCapability(capabilities []string, value string) bool {
	for _, capability := range capabilities {
		if capability == value {
			return true
		}
	}
	return false
}

func deliveryErrorText(err error) string {
	if errors.Is(err, dispatchenvoy.ErrUnavailable) {
		return "envoy listener: " + strings.TrimPrefix(err.Error(), "envoy listener unavailable: ")
	}
	return err.Error()
}

// claimLapsed is the predicate a claim transaction selects beside the pending attempt it locks:
// whether that attempt is free for this sender to take, because nobody holds it or whoever
// claimed it never came back within the lease. The lease is a minute. A send is bounded by the
// listener client's five-second timeout and the short transaction that records its outcome, so a
// claim older than that was left by a process that died between the two. The next retry resumes
// that attempt under its original number, whose idempotency key the listener has already
// deduplicated if the send did land; a retry beside a live claim takes an attempt of its own
// rather than two senders driving one.
//
// Postgres judges it, against the claimed_at Postgres itself wrote. A Dispatch task whose clock
// ran ahead of the database would otherwise read every live claim as lapsed, and one that ran
// behind could never resume a stranded attempt.
const claimLapsed = `(claimed_at is null or claimed_at < now() - interval '1 minute')`

// attemptClaim is the attempt one sender owns and the claim it holds on it. The two travel
// together because every statement of that sender's settle transaction is scoped to the pair.
type attemptClaim struct {
	attempt   int
	claimedAt time.Time
}

// deliveryOutcome maps what a send reported to the state and error an attempt row records.
func deliveryOutcome(deliveryError string) (string, *string) {
	if deliveryError == "" {
		return "sent", nil
	}
	return "failed", &deliveryError
}

// receiptError renders an attempt row's nullable error as the string a receipt payload carries.
func receiptError(failure *string) string {
	if failure == nil {
		return ""
	}
	return *failure
}

// settleDeliveryAttempt runs the settle transaction of a delivery, for comment mentions and
// targeted messages alike: it records what the send did on the claimed attempt row and appends
// that attempt's receipt, holding no listener call.
//
// The receipt's owner is locked before the attempt row, the order every other delivery
// transaction takes them in. Locking the attempt first and the owner second - which appending
// the event alone would do - inverts that order against the claim transaction and deadlocks two
// concurrent retries of the same comment or message.
//
// Both statements this transaction makes are scoped to the claim this sender took, because two
// senders can hold one attempt: a claim outlives its sender by the claimLapsed lease, and a
// sender that comes back after that lease lapsed finds the attempt resumed by someone else.
// Only the sender the row's claim still names settles it or pays anything on it; the other
// leaves the row as it found it.
//
// settle updates the pending row this sender claimed and returns it; pgx.ErrNoRows means it is
// no longer that row, and answered reads what is there now: the row, whether the claim on it is
// still this sender's, and the reply that settled it. Whether the attempt's receipt is still
// owed is a property of that row rather than of which path it is: both reply handlers append
// the attempt's own delivery receipt on their error branch and only *.answered on their body
// branch, and only the body branch records a reply. So this transaction owes the receipt when
// the row whose claim it still holds carries a reply_id, and answeredReceipt builds that
// receipt from the row, stating what the reply recorded rather than what this send reported -
// so every attempt carries exactly one receipt and that receipt agrees with its row.
func settleDeliveryAttempt[T any](
	ctx context.Context,
	s *server,
	receipt model.Event,
	settle func(context.Context, pgx.Tx) (T, error),
	answered func(context.Context, pgx.Tx) (row T, mine bool, replyID *string, err error),
	answeredReceipt func(T) model.Event,
) (T, error) {
	var zero T
	tx, err := s.begin(ctx)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(ctx)
	if err := s.deps.Events.LockOwners(ctx, tx, receipt); err != nil {
		return zero, err
	}
	attempt, err := settle(ctx, tx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		settled, mine, replyID, err := answered(ctx, tx)
		if err != nil {
			return zero, err
		}
		if !mine || replyID == nil {
			if err := tx.Commit(ctx); err != nil {
				return zero, err
			}
			return settled, nil
		}
		// The owed receipt reports the same attempt of the same message or comment, so its
		// owner is the one locked above.
		attempt, receipt = settled, answeredReceipt(settled)
	case err != nil:
		return zero, err
	}
	event, err := s.appendEvent(ctx, tx, receipt)
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, err
	}
	s.publish(event)
	return attempt, nil
}
