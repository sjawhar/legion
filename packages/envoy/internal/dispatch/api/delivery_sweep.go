package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The delivery sweep (INFRA-258). A write that delivers to sessions - a send to one session, the
// live view's model and thinking switch, a broadcast's recipients, an issue message to a session,
// a comment's and a document comment's mentions, and a retry from an agent card - claims each
// attempt before its send and settles it after. A process that dies between the two leaves the
// attempt pending under a claim no process holds; once that claim lapses (claimLapsed, a minute)
// the attempt is owed. Nobody has to ask again for it: every pass of the maintenance loop
// (RunSuggestionOutcomeSweep) sends each owed attempt, once, under its own attempt number and so
// its own idempotency key, as a repeat under the write's key does, so the listener drops a frame
// that did land (DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts).
//
// An attempt is owed when it is its message's, or its mention's, latest attempt, still pending,
// its claim lapsed, and opened over a lease ago: an unclaimed attempt the write itself opened (a
// broadcast recipient's, a mention's) is its live sender's for the first minute. A pending attempt
// a later retry overtook is that retry's to answer, so the sweep sends the attempt it found owed
// and no other (deliverMessageResuming and claimCommentDeliveryAttempt, bound to its number). A
// keyed send whose process died between its message's commit and its first attempt has no attempt
// at all; its key's recovery point names the message, and its first attempt is owed once the key's
// holder is gone.
//
// An owed attempt nothing can send is settled instead of sent: one whose issue closed, since every
// sender refuses a delivery there (requireOpenIssue, lockedComment), and one whose comment no
// longer holds its mention. It fails with that cause and appends the receipt its row owes, in a
// transaction of its own that takes the owner's lock first and judges the cause again under it, so
// an issue reopened meanwhile keeps its attempt owed; a keyed send with no attempt is settled as a
// failed first attempt. A pass settles every such attempt before it sends anything, and its reads
// of what to send leave them out, so however many accumulate - Legion closes an issue at its merge,
// and a deploy can strand a delivery in flight - they never crowd what can be sent out of a pass's
// batch, and are never picked again.
//
// Of two senders that both find an attempt owed - this sweep and a repeat under the write's key, or
// the sweeps of two processes - the one that claims it sends and the other finds it taken and
// sends nothing (deliverMessageResuming, claimCommentDeliveryAttempt).

// deliverySweepBatch bounds how many attempts of each kind one pass resumes, so a pass after a
// crash that stranded many is bounded too; the rest are the next pass's. Undeliverable attempts
// are read in batches of it too, until none is left.
const deliverySweepBatch = 100

// The causes an owed attempt nothing can send is settled with (settleUndeliverable).
const (
	undeliverableIssueClosed  = "not sent: the issue closed before this attempt was delivered"
	undeliverableDocumentGone = "not sent: the document is gone"
	undeliverableMentionGone  = "not sent: the comment no longer mentions this target"
)

