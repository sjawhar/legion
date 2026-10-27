package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// DeliveryResumeInterval is how often RunDeliveryResume looks for stranded attempts. An attempt
// is a candidate a minute after its last claim (the claim lease), so a process that stops while
// it sends has its attempts sent by another within about seventy seconds.
const DeliveryResumeInterval = 10 * time.Second

// deliveryResumeLease names the lease one pass runs under. Two passes at once would be correct -
// every send is a claim that only one sender wins - but each would read the same candidates and
// lose most of its claims, which is reason enough for one process to make the pass.
const deliveryResumeLease = "delivery-resume"

// deliveryResumeBatch bounds the attempts one pass reads from each table, oldest first, and
// deliveryResumeSenders how many of them it sends at once. A try is bounded by the listener
// client's five-second window on its resolution and on its send, and a send that meets a listener
// take-over keeps trying for up to listenerTakeoverWindow (12 s) from its first try, so one send
// takes up to about twenty seconds, and a full batch through a listener that stays down for the
// whole pass over half an hour at worst; the lease is held for the whole pass, so the next pass,
// on whichever process, starts after it.
const (
	deliveryResumeBatch   = 200
	deliveryResumeSenders = 4
)

// strandedAttempt is the predicate a resume candidate meets, on an attempt table aliased d: still
// pending; its last claim older than the claim lease (claimLapsed's minute), or, never claimed,
// the row itself older than that, so the request that opened the row is never raced for its own
// fresh attempt; and created inside the resume horizon, one hour. Inside that horizon a resend is
// provably not a second delivery, since the listener recognises the attempt's key as a repeat for
// contracts.DeliveryDuplicateWindow (72 hours). It is an hour rather than 72 because the case the
// loop exists for is one process stopping while others run, which the loop answers within about
// seventy seconds; an attempt older than an hour means every process was down that long, and a
// steer or an aside that old, sent unasked, could act on stale instructions, so it waits for a
// person's Retry. Postgres judges both ages against the times it wrote, as claimLapsed does.
const strandedAttempt = `d.state = 'pending'
	and coalesce(d.claimed_at, d.created_at) < now() - interval '1 minute'
	and d.created_at > now() - interval '1 hour'`

// RunDeliveryResume sends the delivery attempts a stopped request or process left pending: a
// targeted message's (a direct message, a broadcast recipient's, a person's Retry) and a comment
// mention's. It stops when ctx is done. A request's own send outlives a cut connection already
// (createStoredMessage), so what is left here is a process that died, or a write the database
// refused, between an attempt's claim and its settle.
//
// Each pass runs under deliveryResumeLease for one interval, so one process resumes per interval
// however many serve the database; leases is required, as it is of every leased loop
// (cmd/dispatch exits at boot if the lease pool cannot be opened). The loop runs on a server of
// its own over deps, the dependencies the routes serve requests with (routes.AppContext), so a
// resumed send goes to the same listener and its receipt to the same event broker as any other.
func RunDeliveryResume(ctx context.Context, deps Deps, interval time.Duration, leases duty.Leases) {
	s := &server{deps: deps}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			duty.Exclusive(ctx, leases, deliveryResumeLease, interval, func(ctx context.Context) {
				if err := s.resumeStrandedDeliveries(ctx); err != nil {
					slog.Warn("dispatch: resume stranded delivery attempts", "error", err)
				}
			})
		}
	}
}

// strandedMessageAttempt is one message attempt a pass resumes, with what its send carries.
type strandedMessageAttempt struct {
	messageID   string
	attempt     int
	delivery    string
	urgency     *string
	requestedBy []byte
}

// strandedCommentAttempt is one comment mention attempt a pass resumes.
type strandedCommentAttempt struct {
	commentID, target string
	attempt           int
	delivery          string
}

