package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asks"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// ResolvedMention binds a canonical mention to the session that owned it when the comment was
// created. SessionID is nil when no live holder was found; attemptSessionID remains internal so
// legacy message attempts retain their existing session-id representation. unanswered says the
// lookup was cut short by its bound while the request was still live (resolveMentionTarget), so
// the listener has not said who holds the target.
type ResolvedMention struct {
	Target       string
	Delivery     string
	SessionID    *string
	ResolveError string

	title            string
	attemptSessionID string
	unanswered       bool
}

// mentionedSession is the session this mention delivers the comment to, as far as the comment's
// creation knows: the session it counts under (model.MentionSession) when its lookup pinned one or
// went unanswered. A lookup that ended without a pin for any other reason - no live session holds
// the target, the listener failed, the target does not parse - fails the send, so it names none.
func (m ResolvedMention) mentionedSession() (string, bool) {
	if m.SessionID == nil && !m.unanswered {
		return "", false
	}
	return model.MentionSession(deref(m.SessionID), m.Target)
}

// mentionLookupTimeout bounds each listener lookup a comment's creation makes before its
// transaction opens: every mention's, and the issue route's. A lookup cut short leaves its
// target unresolved (resolveMentionTarget), and the comment is created and answered without it.
// A listener that answers no lookup delays a comment with up to mentionLookupConcurrency
// mentions by one timeout, and each further mentionLookupConcurrency mentions by one more:
// ceil(k/8) timeouts for k mentions.
const mentionLookupTimeout = 250 * time.Millisecond

// mentionLookupConcurrency is how many targets resolveMentionTargets resolves at once, so up to 8
// mentions cost a comment one listener round trip - one 250 ms timeout when the listener answers
// none - and each further 8 add one: ceil(k/8) for k mentions.
const mentionLookupConcurrency = 8

// resolveMentionTargets is the registry lookup behind a comment's deliveries, one try per target.
// What a comment resolved before its transaction (resolveCommentDeliveries) is persisted in the
// comment-created event and handed to the post-commit sender, so a role transition cannot change
// the target after routing suppression has been decided. It resolves up to
// mentionLookupConcurrency targets at once, each bounded by mentionLookupTimeout, and answers
// them in input order.
func (s *server) resolveMentionTargets(ctx context.Context, targets []string, delivery string) []ResolvedMention {
	resolved := make([]ResolvedMention, len(targets))
	var lookups errgroup.Group
	lookups.SetLimit(mentionLookupConcurrency)
	for index, target := range targets {
		lookups.Go(func() error {
			resolved[index] = s.resolveMentionTarget(ctx, target, delivery, mentionLookupTimeout)
			return nil
		})
	}
	_ = lookups.Wait()
	return resolved
}

// resolveMentionTarget resolves one mention target. timeout, when positive, bounds the lookup;
// zero leaves it bounded by the listener client's own window. A lookup that timeout cut short,
// while ctx itself is still live, yields the target unanswered, with no pinned session and no
// resolve error: the listener has not answered, so nothing is known to refuse and nothing is
// pinned. createCommentFor still counts the session a session:<id> target names
// (mentionedSession) when it decides what the mention makes redundant. Its attempt row records
// neither a session nor an error, which is the shape of an attempt stranded before anything was
// resolved, so the sender resolves it when it sends (mentionResolutionFor).
func (s *server) resolveMentionTarget(
	ctx context.Context, target, delivery string, timeout time.Duration,
) ResolvedMention {
	item := ResolvedMention{Target: target, Delivery: delivery}
	route, err := model.ParseRoute(target)
	if err != nil {
		item.ResolveError = err.Error()
		return item
	}
	lookup := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		lookup, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	sessionID, title, resolveError := s.resolveDeliveryTarget(lookup, route, delivery)
	if sessionID == "" && resolveError != "" && ctx.Err() == nil &&
		errors.Is(lookup.Err(), context.DeadlineExceeded) {
		item.unanswered = true
		return item
	}
	item.title = title
	item.attemptSessionID = sessionID
	item.ResolveError = resolveError
	// A missing session is not a resolved target, even though the legacy message delivery
	// model retains its requested id in an attempt row. A live session that lacks a mode is
	// still resolved and participates in the event's session-level suppression calculation.
	// resolveDeliveryTarget writes this exact prefix for one answer only - the listener
	// holds no record under that session id - so the prefix test separates "no record",
	// whose requested id is kept on the attempt row, from a capability refusal. A read that
	// did not answer gives no session id here, so it sets no pin; an existing attempt keeps
	// the one it holds only through the coalesce(session_id, ...) update further down.
	if sessionID != "" && !strings.HasPrefix(resolveError, "no live session ") {
		item.SessionID = new(sessionID)
	}
	return item
}

// sendResolvedDelivery sends one already-resolved delivery and reports what the listener did:
// the envelope it minted, whether the stream already held the message, and the failure to
// record otherwise.
//
// Every failure comes with a nil envelope. The dashboard reads a failed attempt row that carries
// an envelope id as one its session answered with an error (rowAnsweredWithError in
// packages/dispatch/web/src/features/conversation/delivery.ts), which gets no same-mode Retry, so
// an envelope returned beside a failure would read as the session's answer
// (TestSendResolvedDeliveryReportsNoEnvelopeWithAnyFailure).
//
// A receipt timeout is worded differently from every other failure, and only here. The listener
// publishes the envelope before it answers, so a send whose answer missed the window may have
// landed. What the row records is only that CAUSE. Whether retrying is safe depends on how old
// the attempt is - a repeat is recognised only inside DELIVERY_DUPLICATE_WINDOW_MS, whose doc in
// packages/contracts states the promise - so that guidance is composed at render time against
// the attempt's age, never frozen into the row here. A row written today would otherwise still
// promise a safe retry next week.
func (s *server) sendResolvedDelivery(
	ctx context.Context, target ResolvedMention, body, idempotencyKey string, urgency *string, frame []byte,
) (*string, bool, string) {
	if target.ResolveError != "" {
		return nil, false, target.ResolveError
	}
	if target.SessionID == nil {
		return nil, false, "no live session"
	}
	expectsReply := "optional"
	if target.Delivery == "btw" {
		expectsReply = "required"
	}
	send, err := s.deps.Envoy.Send(ctx, dispatchenvoy.SendInput{
		TargetSession:  *target.SessionID,
		Message:        body,
		Payload:        frame,
		IdempotencyKey: idempotencyKey,
		Urgency:        deref(urgency),
		ExpectsReply:   expectsReply,
	})
	if errors.Is(err, dispatchenvoy.ErrReceiptTimeout) {
		return nil, false, receiptTimeoutText
	}
	if err != nil {
		return nil, false, deliveryErrorText(err)
	}
	// A duplicate names an envelope JetStream discarded, so the attempt records none.
	if send.Duplicate {
		return nil, true, ""
	}
	return &send.EnvelopeID, false, ""
}