// What an owed attempt is, spelled once for the reads that send it and the ones that settle it.
// Each reads the attempt row as d; $1 is DeliveryDuplicateWindow, in seconds.
const (
	owedMessageAttempt = `d.state = 'pending' and ` + claimLapsed + `
		and d.created_at < now() - interval '1 minute'
		and d.created_at > now() - make_interval(secs => $1)
		and d.attempt = (select max(attempt) from message_deliveries l where l.message_id = d.message_id)`
	// messageAttemptIssueClosed holds for an attempt of a message on an issue that closed.
	messageAttemptIssueClosed = `exists (
		select 1 from messages m join issues i on i.key = m.issue_key
		where m.id = d.message_id and i.closed_at is not null
	)`
	owedCommentAttempt = `d.state = 'pending' and ` + claimLapsed + `
		and d.created_at < now() - interval '1 minute'
		and d.created_at > now() - make_interval(secs => $1)
		and d.attempt = (
			select max(attempt) from comment_deliveries l where l.comment_id = d.comment_id and l.target = d.target
		)`
	// commentAttemptMentionGone holds for an attempt whose comment no longer holds its mention.
	commentAttemptMentionGone = `not exists (
		select 1 from comment_mentions cm where cm.comment_id = d.comment_id and cm.target = d.target
	)`
	// commentAttemptOwnerClosed holds for an attempt whose comment's owner refuses a delivery, as
	// ownerOpenStatus judges it: an issue that closed, or a document that is no longer a project's.
	commentAttemptOwnerClosed = `not exists (
		select 1 from comments c
		where c.id = d.comment_id and (
			exists (select 1 from issues i where i.key = c.issue_key and i.closed_at is null)
			or exists (select 1 from artifacts a where a.id = c.artifact_id and a.issue_key is null)
		)
	)`
	// owedKeyedSend is every keyed send whose key's holder is gone - it never answered, and its
	// lease lapsed or was released - with a targeted message and no attempt at all: the key as k,
	// its message as m, $1 the key's lease and $2 IdempotencyKeyTTL, in seconds. Its first two
	// conditions are the predicate of request_keys_owed_sends (migration 0118), the partial index
	// that keeps this read to the keyed sends still in flight.
	owedKeyedSend = `from request_keys k
		join messages m on m.id = (k.recovery ->> 'message_id')::uuid
		where k.recovery ? 'message_id' and k.status is null
		  and (k.leased_at is null or k.leased_at < now() - make_interval(secs => $1))
		  and k.created_at > now() - make_interval(secs => $2)
		  and m.target is not null
		  and not exists (select 1 from message_deliveries d where d.message_id = m.id)`
	// keyedSendIssueClosed holds for a keyed send whose message is on an issue that closed.
	keyedSendIssueClosed = `exists (select 1 from issues i where i.key = m.issue_key and i.closed_at is not null)`
	// owedKeyedSendsQuery reads the keyed sends the sweep opens a first attempt for, oldest first,
	// $3 at a time.
	owedKeyedSendsQuery = `select m.id::text, k.recovery ->> 'delivery' ` + owedKeyedSend + `
		  and not ` + keyedSendIssueClosed + `
		order by k.created_at
		limit $3`
)

// owedDelivery is one attempt the sweep resumes, bound to its number: a message's, or a comment
// mention's.
type owedDelivery struct {
	messageID string
	attempt   int
	delivery  string
	// requestedBy is who asked for the message attempt, nil for one the write opened before any
	// row recorded a requester; the attempt's own receipt names them.
	requestedBy *model.Actor
	commentID   string
	target      string
}

// resumeOwedDeliveries is one pass of the delivery sweep: it settles every owed attempt nothing can
// send, then reads every other owed attempt and sends each, broadcastDeliveryWorkers at a time,
// returning once every one has settled. The sends run on a context the pass's own cancellation does
// not reach and are counted in the server's background wait, so a shutdown that cuts the pass short
// lets the sends it started finish. Only a read of what is owed fails it; a send the listener
// refuses is recorded on its attempt, and an attempt it could not settle is logged and left to the
// next pass.
func (s *server) resumeOwedDeliveries(ctx context.Context) error {
	if s.deps.Envoy == nil {
		return nil
	}
	s.detached.start()
	defer s.detached.done()
	if err := s.settleUndeliverable(ctx); err != nil {
		return err
	}
	owed, err := s.owedDeliveries(ctx)
	if err != nil {
		return err
	}
	if len(owed) == 0 {
		return nil
	}
	slog.Info("dispatch: resume deliveries a process left owed", "attempts", len(owed))
	sending := context.WithoutCancel(ctx)
	pending := make(chan owedDelivery)
	var wait sync.WaitGroup
	for range min(broadcastDeliveryWorkers, len(owed)) {
		wait.Go(func() {
			for delivery := range pending {
				// A mark of this worker's own: workers sharing one would refuse each other a
				// connection (store.ErrNestedAcquire).
				worker, err := store.ForConcurrentRead(sending)
				if err == nil {
					err = s.resumeOwedDelivery(worker, delivery)
				}
				if err != nil {
					slog.Error("dispatch: resume an owed delivery", "message", delivery.messageID,
						"comment", delivery.commentID, "target", delivery.target, "attempt", delivery.attempt, "error", err)
				}
			}
		})
	}
	for _, delivery := range owed {
		pending <- delivery
	}
	close(pending)
	wait.Wait()
	return nil
}

