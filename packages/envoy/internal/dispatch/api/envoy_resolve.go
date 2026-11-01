package api

import (
	"cmp"
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
// second timeout (internal/dispatch/envoy/client.go), and each Dispatch process serves every
// request it takes from its own pool of store.sharedPoolSize connections. A listener call made
// while a transaction is open therefore holds one of them - and the rows that transaction
// locked, which the other processes' requests wait on too - for as long as the listener takes
// to answer, so that many concurrent ones empty the pool and stall every unrelated Dispatch
// request until the listener replies. A listener restart, which happens on every listener
// deploy, is enough to trigger it.
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
//
// A sender that never reaches its settle - its process killed, the database refusing the write -
// leaves the attempt pending under its claim. Whichever process runs the delivery sweep next
// sends it once the claim lease (model.DeliveryClaimFreeSQL) has passed, under the attempt's own
// number and idempotency key (delivery_sweep.go); only an attempt past the duplicate window
// waits for a Retry.
//
// Between its first send and its settle a sender may try again (takeoverRetry.retry): a listener
// being replaced refuses connections, answers 503, or does not hold the session yet for a few
// seconds, and a send that met one of those waits it out under the attempt and the idempotency key
// it already holds, renewing its claim before each try, rather than settling the attempt failed. A
// comment's mention resolution that decides which sessions the comment's other routes skip waits
// one out too, before the comment's transaction opens and with nothing held
// (resolveThroughTakeover), and the sender of that mention carries on in the same window rather
// than opening a second one. That attempt keeps the resolution those routes were decided on, for
// whichever sender finishes it (deliverResolvedCommentMention).

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
//
// Both kinds of route resolve by asking the listener about the one thing the route names - the
// holder of a role, or a session id - and neither reads the whole live-session list. That list
// is answered out of the listener's in-memory session cache alone, so a session missing from it
// is either ended or merely not refilled into that cache yet, and an absent row cannot tell the
// two apart: while the cache refills, a working session reads as gone and a person's mention of
// it is refused "no live session". The per-session read goes through to the session store, so it
// answers that question, and a read the message bus did not answer arrives here as an error
// instead of as an absence.
//
// Only ErrNotFound - no record under that id - is "no live session <id>", the text
// comment_delivery.go tests the prefix of and the dashboard prints verbatim. Every other failure
// takes the error path, so an unreachable listener never reads as an ended session.
func (s *server) resolveDeliveryTarget(ctx context.Context, route model.Route, delivery string) (string, string, string) {
	if s.deps.Envoy == nil {
		return "", "", listenerNotConfiguredText
	}
	var target dispatchenvoy.Session
	var err error
	if route.Kind == "role" {
		target, err = s.deps.Envoy.Role(ctx, route.ID)
	} else {
		target, err = s.deps.Envoy.Session(ctx, route.ID)
		if errors.Is(err, dispatchenvoy.ErrNotFound) {
			return route.ID, "", "no live session " + route.ID
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

// listenerNotConfiguredText is what every delivery records when no listener is configured. It
// is the one failure under listenerUnreachablePrefix that no take-over ends.
const listenerNotConfiguredText = "envoy listener: ENVOY_URL is not configured"

// listenerUnreachablePrefix begins the failure deliveryErrorText records for a listener nothing
// answered (dispatchenvoy.ErrUnreachable): a connection refused, reset or timed out, a 502, 503 or
// 504.
const listenerUnreachablePrefix = "envoy listener: "

// listenerAnsweredPrefix begins the failure deliveryErrorText records for every other
// dispatchenvoy.ErrUnavailable: a status the listener chose, such as the 401 it gives a token it
// refuses, or an answer that does not decode.
const listenerAnsweredPrefix = "envoy listener answered: "

// deliveryErrorText is the failure a delivery records for a listener error. Its prefix keeps a
// take-over's failures (listenerUnreachablePrefix) apart from the listener's own answers
// (listenerAnsweredPrefix), because retryableDeliveryFailure reads the recorded text.
func deliveryErrorText(err error) string {
	reason := strings.TrimPrefix(err.Error(), "envoy listener unavailable: ")
	switch {
	case errors.Is(err, dispatchenvoy.ErrUnreachable):
		return listenerUnreachablePrefix + reason
	case errors.Is(err, dispatchenvoy.ErrUnavailable):
		return listenerAnsweredPrefix + reason
	}
	return err.Error()
}

// listenerTakeoverWindow is how long, from its first try, a sender keeps trying a listener that
// is being replaced. The listener's take-over is meant to end within 10 seconds (INFRA-557), so a
// send made at its start still reaches the session once the successor answers; the two seconds
// past that target leave room for the last backoff. A session that is really gone settles
// "no live session" that much later than its first try. It is the window every deployment runs:
// Deps.ListenerTakeoverWindow replaces it only when set, and neither routes.BuildAppContext nor
// cmd/dispatch sets it; only this package's tests do.
const listenerTakeoverWindow = 12 * time.Second

// takeoverFirstBackoff is the wait before a sender's first retry; each later wait doubles it,
// up to takeoverMaxBackoff, so a short blip costs a quarter second and a full take-over a try
// every two seconds.
const (
	takeoverFirstBackoff = 250 * time.Millisecond
	takeoverMaxBackoff   = 2 * time.Second
)

// retryableDeliveryFailure reports whether a failure a try recorded is one a listener take-over
// gives, which a later try can outlast: the listener unreachable (listenerUnreachablePrefix - a
// connection refused, reset or timed out, a 502, 503 or 504 - but never
// listenerNotConfiguredText), a receipt timeout, or the listener holding no record of the session
// yet - "no live session", as the registry read is recorded and as the send's own 404 words it. A
// receipt timeout is safe to repeat because the retry carries the same idempotency key, which the
// listener recognises as a repeat (DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts). Anything
// else is the listener's answer - a session that does not take this mode, a send it refused, a
// status it chose such as a 401 for a token it refuses (listenerAnsweredPrefix) - which no
// successor answers differently, and the attempt settles with it at once.
//
// It reads the recorded text rather than the error behind it because a resumed mention's first
// try sends with the resolve error its row already holds (mentionResolutionFor), which survives
// only as that text.
func retryableDeliveryFailure(failure string) bool {
	switch {
	case failure == listenerNotConfiguredText:
		return false
	case failure == receiptTimeoutText,
		strings.HasPrefix(failure, listenerUnreachablePrefix),
		strings.HasPrefix(failure, "no live session"):
		return true
	}
	return false
}

// takeoverRetry paces one sender's tries at a listener that is being replaced, from the moment of
// its first try. A comment mention's first try is its resolution before the comment's transaction
// (resolveThroughTakeover), so its window spans that resolution and the sender's tries after the
// commit together.
type takeoverRetry struct {
	first   time.Time
	backoff time.Duration
	// window is how long after first a try may still start: listenerTakeoverWindow, or the
	// server's Deps.ListenerTakeoverWindow when set.
	window time.Duration
}

// newTakeoverRetry starts the pacing of a sender about to make its first try, in this server's
// take-over window.
func (s *server) newTakeoverRetry() takeoverRetry {
	return takeoverRetry{
		first:   time.Now(),
		backoff: takeoverFirstBackoff,
		window:  cmp.Or(s.deps.ListenerTakeoverWindow, listenerTakeoverWindow),
	}
}

// retry is the one take-over loop every sender runs: it makes a try, and while again allows
// another after a take-over's failure, renews and tries again; it returns the failure the last try
// recorded, "" for one that succeeded. try makes one try and returns its failure, leaving whatever
// else the try produced in its caller's variables. renew prepares the next try of an attempt - it
// resolves again what the attempt names and renews the claim - and reports whether this sender
// still holds the attempt; a sender that no longer does makes no further try and settles with the
// last one, and an error from renew ends the tries with that error. renew is nil for tries that
// hold no attempt (resolveThroughTakeover).
func (r *takeoverRetry) retry(ctx context.Context, try func() string, renew func() (bool, error)) (string, error) {
	failure := try()
	for r.again(ctx, failure) {
		if renew != nil {
			held, err := renew()
			if err != nil {
				return failure, err
			}
			if !held {
				break
			}
		}
		failure = try()
	}
	return failure, nil
}

// resolveThroughTakeover resolves one target as resolveMentionTargets does, with nothing held,
// and, when wait is set, waits out a listener take-over the way a sender does: a resolution whose
// outcome is a take-over's (retryableDeliveryFailure) is taken again after takeoverRetry's backoff
// until the listener answers otherwise or the window closes. A comment resolves this way the
// mentions and the route its transaction decides suppress_route and the payload's mentions from,
// which the outbox's session routes skip, so those are decided on what the listener answered
// rather than on its silence.
//
// It returns the pacing with the resolution, and the mention's sender after the commit carries on
// in it (deliverResolvedCommentMention) rather than opening a window of its own: a resolution that
// already waited the window out settles with its outcome on that sender's first try, so a comment's
// request waits about one window for a take-over, however long it spent resolving.
func (s *server) resolveThroughTakeover(
	ctx context.Context, target, delivery string, wait bool,
) (ResolvedMention, takeoverRetry) {
	takeover := s.newTakeoverRetry()
	var resolved ResolvedMention
	resolve := func() string {
		resolved = s.resolveMentionTargets(ctx, []string{target}, delivery)[0]
		return resolved.ResolveError
	}
	if !wait {
		resolve()
		return resolved, takeover
	}
	// A try that holds nothing has nothing to renew, so retry answers no error.
	_, _ = takeover.retry(ctx, resolve, nil)
	return resolved, takeover
}

// again reports whether the sender tries once more after a try that recorded failure, having
// waited out the backoff first: only for a take-over's failure, and only while that next try still
// starts inside the window (listenerTakeoverWindow unless the server sets another). Otherwise, or
// when ctx ends during the wait, the sender settles its attempt with failure.
//
// A sender that tries again renews its claim before the try and settles on the claim it renewed
// last, so a claim never ages toward the lease model.DeliveryClaimFreeSQL judges while its sender
// is still trying: the delivery sweep finds nothing to take, and a person's Retry finds the
// attempt held.
func (r *takeoverRetry) again(ctx context.Context, failure string) bool {
	if !retryableDeliveryFailure(failure) || time.Since(r.first)+r.backoff > r.window {
		return false
	}
	wait := time.NewTimer(r.backoff)
	defer wait.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-wait.C:
	}
	r.backoff = min(2*r.backoff, takeoverMaxBackoff)
	return true
}

// attemptClaim is the attempt one sender owns and the claim it holds on it. The two travel
// together because every statement of that sender's settle transaction is scoped to the pair.
//
// A claim transaction takes a pending attempt only while no sender holds it
// (model.DeliveryClaimFreeSQL, selected beside the row it locks): nobody claimed it, or whoever
// did never came back within model.DeliveryClaimLapse. A sender renews its claim before every
// try (takeoverRetry), and a try is bounded by the listener client's own window (five seconds by
// default, DepsInput.EnvoyTimeout in a test) and the short transaction that records its outcome,
// so a claim older than the lease was left by a process that died before its settle. Postgres
// judges it, against the claimed_at Postgres itself wrote: a Dispatch task whose clock ran ahead
// of the database would otherwise read every live claim as lapsed, and one that ran behind could
// never resume a stranded attempt.
//
// A retry beside a live claim takes an attempt of its own rather than two senders driving one. A
// retry beside a lapsed claim resumes that attempt under its original number, and so its
// original idempotency key, whose repeat of a send that did land is recognised and dropped
// (DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts), and in the mode that attempt's row
// pinned rather than the one asked for. For a comment mention that is the whole of it: its key
// names the mention's own mode, not the attempt's (model.CommentMentionIdempotencyKey), so a
// resumed attempt's frame goes under the key its earlier frame went under whichever mode either
// was sent in (claimCommentDeliveryAttempt). A targeted message's key is mode-scoped, so a retry
// there in another mode may not ride that row at all: it settles the stranded attempt and opens
// an attempt of its own (deliverMessageResuming).
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
// senders can hold one attempt: a claim outlives its sender by model.DeliveryClaimLapse, and a
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