// resumeStrandedDeliveries is one pass: it reads both tables' candidates, each with one plain
// query drained before anything is sent, and then sends them deliveryResumeSenders at a time. No
// transaction or cursor is held across the sends: each send's claim takes its locks in its own
// order (the issue, then the message or comment, then the attempt), and a connection held here
// would make every send a second acquisition, which the pool refuses (store.ErrNestedAcquire).
//
// A send resumes its attempt or nothing. Another sender that took the attempt since it was read
// owns it, and a send whose claim or settle cannot be written leaves the row pending under the
// claim it took, which makes it a candidate again a minute later; the horizon bounds the tries.
// A send that meets a listener take-over tries again under the attempt's own number and key,
// renewing its claim before each try, for up to listenerTakeoverWindow (takeoverRetry). Any other
// listener failure, and a take-over's once that window has passed, settles the attempt failed
// with its receipt, as every send does, and a failed attempt is never a candidate.
func (s *server) resumeStrandedDeliveries(ctx context.Context) error {
	messages, err := s.strandedMessageAttempts(ctx)
	if err != nil {
		return err
	}
	comments, err := s.strandedCommentAttempts(ctx)
	if err != nil {
		return err
	}
	if len(messages)+len(comments) == 0 {
		return nil
	}
	slog.Info("dispatch: resuming stranded delivery attempts", "messages", len(messages), "comments", len(comments))
	// A tracking mark of each sender's own: the pool's one-connection guard is per mark, so
	// senders sharing one would trip it and race on its flag. They are taken before any sender
	// starts, so a refusal ends the pass with nothing in flight.
	senders := make([]context.Context, min(deliveryResumeSenders, len(messages)+len(comments)))
	for i := range senders {
		if senders[i], err = store.ForConcurrentRead(ctx); err != nil {
			return err
		}
	}
	sends := make(chan func(context.Context))
	var wait sync.WaitGroup
	for _, senderCtx := range senders {
		wait.Go(func() {
			for send := range sends {
				send(senderCtx)
			}
		})
	}
	for _, attempt := range messages {
		sends <- func(ctx context.Context) { s.resumeMessageAttempt(ctx, attempt) }
	}
	for _, attempt := range comments {
		sends <- func(ctx context.Context) { s.resumeCommentAttempt(ctx, attempt) }
	}
	close(sends)
	wait.Wait()
	return nil
}

// strandedMessageAttempts reads the message attempts a pass resumes. It leaves out an attempt
// whose message is on a closed issue, which recordPendingMessageDelivery refuses for good
// (requireOpenIssue), and an untargeted message's, which no send can have.
func (s *server) strandedMessageAttempts(ctx context.Context) ([]strandedMessageAttempt, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select d.message_id::text, d.attempt, d.delivery, d.urgency, d.requested_by
		from message_deliveries d
		join messages m on m.id = d.message_id
		where `+strandedAttempt+`
		  and m.target is not null
		  and not exists (select 1 from issues i where i.key = m.issue_key and i.closed_at is not null)
		order by d.created_at, d.message_id, d.attempt
		limit $1
	`, deliveryResumeBatch)
	if err != nil {
		return nil, fmt.Errorf("read stranded message attempts: %w", err)
	}
	attempts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (strandedMessageAttempt, error) {
		var attempt strandedMessageAttempt
		err := row.Scan(&attempt.messageID, &attempt.attempt, &attempt.delivery, &attempt.urgency, &attempt.requestedBy)
		return attempt, err
	})
	if err != nil {
		return nil, fmt.Errorf("read stranded message attempts: %w", err)
	}
	return attempts, nil
}

// strandedCommentAttempts reads the comment mention attempts a pass resumes. It leaves out the
// attempts whose claim would be refused for good: lockedComment refuses a comment whose issue is
// closed, or whose document is gone or no longer a project's own (ownerOpenStatus), and
// claimCommentDeliveryAttempt one whose mention row is gone.
func (s *server) strandedCommentAttempts(ctx context.Context) ([]strandedCommentAttempt, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select d.comment_id::text, d.target, d.attempt, d.delivery
		from comment_deliveries d
		join comments c on c.id = d.comment_id
		left join issues i on i.key = c.issue_key
		left join artifacts a on a.id = c.artifact_id
		where `+strandedAttempt+`
		  and exists (select 1 from comment_mentions cm where cm.comment_id = d.comment_id and cm.target = d.target)
		  and ((i.key is not null and i.closed_at is null) or (a.id is not null and a.issue_key is null))
		order by d.created_at, d.comment_id, d.target, d.attempt
		limit $1
	`, deliveryResumeBatch)
	if err != nil {
		return nil, fmt.Errorf("read stranded comment attempts: %w", err)
	}
	attempts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (strandedCommentAttempt, error) {
		var attempt strandedCommentAttempt
		err := row.Scan(&attempt.commentID, &attempt.target, &attempt.attempt, &attempt.delivery)
		return attempt, err
	})
	if err != nil {
		return nil, fmt.Errorf("read stranded comment attempts: %w", err)
	}
	return attempts, nil
}