// resumeOwedDelivery sends one owed attempt, as a repeat under its write's key sends it, bound to
// its number: an attempt a live sender took in the meantime, or that settled, is left as it is, and
// no attempt of the sweep's own is opened.
func (s *server) resumeOwedDelivery(ctx context.Context, owed owedDelivery) error {
	if owed.commentID != "" {
		comment, err := s.loadComment(ctx, s.deps.Store.Pool, owed.commentID)
		if err != nil {
			return err
		}
		created, err := s.loadCommentCreatedEvent(ctx, s.deps.Store.Pool, comment.ID)
		if err != nil {
			return err
		}
		_, err = s.deliverResolvedCommentMention(ctx, comment, created,
			ResolvedMention{Target: owed.target, Delivery: owed.delivery}, comment.Author, owed.attempt)
		return err
	}
	message, err := s.loadMessage(ctx, s.deps.Store.Pool, "", owed.messageID)
	if err != nil {
		return err
	}
	actor := message.Author
	if owed.requestedBy != nil {
		actor = *owed.requestedBy
	}
	if _, err := s.deliverMessageResuming(ctx, message, owed.delivery, nil, actor, nil, owed.attempt); err != nil &&
		!errors.Is(err, errAttemptNotPending) {
		return err
	}
	return nil
}