// receiptTimeoutText is the CAUSE a receipt timeout records: what happened, and the one fact
// about it that never expires - the send may have landed. The advice that follows from it
// ("retrying in the same mode cannot deliver twice") is time-limited and belongs to the
// renderer, which has the attempt's age; see isSafeRetry in the dashboard's delivery module.
// The dashboard keys that wording on this exact string, so both sides read one literal.
const receiptTimeoutText = contracts.ReceiptTimeoutCause

// claimedCommentDelivery is the attempt one delivery call owns. settled is non-nil when that
// attempt already carries its outcome, belongs to a sender that is still working, or is one this
// call makes nothing of, and nothing is to be sent.
type claimedCommentDelivery struct {
	comment model.Comment
	settled *model.CommentDelivery
	// superseded says settled is the pending attempt this caller was bound to, overtaken by a
	// later attempt of the same mention that was sent: its caller settles it failed rather than
	// send it (supersedeCommentDelivery).
	superseded bool
	// mentionMode is the mode the comment recorded on the mention this attempt is of
	// (comment_mentions.delivery), read under the lock the claim transaction already takes on
	// that row. It is what keys the send (model.CommentMentionIdempotencyKey), whatever mode
	// this attempt itself goes in.
	mentionMode string
	attemptClaim
	// pending is the attempt row an earlier call committed and this one resumed, under its
	// original number and idempotency key. Its pinned session, delivery mode and resolve error
	// are the resolution that attempt was opened with - both nil for an attempt stranded before
	// anything was resolved - and a resumed pin is only rechecked, never re-routed.
	pending *model.CommentDelivery
}

// openCommentDeliveryAttempt records a fresh attempt as pending and claimed, before anything
// is resolved or sent. The row is what makes the attempt durable: a process that dies before
// the send leaves it pending rather than losing the attempt entirely. The comment's mention is
// locked, so the highest attempt cannot move between reading it and inserting beside it.
func openCommentDeliveryAttempt(
	ctx context.Context, tx pgx.Tx, comment model.Comment, target ResolvedMention,
) (attemptClaim, error) {
	var claim attemptClaim
	if err := tx.QueryRow(ctx, `
		insert into comment_deliveries (comment_id, target, attempt, delivery, state, claimed_at)
		values (
			$1, $2,
			(select coalesce(max(attempt), 0) + 1 from comment_deliveries where comment_id = $1 and target = $2),
			$3, 'pending', now()
		)
		returning attempt, claimed_at
	`, comment.ID, target.Target, target.Delivery).Scan(&claim.attempt, &claim.claimedAt); err != nil {
		return attemptClaim{}, err
	}
	return claim, nil
}