// resumeMessageAttempt sends one stranded message attempt, bound to it, so it sends that attempt
// or nothing under the key `<message>:<mode>` its first send used. It carries the urgency the
// attempt recorded and the actor who asked for it, or the message's author for an attempt that
// recorded none (written before requested_by was).
func (s *server) resumeMessageAttempt(ctx context.Context, stranded strandedMessageAttempt) {
	message, err := s.loadMessage(ctx, s.deps.Store.Pool, "", stranded.messageID)
	if err != nil {
		slog.Warn("dispatch: resume a stranded message attempt", "message", stranded.messageID,
			"attempt", stranded.attempt, "error", err)
		return
	}
	actor := message.Author
	if stranded.requestedBy != nil {
		if err := json.Unmarshal(stranded.requestedBy, &actor); err != nil {
			slog.Warn("dispatch: resume a stranded message attempt", "message", stranded.messageID,
				"attempt", stranded.attempt, "error", fmt.Errorf("decode requested_by: %w", err))
			return
		}
	}
	_, err = s.deliverMessageResuming(ctx, message, stranded.delivery, stranded.urgency, actor, nil, stranded.attempt)
	if err != nil && !errors.Is(err, errAttemptNotPending) {
		slog.Warn("dispatch: resume a stranded message attempt", "message", stranded.messageID,
			"attempt", stranded.attempt, "error", err)
	}
}

// resumeCommentAttempt sends one stranded comment mention attempt in the resume claim mode, so it
// sends that attempt or nothing, under the key `<comment>:<target>:<mode>` its first send used,
// with the comment's own comment.created event as the frame. comment_deliveries records no
// requester, so the comment's author is the actor its receipt names.
func (s *server) resumeCommentAttempt(ctx context.Context, stranded strandedCommentAttempt) {
	comment, err := s.loadComment(ctx, s.deps.Store.Pool, stranded.commentID)
	if err != nil {
		slog.Warn("dispatch: resume a stranded mention attempt", "comment", stranded.commentID,
			"target", stranded.target, "attempt", stranded.attempt, "error", err)
		return
	}
	created, err := s.loadCommentCreatedEvent(ctx, s.deps.Store.Pool, stranded.commentID)
	if err != nil {
		slog.Warn("dispatch: resume a stranded mention attempt", "comment", stranded.commentID,
			"target", stranded.target, "attempt", stranded.attempt, "error", err)
		return
	}
	mention := ResolvedMention{Target: stranded.target, Delivery: stranded.delivery}
	claim := commentClaim{mode: claimResume, attempt: stranded.attempt}
	if _, err := s.deliverResolvedCommentMention(ctx, comment, created, mention, comment.Author, claim); err != nil {
		slog.Warn("dispatch: resume a stranded mention attempt", "comment", stranded.commentID,
			"target", stranded.target, "attempt", stranded.attempt, "error", err)
	}
}