// owedDeliveries reads what the sweep resumes: the owed message attempts, the first attempt of
// every keyed send that has none, and the owed comment mention attempts, oldest first, less every
// one settleUndeliverable settles.
func (s *server) owedDeliveries(ctx context.Context) ([]owedDelivery, error) {
	window := contracts.DeliveryDuplicateWindow.Seconds()
	var owed []owedDelivery
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select d.message_id::text, d.attempt, d.delivery, d.requested_by
		from message_deliveries d
		where `+owedMessageAttempt+` and not `+messageAttemptIssueClosed+`
		order by d.created_at
		limit $2
	`, window, deliverySweepBatch)
	if err != nil {
		return nil, fmt.Errorf("read the owed message attempts: %w", err)
	}
	messages, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (owedDelivery, error) {
		var delivery owedDelivery
		var requestedBy []byte
		if err := row.Scan(&delivery.messageID, &delivery.attempt, &delivery.delivery, &requestedBy); err != nil {
			return owedDelivery{}, err
		}
		if requestedBy != nil {
			delivery.requestedBy = &model.Actor{}
			if err := json.Unmarshal(requestedBy, delivery.requestedBy); err != nil {
				return owedDelivery{}, fmt.Errorf("decode delivery requester: %w", err)
			}
		}
		return delivery, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the owed message attempts: %w", err)
	}
	owed = append(owed, messages...)

	rows, err = s.deps.Store.Pool.Query(ctx, owedKeyedSendsQuery,
		s.keyLease().Seconds(), contracts.IdempotencyKeyTTL.Seconds(), deliverySweepBatch)
	if err != nil {
		return nil, fmt.Errorf("read the keyed sends with no attempt: %w", err)
	}
	unattempted, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (owedDelivery, error) {
		delivery := owedDelivery{attempt: 1}
		err := row.Scan(&delivery.messageID, &delivery.delivery)
		return delivery, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the keyed sends with no attempt: %w", err)
	}
	owed = append(owed, unattempted...)

	rows, err = s.deps.Store.Pool.Query(ctx, `
		select d.comment_id::text, d.target, d.attempt, d.delivery
		from comment_deliveries d
		where `+owedCommentAttempt+`
		  and not `+commentAttemptMentionGone+` and not `+commentAttemptOwnerClosed+`
		order by d.created_at
		limit $2
	`, window, deliverySweepBatch)
	if err != nil {
		return nil, fmt.Errorf("read the owed comment mention attempts: %w", err)
	}
	comments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (owedDelivery, error) {
		var delivery owedDelivery
		err := row.Scan(&delivery.commentID, &delivery.target, &delivery.attempt, &delivery.delivery)
		return delivery, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the owed comment mention attempts: %w", err)
	}
	return append(owed, comments...), nil
}

// undeliverableAttempt is one owed attempt nothing can send, and the cause it is settled with: a
// message's attempt, a keyed send's missing first attempt (attempt 0, delivery its mode), or a
// comment mention's attempt.
type undeliverableAttempt struct {
	messageID string
	attempt   int
	delivery  string
	commentID string
	target    string
	cause     string
}

// settleUndeliverable settles every owed attempt nothing can send, reading deliverySweepBatch of
// each kind at a time until a read comes back short, or a round settles none of what it read. An
// attempt it cannot settle is logged and left out of what the pass sends all the same.
func (s *server) settleUndeliverable(ctx context.Context) error {
	total := 0
	for {
		undeliverable, full, err := s.undeliverableDeliveries(ctx)
		if err != nil {
			return err
		}
		settled := 0
		for _, attempt := range undeliverable {
			done, err := s.settleUndeliverableAttempt(ctx, attempt)
			if err != nil {
				slog.Error("dispatch: settle an owed delivery nothing can send", "message", attempt.messageID,
					"comment", attempt.commentID, "target", attempt.target, "attempt", attempt.attempt, "error", err)
				continue
			}
			if done {
				settled++
			}
		}
		total += settled
		if !full || settled == 0 {
			if total > 0 {
				slog.Info("dispatch: settled owed deliveries nothing can send", "attempts", total)
			}
			return nil
		}
	}
}

// undeliverableDeliveries reads, oldest first, up to deliverySweepBatch of each kind of owed attempt
// nothing can send, and whether any kind filled its batch.
func (s *server) undeliverableDeliveries(ctx context.Context) ([]undeliverableAttempt, bool, error) {
	window := contracts.DeliveryDuplicateWindow.Seconds()
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select d.message_id::text, d.attempt
		from message_deliveries d
		where `+owedMessageAttempt+` and `+messageAttemptIssueClosed+`
		order by d.created_at
		limit $2
	`, window, deliverySweepBatch)
	if err != nil {
		return nil, false, fmt.Errorf("read the undeliverable message attempts: %w", err)
	}
	messages, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (undeliverableAttempt, error) {
		attempt := undeliverableAttempt{cause: undeliverableIssueClosed}
		return attempt, row.Scan(&attempt.messageID, &attempt.attempt)
	})
	if err != nil {
		return nil, false, fmt.Errorf("read the undeliverable message attempts: %w", err)
	}

	rows, err = s.deps.Store.Pool.Query(ctx, `
		select m.id::text, k.recovery ->> 'delivery' `+owedKeyedSend+` and `+keyedSendIssueClosed+`
		order by k.created_at
		limit $3
	`, s.keyLease().Seconds(), contracts.IdempotencyKeyTTL.Seconds(), deliverySweepBatch)
	if err != nil {
		return nil, false, fmt.Errorf("read the undeliverable keyed sends: %w", err)
	}
	keyed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (undeliverableAttempt, error) {
		attempt := undeliverableAttempt{cause: undeliverableIssueClosed}
		return attempt, row.Scan(&attempt.messageID, &attempt.delivery)
	})
	if err != nil {
		return nil, false, fmt.Errorf("read the undeliverable keyed sends: %w", err)
	}

	rows, err = s.deps.Store.Pool.Query(ctx, `
		select d.comment_id::text, d.target, d.attempt, `+commentAttemptMentionGone+`,
		       exists (select 1 from comments o where o.id = d.comment_id and o.issue_key is not null)
		from comment_deliveries d
		where `+owedCommentAttempt+`
		  and (`+commentAttemptMentionGone+` or `+commentAttemptOwnerClosed+`)
		order by d.created_at
		limit $2
	`, window, deliverySweepBatch)
	if err != nil {
		return nil, false, fmt.Errorf("read the undeliverable comment mention attempts: %w", err)
	}
	mentions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (undeliverableAttempt, error) {
		var attempt undeliverableAttempt
		var gone, onIssue bool
		if err := row.Scan(&attempt.commentID, &attempt.target, &attempt.attempt, &gone, &onIssue); err != nil {
			return undeliverableAttempt{}, err
		}
		switch {
		case gone:
			attempt.cause = undeliverableMentionGone
		case onIssue:
			attempt.cause = undeliverableIssueClosed
		default:
			attempt.cause = undeliverableDocumentGone
		}
		return attempt, nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("read the undeliverable comment mention attempts: %w", err)
	}
	full := len(messages) == deliverySweepBatch || len(keyed) == deliverySweepBatch || len(mentions) == deliverySweepBatch
	return append(append(messages, keyed...), mentions...), full, nil
}