// claimCommentDeliveryAttempt is the first of the two transactions a delivery runs: it locks
// the comment and its mention, settles which attempt this call owns, and commits - all before
// the listener is called at all, so no connection is held across the send.
//
// resume.attempt > 0 binds the call to that attempt, as deliverMessageResuming binds a message's:
// it takes the attempt while it is pending and nobody else holds it, and otherwise sends nothing
// and reports the attempt as it stands (settled) - another sender's, or its outcome - and never
// opens an attempt, since beside a live sender that would be a second frame on the session's
// subject. Nobody else holds it when no sender does (model.DeliveryClaimFreeSQL) or when the
// claim on it is the caller's own (model.DeliveryClaimMineSQL): the mention sender's send binds
// to the attempt its comment committed, which this process has held claimed since
// (mentionSender), and so does a keyed repeat's (replayComment); the delivery sweep binds to the
// attempt it found owed, holding no claim of its own, so an older attempt a card retry overtook
// is never sent in that attempt's place.
//
// A later attempt of the same mention - a Retry from the card opened while this one waited on its
// lane (createCommentDelivery) - decides the bound attempt only once it is delivered: a Retry
// recorded failed is an outcome unknown, since a receipt timeout and a reply callback's error
// both leave an attempt failed whose frame the session may hold, so the bound attempt stands
// (model.MentionAttemptStandsSQL) and this call sends it, under its own number and the mention's
// one idempotency key (model.CommentMentionIdempotencyKey) - the key that Retry's own send went
// under, so a Retry that did land leaves this send a repeat the recipient's host drops. One that
// was sent handed the recipient the comment (model.MentionAttemptDeliveredLaterSQL), so this call
// reports the bound attempt superseded and its caller settles it failed
// (supersedeCommentDelivery), leaving the recipient one copy. While that later attempt is still
// pending neither holds, so this call stands down: it sends nothing, gives its claim back and
// leaves the attempt pending, for the delivery sweep to send once the later attempt failed
// (model.OwedMentionAttemptSQL) or for it to be settled superseded once that attempt is sent - by
// that attempt's own sender (supersedeOvertakenAttempts), or by the sweep
// (settleSupersededMentions). That is the one shape in which neither sending nor superseding
// settles anything: a supersede would lose the comment should the later attempt fail, and a send
// would put a frame on the wire for the host to drop as a repeat should it land, settling this
// attempt a duplicate before the outcome it is waiting on is known.
//
// A zero resume is a person's or an agent's Retry: it resumes the lowest pending attempt nobody
// holds, and otherwise opens one of its own.
func (s *server) claimCommentDeliveryAttempt(
	ctx context.Context, comment model.Comment, target ResolvedMention, resume attemptClaim,
) (claimedCommentDelivery, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return claimedCommentDelivery{}, err
	}
	defer tx.Rollback(ctx)
	stored, err := s.lockedComment(ctx, tx, comment.ID)
	if err != nil {
		return claimedCommentDelivery{}, err
	}
	// The mention's own mode comes from the row this lock is taken on, so the send is keyed by it
	// without a read of its own (model.CommentMentionIdempotencyKey).
	claimed := claimedCommentDelivery{comment: stored}
	if err := tx.QueryRow(ctx, `
		select delivery from comment_mentions where comment_id = $1 and target = $2 for update
	`, stored.ID, target.Target).Scan(&claimed.mentionMode); err != nil {
		return claimedCommentDelivery{}, err
	}
	// A bound call reads its own attempt, in whatever state; an unbound one the lowest pending.
	var takeable bool
	if resume.attempt > 0 {
		var stands, delivered bool
		bound, err := scanCommentDelivery(tx.QueryRow(ctx, `
			select `+commentDeliveryColumns+`, `+model.DeliveryClaimFreeSQL+` or `+model.DeliveryClaimMineSQL("$4")+`,
			       `+model.MentionAttemptStandsSQL("comment_deliveries")+`,
			       `+model.MentionAttemptDeliveredLaterSQL("comment_deliveries")+`
			from comment_deliveries
			where comment_id = $1 and target = $2 and attempt = $3
			for update
		`, stored.ID, target.Target, resume.attempt, resume.claimedAt), &takeable, &stands, &delivered)
		if err != nil {
			return claimedCommentDelivery{}, fmt.Errorf(
				"read attempt %d of comment delivery %s/%s: %w", resume.attempt, stored.ID, target.Target, err)
		}
		switch {
		case bound.State != "pending" || !takeable:
			// Answered or failed already - a reply consumed it while this sender was on its way,
			// or the outbox settled it abandoned and published the session's copy instead
			// (attempt.leftTo in internal/dispatch/outbox) - or held by another live sender, who
			// owns the outcome.
			claimed.settled = &bound
		case delivered:
			// A later attempt of this mention was sent: this one sends nothing and its caller
			// settles it failed, giving its claim back.
			claimed.settled, claimed.superseded = &bound, true
		case !stands:
			// A later attempt is still being sent, and whether it delivers the comment decides
			// this one. Stand down: give the claim back and leave the attempt pending.
			if err := claimed.standDown(ctx, tx, &bound, resume); err != nil {
				return claimedCommentDelivery{}, err
			}
			claimed.settled = &bound
		default:
			if err := claimed.take(ctx, tx, &bound); err != nil {
				return claimedCommentDelivery{}, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return claimedCommentDelivery{}, err
		}
		return claimed, nil
	}
	pending, err := scanCommentDelivery(tx.QueryRow(ctx, `
		select `+commentDeliveryColumns+`, `+model.DeliveryClaimFreeSQL+`
		from comment_deliveries
		where comment_id = $1 and target = $2 and state = 'pending'
		order by attempt
		limit 1
		for update
	`, stored.ID, target.Target), &takeable)
	switch {
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return claimedCommentDelivery{}, err
	case err == nil && takeable:
		// A Retry that asks for another mode resumes this attempt in the mode its row pinned
		// rather than the one asked for: a resumed attempt keeps the resolution it was opened
		// with, mode included (mentionResolutionFor), and the mention's idempotency key carries
		// the mention's own mode whatever an attempt's, so the mode decides only how the frame
		// is presented and the recipient is handed one copy either way. A send in the other mode
		// is what a Retry opened once this attempt has settled makes.
		if err := claimed.take(ctx, tx, &pending); err != nil {
			return claimedCommentDelivery{}, err
		}
	default:
		// No attempt is pending, or a live sender holds the one that is, so this retry gets one of
		// its own rather than two senders driving the same attempt.
		if claimed.attemptClaim, err = openCommentDeliveryAttempt(ctx, tx, stored, target); err != nil {
			return claimedCommentDelivery{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return claimedCommentDelivery{}, err
	}
	return claimed, nil
}

// take claims pending, an attempt nobody else holds - never claimed, claimed by a sender that
// never came back, or claimed by this caller while the send waited on its lane - under its
// original number, so the listener deduplicates a send that did land. The claim it writes is this
// send's own: every statement of the settle is scoped to it.
func (claimed *claimedCommentDelivery) take(ctx context.Context, tx pgx.Tx, pending *model.CommentDelivery) error {
	if err := tx.QueryRow(ctx, `
		update comment_deliveries set claimed_at = now()
		where comment_id = $1 and target = $2 and attempt = $3
		returning claimed_at
	`, pending.CommentID, pending.Target, pending.Attempt).Scan(&claimed.claimedAt); err != nil {
		return err
	}
	claimed.attempt = pending.Attempt
	claimed.pending = pending
	return nil
}

// standDown gives back the claim this caller held on pending and leaves the attempt as it is:
// pending, unclaimed and unsent, because a later attempt of its mention is in flight and only its
// outcome decides this one (claimCommentDeliveryAttempt). The statement is scoped as the claim
// transaction's read was - the claim is given back only while it is this caller's own, or already
// nobody's - so an attempt another sender has taken since keeps that sender's claim. Giving it
// back is what hands the attempt on: the delivery sweep sends it once the later attempt failed,
// and once that attempt is sent it is settled superseded - by that attempt's own sender
// (supersedeOvertakenAttempts), or by the sweep should that settle not happen
// (settleSupersededMentions) - and none of them waits out this caller's lease first.
func (claimed *claimedCommentDelivery) standDown(
	ctx context.Context, tx pgx.Tx, pending *model.CommentDelivery, resume attemptClaim,
) error {
	_, err := tx.Exec(ctx, `
		update comment_deliveries set claimed_at = null
		where comment_id = $1 and target = $2 and attempt = $3 and state = 'pending'
		  and (`+model.DeliveryClaimFreeSQL+` or `+model.DeliveryClaimMineSQL("$4")+`)
	`, pending.CommentID, pending.Target, pending.Attempt, resume.claimedAt)
	return err
}

// commentDeliveryReceipt is the comment.delivery receipt an attempt row owes, read off that
// row: what was delivered, to whom, how it ended, and the reply that ended it. Both the sender
// settling an attempt a reply already answered and replyComment's own error branch append
// exactly this.
func commentDeliveryReceipt(
	comment model.Comment, actor model.Actor, attempt model.CommentDelivery,
) model.Event {
	return ownerOf(comment.IssueKey, comment.ArtifactID).event(
		"comment.delivery",
		actor,
		model.CommentDeliveryEventPayload{
			CommentID: comment.ID, AskID: comment.AskID, Target: attempt.Target,
			Attempt: attempt.Attempt, Delivery: attempt.Delivery, SessionID: attempt.SessionID,
			State: attempt.State, Duplicate: attempt.Duplicate, Error: receiptError(attempt.Error),
			ReplyID: attempt.ReplyID,
		},
	)
}

// completeCommentDelivery is the settle transaction of a comment mention's delivery: it records
// what the send did on the attempt this sender claimed and appends the comment.delivery
// receipt, holding no listener call. recorded is the row recordCommentDeliveryResolution left
// before the send, so this receipt states what that row says - the session holding the frame
// included - rather than what the resolution behind the send happened to find. A failed settle
// changes nothing of the comment's comment.created event: a copy the outbox held back for this
// mention is the outbox's fallback pass's (publishFallbackCopies in internal/dispatch/outbox).
func (s *server) completeCommentDelivery(
	ctx context.Context,
	comment model.Comment,
	recorded model.CommentDelivery,
	actor model.Actor,
	claim attemptClaim,
	envelopeID *string,
	duplicate bool,
	deliveryError string,
) (model.CommentDelivery, error) {
	sent := recorded
	sent.State, sent.Error = deliveryOutcome(deliveryError)
	sent.Duplicate, sent.EnvelopeID = duplicate, envelopeID
	return settleDeliveryAttempt(ctx, s, commentDeliveryReceipt(comment, actor, sent),
		func(ctx context.Context, tx pgx.Tx) (model.CommentDelivery, error) {
			return scanCommentDelivery(tx.QueryRow(ctx, `
				update comment_deliveries set envelope_id = $5, state = $6, error = $7, duplicate = $8
				where comment_id = $1 and target = $2 and attempt = $3
				  and state = 'pending' and claimed_at = $4
				returning `+commentDeliveryColumns,
				comment.ID, sent.Target, claim.attempt, claim.claimedAt, envelopeID, sent.State, sent.Error, duplicate,
			))
		},
		func(ctx context.Context, tx pgx.Tx) (model.CommentDelivery, bool, *string, error) {
			var mine bool
			answered, err := scanCommentDelivery(tx.QueryRow(ctx, `
				select `+commentDeliveryColumns+`, claimed_at is not distinct from $4
				from comment_deliveries
				where comment_id = $1 and target = $2 and attempt = $3
			`, comment.ID, sent.Target, claim.attempt, claim.claimedAt), &mine)
			return answered, mine, answered.ReplyID, err
		},
		func(answered model.CommentDelivery) model.Event {
			return commentDeliveryReceipt(comment, actor, answered)
		},
	)
}

// mentionSupersededError is the cause an attempt is settled with when a later attempt of the same
// mention delivered the comment before this one was sent (supersedeCommentDelivery).
const mentionSupersededError = "not sent: a later attempt of this mention superseded it"

// supersedeCommentDelivery settles failed, with mentionSupersededError, a pending attempt a later
// attempt of the same mention has delivered the comment in the place of - a Retry from the card
// opened while this attempt waited on its lane, and sent - and gives its claim back, so nothing
// sends it and the recipient gets the later attempt's copy alone. Its caller is the sender bound
// to that attempt, when its lane reaches an attempt already overtaken
// (claimCommentDeliveryAttempt), the sender of the later attempt itself, for an attempt a lane
// stood down from while that send was in flight (supersedeOvertakenAttempts), or the delivery
// sweep, for one left pending and claim-free because neither of those settled it
// (settleSupersededMentions). It runs the settle transaction every outcome of a mention goes
// through (settleDeliveryAttempt), so this attempt carries its comment.delivery receipt like any
// other. Its statement is guarded as the claim
// transaction's read was, under the comment's owner lock, and on a later attempt having been sent
// rather than merely opened: an attempt another sender has taken since, one already settled, and
// one whose overtaking attempt has not delivered the comment are left as they are and reported as
// they stand.
func (s *server) supersedeCommentDelivery(
	ctx context.Context, comment model.Comment, bound model.CommentDelivery, actor model.Actor, claim attemptClaim,
) (model.CommentDelivery, error) {
	failed := bound
	failed.State, failed.Error = deliveryOutcome(mentionSupersededError)
	return settleDeliveryAttempt(ctx, s, commentDeliveryReceipt(comment, actor, failed),
		func(ctx context.Context, tx pgx.Tx) (model.CommentDelivery, error) {
			return scanCommentDelivery(tx.QueryRow(ctx, `
				update comment_deliveries set state = 'failed', error = $5, claimed_at = null
				where comment_id = $1 and target = $2 and attempt = $3 and state = 'pending'
				  and (`+model.DeliveryClaimFreeSQL+` or `+model.DeliveryClaimMineSQL("$4")+`)
				  and `+model.MentionAttemptDeliveredLaterSQL("comment_deliveries")+`
				returning `+commentDeliveryColumns,
				comment.ID, bound.Target, claim.attempt, claim.claimedAt, mentionSupersededError,
			))
		},
		func(ctx context.Context, tx pgx.Tx) (model.CommentDelivery, bool, *string, error) {
			var mine bool
			answered, err := scanCommentDelivery(tx.QueryRow(ctx, `
				select `+commentDeliveryColumns+`, claimed_at is not distinct from $4
				from comment_deliveries
				where comment_id = $1 and target = $2 and attempt = $3
			`, comment.ID, bound.Target, claim.attempt, claim.claimedAt), &mine)
			return answered, mine, answered.ReplyID, err
		},
		func(answered model.CommentDelivery) model.Event {
			return commentDeliveryReceipt(comment, actor, answered)
		},
	)
}

// mentionResolutionFor is the resolution one mention delivery is sent with, and the resolve
// error its attempt row records.
//
// A resumed attempt carries the resolution it was opened with, in its pinned session or its
// resolve error, and its first try keeps it: the routing decision - which session this attempt
// targets - stays fixed, exactly as resolveMentionTargets already guarantees against a role
// transition redirecting an in-flight delivery. What a take-over retry of that try resolves is
// deliverResolvedCommentMention's to say: a pinned session is rechecked again, and a resolve error
// is kept for good only on the attempt a comment's other routes were decided on. An attempt
// stranded before either was recorded - the claim transaction commits the row before anything is
// resolved, so a sender that never returns leaves both NULL - has no resolution to keep: it is
// resolved now, under its own attempt number and idempotency key, rather than reported "no live
// session" without the listener ever being called.
//
// A pinned session is rechecked. The resolution behind the attempt was made before the
// comment-creation transaction committed (or, for a genuine retry, at an even earlier
// comment-creation moment) while the send always happens after that commit, whether this is the
// mention sender's send of a fresh comment or a much later retry, and a session's
// advertised capabilities and liveness are live state that changes on every registration. So
// both callers recheck, not just retries - only whether that pinned session can still receive
// this delivery mode right now, through the same resolveMentionTarget/resolveDeliveryTarget
// every other send uses, resolving it as a direct session target so no role lookup (and no
// chance of re-picking a different holder) is involved.
func (s *server) mentionResolutionFor(
	ctx context.Context, target ResolvedMention, pending *model.CommentDelivery,
) (ResolvedMention, *string) {
	if pending == nil || (pending.SessionID == nil && pending.ResolveError == nil) {
		resolved := s.resolveMentionTarget(ctx, target.Target, target.Delivery, 0)
		if resolved.ResolveError == "" {
			return resolved, nil
		}
		return resolved, &resolved.ResolveError
	}
	resumed := ResolvedMention{Target: target.Target, Delivery: pending.Delivery, SessionID: pending.SessionID}
	if pending.ResolveError != nil {
		resumed.ResolveError = *pending.ResolveError
	}
	if pending.SessionID != nil {
		recheck := s.resolveMentionTarget(ctx, "session:"+*pending.SessionID, pending.Delivery, 0)
		resumed.SessionID = recheck.SessionID
		resumed.ResolveError = recheck.ResolveError
		resumed.attemptSessionID = recheck.attemptSessionID
		resumed.title = recheck.title
	}
	return resumed, pending.ResolveError
}

// recordCommentDeliveryResolution writes onto the claimed attempt row, before a send, the
// resolution that send is made with, renews the claim, and returns the row that write left and
// the claim renewed. A pending attempt names the session its frame is going to, so the mentioned
// session can answer that frame while it is still in flight and can still answer it after the
// sender dies without settling - the same rule the message path gets by resolving before it
// claims. The comment path cannot resolve before it claims, because a resumed attempt's pin is
// only known under the mention's lock, so it records the resolution in a statement of its own.
// That statement is scoped to this sender's claim, like both statements of the settle
// transaction: an attempt another sender has since resumed keeps what that sender recorded, and
// this sender reports the resolution it made without writing it anywhere, with held = false and
// the claim it held. The claim moves with every such write, so a sender trying again through a
// listener take-over (takeoverRetry.retry) renews it before each try and settles on the latest.
//
// A session already pinned is kept, the coalesce recordPendingMessageDelivery makes on the
// message twin, spelled once here in SQL too. The recheck behind a resumed
// attempt asks only whether that same session can still receive this mode, so it can return
// that session or nothing at all: a listener that cannot answer - the outage this shape exists
// for - must not un-name the session holding the frame, or that session's reply is refused for
// the life of the comment.
func (s *server) recordCommentDeliveryResolution(
	ctx context.Context,
	comment model.Comment,
	target ResolvedMention,
	claim attemptClaim,
	resolveError *string,
) (model.CommentDelivery, attemptClaim, bool, error) {
	renewed := claim
	// In a transaction of its own, as every write a keyed request makes is, so the request's
	// commit mark hooks it (markRequestKeyCommitted).
	tx, err := s.begin(ctx)
	if err != nil {
		return model.CommentDelivery{}, claim, false, err
	}
	defer tx.Rollback(ctx)
	recorded, err := scanCommentDelivery(tx.QueryRow(ctx, `
		update comment_deliveries
		set claimed_at = now(), delivery = $5, session_id = coalesce(session_id, $6), resolve_error = $7
		where comment_id = $1 and target = $2 and attempt = $3
		  and state = 'pending' and claimed_at = $4
		returning `+commentDeliveryColumns+`, claimed_at`,
		comment.ID, target.Target, claim.attempt, claim.claimedAt, target.Delivery, target.SessionID, resolveError,
	), &renewed.claimedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.CommentDelivery{
			CommentID: comment.ID, Target: target.Target, Attempt: claim.attempt,
			Delivery: target.Delivery, SessionID: target.SessionID,
		}, claim, false, nil
	}
	if err != nil {
		return model.CommentDelivery{}, claim, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.CommentDelivery{}, claim, false, err
	}
	return recorded, renewed, true, nil
}

// deliverResolvedCommentMention delivers one mention of a comment: it claims an attempt in one
// short transaction, resolves with nothing held, records that resolution on the claimed row,
// sends, then records the outcome in a second transaction, and settles behind it every earlier
// attempt its send superseded (supersedeOvertakenAttempts). The listener never runs inside a
// transaction.
//
// resume binds the claim (claimCommentDeliveryAttempt): the attempt the mention sender's lane was
// queued for and the claim this process has held on it since its comment committed, an owed
// attempt's number for the delivery sweep, and nothing for a Retry. takeover is the pacing the
// sends start in; every caller starts a window of its own, since a mention's sends run after the
// comment's response rather than inside it.
//
// A send that meets a listener take-over tries again under the attempt it claimed
// (takeoverRetry.retry): it resolves again only the recipient the attempt names - the session its
// row is pinned to, rechecked as mentionResolutionFor rechecks it, or the mention's own target for a
// row no session is pinned to yet - records that resolution and renews its claim, and sends under
// the same key, until a try ends otherwise or the window closes; the attempt settles with the last
// try's outcome.
//
// Attempt 1 of a comment whose other routes were decided on its mentions' resolution
// (mentions_decide_routes, resolveCommentDeliveries) keeps the resolution it was opened with,
// whichever sender finishes it - the mention sender's lane, the delivery sweep, or a Retry that
// takes it after its sender died. A pinned session is only rechecked, and a resolution that
// reached no session settles failed with its resolve error, never resolved again: the comment's
// route and author fan-outs went out on that outcome, so a session this mention reached now could
// hear the comment from them too, under a key the listener cannot recognise as a repeat of
// theirs. A row that recorded neither a session nor an error - a lookup the listener did not
// answer in time (resolveMentionTarget), whose session route the outbox holds back instead
// (publishFallbackCopies in internal/dispatch/outbox) - has no resolution to keep and is resolved
// as any other.
func (s *server) deliverResolvedCommentMention(
	ctx context.Context,
	comment model.Comment,
	created model.Event,
	target ResolvedMention,
	actor model.Actor,
	resume attemptClaim,
	takeover takeoverRetry,
) (model.CommentDelivery, error) {
	claimed, err := s.claimCommentDeliveryAttempt(ctx, comment, target, resume)
	if err != nil {
		return model.CommentDelivery{}, err
	}
	if claimed.settled != nil {
		if claimed.superseded {
			return s.supersedeCommentDelivery(ctx, claimed.comment, *claimed.settled, actor, resume)
		}
		return *claimed.settled, nil
	}
	stored := claimed.comment
	decided := claimed.pending != nil && claimed.pending.Attempt == 1 &&
		(claimed.pending.SessionID != nil || claimed.pending.ResolveError != nil) && mentionsDecideRoutes(created)
	target, resolveError := s.mentionResolutionFor(ctx, target, claimed.pending)
	recorded, held, _, err := s.recordCommentDeliveryResolution(ctx, stored, target, claimed.attemptClaim, resolveError)
	if err != nil {
		return model.CommentDelivery{}, err
	}
	frame, err := json.Marshal(struct {
		Event    model.Event `json:"event"`
		Delivery any         `json:"delivery"`
	}{
		Event: created,
		Delivery: map[string]any{
			"attempt":    claimed.attempt,
			"mode":       target.Delivery,
			"comment_id": stored.ID,
			"target":     target.Target,
		},
	})
	if err != nil {
		return model.CommentDelivery{}, fmt.Errorf("encode comment mention delivery frame: %w", err)
	}
	// The key is the mention's: its target and the mode the comment recorded on it, never the
	// mode of this attempt (model.CommentMentionIdempotencyKey), so a second frame of this
	// mention repeats the key of the first whichever modes the two attempts went in and the
	// recipient's host drops it - and so does every try this send makes again through a listener
	// take-over.
	// When the issue route's copy is suppressed for this send: model.CommentEventPayload.SuppressRoute.
	key := model.CommentMentionIdempotencyKey(stored.ID, target.Target, claimed.mentionMode)
	var envelopeID *string
	var duplicate bool
	send := func() string {
		var failure string
		envelopeID, duplicate, failure = s.sendResolvedDelivery(ctx, target, stored.Body, key, nil, frame)
		return failure
	}
	var deliveryError string
	if decided && claimed.pending.SessionID == nil {
		// The resolution the comment's routes were decided on reached no session; its resolve
		// error is this attempt's outcome, with nothing to try again.
		deliveryError = send()
	} else {
		deliveryError, err = takeover.retry(ctx, send, func() (bool, error) {
			// A pinned row is rechecked; with no session pinned yet the recipient is still the
			// mention's target, resolved afresh rather than sent the resolve error the row holds.
			var pinned *model.CommentDelivery
			if recorded.SessionID != nil {
				pinned = &recorded
			}
			var renewed bool
			var err error
			target, resolveError = s.mentionResolutionFor(ctx, target, pinned)
			recorded, held, renewed, err = s.recordCommentDeliveryResolution(ctx, stored, target, held, resolveError)
			return renewed, err
		})
		if err != nil {
			return model.CommentDelivery{}, err
		}
	}
	settled, err := s.completeCommentDelivery(
		ctx, stored, recorded, actor, held, envelopeID, duplicate, deliveryError,
	)
	if err != nil {
		return model.CommentDelivery{}, err
	}
	if err := s.supersedeOvertakenAttempts(ctx, stored, settled, actor); err != nil {
		// The attempt stays pending, claim-free and sent by nobody: a later attempt of its mention
		// was sent, and the delivery sweep owes only the attempts that stand
		// (model.OwedMentionAttemptSQL). So no copy is delivered twice and none is lost, and the
		// next delivery sweep settles it superseded (settleSupersededMentions).
		slog.Error("dispatch: settle the attempts a comment mention's send superseded",
			"comment", stored.ID, "target", settled.Target, "attempt", settled.Attempt, "error", err)
	}
	return settled, nil
}

// supersedeOvertakenAttempts settles superseded every earlier attempt of sent's mention that this
// send has delivered the comment in the place of and no sender holds: the attempt a lane stood
// down from because this one was in flight (claimCommentDeliveryAttempt), which the delivery
// sweep sends no more than any other overtaken attempt. A lane that reaches such an attempt after
// this send has landed settles it itself, under the same guard, and the sweep settles one neither
// of them did (settleSupersededMentions), so each is settled once and by whichever comes first.
//
// It costs a read only when this send settled an attempt past the first, which is the only way an
// earlier attempt can exist, and nothing at all when that send delivered nothing: a failed
// attempt supersedes none, and the attempt it overtook stands and is sent again
// (model.MentionAttemptStandsSQL), under the same key this send went under.
func (s *server) supersedeOvertakenAttempts(
	ctx context.Context, comment model.Comment, sent model.CommentDelivery, actor model.Actor,
) error {
	if sent.State != "sent" || sent.Attempt < 2 {
		return nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select `+commentDeliveryColumns+`
		from comment_deliveries
		where comment_id = $1 and target = $2 and attempt < $3
		  and state = 'pending' and `+model.DeliveryClaimFreeSQL+`
		order by attempt
	`, comment.ID, sent.Target, sent.Attempt)
	if err != nil {
		return err
	}
	overtaken, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.CommentDelivery, error) {
		return scanCommentDelivery(row)
	})
	if err != nil {
		return err
	}
	for _, attempt := range overtaken {
		if _, err := s.supersedeCommentDelivery(
			ctx, comment, attempt, actor, attemptClaim{attempt: attempt.Attempt},
		); err != nil {
			return err
		}
	}
	return nil
}

// mentionsDecideRoutes reports whether a comment.created event records that the comment's other
// routes were decided on its mentions' resolution (model.CommentEventPayload.MentionsDecideRoutes):
// the creating request holds the payload it appended, every later sender the one
// loadCommentCreatedEvent decoded. An event written before the field was is read as false.
func mentionsDecideRoutes(created model.Event) bool {
	switch payload := created.Payload.(type) {
	case model.CommentEventPayload:
		return payload.MentionsDecideRoutes
	case map[string]any:
		decided, _ := payload["mentions_decide_routes"].(bool)
		return decided
	}
	return false
}

// loadCommentCreatedEvent reads comment's comment.created event within the comment's owner, so
// the read walks that owner's events by its (issue_key, seq) or (artifact_id, seq) index instead
// of matching the payload id across the whole log. A comment has one comment.created event, and
// both callers ask about recent comments, so the walk starts from the owner's newest event.
func (s *server) loadCommentCreatedEvent(ctx context.Context, q queryer, comment model.Comment) (model.Event, error) {
	ownerColumn, ownerID := commentEventOwner(comment)
	var event model.Event
	var actor, payload []byte
	if err := q.QueryRow(ctx, `
		select id, issue_key, artifact_id::text, coalesce(project_key, ''), seq, type, actor, notify, created_at, payload
		from events
		where `+ownerColumn+` = $1 and type = 'comment.created' and payload ->> 'id' = $2
		order by seq desc
		limit 1
	`, ownerID, comment.ID).Scan(
		&event.ID, &event.IssueKey, &event.ArtifactID, &event.Project, &event.Seq, &event.Type,
		&actor, &event.Notify, &event.CreatedAt, &payload,
	); err != nil {
		return model.Event{}, err
	}
	if err := json.Unmarshal(actor, &event.Actor); err != nil {
		return model.Event{}, fmt.Errorf("decode comment-created event actor: %w", err)
	}
	if err := json.Unmarshal(payload, &event.Payload); err != nil {
		return model.Event{}, fmt.Errorf("decode comment-created event payload: %w", err)
	}
	return event, nil
}

// commentEventOwner is the events column and value naming comment's owner, so a read of its
// events walks that owner's (issue_key, seq) or (artifact_id, seq) index.
func commentEventOwner(comment model.Comment) (column, id string) {
	switch {
	case comment.IssueKey != nil:
		return "issue_key", *comment.IssueKey
	case comment.ArtifactID != nil:
		return "artifact_id", *comment.ArtifactID
	}
	return "issue_key", ""
}

// commentDeliveryColumns is the comment_deliveries select list scanCommentDelivery reads, in
// scan order.
const commentDeliveryColumns = `comment_id::text, target, attempt, delivery, session_id, envelope_id, duplicate, state, error, resolve_error, reply_id::text, created_at`

// scanCommentDelivery decodes one commentDeliveryColumns row; extra receives any columns
// selected after them.
func scanCommentDelivery(row pgx.Row, extra ...any) (model.CommentDelivery, error) {
	var attempt model.CommentDelivery
	fields := []any{
		&attempt.CommentID, &attempt.Target, &attempt.Attempt, &attempt.Delivery, &attempt.SessionID,
		&attempt.EnvelopeID, &attempt.Duplicate, &attempt.State, &attempt.Error, &attempt.ResolveError,
		&attempt.ReplyID, &attempt.CreatedAt,
	}
	if err := row.Scan(append(fields, extra...)...); err != nil {
		return model.CommentDelivery{}, err
	}
	return attempt, nil
}

func loadCommentDelivery(ctx context.Context, q queryer, commentID, target string, attemptNumber int) (model.CommentDelivery, error) {
	return scanCommentDelivery(q.QueryRow(ctx, `
		select `+commentDeliveryColumns+`
		from comment_deliveries
		where comment_id = $1 and target = $2 and attempt = $3
	`, commentID, target, attemptNumber))
}

func (s *server) createCommentDelivery(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	var input struct {
		Actor    *model.Actor `json:"actor"`
		Target   *string      `json:"target"`
		Delivery string       `json:"delivery"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !human {
		if actor, err = bearerSessionActor(r, actor, input.Actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if !validDelivery(input.Delivery) {
		writeError(w, "MENTION_INPUT", http.StatusBadRequest, "delivery must be one of btw, aside, steer")
		return
	}
	comment, err := s.loadComment(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		writeError(w, "COMMENT_NOT_FOUND", http.StatusNotFound, "comment not found")
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select target from comment_mentions where comment_id = $1 order by target
	`, comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	targets := []string{}
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows.Close()
	if len(targets) == 0 {
		writeError(w, "COMMENT_NOT_FOUND", http.StatusNotFound, "comment has no mentions")
		return
	}
	target := ""
	if input.Target == nil {
		if len(targets) != 1 {
			writeError(w, "MENTION_INPUT", http.StatusBadRequest, "target is required when a comment has multiple mentions")
			return
		}
		target = targets[0]
	} else {
		target = *input.Target
		found := false
		for _, candidate := range targets {
			if candidate == target {
				found = true
				break
			}
		}
		if !found {
			writeError(w, "MENTION_INPUT", http.StatusBadRequest, "target must name one of the comment mentions")
			return
		}
	}
	created, err := s.loadCommentCreatedEvent(r.Context(), s.deps.Store.Pool, comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	resolved := ResolvedMention{Target: target, Delivery: input.Delivery}
	// The send outlives the request's connection, as createStoredMessage's does: the attempt is
	// committed before the listener call, a cut connection must not strand it, and one a process
	// death strands the delivery sweep resumes.
	attempt, err := s.deliverResolvedCommentMention(
		context.WithoutCancel(r.Context()), comment, created, resolved, actor, attemptClaim{}, s.newTakeoverRetry(),
	)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, attempt)
}

func (s *server) replyComment(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "reply requires an agent bearer token")
		return
	}
	var input struct {
		Actor   *model.Actor `json:"actor"`
		Target  *string      `json:"target"`
		Attempt int          `json:"attempt"`
		Body    *string      `json:"body"`
		Error   *string      `json:"error"`
		// ReplyToBlock is the id of the block of comment {id} the reply answers ("b2").
		ReplyToBlock *string `json:"reply_to_block"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if actor.Kind != "session" || input.Attempt < 1 || (input.Body == nil) == (input.Error == nil) {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "reply requires one body or error for a delivery attempt")
		return
	}
	if input.Body != nil && strings.TrimSpace(*input.Body) == "" {
		writeError(w, "MENTION_INPUT", http.StatusBadRequest, "comment body is required")
		return
	}
	if input.ReplyToBlock != nil && input.Body == nil {
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "reply_to_block names the block a reply's body answers; an error report has none")
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	comment, err := s.lockedComment(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "COMMENT_NOT_FOUND", http.StatusNotFound, "comment not found")
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	rows, err := tx.Query(r.Context(), `
		select target from comment_mentions where comment_id = $1 order by target for update
	`, comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	targets := []string{}
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			rows.Close()
			s.writeHandlerError(w, err)
			return
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.writeHandlerError(w, err)
		return
	}
	rows.Close()
	target := ""
	if input.Target == nil {
		if len(targets) != 1 {
			writeError(w, "MENTION_INPUT", http.StatusBadRequest, "target is required when a comment has multiple mentions")
			return
		}
		target = targets[0]
	} else {
		target = *input.Target
		found := false
		for _, candidate := range targets {
			if candidate == target {
				found = true
				break
			}
		}
		if !found {
			writeError(w, "MENTION_INPUT", http.StatusBadRequest, "target must name one of the comment mentions")
			return
		}
	}

	attempt, err := scanCommentDelivery(tx.QueryRow(r.Context(), `
		select `+commentDeliveryColumns+`
		from comment_deliveries where comment_id = $1 and target = $2 and attempt = $3 for update
	`, comment.ID, target, input.Attempt))
	if err != nil {
		writeError(w, "COMMENT_NOT_FOUND", http.StatusNotFound, "comment delivery not found")
		return
	}
	if attempt.SessionID == nil || *attempt.SessionID != actor.ID {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "session may reply only to its own delivery")
		return
	}
	// A replay of a callback that already posted its reply writes nothing and answers the stored
	// reply as it is, with no ask_waiting_on: that names whom the ask waits on once a reply is its
	// newest, which a replayed reply need no longer be.
	if attempt.ReplyID != nil {
		reply, err := s.loadComment(r.Context(), tx, *attempt.ReplyID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := hydrateCommentReplyBlocks(r.Context(), tx, &reply); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, reply)
		return
	}
	if attempt.State == "failed" && input.Error != nil {
		if err := tx.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, attempt)
		return
	}
	if input.Error != nil {
		updated, err := scanCommentDelivery(tx.QueryRow(r.Context(), `
			update comment_deliveries
			set state = 'failed', error = $4
			where comment_id = $1 and target = $2 and attempt = $3
			returning `+commentDeliveryColumns,
			comment.ID, attempt.Target, input.Attempt, *input.Error,
		))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		attempt = updated
		event, err := s.appendEvent(r.Context(), tx, commentDeliveryReceipt(comment, actor, attempt))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		s.publish(event)
		WriteJSON(w, http.StatusOK, attempt)
		return
	}
	author, err := json.Marshal(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A callback reply joins its thread exactly as `POST .../comments` would: the shared climb
	// walks `reply_to` up to the head of the thread, so a reply anywhere under an ask stores
	// that ask's `ask_id` - the column every ask read, the Inbox row and the ask card select
	// on - instead of a bare `reply_to` none of them can see. The comment it answers is
	// already loaded and locked, so the climb starts there rather than re-reading it.
	thread, err := s.threadHeadOf(r.Context(), tx, ownerOf(comment.IssueKey, comment.ArtifactID), comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A block the reply names is one of the comment it answers, which lockedComment holds after
	// its owner.
	var block *namedBlock
	if input.ReplyToBlock != nil {
		if thread.AskID != nil {
			writeError(w, "INVALID_COMMENT", http.StatusBadRequest, replyBlockInAskThread)
			return
		}
		block, err = s.replyBlockOf(r.Context(), tx, refs.ItemComment, comment.ID, *input.ReplyToBlock, "reply_to_block", "INVALID_COMMENT")
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// A callback reply requests no turn, so it hands the turn back the way any session reply
	// with no `turn` does.
	turn := thread.replyTurn(actor, nil)
	reply, err := scanComment(tx.QueryRow(r.Context(), `
		insert into comments (issue_key, artifact_id, author, body, reply_to, ask_id, turn, replies_to_block)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		returning `+commentColumns+`
	`, comment.IssueKey, comment.ArtifactID, author, *input.Body, thread.ReplyTo, thread.AskID, turn, block.nodeID()))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	waitingOn := thread.waitingOnAfter(turn)
	if thread.AskID != nil {
		if err := asks.FollowAuthor(r.Context(), tx, *thread.AskID, actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	referenceChanges, err := refs.ReplaceCountedItem(r.Context(), tx, refs.ItemComment, reply.ID, reply.Body, s.deps.ServerURL, reply.CreatedAt)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		update comment_deliveries set reply_id = $4, state = 'sent', error = null
		where comment_id = $1 and target = $2 and attempt = $3
	`, comment.ID, attempt.Target, input.Attempt, reply.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	artifactName, err := s.commentArtifactName(r.Context(), tx, reply)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	payload, err := s.commentEventPayload(
		r.Context(), tx, &reply, artifactName, thread.eventThread(waitingOn), referenceChanges,
	)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	payload.ThreadLogins, err = commentThreadLogins(r.Context(), tx, reply)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, ownerOf(reply.IssueKey, reply.ArtifactID).event(
		"comment.answered", actor, payload,
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := refs.Stamp(r.Context(), tx, "comment", reply.ID, event.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusCreated, commentWriteResponse{Comment: reply, AskWaitingOn: waitingOn})
}