// settleUndeliverableAttempt settles one attempt nothing can send, failed with its cause, and
// reports whether it did: one someone claimed or settled meanwhile, or whose cause no longer holds
// under its owner's lock - an issue reopened - is left as it is.
func (s *server) settleUndeliverableAttempt(ctx context.Context, undeliverable undeliverableAttempt) (bool, error) {
	if undeliverable.commentID != "" {
		return s.settleUndeliverableMention(ctx, undeliverable)
	}
	message, err := s.loadMessage(ctx, s.deps.Store.Pool, "", undeliverable.messageID)
	if err != nil {
		return false, err
	}
	if undeliverable.attempt == 0 {
		return s.settleUndeliverableMessage(ctx, message, func(ctx context.Context, tx pgx.Tx) (model.MessageDelivery, error) {
			// The message's own lock, which a first attempt's opener holds
			// (openFirstMessageDeliveryAttempt), so the two never both open it.
			if err := tx.QueryRow(ctx, `select target from messages where id = $1 for no key update`,
				message.ID).Scan(new(*string)); err != nil {
				return model.MessageDelivery{}, err
			}
			return scanMessageDelivery(tx.QueryRow(ctx, `
				insert into message_deliveries (message_id, attempt, delivery, session_id, state, error, claimed_at, requested_by)
				select m.id, 1, $2, '', 'failed', $3, now(), m.author
				from messages m
				where m.id = $1 and not exists (select 1 from message_deliveries d where d.message_id = m.id)
				  and `+keyedSendIssueClosed+`
				returning `+messageDeliveryColumns,
				message.ID, undeliverable.delivery, undeliverable.cause))
		})
	}
	return s.settleUndeliverableMessage(ctx, message, func(ctx context.Context, tx pgx.Tx) (model.MessageDelivery, error) {
		// claimed_at moves with the settle, as a superseding retry's does, so a sender that comes
		// back to this attempt finds it no longer its own.
		return scanMessageDelivery(tx.QueryRow(ctx, `
			update message_deliveries d set state = 'failed', error = $3, claimed_at = now()
			where d.message_id = $1 and d.attempt = $2 and d.state = 'pending' and `+claimLapsed+`
			  and `+messageAttemptIssueClosed+`
			returning `+messageDeliveryColumns,
			message.ID, undeliverable.attempt, undeliverable.cause))
	})
}

// settleUndeliverableMessage runs fail - the statement that fails one of message's attempts, or
// pgx.ErrNoRows when it no longer may - in a transaction that takes the message's issue's lock
// first, the order every delivery transaction takes it in (settleDeliveryAttempt), and appends
// the failed attempt's receipt.
func (s *server) settleUndeliverableMessage(
	ctx context.Context, message model.Message, fail func(context.Context, pgx.Tx) (model.MessageDelivery, error),
) (bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := s.deps.Events.LockOwners(ctx, tx,
		messageDeliveryReceipt(message, message.Author, model.MessageDelivery{MessageID: message.ID}, "")); err != nil {
		return false, err
	}
	failed, err := fail(ctx, tx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	actor := message.Author
	if failed.RequestedBy != nil {
		actor = *failed.RequestedBy
	}
	receipt, err := s.appendEvent(ctx, tx, messageDeliveryReceipt(message, actor, failed, ""))
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	s.publish(receipt)
	return true, nil
}

// settleUndeliverableMention fails a comment mention's owed attempt with its cause and appends its
// receipt, under the comment's owner's lock, taken first.
func (s *server) settleUndeliverableMention(ctx context.Context, undeliverable undeliverableAttempt) (bool, error) {
	comment, err := s.loadComment(ctx, s.deps.Store.Pool, undeliverable.commentID)
	if err != nil {
		return false, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := s.deps.Events.LockOwners(ctx, tx, commentDeliveryReceipt(comment, comment.Author,
		model.CommentDelivery{Target: undeliverable.target, Attempt: undeliverable.attempt})); err != nil {
		return false, err
	}
	failed, err := scanCommentDelivery(tx.QueryRow(ctx, `
		update comment_deliveries d set state = 'failed', error = $4, claimed_at = now()
		where d.comment_id = $1 and d.target = $2 and d.attempt = $3 and d.state = 'pending' and `+claimLapsed+`
		  and (`+commentAttemptMentionGone+` or `+commentAttemptOwnerClosed+`)
		returning `+commentDeliveryColumns,
		comment.ID, undeliverable.target, undeliverable.attempt, undeliverable.cause))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	receipt, err := s.appendEvent(ctx, tx, commentDeliveryReceipt(comment, comment.Author, failed))
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	s.publish(receipt)
	return true, nil
}
